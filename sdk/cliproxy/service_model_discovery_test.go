package cliproxy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/thinking/provider/claude"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/tidwall/gjson"
)

func loadUpstreamFixture(t *testing.T, name, idKey string) []coreauth.UpstreamModel {
	t.Helper()
	data, errRead := os.ReadFile(filepath.Join("testdata", name))
	if errRead != nil {
		t.Fatal(errRead)
	}
	var objects []json.RawMessage
	if errDecode := json.Unmarshal(data, &objects); errDecode != nil {
		t.Fatal(errDecode)
	}
	models := make([]coreauth.UpstreamModel, 0, len(objects))
	for _, raw := range objects {
		var fields map[string]any
		if errDecode := json.Unmarshal(raw, &fields); errDecode != nil {
			t.Fatal(errDecode)
		}
		id, _ := fields[idKey].(string)
		models = append(models, coreauth.UpstreamModel{ID: id, Raw: raw})
	}
	return models
}

func findModel(models []*ModelInfo, id string) *ModelInfo {
	for _, model := range models {
		if model.ID == id {
			return model
		}
	}
	return nil
}

func discoverForTest(provider string, catalog []*ModelInfo, upstream []coreauth.UpstreamModel) []*ModelInfo {
	return synthesizeDiscoveredModels(provider, catalog, upstream, discoverableUpstreamModels(provider, catalog, upstream))
}

func TestDiscoverClaudeModelsFromPredecessorAndDefaults(t *testing.T) {
	upstream := loadUpstreamFixture(t, "upstream-claude-models.json", "id")
	catalog := []*ModelInfo{
		{ID: "claude-opus-8-9", ContextLength: 1, Description: "older opus"},
		{ID: "claude-opus-9", Object: "model", OwnedBy: "anthropic", Type: "claude", ContextLength: 777, MaxCompletionTokens: 64000,
			Description: "opus", Version: "v9", Created: 1, Thinking: &registry.ThinkingSupport{ZeroAllowed: true, DynamicAllowed: true, Levels: []string{"low", "medium"}}},
		{ID: "claude-sonnet-9", ContextLength: 2},
	}
	models := discoverForTest("claude", catalog, upstream)
	if got := modelIDs(models); !reflect.DeepEqual(got, []string{"claude-opus-9-1", "claude-quill-9"}) {
		t.Fatalf("discovered = %v", got)
	}

	opus := findModel(models, "claude-opus-9-1")
	wantCreated := time.Date(2031, time.March, 10, 12, 30, 0, 0, time.UTC).Unix()
	if opus.ContextLength != 777 || opus.MaxCompletionTokens != 64000 || opus.DisplayName != "Claude Opus 9.1" ||
		opus.Created != wantCreated || opus.Description != "" || opus.Version != "" || opus.Type != "claude" {
		t.Fatalf("opus not cloned from newest same-line catalog model: %+v", opus)
	}
	wantThinking := &registry.ThinkingSupport{ZeroAllowed: true, DynamicAllowed: true, Levels: []string{"low", "medium", "high", "xhigh", "max"}}
	if !reflect.DeepEqual(opus.Thinking, wantThinking) {
		t.Fatalf("opus thinking = %+v, want %+v", opus.Thinking, wantThinking)
	}
	if !reflect.DeepEqual(catalog[1].Thinking.Levels, []string{"low", "medium"}) {
		t.Fatalf("synthesis mutated the catalog: %+v", catalog[1].Thinking)
	}

	quill := findModel(models, "claude-quill-9")
	want := &ModelInfo{
		ID: "claude-quill-9", Object: "model", OwnedBy: "anthropic", Type: "claude", DisplayName: "Claude Quill 9",
		Created:       time.Date(2031, time.February, 1, 0, 0, 0, 0, time.UTC).Unix(),
		ContextLength: discoveredClaudeContextLength, MaxCompletionTokens: discoveredClaudeMaxCompletionTokens,
		SupportedInputModalities: []string{"text", "image"}, SupportedOutputModalities: []string{"text"},
		Thinking: &registry.ThinkingSupport{Min: 1024, Max: 128000, Levels: []string{"low", "medium", "high", "xhigh", "max"}},
	}
	if !reflect.DeepEqual(quill, want) {
		t.Fatalf("quill defaults = %+v, want %+v", quill, want)
	}
}

