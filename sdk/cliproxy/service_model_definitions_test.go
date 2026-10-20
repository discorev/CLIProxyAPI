package cliproxy

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	codexmodels "github.com/router-for-me/CLIProxyAPI/v8/internal/client/codex/models"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func defineModels(auth *coreauth.Auth, raw string) {
	definitions := coreauth.ParseModelDefinitions(json.RawMessage(raw))
	coreauth.SetModelDefinitionsAttribute(auth, definitions)
	coreauth.SetOAuthModelAliasesAttribute(auth, coreauth.ModelDefinitionAliases(definitions))
}

func TestOverlayModelDefinitionPartialFullAndNull(t *testing.T) {
	webSearch := true
	base := &ModelInfo{
		ID: "m", Object: "model", DisplayName: "Base", ContextLength: 100, MaxCompletionTokens: 10,
		SupportedParameters: []string{"tools"}, SupportedInputModalities: []string{"text"},
		NativeCapabilities: &registry.NativeCapabilities{WebSearch: &webSearch},
		Thinking:           &registry.ThinkingSupport{Min: 1, Max: 2, Levels: []string{"low"}},
	}
	snapshot := *base
	snapshotThinking := *base.Thinking

	partial := overlayModelDefinition(base, json.RawMessage(`{"id":"M","display_name":"New","context_length":0,"thinking":null,"restricted_access":true}`))
	if partial.ID != "m" || partial.DisplayName != "New" || partial.ContextLength != 0 || partial.MaxContextLength != 0 {
		t.Fatalf("present fields not applied: %+v", partial)
	}
	if partial.MaxCompletionTokens != 10 || partial.Object != "model" || !reflect.DeepEqual(partial.SupportedParameters, []string{"tools"}) ||
		partial.NativeCapabilities == nil || partial.RestrictedAccess {
		t.Fatalf("absent fields not kept (or restricted_access applied): %+v", partial)
	}
	if partial.Thinking != nil || !partial.ExplicitThinking {
		t.Fatalf("thinking: null did not clear thinking: %+v", partial.Thinking)
	}

	full := overlayModelDefinition(base, json.RawMessage(`{"id":"m","object":"model","owned_by":"me","type":"claude","display_name":"Full",`+
		`"context_length":2000,"max_completion_tokens":3000,"supported_parameters":["x"],"supportedInputModalities":["text","image"],`+
		`"supportedOutputModalities":["text"],"thinking":{"levels":["LOW","high"]},"native_capabilities":{"web_search":false}}`))
	if full.ContextLength != 2000 || full.MaxContextLength != 2000 || full.MaxCompletionTokens != 3000 || full.OwnedBy != "me" ||
		!reflect.DeepEqual(full.SupportedParameters, []string{"x"}) || !reflect.DeepEqual(full.SupportedInputModalities, []string{"text", "image"}) ||
		!full.ExplicitInputModalities || full.NativeCapabilities == nil || *full.NativeCapabilities.WebSearch {
		t.Fatalf("full entry not applied: %+v", full)
	}
	if full.Thinking == nil || !reflect.DeepEqual(full.Thinking.Levels, []string{"low", "high"}) || full.Thinking.Max != 0 {
		t.Fatalf("thinking not replaced whole and normalized: %+v", full.Thinking)
	}

	if base.DisplayName != snapshot.DisplayName || !reflect.DeepEqual(base.SupportedParameters, []string{"tools"}) ||
		!reflect.DeepEqual(*base.Thinking, snapshotThinking) || !*base.NativeCapabilities.WebSearch {
		t.Fatalf("overlay mutated the base model: %+v", base)
	}
	if invalid := overlayModelDefinition(base, json.RawMessage(`{"context_length":"big"}`)); invalid != base {
		t.Fatalf("invalid entry applied: %+v", invalid)
	}
}

