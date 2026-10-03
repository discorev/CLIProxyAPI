package auth

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	log "github.com/sirupsen/logrus"
)

func TestResetPreSendUsesCurrentCredential(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		for _, rule := range []string{"manual", "exhausted"} {
			for _, change := range []string{"token", "removed", "disabled", "status_disabled", "identity", "provider", "readded"} {
				t.Run(provider+"/"+rule+"/"+change, func(t *testing.T) {
					manager, executor, _ := setupResetManager(t, provider)
					inventory := resetTestEntry(provider, 100, 5*time.Hour, 2*time.Hour)
					auth := seedResetAuth(t, manager, "a", provider, inventory)
					reserved, applier, state, err := manager.reserveReset(context.Background(), "a", nil)
					if err != nil {
						t.Fatal(err)
					}
					defer manager.releaseReset(state)
					switch change {
					case "token":
						auth.Metadata["access_token"] = "latest-fake-token"
					case "disabled":
						auth.Disabled = true
					case "status_disabled":
						auth.Status = StatusDisabled
					case "identity":
						auth.Metadata["email"] = "new-account@example.invalid"
					case "provider":
						auth.Provider = map[string]string{"claude": "codex", "codex": "claude"}[provider]
					case "removed", "readded":
						manager.Remove(context.Background(), "a")
					}
					if change != "removed" {
						if _, err := manager.Register(context.Background(), auth); err != nil {
							t.Fatal(err)
						}
					}
					executor.apply = func(_ context.Context, sent *Auth, _ ResetRequest) (ResetResult, error) {
						if sent.Metadata["access_token"] != "latest-fake-token" {
							t.Fatal("POST used reservation's stale access token")
						}
						return ResetResult{Result: "auth_error"}, nil
					}
					result, _, err := manager.executeReset(context.Background(), reserved, applier, state, inventory, resetChoice{rule: rule})
					if change == "token" {
						if err != nil || executor.calls.Load() != 1 || result.NotSent {
							t.Fatalf("rotation refused: result=%+v err=%v", result, err)
						}
					} else if !errors.Is(err, ErrResetUnavailable) || !result.NotSent || executor.calls.Load() != 0 {
						t.Fatalf("invalid credential sent: result=%+v err=%v calls=%d", result, err, executor.calls.Load())
					}
				})
			}
		}
	}
}

func TestResetManualUsesTokenRotatedDuringUsageRefresh(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				manager, executor, _ := setupResetManager(t, provider)
				inventory := resetTestEntry(provider, 100, 5*time.Hour, 2*time.Hour)
				auth := seedResetAuth(t, manager, "a", provider, inventory)
				release := make(chan struct{})
				executor.fetch = func(context.Context, *Auth) (UsageFetchResult, error) {
					<-release
					return UsageFetchResult{Resets: inventory.Resets, Windows: inventory.Windows}, nil
				}
				executor.apply = func(_ context.Context, sent *Auth, _ ResetRequest) (ResetResult, error) {
					if sent.Metadata["access_token"] != "rotated-fake-token" {
						t.Error("manual POST used token from before usage refresh")
					}
					return ResetResult{Result: "auth_error"}, nil
				}
				done := make(chan error, 1)
				go func() { _, _, err := manager.ApplyCredentialReset(context.Background(), "a", ""); done <- err }()
				synctest.Wait()
				auth.Metadata["access_token"] = "rotated-fake-token"
				if _, err := manager.Update(context.Background(), auth); err != nil {
					t.Fatal(err)
				}
				close(release)
				synctest.Wait()
				if err := <-done; err != nil || executor.calls.Load() != 1 {
					t.Fatalf("manual reset failed: %v", err)
				}
			})
		})
	}
}