func TestDiscoverClaudeModelEffortAndThinkingCapabilities(t *testing.T) {
	upstream := []coreauth.UpstreamModel{
		{ID: "claude-x-1", Raw: json.RawMessage(`{"id":"claude-x-1","line":"x","capabilities":{"image_input":{"supported":false},` +
			`"effort":{"supported":true,"max":{"supported":true},"medium":{"supported":false},"low":{"supported":true}},` +
			`"thinking":{"supported":true,"types":{"disabled":{"supported":true}}}}}`)},
		{ID: "claude-y-1", Raw: json.RawMessage(`{"id":"claude-y-1","line":"y","capabilities":{"thinking":{"supported":false}}}`)},
		{ID: "claude-z-1"}, // IDs without an upstream object cannot be synthesized.
	}
	models := discoverForTest("claude", nil, upstream)
	if got := modelIDs(models); !reflect.DeepEqual(got, []string{"claude-x-1", "claude-y-1"}) {
		t.Fatalf("discovered = %v", got)
	}
	wantThinking := &registry.ThinkingSupport{Min: 1024, Max: 128000, ZeroAllowed: true, Levels: []string{"low", "max"}}
	if !reflect.DeepEqual(models[0].Thinking, wantThinking) || !reflect.DeepEqual(models[0].SupportedInputModalities, []string{"text"}) {
		t.Fatalf("effort levels not mapped low to max: %+v %v", models[0].Thinking, models[0].SupportedInputModalities)
	}
	if models[1].Thinking != nil {
		t.Fatalf("unsupported thinking kept: %+v", models[1].Thinking)
	}
}

func TestDiscoverClaudeModelThinkingWithoutPredecessorThinking(t *testing.T) {
	catalog := []*ModelInfo{{ID: "claude-w-0", ContextLength: 3}}
	upstream := []coreauth.UpstreamModel{
		{ID: "claude-w-0", Raw: json.RawMessage(`{"id":"claude-w-0","line":"w","created_at":"2026-01-01T00:00:00Z"}`)},
		{ID: "claude-w-1", Raw: json.RawMessage(`{"id":"claude-w-1","line":"w","capabilities":{"effort":{"low":{"supported":true},"high":{"supported":true}},` +
			`"thinking":{"supported":true,"types":{"disabled":{"supported":false}}}}}`)},
	}
	models := discoverForTest("claude", catalog, upstream)
	if len(models) != 1 || models[0].ContextLength != 3 {
		t.Fatalf("claude-w-1 not cloned from its predecessor: %+v", models)
	}
	want := &registry.ThinkingSupport{Min: discoveredClaudeThinkingMin, Max: discoveredClaudeThinkingMax, Levels: []string{"low", "high"}}
	if !reflect.DeepEqual(models[0].Thinking, want) {
		t.Fatalf("thinking = %+v, want %+v", models[0].Thinking, want)
	}
	if catalog[0].Thinking != nil {
		t.Fatalf("synthesis mutated the catalog: %+v", catalog[0].Thinking)
	}
}

func TestDiscoverClaudeAdaptiveOnlyModelSkipsManualThinking(t *testing.T) {
	upstream := []coreauth.UpstreamModel{
		{ID: "claude-a-1", Raw: json.RawMessage(`{"id":"claude-a-1","line":"a","capabilities":{"effort":{"low":{"supported":true},"high":{"supported":true}},` +
			`"thinking":{"supported":true,"types":{"adaptive":{"supported":true},"enabled":{"supported":false}}}}}`)},
		{ID: "claude-b-1", Raw: json.RawMessage(`{"id":"claude-b-1","line":"b","capabilities":{"effort":{"low":{"supported":true},"high":{"supported":true}},` +
			`"thinking":{"supported":true,"types":{"adaptive":{"supported":true},"enabled":{"supported":true}}}}}`)},
		{ID: "claude-c-1", Raw: json.RawMessage(`{"id":"claude-c-1","line":"c","capabilities":` +
			`{"thinking":{"supported":true,"types":{"adaptive":{"supported":true},"enabled":{"supported":false}}}}}`)},
	}
	models := discoverForTest("claude", nil, upstream)
	if len(models) != 3 {
		t.Fatalf("discovered = %v", modelIDs(models))
	}
	if want := (&registry.ThinkingSupport{DynamicAllowed: true, Levels: []string{"low", "high"}}); !reflect.DeepEqual(models[0].Thinking, want) {
		t.Fatalf("adaptive-only thinking = %+v, want %+v", models[0].Thinking, want)
	}
	if models[1].Thinking == nil || models[1].Thinking.Max != discoveredClaudeThinkingMax || models[1].Thinking.DynamicAllowed {
		t.Fatalf("manual-capable model lost its budget range: %+v", models[1].Thinking)
	}

	for _, info := range []*ModelInfo{models[0], models[2]} {
		for _, model := range []string{info.ID + "(auto)", info.ID + "(8192)"} {
			body, errApply := thinking.ApplyThinkingWithModelInfo([]byte(`{"model":"`+info.ID+`"}`), nil, model, "claude", "claude", "claude", info)
			if errApply != nil {
				t.Fatalf("%s: %v", model, errApply)
			}
			if got := gjson.GetBytes(body, "thinking.type").String(); got != "adaptive" || gjson.GetBytes(body, "thinking.budget_tokens").Exists() {
				t.Fatalf("%s sent manual thinking to an adaptive-only model: %s", model, body)
			}
		}
	}
}

