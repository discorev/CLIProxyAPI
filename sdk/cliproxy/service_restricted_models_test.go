package cliproxy

import (
	"context"
	"errors"
	"io"
	"maps"
	"net/http"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

type restrictedModelListerFunc func(context.Context, *coreauth.Auth) ([]string, error)

func (f restrictedModelListerFunc) ListUpstreamModels(ctx context.Context, auth *coreauth.Auth) ([]string, error) {
	return f(ctx, auth)
}

func restrictedTestAuth(provider, id string) *coreauth.Auth {
	return &coreauth.Auth{
		ID: id, Provider: provider, Status: coreauth.StatusActive,
		Attributes: map[string]string{"auth_kind": "oauth", "plan_type": "pro"},
		Metadata:   map[string]any{"access_token": "fake-token", "account_id": "account-1"},
	}
}

func modelIDs(models []*ModelInfo) []string {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	return ids
}

func containsModelID(models []*ModelInfo, id string) bool {
	for _, model := range models {
		if model.ID == id {
			return true
		}
	}
	return false
}

func TestRestrictedModelFilterFailsClosedExceptConfiguredModels(t *testing.T) {
	for _, provider := range []string{"claude", "codex", "gemini"} {
		t.Run(provider, func(t *testing.T) {
			auth := restrictedTestAuth(provider, provider+"-restricted-test")
			service := &Service{}
			service.restrictedAccess.restrictedIDs = func(string) map[string]struct{} {
				return map[string]struct{}{"restricted": {}, "configured": {}}
			}
			models := []*ModelInfo{
				{ID: "ordinary"},
				{ID: "restricted", RestrictedAccess: true},
				{ID: "configured", RestrictedAccess: true, UserDefined: true},
			}
			wantUnknown := []string{"ordinary"}
			if provider == "gemini" {
				wantUnknown = []string{"ordinary", "configured"}
			}
			if got := modelIDs(service.filterRestrictedModels(auth, models)); !reflect.DeepEqual(got, wantUnknown) {
				t.Fatalf("unknown access models = %v, want %v", got, wantUnknown)
			}
			service.restrictedAccess.entries = map[string]*restrictedModelAccessEntry{
				auth.ID: {identity: restrictedModelIdentity(auth), listed: map[string]struct{}{"restricted": {}}},
			}
			wantGranted := []string{"ordinary", "restricted"}
			if provider == "gemini" {
				wantGranted = wantUnknown // A non-OAuth provider cannot prove access.
			}
			if got := modelIDs(service.filterRestrictedModels(auth, models)); !reflect.DeepEqual(got, wantGranted) {
				t.Fatalf("granted models = %v, want %v", got, wantGranted)
			}
			service.restrictedAccess.entries[auth.ID].listed = map[string]struct{}{"other": {}}
			if got := modelIDs(service.filterRestrictedModels(auth, models)); !reflect.DeepEqual(got, wantUnknown) {
				t.Fatalf("ungranted models = %v, want %v", got, wantUnknown)
			}
			newAuth := auth.Clone()
			newAuth.Metadata["account_id"] = "different-account"
			if provider == "claude" {
				newAuth.Metadata["access_token"] = "different-token" // Legacy Claude fixture has no account UUID.
			}
			if got := modelIDs(service.filterRestrictedModels(newAuth, models)); !reflect.DeepEqual(got, wantUnknown) {
				t.Fatalf("credential identity change retained grants: %v", got)
			}
			apiKey := &coreauth.Auth{ID: auth.ID, Provider: provider, Attributes: map[string]string{"api_key": "fake-key"}}
			if got := modelIDs(service.filterRestrictedModels(apiKey, models)); !reflect.DeepEqual(got, wantUnknown) {
				t.Fatalf("API key used cached OAuth grants: %v", got)
			}
		})
	}
}

func TestConfiguredAPIKeyModelsRemainAvailable(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			id := provider + "-restricted-config-test"
			cfg := &config.Config{}
			name := "configured-restricted-model"
			if provider == "claude" {
				cfg.ClaudeKey = []config.ClaudeKey{{APIKey: "fake-key", Models: []internalconfig.ClaudeModel{{Name: name}}}}
			} else {
				cfg.CodexKey = []config.CodexKey{{APIKey: "fake-key", Models: []internalconfig.CodexModel{{Name: name}}}}
			}
			service := &Service{cfg: cfg}
			service.restrictedAccess.restrictedIDs = func(string) map[string]struct{} {
				return map[string]struct{}{name: {}, "unconfigured-restricted": {}}
			}
			reg := GlobalModelRegistry()
			t.Cleanup(func() { reg.UnregisterClient(id) })
			service.registerModelsForAuth(context.Background(), &coreauth.Auth{
				ID: id, Provider: provider, Status: coreauth.StatusActive,
				Attributes: map[string]string{"api_key": "fake-key", "source": "config:" + provider, "config_index": "0"},
			})
			if got := modelIDs(reg.GetModelsForClient(id)); !reflect.DeepEqual(got, []string{name}) {
				t.Fatalf("explicit %s API-key models = %v, want %s", provider, got, name)
			}
			apiKey := &coreauth.Auth{ID: id, Provider: provider, Attributes: map[string]string{"api_key": "fake-key", "source": "config:" + provider, "config_index": "0"}}
			if got := service.filterRestrictedModels(apiKey, []*ModelInfo{{ID: "unconfigured-restricted", UserDefined: true}}); len(got) != 0 {
				t.Fatalf("unconfigured API-key model bypassed catalog restriction: %v", modelIDs(got))
			}
		})
	}
}