func TestResetLateCompletionCannotAffectReplacement(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		for _, change := range []string{"identity", "provider", "readded"} {
			for _, outcome := range []string{"reset", "unknown", "auth_error", "not_sent"} {
				t.Run(provider+"/"+change+"/"+outcome, func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						manager, executor, clock := setupResetManager(t, provider)
						inventory := resetTestEntry(provider, 100, 5*time.Hour, 2*time.Hour)
						auth := seedResetAuth(t, manager, "a", provider, inventory)
						reserved, applier, oldState, err := manager.reserveReset(context.Background(), "a", nil)
						if err != nil {
							t.Fatal(err)
						}
						release := make(chan struct{})
						executor.apply = func(context.Context, *Auth, ResetRequest) (ResetResult, error) {
							<-release
							return ResetResult{Result: outcome, NotSent: outcome == "not_sent"}, nil
						}
						go func() {
							defer manager.releaseReset(oldState)
							_, _, _ = manager.executeReset(context.Background(), reserved, applier, oldState, inventory, resetChoice{rule: "exhausted"})
						}()
						synctest.Wait()
						switch change {
						case "identity":
							auth.Metadata["email"] = "new-account@example.invalid"
						case "provider":
							auth.Provider = map[string]string{"claude": "codex", "codex": "claude"}[provider]
							manager.RegisterExecutor(&fakeResetExecutor{provider: auth.Provider})
						case "readded":
							// Even identical metadata starts a new lifecycle after removal.
							manager.Remove(context.Background(), "a")
						}
						auth.Quota = QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: clock.now().Add(time.Hour)}
						if _, err := manager.Register(context.Background(), auth); err != nil {
							t.Fatal(err)
						}
						_, _, newState, err := manager.reserveReset(context.Background(), "a", nil)
						if err != nil || newState == oldState {
							t.Fatalf("new account inherited old reservation: %v", err)
						}
						defer manager.releaseReset(newState)
						newState.attempted = clock.now().Add(-time.Minute)
						newState.retryAt = clock.now().Add(time.Hour)
						newState.authError = true
						want := *newState
						cached := &usageEntry{CredentialUsage: inventory}
						manager.usage.entries["a"] = cached
						close(release)
						synctest.Wait()
						current, _ := manager.GetByID("a")
						if manager.usage.resets["a"] != newState || *newState != want || manager.usageSnapshot("a") != cached || !current.Quota.Exceeded || executor.fetches.Load() != 0 {
							t.Fatal("old completion changed replacement's reservation, backoff, quota or usage")
						}
					})
				})
			}
		}
	}
}

func TestResetAuthenticationFailureBackoffWithoutSpendLock(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				hook := setupTestLoggerHook(t)
				log.SetLevel(log.InfoLevel)
				manager, executor, clock := setupResetManager(t, provider)
				inventory := resetTestEntry(provider, 100, 5*time.Hour, 2*time.Hour)
				seedResetAuth(t, manager, "a", provider, inventory)
				// A fresh preflight has already unlocked this older spend attempt.
				manager.usage.resets = map[string]*resetAttempt{"a": {attempted: clock.now().Add(-time.Minute)}}
				executor.apply = func(context.Context, *Auth, ResetRequest) (ResetResult, error) {
					return ResetResult{Result: "auth_error"}, nil
				}
				manager.StartResetLoop()
				synctest.Wait()
				state := manager.usage.resets["a"]
				if executor.calls.Load() != 1 || executor.fetches.Load() != 0 || !state.attempted.IsZero() || resetLocked(state, manager.usageSnapshot("a"), clock.now()) || !state.retryAt.Equal(clock.now().Add(15*time.Minute)) {
					t.Fatalf("auth error acquired/retained spend lock or missed backoff: %+v", state)
				}
				logged := false
				for _, entry := range hook.AllEntries() {
					if entry.Message == "subscription reset attempt" && entry.Level == log.InfoLevel && entry.Data["auth_id"] == "a" && entry.Data["provider"] == provider && entry.Data["outcome"] == "auth_error" {
						logged = true
					}
				}
				if !logged {
					t.Fatal("authentication refusal outcome not logged")
				}
				clock.advance(15*time.Minute - time.Nanosecond)
				manager.sweepResets(context.Background())
				synctest.Wait()
				if _, _, err := manager.ApplyCredentialReset(context.Background(), "a", ""); !errors.Is(err, ErrResetUnavailable) || executor.calls.Load() != 1 {
					t.Fatal("manual or automatic reset bypassed authentication backoff")
				}
				clock.advance(time.Nanosecond)
				manager.sweepResets(context.Background())
				synctest.Wait()
				if executor.calls.Load() != 2 || executor.fetches.Load() != 0 {
					t.Fatal("authentication backoff did not expire independently of inventory fetch")
				}
				manager.StopResetLoop()
				synctest.Wait()
			})
		})
	}
}