func TestDefinedModelInheritance(t *testing.T) {
	claudeCatalog := []*ModelInfo{
		{ID: "claude-fable-5", Created: 1, ContextLength: 5},
		{ID: "claude-fable-5-1", Created: 2, ContextLength: 7, DisplayName: "Fable 5.1", Description: "d", Version: "v",
			Thinking: &registry.ThinkingSupport{Levels: []string{"low"}}},
		{ID: "claude-opus-5", Created: 3, ContextLength: 9},
	}
	fable := appendDefinedModel("claude", claudeCatalog, nil, nil, "claude-fable-6", "")[0]
	if fable.ID != "claude-fable-6" || fable.ContextLength != 7 || fable.DisplayName != "" || fable.Description != "" || fable.Version != "" ||
		fable.MetadataModelID != "" || fable.Thinking == claudeCatalog[1].Thinking || !reflect.DeepEqual(fable.Thinking, claudeCatalog[1].Thinking) {
		t.Fatalf("Claude model did not inherit the newest same-family catalog model: %+v", fable)
	}
	routed := appendDefinedModel("claude", claudeCatalog, nil, nil, "my-fable", "claude-fable-6")[0]
	if routed.ID != "my-fable" || routed.MetadataModelID != "claude-fable-6" || routed.ContextLength != 7 {
		t.Fatalf("upstream_id did not pick the family or the upstream name: %+v", routed)
	}
	secret := appendDefinedModel("claude", claudeCatalog, nil, nil, "claude-mythos-9", "")[0]
	want := claudeDefaultModel()
	want.ID = "claude-mythos-9"
	if !reflect.DeepEqual(*secret, want) {
		t.Fatalf("Claude model without a family = %+v, want defaults %+v", secret, want)
	}

	codexCatalog := []*ModelInfo{
		{ID: "gpt-a", Created: 1, MaxCompletionTokens: 1},
		{ID: "gpt-b", Created: 2, Object: "model", OwnedBy: "openai", Type: "openai", MaxCompletionTokens: 99, ContextLength: 272000,
			DisplayName: "GPT B", Description: "b", SupportedParameters: []string{"tools"}},
	}
	// A catalog ID the credential does not resolve inherits that exact entry.
	exact := appendDefinedModel("claude", claudeCatalog, nil, nil, "claude-fable-5", "")[0]
	if exact.ContextLength != 5 || exact.Created != 1 || exact.MetadataModelID != "" {
		t.Fatalf("catalog ID did not inherit its own entry: %+v", exact)
	}
	exactRoute := appendDefinedModel("claude", claudeCatalog, nil, nil, "mine", "claude-fable-5")[0]
	if exactRoute.ID != "mine" || exactRoute.MetadataModelID != "claude-fable-5" || exactRoute.ContextLength != 5 {
		t.Fatalf("upstream_id did not inherit the upstream catalog entry: %+v", exactRoute)
	}

	codex := appendDefinedModel("codex", codexCatalog, nil, nil, "gpt-secret", "")[0]
	if codex.ID != "gpt-secret" || codex.Version != "gpt-secret" || codex.MaxCompletionTokens != 99 || codex.ContextLength != 272000 ||
		codex.DisplayName != "" || codex.Description != "" || !reflect.DeepEqual(codex.SupportedParameters, []string{"tools"}) {
		t.Fatalf("Codex model did not inherit the template: %+v", codex)
	}

	if codexExact := appendDefinedModel("codex", codexCatalog, nil, nil, "gpt-a", "")[0]; codexExact.MaxCompletionTokens != 1 {
		t.Fatalf("Codex catalog ID did not inherit its own entry: %+v", codexExact)
	}

	resolved := []*ModelInfo{{ID: "kimi-k2", OwnedBy: "moonshot", Type: "kimi"}}
	kimi := appendDefinedModel("kimi", nil, nil, resolved, "kimi-secret", "")
	if len(kimi) != 2 || !reflect.DeepEqual(*kimi[1], ModelInfo{ID: "kimi-secret", Object: "model", OwnedBy: "moonshot", Type: "kimi"}) {
		t.Fatalf("other provider model = %+v", kimi[len(kimi)-1])
	}
	if bare := appendDefinedModel("xai", nil, nil, nil, "grok-secret", "")[0]; bare.OwnedBy != "xai" || bare.Type != "xai" || bare.Thinking != nil {
		t.Fatalf("other provider without models = %+v", bare)
	}
	if again := appendDefinedModel("kimi", nil, nil, resolved, "KIMI-K2", ""); len(again) != 1 {
		t.Fatalf("resolved model defined twice: %v", modelIDs(again))
	}
}