func TestEmptyAPIKeyModelListDoesNotGrantRestrictedCatalogModels(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			id := provider + "-empty-model-list-test"
			cfg := &config.Config{}
			var modelID string
			if provider == "claude" {
				cfg.ClaudeKey = []config.ClaudeKey{{APIKey: "fake-key"}}
				modelID = registry.GetClaudeModels()[0].ID
			} else {
				cfg.CodexKey = []config.CodexKey{{APIKey: "fake-key"}}
				modelID = registry.GetCodexProModels()[0].ID
			}
			service := &Service{cfg: cfg}
			service.restrictedAccess.restrictedIDs = func(string) map[string]struct{} {
				return map[string]struct{}{modelID: {}}
			}
			reg := GlobalModelRegistry()
			t.Cleanup(func() { reg.UnregisterClient(id) })
			auth := &coreauth.Auth{
				ID: id, Provider: provider, Status: coreauth.StatusActive,
				Attributes: map[string]string{"api_key": "fake-key", "source": "config:" + provider, "config_index": "0"},
			}
			service.registerModelsForAuth(context.Background(), auth)
			if containsModelID(reg.GetModelsForClient(id), modelID) {
				t.Fatalf("%s API key with no configured models registered restricted catalog model %q", provider, modelID)
			}
			if len(service.explicitRestrictedModelIDs(auth)) != 0 {
				t.Fatal("empty models list counted catalog defaults as explicit")
			}
		})
	}
}

