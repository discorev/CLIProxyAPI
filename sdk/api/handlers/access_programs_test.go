package handlers

import (
	"context"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

func TestResolveAccessProgramModel(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("test-access-program-blue", "codex", []*registry.ModelInfo{
		{ID: "gpt-access-test"},
		{ID: "gpt-access-test-daybreak-blue", MetadataModelID: "gpt-access-test", AccessProgram: "daybreak_blue"},
	})
	t.Cleanup(func() { reg.UnregisterClient("test-access-program-blue") })
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, coreauth.NewManager(nil, nil, nil))

	for _, tt := range []struct {
		name  string
		model string
		body  string
		want  string
	}{
		{name: "blue routes to the variant", model: "gpt-access-test", body: `{"access_programs":{"cyber":"daybreak_blue"}}`, want: "gpt-access-test-daybreak-blue"},
		{name: "suffix kept", model: "gpt-access-test(high)", body: `{"access_programs":{"cyber":"Daybreak_Blue"}}`, want: "gpt-access-test-daybreak-blue(high)"},
		{name: "standard passes through", model: "gpt-access-test", body: `{"access_programs":{"cyber":"standard"}}`, want: "gpt-access-test"},
		{name: "absent passes through", model: "gpt-access-test", body: `{"input":"hi"}`, want: "gpt-access-test"},
		{name: "variant passes through", model: "gpt-access-test-daybreak-blue", body: `{"access_programs":{"cyber":"daybreak_red"}}`, want: "gpt-access-test-daybreak-blue"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, errMsg := handler.resolveAccessProgramModel(tt.model, []byte(tt.body))
			if errMsg != nil || got != tt.want {
				t.Fatalf("resolveAccessProgramModel = %q, %v; want %q", got, errMsg, tt.want)
			}
		})
	}

	_, errMsg := handler.resolveAccessProgramModel("gpt-access-test", []byte(`{"access_programs":{"cyber":"daybreak_red"}}`))
	if errMsg == nil || errMsg.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing variant error = %+v, want 400", errMsg)
	}
	if code := gjson.Get(errMsg.Error.Error(), "error.code").String(); code != "model_not_found" {
		t.Fatalf("missing variant error code = %q, body %s", code, errMsg.Error.Error())
	}
}

// A program no credential offers is rejected before any credential is
// selected, so the request is never silently downgraded.
func TestExecuteRejectsAccessProgramWithoutVariant(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("test-access-program-standard", "codex", []*registry.ModelInfo{{ID: "gpt-access-standard-test"}})
	t.Cleanup(func() { reg.UnregisterClient("test-access-program-standard") })
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, coreauth.NewManager(nil, nil, nil))
	body := []byte(`{"model":"gpt-access-standard-test","access_programs":{"cyber":"daybreak_blue"},"input":"hi"}`)

	_, _, errMsg := handler.ExecuteWithAuthManager(context.Background(), "openai-response", "gpt-access-standard-test", body, "")
	if errMsg == nil || gjson.Get(errMsg.Error.Error(), "error.code").String() != "model_not_found" {
		t.Fatalf("Execute error = %+v, want model_not_found", errMsg)
	}
	_, _, errChan := handler.ExecuteStreamWithAuthManager(context.Background(), "openai-response", "gpt-access-standard-test", body, "")
	streamErr := <-errChan
	if streamErr == nil || gjson.Get(streamErr.Error.Error(), "error.code").String() != "model_not_found" {
		t.Fatalf("stream error = %+v, want model_not_found", streamErr)
	}
}

// An ordinary model whose ID matches a variant's, such as an auth file alias
// routing to the base, is not proof that any credential's upstream model list
// includes the program, and must not share program-specific routing with a
// genuine variant.
func TestResolveAccessProgramModelIgnoresOrdinaryVariantIDs(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("test-access-program-alias", "codex", []*registry.ModelInfo{
		{ID: "gpt-access-alias-test"},
		{ID: "gpt-access-alias-test-daybreak-blue", MetadataModelID: "gpt-access-alias-test"},
	})
	t.Cleanup(func() { reg.UnregisterClient("test-access-program-alias") })
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, coreauth.NewManager(nil, nil, nil))
	body := []byte(`{"access_programs":{"cyber":"daybreak_blue"}}`)

	if got, errMsg := handler.resolveAccessProgramModel("gpt-access-alias-test", body); errMsg == nil || gjson.Get(errMsg.Error.Error(), "error.code").String() != "model_not_found" {
		t.Fatalf("alias only = %q, %+v; want model_not_found", got, errMsg)
	}

	reg.RegisterClient("test-access-program-alias-genuine", "codex", []*registry.ModelInfo{
		{ID: "gpt-access-alias-test"},
		{ID: "gpt-access-alias-test-daybreak-blue", MetadataModelID: "gpt-access-alias-test", AccessProgram: "daybreak_blue"},
	})
	t.Cleanup(func() { reg.UnregisterClient("test-access-program-alias-genuine") })
	if got, errMsg := handler.resolveAccessProgramModel("gpt-access-alias-test", body); errMsg == nil || gjson.Get(errMsg.Error.Error(), "error.code").String() != "model_not_found" {
		t.Fatalf("alias colliding with a variant = %q, %+v; want model_not_found", got, errMsg)
	}

	reg.UnregisterClient("test-access-program-alias")
	if got, errMsg := handler.resolveAccessProgramModel("gpt-access-alias-test", body); errMsg != nil || got != "gpt-access-alias-test-daybreak-blue" {
		t.Fatalf("genuine variant only = %q, %+v; want the variant", got, errMsg)
	}
}