func TestModelDefinitionPrecedence(t *testing.T) {
	auth := restrictedTestAuth("claude", "claude-definition-precedence-test")
	service := discoveryTestService(t, auth)
	catalogModel := registry.GetClaudeModels()[0]
	const discoveredID = "claude-discovery-test-9"
	service.restrictedAccess.entries = map[string]*restrictedModelAccessEntry{auth.ID: {
		identity: restrictedModelIdentity(auth),
		upstream: []coreauth.UpstreamModel{claudeUpstreamObject(discoveredID, "Discovered")},
	}}
	service.cfg.OAuthSettings = map[string][]config.OAuthModelSetting{"claude": {
		{Name: catalogModel.ID, DisplayName: "Settings C", MaxCompletionTokens: 111},
		{Name: discoveredID, DisplayName: "Settings D", ContextLength: 222},
	}}
	defineModels(auth, `[{"id":"`+catalogModel.ID+`","display_name":"Auth C"},{"id":"`+discoveredID+`","context_length":333}]`)

	service.registerModelsForAuth(context.Background(), auth)
	registered := GlobalModelRegistry().GetModelsForClient(auth.ID)
	curated := findModel(registered, catalogModel.ID)
	if curated == nil || curated.DisplayName != "Auth C" || curated.MaxCompletionTokens != 111 || curated.ContextLength != catalogModel.ContextLength {
		t.Fatalf("catalog model precedence wrong (want auth name, settings tokens, catalog context): %+v", curated)
	}
	discovered := findModel(registered, discoveredID)
	if discovered == nil || discovered.DisplayName != "Settings D" || discovered.ContextLength != 333 || discovered.MaxContextLength != 333 ||
		discovered.Thinking == nil || discovered.Thinking.Max != discoveredClaudeThinkingMax {
		t.Fatalf("discovered model precedence wrong (want settings name, auth context, discovered thinking): %+v", discovered)
	}
}

func TestDefinedModelsBypassRestrictedAndExcludedWins(t *testing.T) {
	auth := restrictedTestAuth("claude", "claude-definition-restricted-test")
	service := discoveryTestService(t, auth)
	restrictedID := registry.GetClaudeModels()[0].ID
	service.restrictedAccess.restrictedIDs = func(string) map[string]struct{} { return map[string]struct{}{restrictedID: {}} }
	service.registerModelsForAuth(context.Background(), auth)
	if containsModelID(GlobalModelRegistry().GetModelsForClient(auth.ID), restrictedID) {
		t.Fatalf("restricted model %s registered without a grant", restrictedID)
	}

	auth.Attributes["excluded_models"] = "claude-secret-*"
	defineModels(auth, `[{"id":"`+restrictedID+`"},{"id":"claude-secret-1"},{"id":"mine","upstream_id":"claude-secret-2"},{"id":"claude-kept-1"}]`)
	service.registerModelsForAuth(context.Background(), auth)
	registered := GlobalModelRegistry().GetModelsForClient(auth.ID)
	if !containsModelID(registered, restrictedID) || !containsModelID(registered, "claude-kept-1") {
		t.Fatalf("defined models not registered: %v", modelIDs(registered))
	}
	if containsModelID(registered, "claude-secret-1") || containsModelID(registered, "mine") {
		t.Fatalf("excluded models defined: %v", modelIDs(registered))
	}
	if _, ok := service.explicitRestrictedModelIDs(auth)[restrictedID]; !ok {
		t.Fatal("defined restricted model not opted out of grant refreshes")
	}
}