func TestRestrictedModelFetchRetainsLastListAndOnlyReregistersOnGrantChanges(t *testing.T) {
	const id = "restricted-cache-refresh-test"
	auth := restrictedTestAuth("codex", id)
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	service := &Service{cfg: &config.Config{}, coreManager: manager, pluginHost: pluginhost.New()}
	reg := GlobalModelRegistry()
	t.Cleanup(func() { reg.UnregisterClient(id) })
	original := pluginHostModelsForProvider
	pluginHostModelsForProvider = func(_ *pluginhost.Host, _ string) []*ModelInfo {
		return []*ModelInfo{{ID: "restricted"}}
	}
	t.Cleanup(func() { pluginHostModelsForProvider = original })
	service.restrictedAccess.restrictedIDs = func(string) map[string]struct{} {
		return map[string]struct{}{"restricted": {}}
	}
	service.registerModelsForAuth(context.Background(), auth)
	entry := &restrictedModelAccessEntry{identity: restrictedModelIdentity(auth), fetching: true}
	service.restrictedAccess.entries = map[string]*restrictedModelAccessEntry{id: entry}
	initialEpoch := reg.ClientRegistrationEpoch(id)
	fetch := func(ids []string, err error) {
		t.Helper()
		service.fetchRestrictedModels(context.Background(), auth, restrictedModelListerFunc(func(context.Context, *coreauth.Auth) ([]string, error) {
			return ids, err
		}), entry)
	}
	fetch([]string{"ordinary", "restricted"}, nil)
	if got := reg.ClientRegistrationEpoch(id); got <= initialEpoch {
		t.Fatalf("new grant did not re-register: epoch %d <= %d", got, initialEpoch)
	}
	grantedEpoch := reg.ClientRegistrationEpoch(id)
	fetch([]string{"other", "restricted"}, nil)
	if got := reg.ClientRegistrationEpoch(id); got != grantedEpoch {
		t.Fatalf("change to unrestricted IDs re-registered: epoch %d != %d", got, grantedEpoch)
	}
	now := time.Date(2026, time.October, 7, 12, 0, 0, 0, time.UTC)
	service.restrictedAccess.now = func() time.Time { return now }
	entry.nextAt = now.Add(restrictedModelRefreshInterval)
	fetch(nil, errors.New("upstream unavailable"))
	if !entry.nextAt.Equal(now.Add(restrictedModelRefreshInterval)) {
		t.Fatalf("failed fetch next attempt = %v, want %v", entry.nextAt, now.Add(restrictedModelRefreshInterval))
	}
	if got := reg.ClientRegistrationEpoch(id); got != grantedEpoch {
		t.Fatalf("failed fetch re-registered: epoch %d != %d", got, grantedEpoch)
	}
	if !maps.Equal(entry.listed, map[string]struct{}{"other": {}, "restricted": {}}) {
		t.Fatalf("failed fetch discarded last successful list: %+v", entry)
	}
	fetch([]string{}, nil)
	if got := reg.ClientRegistrationEpoch(id); got <= grantedEpoch {
		t.Fatalf("revocation did not re-register: epoch %d <= %d", got, grantedEpoch)
	}
	revokedEpoch := reg.ClientRegistrationEpoch(id)
	fetch([]string{}, nil)
	if got := reg.ClientRegistrationEpoch(id); got != revokedEpoch {
		t.Fatalf("unchanged empty grant set re-registered: epoch %d != %d", got, revokedEpoch)
	}
	service.applyCoreAuthRemoval(context.Background(), id)
	service.restrictedAccess.mu.RLock()
	removedEntry := service.restrictedAccess.entries[id]
	service.restrictedAccess.mu.RUnlock()
	if removedEntry != nil {
		t.Fatal("removed credential retained cached access")
	}
	removedEpoch := reg.ClientRegistrationEpoch(id)
	fetch([]string{"restricted"}, nil)
	if got := reg.ClientRegistrationEpoch(id); got != removedEpoch {
		t.Fatalf("removed credential's stale fetch re-registered: epoch %d != %d", got, removedEpoch)
	}
}