func TestDiscoverCodexModelsFromNewestTemplate(t *testing.T) {
	upstream := loadUpstreamFixture(t, "upstream-codex-models.json", "slug")
	webSearch := true
	catalog := []*ModelInfo{
		{ID: "gpt-fixture-old", Created: 1, MaxCompletionTokens: 1},
		{ID: "gpt-fixture-a", Object: "model", OwnedBy: "openai", Type: "openai", Created: 3, MaxCompletionTokens: 99, ContextLength: 1,
			Description: "template", Version: "gpt-fixture-a", SupportedParameters: []string{"tools"}, SupportedOutputModalities: []string{"text"},
			NativeCapabilities: &registry.NativeCapabilities{WebSearch: &webSearch}, Thinking: &registry.ThinkingSupport{Levels: []string{"low"}}},
		{ID: "gpt-fixture-mid", Created: 2, MaxCompletionTokens: 2},
	}
	models := discoverForTest("codex", catalog, upstream)
	// gpt-fixture-hidden is hidden; gpt-fixture-a is in the catalog.
	if got := modelIDs(models); !reflect.DeepEqual(got, []string{"gpt-fixture-new", "gpt-fixture-b"}) {
		t.Fatalf("discovered = %v", got)
	}
	want := &ModelInfo{
		ID: "gpt-fixture-new", Object: "model", OwnedBy: "openai", Type: "openai", DisplayName: "GPT-Fixture-New",
		Description: "Synthetic model for discovery tests.", Version: "gpt-fixture-new",
		ContextLength: 400000, MaxCompletionTokens: 99, SupportedParameters: []string{"tools"},
		SupportedInputModalities: []string{"text", "image"}, SupportedOutputModalities: []string{"text"},
		NativeCapabilities: &registry.NativeCapabilities{WebSearch: &webSearch},
		Thinking:           &registry.ThinkingSupport{Levels: []string{"low", "medium", "high", "xhigh"}},
	}
	if !reflect.DeepEqual(models[0], want) {
		t.Fatalf("codex synthesis = %+v, want %+v", models[0], want)
	}
	if !reflect.DeepEqual(catalog[1].Thinking.Levels, []string{"low"}) {
		t.Fatalf("synthesis mutated the template: %+v", catalog[1].Thinking)
	}
}

func discoveryTestService(t *testing.T, auth *coreauth.Auth) *Service {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	service := &Service{cfg: &config.Config{}, coreManager: manager, pluginHost: pluginhost.New()}
	service.restrictedAccess.restrictedIDs = func(string) map[string]struct{} { return map[string]struct{}{} }
	t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(auth.ID) })
	return service
}

func claudeUpstreamObject(id, displayName string) coreauth.UpstreamModel {
	raw := `{"type":"model","id":"` + id + `","display_name":"` + displayName + `","created_at":"2026-10-01T00:00:00Z","line":"discovery-test",` +
		`"capabilities":{"thinking":{"supported":true,"types":{"disabled":{"supported":true}}}}}`
	return coreauth.UpstreamModel{ID: id, Raw: json.RawMessage(raw)}
}