func TestOAuthSettingsDefineNewModels(t *testing.T) {
	auth := restrictedTestAuth("codex", "codex-settings-definition-test")
	service := discoveryTestService(t, auth)
	service.cfg.OAuthSettings = map[string][]config.OAuthModelSetting{"codex": {
		{Name: "gpt-settings-secret", DisplayName: "Secret", ContextLength: 400000, Thinking: &registry.ThinkingSupport{Levels: []string{"low", "high"}}},
		{Name: "gpt-legacy-unknown", MaxContextLength: 1},               // Legacy entries only override.
		{Name: "gpt-aliased-unknown", Alias: "x", DisplayName: "Alias"}, // Aliased entries only override.
	}}
	service.registerModelsForAuth(context.Background(), auth)
	registered := GlobalModelRegistry().GetModelsForClient(auth.ID)
	secret := findModel(registered, "gpt-settings-secret")
	template := codexTemplateModel(codexCatalogModelsForPlan("pro"))
	if secret == nil || secret.DisplayName != "Secret" || secret.ContextLength != 400000 || secret.MaxContextLength != 400000 ||
		secret.MaxCompletionTokens != template.MaxCompletionTokens || !secret.ExplicitThinking ||
		!reflect.DeepEqual(secret.Thinking, &registry.ThinkingSupport{Levels: []string{"low", "high"}}) {
		t.Fatalf("settings model not defined from the Codex template: %+v", secret)
	}
	if containsModelID(registered, "gpt-legacy-unknown") || containsModelID(registered, "gpt-aliased-unknown") || containsModelID(registered, "x") {
		t.Fatalf("override-only settings defined models: %v", modelIDs(registered))
	}

	// Another credential of the channel gets the same definition.
	other := restrictedTestAuth("codex", "codex-settings-definition-test-2")
	other.Metadata["account_id"] = "account-2"
	otherService := discoveryTestService(t, other)
	otherService.cfg = service.cfg
	otherService.registerModelsForAuth(context.Background(), other)
	if !containsModelID(GlobalModelRegistry().GetModelsForClient(other.ID), "gpt-settings-secret") {
		t.Fatal("settings definition missing on another credential")
	}
}

func TestOAuthSettingsContextLengthAndLegacyMaxContextLength(t *testing.T) {
	cfg := &config.Config{OAuthSettings: map[string][]config.OAuthModelSetting{"codex": {
		{Name: "both", ContextLength: 500000, MaxContextLength: 400000},
		{Name: "context", ContextLength: 300000},
		{Name: "legacy", MaxContextLength: 200000},
	}}}
	models := []*ModelInfo{{ID: "both", ContextLength: 1}, {ID: "context", ContextLength: 1}, {ID: "legacy", ContextLength: 1}, {ID: "untouched", ContextLength: 1}}
	out := applyOAuthSettings(cfg, "codex", "oauth", models)
	got := make([][2]int, 0, len(out))
	for _, model := range out {
		got = append(got, [2]int{model.ContextLength, model.MaxContextLength})
	}
	if want := [][2]int{{500000, 400000}, {300000, 300000}, {200000, 200000}, {1, 0}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("context lengths = %v, want %v", got, want)
	}
}

func TestAuthFileUpstreamIDAndCodexClientContextWindow(t *testing.T) {
	auth := restrictedTestAuth("codex", "codex-definition-upstream-test")
	service := discoveryTestService(t, auth)
	catalog := findModel(codexCatalogModelsForPlan("pro"), "gpt-6-sol")
	if catalog == nil {
		t.Skip("gpt-6-sol not in the Codex pro catalog")
	}
	defineModels(auth, `[{"id":"my-sol","upstream_id":"gpt-6-sol","display_name":"Mine"},{"id":"gpt-6-sol","context_length":123456}]`)
	service.registerModelsForAuth(context.Background(), auth)
	registered := GlobalModelRegistry().GetModelsForClient(auth.ID)
	sol := findModel(registered, "gpt-6-sol")
	mine := findModel(registered, "my-sol")
	if sol == nil || sol.ContextLength != 123456 {
		t.Fatalf("upstream model missing or not overridden: %+v", sol)
	}
	// my-sol forks gpt-6-sol, so gpt-6-sol's overrides apply before its own.
	if mine == nil || mine.MetadataModelID != "gpt-6-sol" || mine.DisplayName != "Mine" || mine.ContextLength != 123456 {
		t.Fatalf("upstream_id model = %+v", mine)
	}

	resp := codexmodels.BuildResponse([]map[string]any{{
		"id": sol.ID, "object": "model", "context_length": sol.ContextLength, "max_context_length": sol.MaxContextLength,
	}}, nil, false)
	entries, _ := resp["models"].([]map[string]any)
	if len(entries) != 1 || entries[0]["context_window"] != 123456 {
		t.Fatalf("Codex client not told the overridden context window: %#v", entries)
	}
}

