package handlers

import (
	"context"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
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
