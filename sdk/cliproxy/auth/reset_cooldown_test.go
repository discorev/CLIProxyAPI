package auth

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestResetConfirmedClearsOnlyRestoredQuotaCooldowns(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		for _, outcome := range []string{"reset", "unknown", "already_used", "not_limited", "auth_error"} {
			t.Run(provider+"/"+outcome, func(t *testing.T) {
				manager, executor, _ := setupResetManager(t, provider)
				inventory := resetTestEntry(provider, 100, 5*time.Hour, 2*time.Hour)
				auth := seedResetAuth(t, manager, "reset-cooldown-"+provider, provider, inventory)
				next := time.Now().Add(24 * time.Hour)
				quotaState := func(reason string, status int) *ModelState {
					return &ModelState{Status: StatusError, Unavailable: true, NextRetryAfter: next, LastError: &Error{HTTPStatus: status}, Quota: QuotaState{Exceeded: true, Reason: reason, NextRecoverAt: next}}
				}
				auth.Quota = QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: next}
				auth.Unavailable = true
				auth.NextRetryAfter = next
				auth.Status = StatusError
				auth.LastError = &Error{HTTPStatus: 429}
				auth.ModelStates = map[string]*ModelState{
					"restored":   quotaState("credential_quota", 429),
					"fable":      quotaState("quota", 429),
					"bad-auth":   {Status: StatusError, Unavailable: true, NextRetryAfter: next, LastError: &Error{HTTPStatus: 401}},
					"bad-server": {Status: StatusError, Unavailable: true, NextRetryAfter: next, LastError: &Error{HTTPStatus: 503}},
					"mixed":      quotaState("credential_quota", 403),
				}
				if _, err := manager.Update(context.Background(), auth); err != nil {
					t.Fatal(err)
				}
				reg := registry.GetGlobalRegistry()
				reg.RegisterClient(auth.ID, provider, []*registry.ModelInfo{{ID: "restored"}, {ID: "fable"}, {ID: "bad-auth"}, {ID: "bad-server"}, {ID: "mixed"}})
				t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
				reg.SetModelQuotaExceeded(auth.ID, "restored")
				reg.SuspendClientModel(auth.ID, "restored", "quota")
				store := &recordingCooldownStateStore{}
				manager.SetCooldownStateStore(store)
				executor.fetch = func(context.Context, *Auth) (UsageFetchResult, error) {
					return UsageFetchResult{Resets: inventory.Resets, Windows: inventory.Windows}, nil
				}
				executor.apply = func(context.Context, *Auth, ResetRequest) (ResetResult, error) {
					return ResetResult{Result: outcome}, nil
				}
				if _, _, err := manager.ApplyCredentialReset(context.Background(), auth.ID, ""); err != nil {
					t.Fatal(err)
				}
				manager.mu.RLock()
				got := manager.auths[auth.ID].Clone()
				manager.mu.RUnlock()
				if outcome != "reset" {
					if !got.Quota.Exceeded || !got.ModelStates["restored"].Quota.Exceeded || store.saveCount.Load() != 0 {
						t.Fatal("unconfirmed result cleared cooldown")
					}
					return
				}
				if got.ModelStates["restored"].Quota.Exceeded || got.ModelStates["restored"].Unavailable {
					t.Fatal("credential quota not cleared")
				}
				if retained := got.ModelStates["fable"].Quota.Exceeded; retained != (provider == "claude") {
					t.Fatalf("fable retained=%v provider=%s", retained, provider)
				}
				for _, model := range []string{"bad-auth", "bad-server", "mixed"} {
					if state := got.ModelStates[model]; !state.Unavailable || state.LastError == nil || !state.NextRetryAfter.Equal(next) {
						t.Fatalf("non-quota state %s cleared: %+v", model, state)
					}
				}
				if got.ModelStates["mixed"].Quota.Exceeded {
					t.Fatal("mixed state's restored quota was retained")
				}
				if reg.GetModelCount("restored") != 1 {
					t.Fatal("registry not resumed")
				}
				picked, err := manager.scheduler.pickSingle(context.Background(), provider, "restored", cliproxyexecutor.Options{}, nil)
				if err != nil || picked == nil || picked.ID != auth.ID {
					t.Fatalf("scheduler stale: picked=%v err=%v", picked, err)
				}
				if store.saveCount.Load() == 0 {
					t.Fatal("cooldown changes not persisted")
				}
				for _, record := range store.savedRecords() {
					if record.Model == "restored" {
						t.Fatal("cleared quota persisted")
					}
				}
			})
		}
	}
}