func TestRestrictedModelsOptInOnlyThroughAuthFileIDs(t *testing.T) {
	auth := restrictedTestAuth("claude", "claude-definition-optin-test")
	service := discoveryTestService(t, auth)
	restricted := findModel(registry.GetClaudeModels(), "claude-opus-4-6")
	if restricted == nil {
		t.Skip("claude-opus-4-6 not in the Claude catalog")
	}
	service.restrictedAccess.restrictedIDs = func(string) map[string]struct{} { return map[string]struct{}{restricted.ID: {}} }
	service.cfg.OAuthSettings = map[string][]config.OAuthModelSetting{"claude": {{Name: restricted.ID, DisplayName: "Settings R"}}}
	defineModels(auth, `[{"id":"mine","upstream_id":"`+restricted.ID+`"}]`)

	service.registerModelsForAuth(context.Background(), auth)
	registered := GlobalModelRegistry().GetModelsForClient(auth.ID)
	if containsModelID(registered, restricted.ID) {
		t.Fatalf("settings entry or upstream_id listed restricted %s without a grant: %v", restricted.ID, modelIDs(registered))
	}
	mine := findModel(registered, "mine")
	if mine == nil || mine.MetadataModelID != restricted.ID || mine.Created != restricted.Created || mine.ContextLength != restricted.ContextLength {
		t.Fatalf("upstream_id route missing or not inherited from %s: %+v", restricted.ID, mine)
	}
	if got := service.explicitRestrictedModelIDs(auth); !reflect.DeepEqual(got, map[string]struct{}{"mine": {}}) {
		t.Fatalf("explicit opt-ins = %v, want only the auth-file id", got)
	}

	// With a grant the gate keeps the model and the settings override applies.
	service.restrictedAccess.entries = map[string]*restrictedModelAccessEntry{auth.ID: {
		identity: restrictedModelIdentity(auth),
		listed:   map[string]struct{}{restricted.ID: {}},
	}}
	service.registerModelsForAuth(context.Background(), auth)
	registered = GlobalModelRegistry().GetModelsForClient(auth.ID)
	if granted := findModel(registered, restricted.ID); granted == nil || granted.DisplayName != "Settings R" {
		t.Fatalf("granted restricted model missing or settings not applied: %+v", granted)
	}
	if !containsModelID(registered, "mine") {
		t.Fatalf("upstream_id route missing with a grant: %v", modelIDs(registered))
	}
}

func TestSettingsCannotDefineRestrictedModelByCaseVariant(t *testing.T) {
	auth := restrictedTestAuth("claude", "claude-definition-case-test")
	service := discoveryTestService(t, auth)
	restricted := findModel(registry.GetClaudeModels(), "claude-opus-4-6")
	if restricted == nil {
		t.Skip("claude-opus-4-6 not in the Claude catalog")
	}
	service.restrictedAccess.restrictedIDs = func(string) map[string]struct{} { return map[string]struct{}{restricted.ID: {}} }
	service.cfg.OAuthSettings = map[string][]config.OAuthModelSetting{"claude": {{Name: " " + strings.ToUpper(restricted.ID), DisplayName: "Settings R"}}}

	service.registerModelsForAuth(context.Background(), auth)
	for _, model := range GlobalModelRegistry().GetModelsForClient(auth.ID) {
		if strings.EqualFold(strings.TrimSpace(model.ID), restricted.ID) {
			t.Fatalf("case-variant settings entry defined restricted %s without a grant", model.ID)
		}
	}
}

