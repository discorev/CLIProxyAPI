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

func TestRestrictedModelFetchRetainsLastListAndOnlyReregistersOnGrantChanges(t *testing.T) {
	const id = "restricted-cache-refresh-test"
	auth := restrictedTestAuth("codex", id)
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	service := &Service{cfg: &config.Config{}, coreManager: manager}
	reg := GlobalModelRegistry()
	t.Cleanup(func() { reg.UnregisterClient(id) })
	service.registerModelsForAuth(context.Background(), auth)
	service.restrictedAccess.restrictedIDs = func(string) map[string]struct{} {
		return map[string]struct{}{"restricted": {}}
	}
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
	if !containsModelID(reg.GetModelsForClient(id), restricted) {
		t.Fatal("fixture did not reproduce the stale final-pass overwrite")
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