func TestRestrictedModelFetchDoesNotRepeatExcludedGrantRegistration(t *testing.T) {
	const id = "restricted-excluded-grant-test"
	const newGrant = "restricted-new-grant"
	excluded := registry.GetCodexProModels()[0].ID
	auth := restrictedTestAuth("codex", id)
	auth.Attributes["excluded_models"] = excluded
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	service := &Service{cfg: &config.Config{}, coreManager: manager, pluginHost: pluginhost.New()}
	service.restrictedAccess.restrictedIDs = func(string) map[string]struct{} {
		return map[string]struct{}{excluded: {}, newGrant: {}}
	}
	entry := &restrictedModelAccessEntry{identity: restrictedModelIdentity(auth)}
	service.restrictedAccess.entries = map[string]*restrictedModelAccessEntry{id: entry}
	reg := GlobalModelRegistry()
	t.Cleanup(func() { reg.UnregisterClient(id) })
	original := pluginHostModelsForProvider
	pluginHostModelsForProvider = func(_ *pluginhost.Host, _ string) []*ModelInfo {
		return []*ModelInfo{{ID: newGrant}}
	}
	t.Cleanup(func() { pluginHostModelsForProvider = original })
	service.registerModelsForAuth(context.Background(), auth)
	initialEpoch := reg.ClientRegistrationEpoch(id)
	fetch := func(ids ...string) {
		t.Helper()
		service.fetchRestrictedModels(context.Background(), auth, restrictedModelListerFunc(func(context.Context, *coreauth.Auth) ([]string, error) {
			return ids, nil
		}), entry)
	}
	fetch(excluded)
	firstEpoch := reg.ClientRegistrationEpoch(id)
	if firstEpoch <= initialEpoch {
		t.Fatal("newly seen excluded grant did not trigger its initial reconciliation")
	}
	if containsModelID(reg.GetModelsForClient(id), excluded) {
		t.Fatal("excluded grant was registered")
	}
	fetch(excluded)
	if got := reg.ClientRegistrationEpoch(id); got != firstEpoch {
		t.Fatalf("unchanged excluded grant re-registered: %d != %d", got, firstEpoch)
	}
	fetch(excluded, newGrant)
	if !containsModelID(reg.GetModelsForClient(id), newGrant) || reg.ClientRegistrationEpoch(id) <= firstEpoch {
		t.Fatal("new grant did not register")
	}
	fetch(excluded)
	if containsModelID(reg.GetModelsForClient(id), newGrant) {
		t.Fatal("revoked grant remained registered")
	}
	reg.RegisterClient(id, "codex", []*ModelInfo{{ID: newGrant}}) // Denied drift must always be repaired.
	fetch(excluded)
	if containsModelID(reg.GetModelsForClient(id), newGrant) {
		t.Fatal("unchanged denial did not remove a stale grant")
	}
}

func TestRestrictedModelFetchRegrantsAfterNoopRevocationAndExclusionRemoval(t *testing.T) {
	const id = "restricted-noop-revocation-test"
	restricted := registry.GetCodexProModels()[0].ID
	ctx := context.Background()
	auth := restrictedTestAuth("codex", id)
	auth.Attributes["excluded_models"] = restricted
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatal(err)
	}
	service := &Service{cfg: &config.Config{}, coreManager: manager, pluginHost: pluginhost.New()}
	service.restrictedAccess.restrictedIDs = func(string) map[string]struct{} {
		return map[string]struct{}{restricted: {}}
	}
	entry := &restrictedModelAccessEntry{identity: restrictedModelIdentity(auth)}
	service.restrictedAccess.entries = map[string]*restrictedModelAccessEntry{id: entry}
	reg := GlobalModelRegistry()
	t.Cleanup(func() { reg.UnregisterClient(id) })
	service.registerModelsForAuth(ctx, auth)
	fetch := func(ids ...string) {
		t.Helper()
		service.fetchRestrictedModels(ctx, auth, restrictedModelListerFunc(func(context.Context, *coreauth.Auth) ([]string, error) {
			return ids, nil
		}), entry)
	}
	fetch(restricted)
	if !maps.Equal(entry.applied, map[string]struct{}{restricted: {}}) || containsModelID(reg.GetModelsForClient(id), restricted) {
		t.Fatal("excluded grant was not tracked without registering the model")
	}
	before := reg.ClientRegistrationEpoch(id)
	fetch()
	if got := reg.ClientRegistrationEpoch(id); got != before {
		t.Fatalf("no-op revocation re-registered: epoch %d != %d", got, before)
	}
	if len(entry.applied) != 0 {
		t.Fatalf("no-op revocation retained stale applied grants: %v", entry.applied)
	}

	auth = auth.Clone()
	delete(auth.Attributes, "excluded_models")
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatal(err)
	}
	service.registerModelsForAuth(ctx, auth)
	if containsModelID(reg.GetModelsForClient(id), restricted) {
		t.Fatal("revoked model registered when exclusion was removed")
	}
	before = reg.ClientRegistrationEpoch(id)
	fetch(restricted)
	if !containsModelID(reg.GetModelsForClient(id), restricted) || reg.ClientRegistrationEpoch(id) <= before {
		t.Fatal("restored grant did not register after no-op revocation and exclusion removal")
	}
}

