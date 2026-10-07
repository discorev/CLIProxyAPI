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

func TestCodexBuiltinsPreserveCatalogRestriction(t *testing.T) {
	const id = "gpt-image-2.5-flare"
	// Every tier overlays the built-in metadata, but the catalog owns access.
	modelsCatalogStore.mu.Lock()
	original := modelsCatalogStore.data
	modelsCatalogStore.data = &staticModelsJSON{
		CodexFree: []*ModelInfo{{ID: id, RestrictedAccess: true}},
		CodexTeam: []*ModelInfo{{ID: id, RestrictedAccess: true}},
		CodexPlus: []*ModelInfo{{ID: id, RestrictedAccess: true}},
		CodexPro:  []*ModelInfo{{ID: id, RestrictedAccess: true}},
	}
	modelsCatalogStore.mu.Unlock()
	t.Cleanup(func() {
		modelsCatalogStore.mu.Lock()
		modelsCatalogStore.data = original
		modelsCatalogStore.mu.Unlock()
	})
	for _, tier := range []struct {
		name string
		get  func() []*ModelInfo
	}{
		{"free", GetCodexFreeModels}, {"team", GetCodexTeamModels},
		{"plus", GetCodexPlusModels}, {"pro", GetCodexProModels},
	} {
		t.Run(tier.name, func(t *testing.T) {
			for _, model := range tier.get() {
				if model.ID == id {
					if !model.RestrictedAccess || model.DisplayName != "GPT Image 2.5 Flare" {
						t.Fatalf("built-in lost catalog access flag or built-in metadata: %+v", model)
					}
					return
				}
			}
			t.Fatalf("built-in %q not found", id)
		})
	}
}