// A model router's built-in Codex provider route is resolved like a direct
// request: by its target model, keeping the provider.
func TestResolveAccessProgramRouteForCodexProviderRoutes(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("test-access-program-route", "codex", []*registry.ModelInfo{
		{ID: "gpt-access-route-test"},
		{ID: "gpt-access-route-test-daybreak-blue", MetadataModelID: "gpt-access-route-test", AccessProgram: "daybreak_blue"},
	})
	t.Cleanup(func() { reg.UnregisterClient("test-access-program-route") })
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, coreauth.NewManager(nil, nil, nil))
	body := []byte(`{"access_programs":{"cyber":"daybreak_blue"}}`)

	decision := modelRouteDecision{Provider: "codex", Model: "gpt-access-route-test"}
	if _, errMsg := handler.resolveAccessProgramRoute("client-model", "client-model", &decision, body); errMsg != nil || decision.Model != "gpt-access-route-test-daybreak-blue" {
		t.Fatalf("target model route = %+v, %+v; want the variant", decision, errMsg)
	}
	decision = modelRouteDecision{Provider: "codex"}
	if _, errMsg := handler.resolveAccessProgramRoute("gpt-access-route-test", "gpt-access-route-test", &decision, body); errMsg != nil || decision.Model != "gpt-access-route-test-daybreak-blue" {
		t.Fatalf("requested model route = %+v, %+v; want the variant", decision, errMsg)
	}
	decision = modelRouteDecision{Provider: "claude", Model: "gpt-access-route-test"}
	if _, errMsg := handler.resolveAccessProgramRoute("client-model", "client-model", &decision, body); errMsg != nil || decision.Model != "gpt-access-route-test" {
		t.Fatalf("other provider route = %+v, %+v; want unchanged", decision, errMsg)
	}
}

// A router-chosen Codex route for a program no credential offers fails before
// credential selection on both entry points.
func TestExecuteRejectsAccessProgramWithoutVariantOnCodexProviderRoute(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("test-access-program-route-standard", "codex", []*registry.ModelInfo{{ID: "gpt-access-route-standard-test"}})
	t.Cleanup(func() { reg.UnregisterClient("test-access-program-route-standard") })
	host := &handlerDirectExecutorRouteHost{}
	host.hasRouters = true
	host.route = func(context.Context, pluginapi.ModelRouteRequest) (pluginapi.ModelRouteResponse, bool) {
		return pluginapi.ModelRouteResponse{Handled: true, TargetKind: pluginapi.ModelRouteTargetProvider, Target: "codex", TargetModel: "gpt-access-route-standard-test"}, true
	}
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, coreauth.NewManager(nil, nil, nil))
	handler.SetModelRouterHost(host)
	body := []byte(`{"model":"client-model","access_programs":{"cyber":"daybreak_blue"},"input":"hi"}`)

	_, _, errMsg := handler.ExecuteWithAuthManager(context.Background(), "openai-response", "client-model", body, "")
	if errMsg == nil || gjson.Get(errMsg.Error.Error(), "error.code").String() != "model_not_found" {
		t.Fatalf("Execute error = %+v, want model_not_found", errMsg)
	}
	_, _, errChan := handler.ExecuteStreamWithAuthManager(context.Background(), "openai-response", "client-model", body, "")
	streamErr := <-errChan
	if streamErr == nil || gjson.Get(streamErr.Error.Error(), "error.code").String() != "model_not_found" {
		t.Fatalf("stream error = %+v, want model_not_found", streamErr)
	}
}

// A compact request is resolved like any other, so a program no credential
// offers is rejected before credential selection.
func TestExecuteCompactRejectsAccessProgramWithoutVariant(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("test-access-program-compact", "codex", []*registry.ModelInfo{{ID: "gpt-access-compact-test"}})
	t.Cleanup(func() { reg.UnregisterClient("test-access-program-compact") })
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, coreauth.NewManager(nil, nil, nil))
	body := []byte(`{"model":"gpt-access-compact-test","access_programs":{"cyber":"daybreak_blue"},"input":"hi"}`)

	_, _, errMsg := handler.ExecuteWithAuthManager(context.Background(), "openai-response", "gpt-access-compact-test", body, "responses/compact")
	if errMsg == nil || gjson.Get(errMsg.Error.Error(), "error.code").String() != "model_not_found" {
		t.Fatalf("compact error = %+v, want model_not_found", errMsg)
	}
}