func TestRestrictedRegistrationCommitsBeforeRevocationPublishes(t *testing.T) {
	const id = "restricted-registration-revocation-race-test"
	const pluginRestricted = "restricted-race-plugin"
	builtinRestricted := registry.GetCodexProModels()[0].ID
	auth := restrictedTestAuth("codex", id)
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	service := &Service{cfg: &config.Config{}, coreManager: manager, pluginHost: pluginhost.New()}
	entry := &restrictedModelAccessEntry{
		identity: restrictedModelIdentity(auth),
		listed:   map[string]struct{}{builtinRestricted: {}, pluginRestricted: {}},
	}
	service.restrictedAccess.entries = map[string]*restrictedModelAccessEntry{id: entry}
	reg := GlobalModelRegistry()
	t.Cleanup(func() { reg.UnregisterClient(id) })
	original := pluginHostModelsForProvider
	pluginHostModelsForProvider = func(_ *pluginhost.Host, _ string) []*ModelInfo {
		return []*ModelInfo{{ID: pluginRestricted}}
	}
	t.Cleanup(func() { pluginHostModelsForProvider = original })
	filteredBuiltin := make(chan struct{})
	unblock := make(chan struct{})
	defer func() {
		select {
		case <-unblock:
		default:
			close(unblock)
		}
	}()
	var calls atomic.Int32
	service.restrictedAccess.restrictedIDs = func(string) map[string]struct{} {
		if calls.Add(1) == 2 { // Second filter runs after the built-in models were filtered.
			close(filteredBuiltin)
			<-unblock
		}
		return map[string]struct{}{builtinRestricted: {}, pluginRestricted: {}}
	}
	registered := make(chan struct{})
	go func() {
		service.registerModelsForAuth(context.Background(), auth)
		close(registered)
	}()
	<-filteredBuiltin
	fetchStarted := make(chan struct{})
	fetched := make(chan struct{})
	go func() {
		service.fetchRestrictedModels(context.Background(), auth, restrictedModelListerFunc(func(context.Context, *coreauth.Auth) ([]string, error) {
			close(fetchStarted)
			return nil, nil
		}), entry)
		close(fetched)
	}()
	<-fetchStarted
	deadline := time.After(5 * time.Second)
	for {
		if service.restrictedAccess.mu.TryRLock() {
			service.restrictedAccess.mu.RUnlock()
		} else {
			break // The revocation publisher is waiting for the registration's read lock.
		}
		select {
		case <-deadline:
			t.Fatal("revocation publisher never waited for registration")
		default:
			runtime.Gosched()
		}
	}
	close(unblock)
	<-registered
	<-fetched
	if containsModelID(reg.GetModelsForClient(id), builtinRestricted) || containsModelID(reg.GetModelsForClient(id), pluginRestricted) {
		t.Fatal("revoked models survived a registration racing with publication")
	}
}

