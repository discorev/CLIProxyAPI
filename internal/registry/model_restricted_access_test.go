package registry

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRestrictedAccessCatalogDecodingAndResponsePrivacy(t *testing.T) {
	var catalog staticModelsJSON
	if err := json.Unmarshal([]byte(`{"claude":[{"id":"claude-mythos-5-1","restricted_access":true}],"codex-pro":[{"id":"gpt-daybreak-blue-latest","restricted_access":true}]}`), &catalog); err != nil {
		t.Fatal(err)
	}
	for _, model := range []*ModelInfo{catalog.Claude[0], catalog.CodexPro[0]} {
		if !model.RestrictedAccess || !cloneModelInfo(model).RestrictedAccess || !cloneModelInfos([]*ModelInfo{model})[0].RestrictedAccess {
			t.Fatalf("restricted flag lost during decoding or cloning: %+v", model)
		}
		encoded, err := json.Marshal(model) // The management endpoint directly marshals ModelInfo.
		if err != nil || strings.Contains(string(encoded), "restricted_access") {
			t.Fatalf("model metadata leaked flag: %s, %v", encoded, err)
		}
	}
	registry := newTestModelRegistry()
	registry.RegisterClient("claude-test", "claude", catalog.Claude)
	registry.RegisterClient("codex-test", "codex", catalog.CodexPro)
	for _, format := range []string{"openai", "claude", "gemini"} {
		encoded, err := json.Marshal(registry.GetAvailableModels(format))
		if err != nil || strings.Contains(string(encoded), "restricted_access") {
			t.Fatalf("%s model list leaked flag: %s, %v", format, encoded, err)
		}
	}
}

func TestRestrictedAccessCatalogFlagChangeTriggersRefresh(t *testing.T) {
	oldCatalog := &staticModelsJSON{Claude: []*ModelInfo{{ID: "claude-mythos-5-1"}}}
	newCatalog := &staticModelsJSON{Claude: []*ModelInfo{{ID: "claude-mythos-5-1", RestrictedAccess: true}}}
	if changed := detectChangedProviders(oldCatalog, newCatalog); len(changed) != 1 || changed[0] != "claude" {
		t.Fatalf("restricted_access flag change did not refresh Claude registrations: %v", changed)
	}
}