func TestRegistrationUsesCatalogEntryOverDiscoveredOne(t *testing.T) {
	auth := restrictedTestAuth("claude", "claude-discovery-registration-test")
	service := discoveryTestService(t, auth)
	catalogModel := registry.GetClaudeModels()[0]
	service.restrictedAccess.entries = map[string]*restrictedModelAccessEntry{auth.ID: {
		identity: restrictedModelIdentity(auth),
		upstream: []coreauth.UpstreamModel{
			claudeUpstreamObject(catalogModel.ID, "Upstream Name"),
			claudeUpstreamObject("claude-discovery-test-9", "Discovery Test 9"),
		},
	}}
	service.registerModelsForAuth(context.Background(), auth)
	registered := GlobalModelRegistry().GetModelsForClient(auth.ID)
	discovered := findModel(registered, "claude-discovery-test-9")
	if discovered == nil || discovered.DisplayName != "Discovery Test 9" {
		t.Fatalf("discovered model not registered: %v", modelIDs(registered))
	}
	if curated := findModel(registered, catalogModel.ID); curated == nil || curated.DisplayName != catalogModel.DisplayName {
		t.Fatalf("catalog model %q not registered from the catalog: %+v", catalogModel.ID, curated)
	}
	count := 0
	for _, model := range registered {
		if model.ID == catalogModel.ID {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("catalog model registered %d times", count)
	}

	// API-key credentials never discover.
	apiKeyAuth := auth.Clone()
	apiKeyAuth.Attributes = map[string]string{"auth_kind": "apikey", "api_key": "fake-key"}
	service.restrictedAccess.mu.RLock()
	got := service.discoveredModelsLocked(apiKeyAuth, nil)
	service.restrictedAccess.mu.RUnlock()
	if got != nil {
		t.Fatalf("API-key credential discovered models: %v", modelIDs(got))
	}
}

func TestDiscoveredSetChangeTriggersReregistration(t *testing.T) {
	auth := restrictedTestAuth("claude", "claude-discovery-refresh-test")
	service := discoveryTestService(t, auth)
	path := filepath.Join(t.TempDir(), "upstream.json")
	service.restrictedAccess.persistPath = path
	reg := GlobalModelRegistry()
	service.registerModelsForAuth(context.Background(), auth)
	entry := &restrictedModelAccessEntry{identity: restrictedModelIdentity(auth), fetching: true}
	service.restrictedAccess.entries = map[string]*restrictedModelAccessEntry{auth.ID: entry}
	catalogID := registry.GetClaudeModels()[0].ID
	fetch := func(models ...coreauth.UpstreamModel) {
		t.Helper()
		service.fetchRestrictedModels(context.Background(), auth, upstreamListerFunc(func(context.Context, *coreauth.Auth) ([]coreauth.UpstreamModel, error) {
			return models, nil
		}), entry)
	}

	initialEpoch := reg.ClientRegistrationEpoch(auth.ID)
	fetch(claudeUpstreamObject(catalogID, "Catalog"), claudeUpstreamObject("claude-discovery-test-9", "Discovery Test 9"))
	if got := reg.ClientRegistrationEpoch(auth.ID); got <= initialEpoch {
		t.Fatalf("new discovered model did not re-register: epoch %d <= %d", got, initialEpoch)
	}
	if !containsModelID(reg.GetModelsForClient(auth.ID), "claude-discovery-test-9") {
		t.Fatal("discovered model missing after re-registration")
	}
	info, errStat := os.Stat(path)
	if errStat != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("upstream copy not written with mode 0600: %v %v", info, errStat)
	}

	discoveredEpoch := reg.ClientRegistrationEpoch(auth.ID)
	fetch(claudeUpstreamObject(catalogID, "Catalog"), claudeUpstreamObject("claude-discovery-test-9", "Discovery Test 9"))
	if got := reg.ClientRegistrationEpoch(auth.ID); got != discoveredEpoch {
		t.Fatalf("unchanged discovered set re-registered: epoch %d != %d", got, discoveredEpoch)
	}

	fetch(claudeUpstreamObject(catalogID, "Catalog"))
	if got := reg.ClientRegistrationEpoch(auth.ID); got <= discoveredEpoch {
		t.Fatalf("lost discovered model did not re-register: epoch %d <= %d", got, discoveredEpoch)
	}
	if containsModelID(reg.GetModelsForClient(auth.ID), "claude-discovery-test-9") {
		t.Fatal("lost discovered model still registered")
	}
}

type upstreamListerFunc func(context.Context, *coreauth.Auth) ([]coreauth.UpstreamModel, error)