func TestRestrictedModelFetchRepairsRegistryDrift(t *testing.T) {
	const id = "restricted-registration-drift-test"
	const restricted = "restricted-drift-model"
	auth := restrictedTestAuth("codex", id)
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	service := &Service{cfg: &config.Config{}, coreManager: manager, pluginHost: pluginhost.New()}
	service.restrictedAccess.restrictedIDs = func(string) map[string]struct{} {
		return map[string]struct{}{restricted: {}}
	}
	entry := &restrictedModelAccessEntry{identity: restrictedModelIdentity(auth), listed: map[string]struct{}{restricted: {}}}
	service.restrictedAccess.entries = map[string]*restrictedModelAccessEntry{id: entry}
	reg := GlobalModelRegistry()
	t.Cleanup(func() { reg.UnregisterClient(id) })
	original := pluginHostModelsForProvider
	pluginHostModelsForProvider = func(_ *pluginhost.Host, _ string) []*ModelInfo {
		return []*ModelInfo{{ID: restricted}}
	}
	t.Cleanup(func() { pluginHostModelsForProvider = original })
	service.registerModelsForAuth(context.Background(), auth)
	if !containsModelID(reg.GetModelsForClient(id), restricted) {
		t.Fatal("fixture did not register granted model")
	}
	fetch := func(ids []string) {
		t.Helper()
		service.fetchRestrictedModels(context.Background(), auth, restrictedModelListerFunc(func(context.Context, *coreauth.Auth) ([]string, error) {
			return ids, nil
		}), entry)
	}
	reg.RegisterClient(id, "codex", []*ModelInfo{{ID: "ordinary"}}) // Drift: grant remains, registration lost it.
	before := reg.ClientRegistrationEpoch(id)
	fetch([]string{restricted})
	if !containsModelID(reg.GetModelsForClient(id), restricted) || reg.ClientRegistrationEpoch(id) <= before {
		t.Fatal("identical grant did not restore a missing restricted model")
	}
	fetch(nil)
	reg.RegisterClient(id, "codex", []*ModelInfo{{ID: "alias-for-restricted", MetadataModelID: restricted}}) // Drift: denied alias appears.
	before = reg.ClientRegistrationEpoch(id)
	fetch(nil)
	if containsModelID(reg.GetModelsForClient(id), "alias-for-restricted") || reg.ClientRegistrationEpoch(id) <= before {
		t.Fatal("identical denial did not remove a stale restricted alias")
	}
}

func TestRegisteredRestrictedIDsResolveAliasesAndPrefixes(t *testing.T) {
	const id = "restricted-registered-routes-test"
	auth := restrictedTestAuth("claude", id)
	auth.Prefix = "tenant"
	reg := GlobalModelRegistry()
	t.Cleanup(func() { reg.UnregisterClient(id) })
	reg.RegisterClient(id, "claude", []*ModelInfo{
		{ID: "alias", MetadataModelID: "restricted-a"},
		{ID: "tenant/restricted-b"},
	})
	want := map[string]struct{}{"restricted-a": {}, "restricted-b": {}}
	if got := registeredRestrictedIDs(auth, want); !maps.Equal(got, want) {
		t.Fatalf("registered canonical IDs = %v, want %v", got, want)
	}
}

func TestRestrictedCodexBuiltinHiddenUntilGranted(t *testing.T) {
	const modelID = "gpt-image-2.5-flare"
	auth := restrictedTestAuth("codex", "restricted-codex-builtin-test")
	service := &Service{}
	service.restrictedAccess.restrictedIDs = func(string) map[string]struct{} {
		return map[string]struct{}{modelID: {}}
	}
	models := registry.WithCodexBuiltins([]*registry.ModelInfo{{ID: modelID, RestrictedAccess: true}})
	if containsModelID(service.filterRestrictedModels(auth, models), modelID) {
		t.Fatal("catalog-restricted built-in visible with unknown access")
	}
	service.restrictedAccess.entries = map[string]*restrictedModelAccessEntry{
		auth.ID: {identity: restrictedModelIdentity(auth), listed: map[string]struct{}{modelID: {}}},
	}
	if !containsModelID(service.filterRestrictedModels(auth, models), modelID) {
		t.Fatal("granted built-in model was hidden")
	}
}

