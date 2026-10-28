package cliproxy

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func accessProgramVariantsForTest(service *Service, auth *coreauth.Auth, excluded []string, models []*ModelInfo) []*ModelInfo {
	service.restrictedAccess.mu.RLock()
	defer service.restrictedAccess.mu.RUnlock()
	return service.appendAccessProgramVariantsLocked(auth, excluded, models)
}

func TestAccessProgramVariantsFromUpstreamList(t *testing.T) {
	auth := restrictedTestAuth("codex", "codex-access-program-variants-test")
	service := discoveryTestService(t, auth)
	service.restrictedAccess.entries = map[string]*restrictedModelAccessEntry{auth.ID: {
		identity: restrictedModelIdentity(auth),
		upstream: []coreauth.UpstreamModel{
			codexAccessProgramObject("gpt-fixture-a", "standard", "daybreak_blue"),
			codexAccessProgramObject("gpt-fixture-b", "standard"),
			codexAccessProgramObjectWithVisibility("gpt-fixture-hidden", "hide", "standard", "daybreak_blue"),
		},
	}}
	base := &ModelInfo{ID: "gpt-fixture-a", Object: "model", OwnedBy: "openai", Type: "openai", DisplayName: "GPT-Fixture-A",
		ContextLength: 5, Thinking: &registry.ThinkingSupport{Levels: []string{"low"}}}
	// gpt-fixture-b only lists standard, and gpt-fixture-hidden is hidden even though it lists daybreak_blue.
	models := []*ModelInfo{base, {ID: "gpt-fixture-b"}, {ID: "gpt-fixture-hidden"}}

	got := accessProgramVariantsForTest(service, auth, nil, models)
	if ids := modelIDs(got); !reflect.DeepEqual(ids, []string{"gpt-fixture-a", "gpt-fixture-b", "gpt-fixture-hidden", "gpt-fixture-a-daybreak-blue"}) {
		t.Fatalf("models = %v", ids)
	}
	want := cloneModelInfoForCatalogRoute(base)
	want.ID, want.MetadataModelID, want.DisplayName, want.AccessProgram = "gpt-fixture-a-daybreak-blue", "gpt-fixture-a", "GPT-Fixture-A (Daybreak Blue)", "daybreak_blue"
	if variant := got[3]; !reflect.DeepEqual(*variant, want) {
		t.Fatalf("variant = %+v, want %+v", variant, want)
	}
	if encoded, _ := json.Marshal(got[3]); strings.Contains(string(encoded), "daybreak_blue") || strings.Contains(string(encoded), "gpt-fixture-a\"") {
		t.Fatalf("variant JSON exposes internal routing fields: %s", encoded)
	}

	if got := accessProgramVariantsForTest(service, auth, []string{"gpt-fixture-a-daybreak-*"}, models); len(got) != len(models) {
		t.Fatalf("excluded variant registered: %v", modelIDs(got))
	}
	if got := accessProgramVariantsForTest(service, auth, nil, models[1:]); len(got) != 2 {
		t.Fatalf("variant registered without its base: %v", modelIDs(got))
	}
	route := []*ModelInfo{{ID: "gpt-fixture-a", MetadataModelID: "gpt-fixture-a-upstream"}}
	if got := accessProgramVariantsForTest(service, auth, nil, route); len(got) != 2 || got[1].MetadataModelID != "gpt-fixture-a-upstream" {
		t.Fatalf("variant of a route does not target the base's upstream model: %+v", got)
	}
	existing := []*ModelInfo{base, {ID: "gpt-fixture-a-daybreak-blue", DisplayName: "user defined"}}
	if got := accessProgramVariantsForTest(service, auth, nil, existing); len(got) != 2 || got[1].AccessProgram != "" {
		t.Fatalf("variant replaced an already resolved model: %+v", got)
	}

	alias := []coreauth.UpstreamModel{codexAccessProgramObject("gpt-fixture-single", "daybreak_blue"), codexAccessProgramObject("gpt-fixture-a", "standard", "daybreak_blue")}
	if got := accessProgramVariants(alias); len(got) != 1 || got[0].base != "gpt-fixture-a" {
		t.Fatalf("single-program alias got a variant: %+v", got)
	}

	apiKey := auth.Clone()
	apiKey.Attributes = map[string]string{"auth_kind": "apikey", "api_key": "fake-key"}
	if got := accessProgramVariantsForTest(service, apiKey, nil, models); len(got) != len(models) {
		t.Fatalf("API-key credential got variants: %v", modelIDs(got))
	}
}

func codexAccessProgramObject(id string, programs ...string) coreauth.UpstreamModel {
	return codexAccessProgramObjectWithVisibility(id, "list", programs...)
}

func codexAccessProgramObjectWithVisibility(id, visibility string, programs ...string) coreauth.UpstreamModel {
	encoded, _ := json.Marshal(programs)
	raw := `{"slug":"` + id + `","visibility":"` + visibility + `","available_access_programs":{"cyber":` + string(encoded) + `}}`
	return coreauth.UpstreamModel{ID: id, Raw: json.RawMessage(raw)}
}

func TestAccessProgramChangeTriggersReregistration(t *testing.T) {
	auth := restrictedTestAuth("codex", "codex-access-program-refresh-test")
	service := discoveryTestService(t, auth)
	reg := GlobalModelRegistry()
	base := registry.GetCodexProModels()[0].ID
	variant := base + "-daybreak-blue"
	service.registerModelsForAuth(context.Background(), auth)
	entry := &restrictedModelAccessEntry{identity: restrictedModelIdentity(auth), fetching: true}
	service.restrictedAccess.entries = map[string]*restrictedModelAccessEntry{auth.ID: entry}
	fetch := func(models ...coreauth.UpstreamModel) {
		t.Helper()
		service.fetchRestrictedModels(context.Background(), auth, upstreamListerFunc(func(context.Context, *coreauth.Auth) ([]coreauth.UpstreamModel, error) {
			return models, nil
		}), entry)
	}
	if containsModelID(reg.GetModelsForClient(auth.ID), variant) {
		t.Fatal("variant registered before the upstream list granted it")
	}

	initialEpoch := reg.ClientRegistrationEpoch(auth.ID)
	fetch(codexAccessProgramObject(base, "standard", "daybreak_blue"))
	if got := reg.ClientRegistrationEpoch(auth.ID); got <= initialEpoch {
		t.Fatalf("granted program did not re-register: epoch %d <= %d", got, initialEpoch)
	}
	info := registry.GetGlobalRegistry().GetClientModelInfo(auth.ID, variant)
	if info == nil || info.AccessProgram != "daybreak_blue" || info.MetadataModelID != base {
		t.Fatalf("registered variant = %+v", info)
	}

	grantedEpoch := reg.ClientRegistrationEpoch(auth.ID)
	fetch(codexAccessProgramObject(base, "standard", "daybreak_blue"))
	if got := reg.ClientRegistrationEpoch(auth.ID); got != grantedEpoch {
		t.Fatalf("unchanged programs re-registered: epoch %d != %d", got, grantedEpoch)
	}

	fetch(codexAccessProgramObject(base, "standard"))
	if got := reg.ClientRegistrationEpoch(auth.ID); got <= grantedEpoch {
		t.Fatalf("revoked program did not re-register: epoch %d <= %d", got, grantedEpoch)
	}
	if containsModelID(reg.GetModelsForClient(auth.ID), variant) {
		t.Fatal("revoked variant still registered")
	}
}