func (f upstreamListerFunc) ListUpstreamModels(ctx context.Context, auth *coreauth.Auth) ([]coreauth.UpstreamModel, error) {
	return f(ctx, auth)
}

func TestUpstreamModelCopyRoundTrip(t *testing.T) {
	auth := restrictedTestAuth("claude", "claude-discovery-seed-test")
	identity := restrictedModelIdentity(auth)
	upstream := []coreauth.UpstreamModel{claudeUpstreamObject("claude-discovery-seed-9", "Seed 9")}
	path := filepath.Join(t.TempDir(), "upstream.json")

	writer := &Service{}
	writer.restrictedAccess.persistPath = path
	writer.persistUpstreamModels(identity, upstream)
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if strings.Contains(string(data), identity) || strings.Contains(string(data), "fake-token") || strings.Contains(string(data), "account-1") {
		t.Fatalf("upstream copy leaks credential identity: %s", data)
	}

	reader := discoveryTestService(t, auth)
	reader.seedUpstreamModels(path)
	reader.restrictedAccess.mu.RLock()
	seeded := reader.upstreamModelsLocked(auth)
	entries := len(reader.restrictedAccess.entries)
	reader.restrictedAccess.mu.RUnlock()
	if !upstreamModelsEqual(seeded, upstream) {
		t.Fatalf("seeded = %+v, want %+v", seeded, upstream)
	}
	if entries != 0 {
		t.Fatal("seeding created cache entries that could delay the first fetch")
	}
	reader.registerModelsForAuth(context.Background(), auth)
	if !containsModelID(GlobalModelRegistry().GetModelsForClient(auth.ID), "claude-discovery-seed-9") {
		t.Fatal("first registration ignored the seeded upstream copy")
	}

	if errWrite := os.WriteFile(path, []byte("not json"), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	corrupt := &Service{}
	corrupt.seedUpstreamModels(path)
	corrupt.persistUpstreamModels(identity, upstream)
	if persisted, errLoad := loadPersistedUpstreamModels(path); errLoad != nil || !upstreamModelsEqual(persisted[upstreamModelsKey(identity)], upstream) {
		t.Fatalf("corrupt copy not replaced: %v %v", persisted, errLoad)
	}
}

// swapTestCatalog publishes the embedded catalog with an edited Claude section
// through a local catalog source, waits for the Claude refresh notification
// (running onRefresh in it), and restores the embedded catalog on cleanup.
func swapTestCatalog(t *testing.T, edit func(claude []json.RawMessage) []json.RawMessage, onRefresh func()) {
	t.Helper()
	data, errRead := os.ReadFile(filepath.Join("..", "..", "internal", "registry", "models", "models.json"))
	if errRead != nil {
		t.Fatal(errRead)
	}
	var sections map[string]json.RawMessage
	var claude []json.RawMessage
	if errDecode := json.Unmarshal(data, &sections); errDecode != nil {
		t.Fatal(errDecode)
	}
	if errDecode := json.Unmarshal(sections["claude"], &claude); errDecode != nil {
		t.Fatal(errDecode)
	}
	edited, errEncode := json.Marshal(edit(claude))
	if errEncode != nil {
		t.Fatal(errEncode)
	}
	sections["claude"] = edited
	if data, errEncode = json.Marshal(sections); errEncode != nil {
		t.Fatal(errEncode)
	}
	path := filepath.Join(t.TempDir(), "models.json")
	if errWrite := os.WriteFile(path, data, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	refreshed := make(chan struct{}, 4)
	registry.SetModelRefreshCallback(func(providers []string) {
		if slices.Contains(providers, "claude") {
			if onRefresh != nil {
				onRefresh()
			}
			refreshed <- struct{}{}
		}
	})
	await := func() {
		t.Helper()
		deadline := time.NewTimer(5 * time.Second)
		defer deadline.Stop()
		select {
		case <-refreshed:
		case <-deadline.C:
			t.Fatal("Claude catalog refresh not published")
		}
	}
	registry.SetLocalModelCatalogs(true) // Unset sources use the embedded catalogs, never the network.
	ctx, cancel := context.WithCancel(context.Background())
	registry.StartModelCatalogUpdaters(ctx, registry.CatalogSources{Catalog: path}, false)
	await()
	t.Cleanup(func() {
		cancel()
		restoreCtx, restoreCancel := context.WithCancel(context.Background())
		defer restoreCancel()
		registry.StartModelCatalogUpdaters(restoreCtx, registry.CatalogSources{}, false)
		await()
		registry.SetModelRefreshCallback(nil)
		registry.SetLocalModelCatalogs(false)
	})
}

func TestEmptyClaudeCatalogStillDiscovers(t *testing.T) {
	auth := restrictedTestAuth("claude", "claude-discovery-empty-catalog-test")
	service := discoveryTestService(t, auth)
	service.restrictedAccess.entries = map[string]*restrictedModelAccessEntry{auth.ID: {
		identity: restrictedModelIdentity(auth),
		upstream: []coreauth.UpstreamModel{claudeUpstreamObject("claude-discovery-test-9", "Discovery Test 9")},
	}}
	swapTestCatalog(t, func([]json.RawMessage) []json.RawMessage { return []json.RawMessage{} }, nil)
	if len(registry.GetClaudeModels()) != 0 {
		t.Fatal("Claude catalog section not emptied")
	}
	service.registerModelsForAuth(context.Background(), auth)
	discovered := findModel(GlobalModelRegistry().GetModelsForClient(auth.ID), "claude-discovery-test-9")
	if discovered == nil || discovered.ContextLength != discoveredClaudeContextLength {
		t.Fatalf("empty catalog disabled discovery or skipped defaults: %+v", discovered)
	}
}

func TestCatalogCatchUpReplacesDiscoveredModel(t *testing.T) {
	auth := restrictedTestAuth("claude", "claude-discovery-catch-up-test")
	service := discoveryTestService(t, auth)
	const id = "claude-discovery-test-9"
	service.restrictedAccess.entries = map[string]*restrictedModelAccessEntry{auth.ID: {
		identity: restrictedModelIdentity(auth),
		upstream: []coreauth.UpstreamModel{claudeUpstreamObject(id, "Discovered")},
	}}
	service.registerModelsForAuth(context.Background(), auth)
	if discovered := findModel(GlobalModelRegistry().GetModelsForClient(auth.ID), id); discovered == nil || discovered.DisplayName != "Discovered" {
		t.Fatalf("discovered model not registered: %+v", discovered)
	}

	curated := json.RawMessage(`{"id":"` + id + `","object":"model","owned_by":"anthropic","type":"claude","display_name":"Curated","context_length":4242}`)
	swapTestCatalog(t, func(claude []json.RawMessage) []json.RawMessage { return append(claude, curated) }, func() {
		service.refreshModelRegistrationForAuth(auth)
	})
	count := 0
	var got *ModelInfo
	for _, model := range GlobalModelRegistry().GetModelsForClient(auth.ID) {
		if model.ID == id {
			count++
			got = model
		}
	}
	if count != 1 || got.DisplayName != "Curated" || got.ContextLength != 4242 {
		t.Fatalf("curated entry did not replace the discovered one (count %d): %+v", count, got)
	}
}

func TestUpstreamModelCopyPrivateDirAndSizeLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache", upstreamModelsFileName)
	if errWrite := writePersistedUpstreamModels(path, map[string][]coreauth.UpstreamModel{"k": {{ID: "m"}}}); errWrite != nil {
		t.Fatal(errWrite)
	}
	if info, errStat := os.Stat(filepath.Dir(path)); errStat != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("cache directory not created private: %v %v", info, errStat)
	}
	if persisted, errLoad := loadPersistedUpstreamModels(path); errLoad != nil || len(persisted["k"]) != 1 {
		t.Fatalf("round trip = %v, %v", persisted, errLoad)
	}

	if errTruncate := os.Truncate(path, upstreamModelsMaxBytes+1); errTruncate != nil {
		t.Fatal(errTruncate)
	}
	if persisted, errLoad := loadPersistedUpstreamModels(path); errLoad == nil || persisted != nil {
		t.Fatalf("oversized copy loaded: %v, %v", persisted, errLoad)
	}
	if persisted, errLoad := loadPersistedUpstreamModels(filepath.Dir(path)); errLoad == nil || persisted != nil {
		t.Fatalf("directory loaded as the copy: %v, %v", persisted, errLoad)
	}
	if persisted, errLoad := loadPersistedUpstreamModels(""); errLoad != nil || persisted != nil {
		t.Fatalf("disabled copy = %v, %v", persisted, errLoad)
	}
}