func TestRestrictedPluginModelsCannotBypassCatalogAccess(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		for _, mode := range []string{"static", "auth-bound"} {
			t.Run(provider+"/"+mode, func(t *testing.T) {
				const restricted = "restricted-plugin-model"
				id := provider + "-" + mode + "-restricted-plugin-test"
				auth := restrictedTestAuth(provider, id)
				service := &Service{cfg: &config.Config{}, pluginHost: pluginhost.New()}
				service.restrictedAccess.restrictedIDs = func(string) map[string]struct{} {
					return map[string]struct{}{restricted: {}}
				}
				reg := GlobalModelRegistry()
				t.Cleanup(func() { reg.UnregisterClient(id) })
				pluginModels := []*ModelInfo{{ID: restricted, UserDefined: true}, {ID: "ordinary-plugin-model"}}
				if mode == "static" {
					original := pluginHostModelsForProvider
					pluginHostModelsForProvider = func(_ *pluginhost.Host, key string) []*ModelInfo {
						if key != provider {
							t.Errorf("plugin queried for provider %q, want %q", key, provider)
						}
						return pluginModels
					}
					t.Cleanup(func() { pluginHostModelsForProvider = original })
				} else {
					original := pluginHostModelsForAuth
					pluginHostModelsForAuth = func(_ *pluginhost.Host, _ context.Context, _ *coreauth.Auth) pluginhost.AuthModelResult {
						return pluginhost.AuthModelResult{Handled: true, Models: pluginModels}
					}
					t.Cleanup(func() { pluginHostModelsForAuth = original })
				}
				check := func(wantRestricted bool) {
					t.Helper()
					service.registerModelsForAuth(context.Background(), auth)
					foundRestricted, foundOrdinary := false, false
					for _, model := range reg.GetModelsForClient(id) {
						if model.ID == restricted {
							foundRestricted = true
						}
						if model.ID == "ordinary-plugin-model" {
							foundOrdinary = true
						}
					}
					if foundRestricted != wantRestricted || !foundOrdinary {
						t.Fatalf("plugin registration: restricted=%v ordinary=%v, want restricted=%v", foundRestricted, foundOrdinary, wantRestricted)
					}
				}
				check(false) // Unknown grants fail closed, even with UserDefined=true.
				service.restrictedAccess.entries = map[string]*restrictedModelAccessEntry{
					id: {identity: restrictedModelIdentity(auth), listed: map[string]struct{}{}},
				}
				check(false) // Known denial also fails closed.
				service.restrictedAccess.entries[id].listed[restricted] = struct{}{}
				check(true)
			})
		}
	}
}

type restrictedRoundTripFunc func(*http.Request) (*http.Response, error)

func (f restrictedRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestRestrictedModelQueueIsAsyncAndRefreshesAtFifteenMinutes(t *testing.T) {
	const id = "restricted-async-refresh-test"
	auth := restrictedTestAuth("codex", id)
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	manager.RegisterExecutor(runtimeexecutor.NewCodexAutoExecutor(nil))
	now := time.Date(2026, time.October, 7, 12, 0, 0, 0, time.UTC)
	requests := make(chan struct{}, 2)
	var calls atomic.Int32
	unblock := make(chan struct{})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", restrictedRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		call := calls.Add(1)
		requests <- struct{}{}
		if req.Header.Get("Authorization") != "Bearer fake-token" {
			t.Errorf("missing OAuth bearer token")
		}
		if req.URL.Host != "chatgpt.com" {
			t.Errorf("unexpected upstream host %q", req.URL.Host)
		}
		if call == 1 {
			<-unblock
		}
		status, body := http.StatusServiceUnavailable, `unavailable`
		if call == 2 {
			status, body = http.StatusOK, `{"models":[]}`
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	}))
	service := &Service{cfg: &config.Config{}, coreManager: manager}
	service.restrictedAccess.ctx = ctx
	service.restrictedAccess.now = func() time.Time { return now }
	reg := GlobalModelRegistry()
	t.Cleanup(func() { reg.UnregisterClient(id) })
	service.completeModelRegistrationForAuth(context.Background(), auth) // Must return while the fetch is blocked.
	<-requests
	service.restrictedAccess.mu.RLock()
	entry := service.restrictedAccess.entries[id]
	if entry == nil || !entry.fetching || !entry.nextAt.Equal(now.Add(restrictedModelRefreshInterval)) {
		t.Fatalf("unexpected first fetch state: %+v", entry)
	}
	firstDone := entry.done
	service.restrictedAccess.mu.RUnlock()
	service.queueRestrictedModelFetch(context.Background(), auth)
	if calls.Load() != 1 {
		t.Fatal("duplicate fetch started while an earlier fetch was in flight")
	}
	close(unblock)
	<-firstDone
	if !entry.nextAt.Equal(now.Add(restrictedModelRefreshInterval)) {
		t.Fatalf("failed fetch changed next refresh time: %v", entry.nextAt)
	}
	service.queueRestrictedModelFetch(context.Background(), auth)
	if calls.Load() != 1 {
		t.Fatal("fetched again before the 15-minute interval")
	}
	now = now.Add(restrictedModelRefreshInterval)
	service.queueRestrictedModelFetch(context.Background(), auth)
	<-requests
	service.restrictedAccess.mu.RLock()
	secondDone := entry.done
	service.restrictedAccess.mu.RUnlock()
	<-secondDone
}