func TestUpstreamIDRouteDoesNotCountAsGrant(t *testing.T) {
	catalogID := registry.GetClaudeModels()[0].ID
	restrictedID := findModel(registry.GetClaudeModels(), "claude-opus-4-6")
	if restrictedID == nil || restrictedID.ID == catalogID {
		t.Skip("claude-opus-4-6 not in the Claude catalog")
	}
	auth := restrictedTestAuth("claude", "claude-definition-route-grant-test")
	// Define before registering with the manager: refreshes re-read its copy.
	defineModels(auth, `[{"id":"mine","upstream_id":"`+restrictedID.ID+`"}]`)
	service := discoveryTestService(t, auth)
	service.restrictedAccess.restrictedIDs = func(string) map[string]struct{} { return map[string]struct{}{restrictedID.ID: {}} }
	reg := GlobalModelRegistry()
	service.registerModelsForAuth(context.Background(), auth)
	entry := &restrictedModelAccessEntry{identity: restrictedModelIdentity(auth), fetching: true}
	service.restrictedAccess.entries = map[string]*restrictedModelAccessEntry{auth.ID: entry}
	fetch := func(models ...coreauth.UpstreamModel) {
		t.Helper()
		service.fetchRestrictedModels(context.Background(), auth, upstreamListerFunc(func(context.Context, *coreauth.Auth) ([]coreauth.UpstreamModel, error) {
			return models, nil
		}), entry)
	}

	epoch := reg.ClientRegistrationEpoch(auth.ID)
	fetch(claudeUpstreamObject(catalogID, "Catalog"))
	if got := reg.ClientRegistrationEpoch(auth.ID); got != epoch {
		t.Fatalf("ungranted upstream_id route treated as a denied grant: epoch %d != %d", got, epoch)
	}
	fetch(claudeUpstreamObject(catalogID, "Catalog"), claudeUpstreamObject(restrictedID.ID, "Restricted"))
	if got := reg.ClientRegistrationEpoch(auth.ID); got <= epoch {
		t.Fatalf("new grant did not re-register: epoch %d <= %d", got, epoch)
	}
	if registered := reg.GetModelsForClient(auth.ID); !containsModelID(registered, restrictedID.ID) || !containsModelID(registered, "mine") {
		t.Fatalf("granted model or route missing: %v", modelIDs(registered))
	}
}

func TestDefinitionAliasRespectsExcludedModels(t *testing.T) {
	auth := restrictedTestAuth("claude", "claude-definition-excluded-alias-test")
	service := discoveryTestService(t, auth)
	upstream := registry.GetClaudeModels()[0].ID
	auth.Attributes["excluded_models"] = "local-secret"
	defineModels(auth, `[{"id":"local-secret","upstream_id":"`+upstream+`"}]`)
	service.registerModelsForAuth(context.Background(), auth)
	registered := GlobalModelRegistry().GetModelsForClient(auth.ID)
	if containsModelID(registered, "local-secret") || !containsModelID(registered, upstream) {
		t.Fatalf("excluded alias registered or upstream model lost: %v", modelIDs(registered))
	}
}

func TestUpstreamIDOnExistingIDIndependentOfCatalogOrder(t *testing.T) {
	auth := restrictedTestAuth("codex", "codex-definition-collision-test")
	service := discoveryTestService(t, auth)
	catalog := codexCatalogModelsForPlan("pro")
	local, target := -1, -1
	for i, model := range catalog {
		switch model.ID {
		case "gpt-5.5":
			local = i
		case "gpt-6-astra":
			target = i
		}
	}
	if local < 0 || target < 0 || local > target {
		t.Skip("Codex pro catalog does not list gpt-5.5 before gpt-6-astra")
	}
	defineModels(auth, `[{"id":"gpt-5.5","upstream_id":"gpt-6-astra"}]`)
	service.registerModelsForAuth(context.Background(), auth)
	registered := GlobalModelRegistry().GetModelsForClient(auth.ID)
	astra := catalog[target]
	route := findModel(registered, "gpt-5.5")
	if route == nil || route.MetadataModelID != "gpt-6-astra" || route.DisplayName != astra.DisplayName || route.ContextLength != astra.ContextLength {
		t.Fatalf("gpt-5.5 not routed to gpt-6-astra: %+v", route)
	}
	count := 0
	for _, model := range registered {
		if model.ID == "gpt-5.5" {
			count++
		}
	}
	if count != 1 || !containsModelID(registered, "gpt-6-astra") {
		t.Fatalf("collision registered gpt-5.5 %d times or lost gpt-6-astra: %v", count, modelIDs(registered))
	}
}
