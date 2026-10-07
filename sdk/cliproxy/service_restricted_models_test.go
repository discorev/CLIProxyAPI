package cliproxy

import (
	"context"
	"errors"
	"io"
	"maps"
	"net/http"
	"reflect"
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

func TestRestrictedModelFilterFailsClosedExceptConfiguredModels(t *testing.T) {
	for _, provider := range []string{"claude", "codex", "gemini"} {
		t.Run(provider, func(t *testing.T) {
			auth := restrictedTestAuth(provider, provider+"-restricted-test")
			service := &Service{}
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
				auth.ID: {identity: restrictedModelIdentity(auth), listed: map[string]struct{}{"restricted": {}}, fetched: true},
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
	if !entry.nextAt.Equal(now.Add(restrictedModelRetryInterval)) {
		t.Fatalf("failed fetch next attempt = %v, want %v", entry.nextAt, now.Add(restrictedModelRetryInterval))
	}
	if got := reg.ClientRegistrationEpoch(id); got != grantedEpoch {
		t.Fatalf("failed fetch re-registered: epoch %d != %d", got, grantedEpoch)
	}
	if !entry.fetched || !maps.Equal(entry.listed, map[string]struct{}{"other": {}, "restricted": {}}) {
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

func TestRestrictedRevocationFailsClosedAcrossPluginDiscoveryErrors(t *testing.T) {
	const id = "restricted-plugin-discovery-revocation-test"
	const restricted = "restricted-plugin-discovery-model"
	auth := restrictedTestAuth("codex", id)
	auth.Prefix = "tenant"
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	service := &Service{cfg: &config.Config{}, coreManager: manager, pluginHost: pluginhost.New()}
	service.restrictedAccess.restrictedIDs = func(string) map[string]struct{} {
		return map[string]struct{}{restricted: {}}
	}
	entry := &restrictedModelAccessEntry{identity: restrictedModelIdentity(auth), listed: map[string]struct{}{restricted: {}}, fetched: true}
	service.restrictedAccess.entries = map[string]*restrictedModelAccessEntry{id: entry}
	reg := GlobalModelRegistry()
	t.Cleanup(func() { reg.UnregisterClient(id) })
	var failing bool
	var failedCalls int
	original := pluginHostModelsForAuth
	pluginHostModelsForAuth = func(_ *pluginhost.Host, _ context.Context, _ *coreauth.Auth) pluginhost.AuthModelResult {
		if failing {
			failedCalls++
			return pluginhost.AuthModelResult{Handled: true, Err: errors.New("plugin discovery unavailable")}
		}
		return pluginhost.AuthModelResult{Handled: true, Models: []*ModelInfo{{ID: restricted}, {ID: "ordinary-plugin-model"}}}
	}
	t.Cleanup(func() { pluginHostModelsForAuth = original })
	service.registerModelsForAuth(context.Background(), auth)
	if !containsModelID(reg.GetModelsForClient(id), "tenant/"+restricted) {
		t.Fatal("fixture did not register the prefixed restricted model")
	}

	fetchDenied := func() {
		t.Helper()
		service.fetchRestrictedModels(context.Background(), auth, restrictedModelListerFunc(func(context.Context, *coreauth.Auth) ([]string, error) {
			return []string{"ordinary-plugin-model"}, nil
		}), entry)
	}
	failing = true
	fetchDenied()
	if failedCalls != 2 {
		t.Fatalf("plugin discovery failed %d times, want both refresh passes", failedCalls)
	}
	if models := reg.GetModelsForClient(id); len(models) != 0 {
		t.Fatalf("plugin failure preserved a revoked registration: %v", modelIDs(models))
	}
	if !entry.pending.Load() {
		t.Fatal("failed refresh was incorrectly marked reconciled")
	}
	failing = false
	fetchDenied() // Identical denial must retry the failed reconciliation.
	if containsModelID(reg.GetModelsForClient(id), restricted) || containsModelID(reg.GetModelsForClient(id), "tenant/"+restricted) {
		t.Fatal("plugin recovery restored the revoked restricted model")
	}
	if !containsModelID(reg.GetModelsForClient(id), "ordinary-plugin-model") || entry.pending.Load() {
		t.Fatal("plugin recovery failed to restore ordinary models and clear pending reconciliation")
	}
	epoch := reg.ClientRegistrationEpoch(id)
	fetchDenied()
	if got := reg.ClientRegistrationEpoch(id); got != epoch {
		t.Fatalf("reconciled denial re-registered: %d != %d", got, epoch)
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
	entry := &restrictedModelAccessEntry{identity: restrictedModelIdentity(auth), listed: map[string]struct{}{restricted: {}}, fetched: true}
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

func TestRestrictedExcludedGrantDoesNotReregisterOnUnchangedFetch(t *testing.T) {
	const id = "restricted-excluded-grant-test"
	restricted := registry.GetCodexProModels()[0].ID
	auth := restrictedTestAuth("codex", id)
	auth.Attributes["excluded_models"] = restricted
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	service := &Service{cfg: &config.Config{}, coreManager: manager}
	service.restrictedAccess.restrictedIDs = func(string) map[string]struct{} {
		return map[string]struct{}{restricted: {}}
	}
	entry := &restrictedModelAccessEntry{identity: restrictedModelIdentity(auth), listed: map[string]struct{}{restricted: {}}, fetched: true}
	service.restrictedAccess.entries = map[string]*restrictedModelAccessEntry{id: entry}
	reg := GlobalModelRegistry()
	t.Cleanup(func() { reg.UnregisterClient(id) })
	service.registerModelsForAuth(context.Background(), auth)
	if containsModelID(reg.GetModelsForClient(id), restricted) {
		t.Fatal("fixture registered a configured exclusion")
	}
	epoch := reg.ClientRegistrationEpoch(id)
	service.fetchRestrictedModels(context.Background(), auth, restrictedModelListerFunc(func(context.Context, *coreauth.Auth) ([]string, error) {
		return []string{restricted}, nil
	}), entry)
	if got := reg.ClientRegistrationEpoch(id); got != epoch {
		t.Fatalf("unchanged, excluded grant re-registered: %d != %d", got, epoch)
	}
}

func TestRestrictedCodexBuiltinHiddenUntilGranted(t *testing.T) {
	const modelID = "gpt-image-2.5-flare"
	auth := restrictedTestAuth("codex", "restricted-codex-builtin-test")
	service := &Service{}
	models := registry.WithCodexBuiltins([]*registry.ModelInfo{{ID: modelID, RestrictedAccess: true}})
	if containsModelID(service.filterRestrictedModels(auth, models), modelID) {
		t.Fatal("catalog-restricted built-in visible with unknown access")
	}
	service.restrictedAccess.entries = map[string]*restrictedModelAccessEntry{
		auth.ID: {identity: restrictedModelIdentity(auth), fetched: true, listed: map[string]struct{}{modelID: {}}},
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
					id: {identity: restrictedModelIdentity(auth), listed: map[string]struct{}{}, fetched: true},
				}
				check(false) // Known denial also fails closed.
				service.restrictedAccess.entries[id].listed[restricted] = struct{}{}
				check(true)
			})
		}
	}
}

func TestCatalogRestrictionCannotBeUndoneByConcurrentRegistration(t *testing.T) {
	const id = "restricted-catalog-race-test"
	const modelID = "newly-restricted-plugin-model"
	auth := restrictedTestAuth("codex", id)
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	service := &Service{cfg: &config.Config{}, coreManager: manager, pluginHost: pluginhost.New()}
	var restricted atomic.Bool
	service.restrictedAccess.restrictedIDs = func(string) map[string]struct{} {
		if restricted.Load() {
			return map[string]struct{}{modelID: {}}
		}
		return nil
	}
	entry := &restrictedModelAccessEntry{identity: restrictedModelIdentity(auth), fetched: true, listed: map[string]struct{}{}}
	service.restrictedAccess.entries = map[string]*restrictedModelAccessEntry{id: entry}
	reg := GlobalModelRegistry()
	t.Cleanup(func() { reg.UnregisterClient(id) })
	original := pluginHostModelsForProvider
	pluginHostModelsForProvider = func(_ *pluginhost.Host, _ string) []*ModelInfo {
		return []*ModelInfo{{ID: modelID}}
	}
	t.Cleanup(func() { pluginHostModelsForProvider = original })

	filtered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	service.restrictedAccess.beforeCommit = func() {
		if calls.Add(1) == 1 {
			close(filtered)
			<-release
		}
	}
	registrationDone := make(chan struct{})
	go func() {
		defer close(registrationDone)
		service.registerModelsForAuth(context.Background(), auth)
	}()
	<-filtered
	// Simulate catalog publication followed by its completed refresh callback.
	restricted.Store(true)
	service.refreshModelRegistrationForAuth(auth)
	if containsModelID(reg.GetModelsForClient(id), modelID) {
		t.Fatal("catalog refresh left a newly restricted model registered")
	}
	close(release)
	<-registrationDone
	if containsModelID(reg.GetModelsForClient(id), modelID) {
		t.Fatal("earlier unrestricted registration overwrote the catalog refresh")
	}
	epoch := reg.ClientRegistrationEpoch(id)
	service.fetchRestrictedModels(context.Background(), auth, restrictedModelListerFunc(func(context.Context, *coreauth.Auth) ([]string, error) {
		return nil, nil
	}), entry)
	if got := reg.ClientRegistrationEpoch(id); got != epoch {
		t.Fatalf("unchanged empty access list re-registered: %d != %d", got, epoch)
	}
	if containsModelID(reg.GetModelsForClient(id), modelID) {
		t.Fatal("unchanged empty fetch exposed newly restricted model")
	}
}

func TestRestrictedRevocationCannotBeUndoneByConcurrentRegistration(t *testing.T) {
	const id = "restricted-concurrent-revocation-test"
	const restricted = "restricted-plugin-race-model"
	auth := restrictedTestAuth("codex", id)
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	service := &Service{cfg: &config.Config{}, coreManager: manager, pluginHost: pluginhost.New()}
	service.restrictedAccess.restrictedIDs = func(string) map[string]struct{} {
		return map[string]struct{}{restricted: {}}
	}
	service.restrictedAccess.entries = map[string]*restrictedModelAccessEntry{
		id: {identity: restrictedModelIdentity(auth), listed: map[string]struct{}{restricted: {}}, fetched: true},
	}
	reg := GlobalModelRegistry()
	t.Cleanup(func() { reg.UnregisterClient(id) })
	original := pluginHostModelsForProvider
	pluginHostModelsForProvider = func(_ *pluginhost.Host, _ string) []*ModelInfo {
		return []*ModelInfo{{ID: restricted}}
	}
	t.Cleanup(func() { pluginHostModelsForProvider = original })
	service.registerModelsForAuth(context.Background(), auth)
	if !containsModelID(reg.GetModelsForClient(id), restricted) {
		t.Fatal("fixture failed to register granted model")
	}
	filtered := make(chan struct{})
	release := make(chan struct{})
	service.restrictedAccess.beforeCommit = func() {
		service.restrictedAccess.beforeCommit = nil // Only pause the stale registration.
		close(filtered)
		<-release
	}
	registrationDone := make(chan struct{})
	go func() {
		defer close(registrationDone)
		service.registerModelsForAuth(context.Background(), auth)
	}()
	<-filtered
	readyToPublish := make(chan struct{})
	service.restrictedAccess.beforePublish = func() { close(readyToPublish) }
	entry := service.restrictedAccess.entries[id]
	fetchDone := make(chan struct{})
	go func() {
		defer close(fetchDone)
		service.fetchRestrictedModels(context.Background(), auth, restrictedModelListerFunc(func(context.Context, *coreauth.Auth) ([]string, error) {
			return nil, nil
		}), entry)
	}()
	<-readyToPublish // Fetch is ready, but the in-flight registration owns the access snapshot.
	writerBlocked := !service.restrictedAccess.mu.TryLock()
	if !writerBlocked {
		service.restrictedAccess.mu.Unlock()
	}
	close(release)
	<-registrationDone
	<-fetchDone
	if !writerBlocked {
		t.Fatal("revocation could publish between filtering and the registry commit")
	}
	if containsModelID(reg.GetModelsForClient(id), restricted) {
		t.Fatal("concurrent stale registration restored the revoked model")
	}
	// A subsequent identical fetch does not re-register; the first refresh must win.
	service.restrictedAccess.beforePublish = nil
	epoch := reg.ClientRegistrationEpoch(id)
	service.fetchRestrictedModels(context.Background(), auth, restrictedModelListerFunc(func(context.Context, *coreauth.Auth) ([]string, error) {
		return nil, nil
	}), entry)
	if got := reg.ClientRegistrationEpoch(id); got != epoch {
		t.Fatalf("unchanged denial unexpectedly re-registered: %d != %d", got, epoch)
	}
}

func containsModelID(models []*ModelInfo, id string) bool {
	for _, model := range models {
		if model.ID == id {
			return true
		}
	}
	return false
}

type restrictedRoundTripFunc func(*http.Request) (*http.Response, error)

func (f restrictedRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestRestrictedModelQueueIsAsyncAndRefreshesAtSixHours(t *testing.T) {
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
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"models":[]}`)), Request: req}, nil
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
	if entry == nil || !entry.fetching || !entry.nextAt.Equal(now.Add(6*time.Hour)) {
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
	service.queueRestrictedModelFetch(context.Background(), auth)
	if calls.Load() != 1 {
		t.Fatal("fetched again before the six-hour interval")
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

func TestLegacyClaudeTokenRotationFailsClosedUntilFirstFetch(t *testing.T) {
	const id = "legacy-claude-token-rotation-test"
	const modelID = "restricted-legacy-claude-plugin-model"
	oldAuth := restrictedTestAuth("claude", id) // No account or organization UUID.
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), oldAuth); err != nil {
		t.Fatal(err)
	}
	service := &Service{cfg: &config.Config{}, coreManager: manager, pluginHost: pluginhost.New()}
	service.restrictedAccess.restrictedIDs = func(string) map[string]struct{} {
		return map[string]struct{}{modelID: {}}
	}
	service.restrictedAccess.entries = map[string]*restrictedModelAccessEntry{
		id: {identity: restrictedModelIdentity(oldAuth), listed: map[string]struct{}{modelID: {}}, fetched: true},
	}
	reg := GlobalModelRegistry()
	t.Cleanup(func() { reg.UnregisterClient(id) })
	original := pluginHostModelsForProvider
	pluginHostModelsForProvider = func(_ *pluginhost.Host, _ string) []*ModelInfo {
		return []*ModelInfo{{ID: modelID}}
	}
	t.Cleanup(func() { pluginHostModelsForProvider = original })
	service.registerModelsForAuth(context.Background(), oldAuth)
	if !containsModelID(reg.GetModelsForClient(id), modelID) {
		t.Fatal("fixture failed to register the original token's grant")
	}

	rotated := oldAuth.Clone()
	rotated.Metadata["access_token"] = "rotated-token"
	if restrictedModelIdentity(rotated) == restrictedModelIdentity(oldAuth) {
		t.Fatal("legacy Claude token rotation retained the previous identity")
	}
	if _, err := manager.Update(context.Background(), rotated); err != nil {
		t.Fatal(err)
	}
	service.registerModelsForAuth(context.Background(), rotated)
	if containsModelID(reg.GetModelsForClient(id), modelID) {
		t.Fatal("rotated token inherited the old grant before its own fetch")
	}
	entry := &restrictedModelAccessEntry{identity: restrictedModelIdentity(rotated)}
	service.restrictedAccess.entries[id] = entry
	before := reg.ClientRegistrationEpoch(id)
	fetchEmpty := func() {
		service.fetchRestrictedModels(context.Background(), rotated, restrictedModelListerFunc(func(context.Context, *coreauth.Auth) ([]string, error) {
			return nil, nil
		}), entry)
	}
	fetchEmpty()
	if got := reg.ClientRegistrationEpoch(id); got <= before {
		t.Fatalf("first empty fetch did not reconcile new identity: epoch %d <= %d", got, before)
	}
	first := reg.ClientRegistrationEpoch(id)
	fetchEmpty()
	if got := reg.ClientRegistrationEpoch(id); got != first {
		t.Fatalf("unchanged empty fetch unexpectedly re-registered: epoch %d != %d", got, first)
	}
	if containsModelID(reg.GetModelsForClient(id), modelID) {
		t.Fatal("legacy token rotation exposed the old grant after fetch")
	}
}

func TestOAuthReplacementWithAPIKeyCannotRestoreOldGrant(t *testing.T) {
	const id = "restricted-oauth-to-apikey-test"
	const modelID = "old-claude-grant-plugin-model"
	oldAuth := restrictedTestAuth("claude", id)
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), oldAuth); err != nil {
		t.Fatal(err)
	}
	service := &Service{cfg: &config.Config{ClaudeKey: []config.ClaudeKey{{APIKey: "replacement-key"}}}, coreManager: manager, pluginHost: pluginhost.New()}
	service.restrictedAccess.restrictedIDs = func(string) map[string]struct{} {
		return map[string]struct{}{modelID: {}}
	}
	service.restrictedAccess.entries = map[string]*restrictedModelAccessEntry{
		id: {identity: restrictedModelIdentity(oldAuth), listed: map[string]struct{}{modelID: {}}, fetched: true},
	}
	reg := GlobalModelRegistry()
	t.Cleanup(func() { reg.UnregisterClient(id) })
	original := pluginHostModelsForProvider
	pluginHostModelsForProvider = func(_ *pluginhost.Host, _ string) []*ModelInfo {
		return []*ModelInfo{{ID: modelID}}
	}
	t.Cleanup(func() { pluginHostModelsForProvider = original })
	service.registerModelsForAuth(context.Background(), oldAuth)
	if !containsModelID(reg.GetModelsForClient(id), modelID) {
		t.Fatal("fixture failed to register the OAuth grant")
	}

	var registrations atomic.Int32
	atFinalPass := make(chan struct{})
	release := make(chan struct{})
	service.restrictedAccess.beforeCommit = func() {
		if registrations.Add(1) == 2 {
			close(atFinalPass)
			<-release
		}
	}
	oldRefreshDone := make(chan struct{})
	go func() {
		defer close(oldRefreshDone)
		service.refreshModelRegistrationForAuth(oldAuth)
	}()
	<-atFinalPass

	newAuth := oldAuth.Clone()
	newAuth.Attributes = map[string]string{"api_key": "replacement-key", "source": "config:claude", "config_index": "0"}
	newAuth.Metadata = nil
	if _, err := manager.Update(context.Background(), newAuth); err != nil {
		t.Fatal(err)
	}
	service.registerModelsForAuth(context.Background(), newAuth)
	if containsModelID(reg.GetModelsForClient(id), modelID) {
		t.Fatal("API-key registration inherited OAuth grant")
	}
	close(release)
	<-oldRefreshDone
	service.queueRestrictedModelFetch(context.Background(), newAuth)
	service.sweepRestrictedModelAccess(context.Background())
	service.restrictedAccess.mu.RLock()
	entry := service.restrictedAccess.entries[id]
	service.restrictedAccess.mu.RUnlock()
	if entry != nil {
		t.Fatal("API-key replacement retained the OAuth access cache")
	}
	if containsModelID(reg.GetModelsForClient(id), modelID) {
		t.Fatal("old final pass restored the OAuth grant after API-key replacement")
	}
	// A delayed old refresh may begin after the API-key registration itself.
	service.registerModelsForAuth(context.Background(), oldAuth)
	if containsModelID(reg.GetModelsForClient(id), modelID) {
		t.Fatal("late old registration restored the OAuth grant")
	}

	// A stale attempt must not supersede a current registration still in flight.
	filteredNew := make(chan struct{})
	releaseNew := make(chan struct{})
	service.restrictedAccess.beforeCommit = func() {
		close(filteredNew)
		<-releaseNew
	}
	before := reg.ClientRegistrationEpoch(id)
	newDone := make(chan struct{})
	go func() {
		defer close(newDone)
		service.registerModelsForAuth(context.Background(), newAuth)
	}()
	<-filteredNew
	service.registerModelsForAuth(context.Background(), oldAuth)
	close(releaseNew)
	<-newDone
	if got := reg.ClientRegistrationEpoch(id); got <= before {
		t.Fatalf("stale old attempt suppressed current registration: epoch %d <= %d", got, before)
	}
	if containsModelID(reg.GetModelsForClient(id), modelID) {
		t.Fatal("concurrent late old attempt restored the OAuth grant")
	}
}

func TestRestrictedAccountReplacementReconcilesUnchangedEmptyFetch(t *testing.T) {
	const id = "restricted-account-replacement-test"
	const restricted = "restricted-replacement-plugin-model"
	oldAuth := restrictedTestAuth("codex", id)
	manager := coreauth.NewManager(nil, nil, nil)
	if _, errRegister := manager.Register(context.Background(), oldAuth); errRegister != nil {
		t.Fatal(errRegister)
	}
	manager.RegisterExecutor(runtimeexecutor.NewCodexAutoExecutor(nil))
	service := &Service{cfg: &config.Config{}, coreManager: manager, pluginHost: pluginhost.New()}
	service.restrictedAccess.restrictedIDs = func(string) map[string]struct{} {
		return map[string]struct{}{restricted: {}}
	}
	service.restrictedAccess.entries = map[string]*restrictedModelAccessEntry{
		id: {identity: restrictedModelIdentity(oldAuth), listed: map[string]struct{}{restricted: {}}, fetched: true},
	}
	reg := GlobalModelRegistry()
	t.Cleanup(func() { reg.UnregisterClient(id) })
	original := pluginHostModelsForProvider
	pluginHostModelsForProvider = func(_ *pluginhost.Host, _ string) []*ModelInfo {
		return []*ModelInfo{{ID: restricted}}
	}
	t.Cleanup(func() { pluginHostModelsForProvider = original })
	service.registerModelsForAuth(context.Background(), oldAuth)
	if !containsModelID(reg.GetModelsForClient(id), restricted) {
		t.Fatal("fixture failed to register the old account's grant")
	}

	var registrations atomic.Int32
	atFinalPass := make(chan struct{})
	release := make(chan struct{})
	service.restrictedAccess.beforeCommit = func() {
		if registrations.Add(1) == 2 {
			close(atFinalPass)
			<-release
		}
	}
	oldRefreshDone := make(chan struct{})
	go func() {
		defer close(oldRefreshDone)
		service.refreshModelRegistrationForAuth(oldAuth)
	}()
	<-atFinalPass

	newAuth := oldAuth.Clone()
	newAuth.Metadata["account_id"] = "account-2"
	if _, errUpdate := manager.Update(context.Background(), newAuth); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	service.registerModelsForAuth(context.Background(), newAuth)
	if containsModelID(reg.GetModelsForClient(id), restricted) {
		t.Fatal("new account inherited the old account's cached grant")
	}
	close(release)
	<-oldRefreshDone
	if containsModelID(reg.GetModelsForClient(id), restricted) {
		t.Fatal("stale final pass restored the previous account's grant")
	}

	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", restrictedRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"models":[]}`)), Request: req}, nil
	}))
	staleEpoch := reg.ClientRegistrationEpoch(id)
	service.queueRestrictedModelFetch(ctx, newAuth)
	service.restrictedAccess.mu.RLock()
	entry := service.restrictedAccess.entries[id]
	if entry == nil || entry.identity != restrictedModelIdentity(newAuth) || entry.done == nil {
		service.restrictedAccess.mu.RUnlock()
		t.Fatalf("new account did not replace the cached identity: %+v", entry)
	}
	firstDone := entry.done
	service.restrictedAccess.mu.RUnlock()
	<-firstDone
	if containsModelID(reg.GetModelsForClient(id), restricted) {
		t.Fatal("first empty fetch left the old account's grant registered")
	}
	firstEpoch := reg.ClientRegistrationEpoch(id)
	if firstEpoch <= staleEpoch {
		t.Fatalf("first empty fetch did not reconcile the registration: epoch %d <= %d", firstEpoch, staleEpoch)
	}
	service.restrictedAccess.mu.Lock()
	entry.done = nil // Direct subsequent fetch does not reuse the completed request's channel.
	service.restrictedAccess.mu.Unlock()
	service.fetchRestrictedModels(context.Background(), newAuth, restrictedModelListerFunc(func(context.Context, *coreauth.Auth) ([]string, error) {
		return nil, nil
	}), entry)
	if got := reg.ClientRegistrationEpoch(id); got != firstEpoch {
		t.Fatalf("unchanged empty fetch re-registered: epoch %d != %d", got, firstEpoch)
	}
}