func TestRestrictedModelIdentityPreservesKnownAccountAcrossTokenRefresh(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		auth := restrictedTestAuth(provider, provider+"-stable-account-test")
		if provider == "claude" {
			auth.Metadata["account_uuid"] = "claude-account"
			auth.Metadata["organization_uuid"] = "claude-org"
		}
		original := restrictedModelIdentity(auth)
		rotated := auth.Clone()
		rotated.Metadata["access_token"] = "refreshed-token"
		if got := restrictedModelIdentity(rotated); got != original {
			t.Errorf("%s token refresh changed access identity: %q != %q", provider, got, original)
		}
		other := auth.Clone()
		if provider == "claude" {
			other.Metadata["organization_uuid"] = "another-org"
		} else {
			other.Metadata["account_id"] = "another-account"
		}
		if got := restrictedModelIdentity(other); got == original {
			t.Errorf("%s account replacement kept access identity", provider)
		}
	}
}

func TestRestrictedAPIKeyAliasUsesConfiguredUpstreamName(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			const upstream = "restricted-upstream"
			cfg := &config.Config{}
			if provider == "claude" {
				cfg.ClaudeKey = []config.ClaudeKey{{APIKey: "fake-key", Models: []internalconfig.ClaudeModel{{Name: upstream, Alias: "public-alias"}}}}
			} else {
				cfg.CodexKey = []config.CodexKey{{APIKey: "fake-key", Models: []internalconfig.CodexModel{{Name: upstream, Alias: "public-alias"}}}}
			}
			service := &Service{cfg: cfg}
			service.restrictedAccess.restrictedIDs = func(string) map[string]struct{} {
				return map[string]struct{}{upstream: {}}
			}
			id := provider + "-alias-restricted-test"
			reg := GlobalModelRegistry()
			t.Cleanup(func() { reg.UnregisterClient(id) })
			auth := &coreauth.Auth{ID: id, Provider: provider, Attributes: map[string]string{"api_key": "fake-key", "source": "config:" + provider, "config_index": "0"}}
			service.registerModelsForAuth(context.Background(), auth)
			if !containsModelID(reg.GetModelsForClient(id), "public-alias") {
				t.Fatal("explicit upstream model was hidden behind its alias")
			}
			// Listing only the alias, not its upstream name, must not grant it.
			if _, ok := service.explicitRestrictedModelIDs(auth)["public-alias"]; ok {
				t.Fatal("alias counted as an explicit upstream model")
			}
			aliasOnly := &Service{cfg: &config.Config{}}
			if provider == "claude" {
				aliasOnly.cfg.ClaudeKey = []config.ClaudeKey{{APIKey: "fake-key", Models: []internalconfig.ClaudeModel{{Name: "ordinary-upstream", Alias: upstream}}}}
			} else {
				aliasOnly.cfg.CodexKey = []config.CodexKey{{APIKey: "fake-key", Models: []internalconfig.CodexModel{{Name: "ordinary-upstream", Alias: upstream}}}}
			}
			aliasOnly.restrictedAccess.restrictedIDs = service.restrictedAccess.restrictedIDs
			if got := aliasOnly.filterRestrictedModels(auth, []*ModelInfo{{ID: upstream, MetadataModelID: upstream}}); len(got) != 0 {
				t.Fatalf("alias alone granted an unconfigured restricted upstream model: %v", modelIDs(got))
			}
		})
	}
}
