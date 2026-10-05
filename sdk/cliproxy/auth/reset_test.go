package auth

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
)

type fakeResetExecutor struct {
	ProviderExecutor
	provider string
	fetch    func(context.Context, *Auth) (UsageFetchResult, error)
	apply    func(context.Context, *Auth, ResetRequest) (ResetResult, error)
	calls    atomic.Int32
	fetches  atomic.Int32
	ids      chan string
}

func (e *fakeResetExecutor) Identifier() string { return e.provider }
func (e *fakeResetExecutor) FetchUsage(ctx context.Context, auth *Auth) (UsageFetchResult, error) {
	e.fetches.Add(1)
	return e.fetch(ctx, auth)
}
func (e *fakeResetExecutor) ApplyReset(ctx context.Context, auth *Auth, req ResetRequest) (ResetResult, error) {
	e.calls.Add(1)
	e.ids <- auth.ID
	if e.apply != nil {
		return e.apply(ctx, auth, req)
	}
	return ResetResult{Result: "reset"}, nil
}

type resetClock struct{ value atomic.Int64 }

func (c *resetClock) now() time.Time          { return time.Unix(0, c.value.Load()).UTC() }
func (c *resetClock) advance(d time.Duration) { c.value.Add(int64(d)) }

func setupResetManager(t *testing.T, provider string) (*Manager, *fakeResetExecutor, *resetClock) {
	t.Helper()
	manager := NewManager(nil, nil, nil)
	// Automatic resets are live only with auto-apply on.
	manager.SetConfig(&config.Config{ResetCredits: config.ResetCreditsConfig{AutoApply: true}})
	clock := &resetClock{}
	clock.value.Store(resetTestNow.UnixNano())
	manager.usage.now = clock.now
	manager.usage.newTicker = func() usageTicker { return &fakeUsageTicker{ticks: make(chan time.Time), stopped: make(chan struct{})} }
	manager.usage.entries = make(map[string]*usageEntry)
	manager.usage.flights = make(map[string]*usageFlight)
	executor := &fakeResetExecutor{provider: provider, ids: make(chan string, 32)}
	executor.fetch = func(context.Context, *Auth) (UsageFetchResult, error) {
		clock.advance(time.Nanosecond)
		return UsageFetchResult{Windows: []UsageWindow{{Kind: "7d", UsedPercent: 0, ResetsAt: resetTestNow.Add(5 * time.Hour)}}}, nil
	}
	manager.RegisterExecutor(executor)
	t.Cleanup(manager.StopResetLoop)
	return manager, executor, clock
}

func seedResetAuth(t *testing.T, manager *Manager, id, provider string, entry CredentialUsage) *Auth {
	t.Helper()
	auth := registerUsageAuth(t, manager, id, provider)
	manager.usage.mu.Lock()
	manager.usage.entries[id] = &usageEntry{CredentialUsage: entry, fetchStartedAt: entry.FetchedAt, windowVersions: make(map[string]uint64)}
	manager.usage.mu.Unlock()
	return auth
}

func TestResetLoopSelection(t *testing.T) {
	for _, tt := range []struct {
		name           string
		secondUsed     float64
		secondRecovery time.Duration
		disabled       bool
		want           string
	}{
		{"headroom prevents claim", 40, 6 * time.Hour, false, ""},
		{"furthest recovery", 100, 8 * time.Hour, false, "b"},
		{"one claim per tick", 100, 5 * time.Hour, false, "a"},
		{"disabled headroom ignored", 0, 6 * time.Hour, true, "a"},
		{"soon recovery never claimed", 100, 30 * time.Minute, false, "a"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				manager, executor, _ := setupResetManager(t, "claude")
				seedResetAuth(t, manager, "a", "claude", resetTestEntry("claude", 100, 5*time.Hour, 0))
				second := seedResetAuth(t, manager, "b", "claude", resetTestEntry("claude", tt.secondUsed, tt.secondRecovery, 0))
				if tt.disabled {
					second.Disabled = true
					if _, err := manager.Update(context.Background(), second); err != nil {
						t.Fatal(err)
					}
				}
				manager.StartResetLoop()
				manager.StartResetLoop()
				synctest.Wait()
				wantCalls := int32(0)
				if tt.want != "" {
					wantCalls = 1
				}
				if executor.calls.Load() != wantCalls {
					t.Fatalf("calls=%d", executor.calls.Load())
				}
				if tt.want != "" {
					if got := <-executor.ids; got != tt.want {
						t.Fatalf("claimed %s, want %s", got, tt.want)
					}
				}
				manager.StopResetLoop()
				synctest.Wait()
			})
		})
	}
}

func TestResetLoopOffAndUnsupportedCredentials(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager, executor, _ := setupResetManager(t, "codex")
		auth := seedResetAuth(t, manager, "a", "codex", resetTestEntry("codex", 100, 5*time.Hour, time.Hour))
		manager.sweepResets(context.Background())
		synctest.Wait()
		if executor.calls.Load() != 0 || executor.fetches.Load() != 0 || manager.ResetLoopRunning() {
			t.Fatal("off loop did work")
		}
		auth.Attributes = map[string]string{"api_key": "fake-key"}
		if _, err := manager.Update(context.Background(), auth); err != nil {
			t.Fatal(err)
		}
		manager.StartResetLoop()
		synctest.Wait()
		if executor.calls.Load() != 0 || executor.fetches.Load() != 0 {
			t.Fatal("API key was used")
		}
		manager.StopResetLoop()
		synctest.Wait()
	})
}

func TestResetLoopFreshFetchLockAndFailedCodexRetry(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				manager, executor, clock := setupResetManager(t, provider)
				inventory := resetTestEntry(provider, 100, 5*time.Hour, 2*time.Hour)
				seedResetAuth(t, manager, "a", provider, inventory)
				release := make(chan struct{})
				var fail atomic.Bool
				fail.Store(true)
				executor.apply = func(context.Context, *Auth, ResetRequest) (ResetResult, error) {
					return ResetResult{Result: "unknown"}, errors.New("ambiguous transport failure")
				}
				executor.fetch = func(context.Context, *Auth) (UsageFetchResult, error) {
					if executor.fetches.Load() == 1 {
						<-release
					}
					clock.advance(time.Nanosecond)
					if fail.Load() {
						return UsageFetchResult{}, errors.New("failed fetch")
					}
					return UsageFetchResult{Resets: inventory.Resets, Windows: inventory.Windows}, nil
				}
				manager.StartResetLoop()
				synctest.Wait()
				if executor.calls.Load() != 1 || executor.fetches.Load() != 1 {
					t.Fatal("missing first attempt/refresh")
				}
				manager.sweepResets(context.Background())
				synctest.Wait()
				if executor.calls.Load() != 1 {
					t.Fatal("attempted while refresh in flight")
				}
				if _, _, err := manager.ApplyCredentialReset(context.Background(), "a", ""); !errors.Is(err, ErrResetInFlight) {
					t.Fatalf("manual bypassed guard: %v", err)
				}
				close(release)
				synctest.Wait()
				manager.sweepResets(context.Background())
				synctest.Wait()
				if executor.calls.Load() != 1 || executor.fetches.Load() != 1 {
					t.Fatal("failed fetch bypassed backoff or lock")
				}
				fail.Store(false)
				clock.advance(15 * time.Minute)
				manager.sweepResets(context.Background())
				synctest.Wait()
				if executor.calls.Load() != 1 || executor.fetches.Load() != 2 {
					t.Fatal("locked credential did not refresh first")
				}
				manager.sweepResets(context.Background())
				synctest.Wait()
				if executor.calls.Load() != 2 {
					t.Fatal("fresh successful fetch did not unlock")
				}
				manager.StopResetLoop()
				synctest.Wait()
			})
		})
	}
}

func TestResetLoopClaudeBackoff(t *testing.T) {
	for _, outcome := range []string{"cooldown", "ineligible", "unavailable", "rate_limited", "auth_error"} {
		t.Run(outcome, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				manager, executor, clock := setupResetManager(t, "claude")
				inventory := resetTestEntry("claude", 100, 5*time.Hour, 0)
				seedResetAuth(t, manager, "a", "claude", inventory)
				executor.apply = func(context.Context, *Auth, ResetRequest) (ResetResult, error) {
					return ResetResult{Result: outcome}, nil
				}
				executor.fetch = func(context.Context, *Auth) (UsageFetchResult, error) {
					clock.advance(time.Nanosecond)
					return UsageFetchResult{Resets: inventory.Resets, Windows: inventory.Windows}, nil
				}
				manager.StartResetLoop()
				synctest.Wait()
				clock.advance(14 * time.Minute)
				manager.sweepResets(context.Background())
				synctest.Wait()
				if executor.calls.Load() != 1 {
					t.Fatalf("backoff bypassed: %d", executor.calls.Load())
				}
				clock.advance(time.Minute)
				manager.sweepResets(context.Background())
				synctest.Wait()
				if executor.calls.Load() != 2 {
					t.Fatal("elapsed backoff still blocked")
				}
				manager.StopResetLoop()
				synctest.Wait()
			})
		})
	}
}

func TestResetLoopClaudeCooldownFromRefreshedStatus(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager, executor, clock := setupResetManager(t, "claude")
		inventory := resetTestEntry("claude", 100, 5*time.Hour, 0)
		seedResetAuth(t, manager, "a", "claude", inventory)
		until := resetTestNow.Add(5 * time.Minute)
		executor.apply = func(context.Context, *Auth, ResetRequest) (ResetResult, error) {
			return ResetResult{Result: "cooldown"}, nil
		}
		executor.fetch = func(context.Context, *Auth) (UsageFetchResult, error) {
			clock.advance(time.Nanosecond)
			resets := cloneCredentialResets(inventory.Resets)
			resets.CooldownUntil = &until
			return UsageFetchResult{Resets: resets, Windows: inventory.Windows}, nil
		}
		manager.StartResetLoop()
		synctest.Wait()
		clock.advance(4 * time.Minute)
		manager.sweepResets(context.Background())
		synctest.Wait()
		if executor.calls.Load() != 1 {
			t.Fatal("cooldown bypassed")
		}
		clock.advance(time.Minute)
		manager.sweepResets(context.Background())
		synctest.Wait()
		if executor.calls.Load() != 2 {
			t.Fatal("explicit cooldown did not replace fallback")
		}
		manager.StopResetLoop()
		synctest.Wait()
	})
}

func TestResetPostAttemptFetchMustStartAfterAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager, executor, clock := setupResetManager(t, "codex")
		inventory := resetTestEntry("codex", 0, 5*time.Hour, 2*time.Hour)
		seedResetAuth(t, manager, "a", "codex", inventory)
		postRelease, oldFetchRelease := make(chan struct{}), make(chan struct{})
		executor.apply = func(context.Context, *Auth, ResetRequest) (ResetResult, error) {
			<-postRelease
			return ResetResult{Result: "unknown"}, nil
		}
		executor.fetch = func(context.Context, *Auth) (UsageFetchResult, error) {
			if executor.fetches.Load() == 2 {
				<-oldFetchRelease
			}
			clock.advance(time.Nanosecond)
			return UsageFetchResult{Resets: inventory.Resets, Windows: inventory.Windows}, nil
		}
		done := make(chan error, 1)
		go func() { _, _, err := manager.ApplyCredentialReset(context.Background(), "a", ""); done <- err }()
		synctest.Wait()
		clock.advance(UsageMinFetchInterval)
		go func() { _, _ = manager.RefreshUsage(context.Background(), "a") }()
		synctest.Wait()
		clock.advance(time.Second)
		close(postRelease)
		synctest.Wait()
		if executor.fetches.Load() != 2 {
			t.Fatal("did not join old fetch")
		}
		clock.advance(UsageMinFetchInterval)
		close(oldFetchRelease)
		synctest.Wait()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if executor.fetches.Load() != 3 {
			t.Fatalf("stale in-flight fetch used to unlock: %d", executor.fetches.Load())
		}
		manager.usage.mu.RLock()
		locked := resetLocked(manager.usage.resets["a"], manager.usage.entries["a"], clock.now())
		manager.usage.mu.RUnlock()
		if locked {
			t.Fatal("post-attempt fetch did not unlock")
		}
	})
}

func TestResetRefreshFailureNearExpiryKeepsMinimumBackoff(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			manager, executor, clock := setupResetManager(t, provider)
			inventory := resetTestEntry(provider, 0, 5*time.Hour, 20*time.Minute)
			seedResetAuth(t, manager, "a", provider, inventory)
			executor.fetch = func(context.Context, *Auth) (UsageFetchResult, error) {
				return UsageFetchResult{LastError: "reset list failed"}, nil
			}
			for range 2 {
				entry, err := manager.RefreshUsage(context.Background(), "a")
				if err != nil {
					t.Fatal(err)
				}
				if entry.Resets != nil {
					t.Fatal("failed reset inventory remained spendable")
				}
				cached := manager.usageSnapshot("a")
				if !cached.retryAt.Equal(clock.now().Add(UsageMinFetchInterval)) {
					t.Fatalf("retry=%v", cached.retryAt)
				}
				clock.advance(UsageMinFetchInterval)
			}
		})
	}
}

func TestResetInventorySnapshotsAreIndependent(t *testing.T) {
	manager, executor, _ := setupResetManager(t, "claude")
	inventory := resetTestEntry("claude", 100, 5*time.Hour, time.Hour)
	next := "grant"
	inventory.Resets.NextGrantID = &next
	inventory.Resets.Grants[0].PercentUsed = map[string]int{"seven_day": 100}
	seedResetAuth(t, manager, "a", "claude", inventory)
	executor.fetch = func(context.Context, *Auth) (UsageFetchResult, error) {
		return UsageFetchResult{Resets: inventory.Resets, Raw: map[string]json.RawMessage{"usage": json.RawMessage(`{}`)}}, nil
	}
	entry, err := manager.RefreshUsage(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	entry.Resets.Grants[0].Clears[0] = "bad"
	entry.Resets.Grants[0].PercentUsed["seven_day"] = 0
	*entry.Resets.NextGrantID = "bad"
	*entry.Resets.Grants[0].EndsAt = time.Time{}
	got := manager.UsageSnapshot("a")
	if got.Resets.Grants[0].Clears[0] != "five_hour" || got.Resets.Grants[0].PercentUsed["seven_day"] != 100 || *got.Resets.NextGrantID != "grant" || got.Resets.Grants[0].EndsAt.IsZero() {
		t.Fatal("public snapshot mutated cache")
	}
}

func TestResetLoopAllExhaustedRequiresEveryClaudeFetch(t *testing.T) {
	for _, scenario := range []string{"never fetched", "first fetch in flight", "empty windows", "failed fetch", "refresh due", "fable only", "all exhausted", "ancillary failure"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				manager, executor, clock := setupResetManager(t, "claude")
				seedResetAuth(t, manager, "a", "claude", resetTestEntry("claude", 100, 5*time.Hour, 0))
				registerUsageAuth(t, manager, "b", "claude")
				inventory := resetTestEntry("claude", 100, 3*time.Hour, 0)
				release := make(chan struct{})
				executor.fetch = func(_ context.Context, auth *Auth) (UsageFetchResult, error) {
					if auth.ID == "b" && (scenario == "never fetched" || scenario == "first fetch in flight") {
						<-release
					}
					clock.advance(time.Nanosecond)
					return UsageFetchResult{Resets: inventory.Resets, Windows: inventory.Windows}, nil
				}
				if scenario == "first fetch in flight" {
					go func() { _, _ = manager.RefreshUsage(context.Background(), "b") }()
					synctest.Wait()
				} else if scenario != "never fetched" {
					switch scenario {
					case "empty windows":
						inventory.Windows = nil
					case "failed fetch":
						inventory.Resets = nil
						inventory.LastError = "usage failed"
					case "refresh due":
						inventory.FetchedAt = clock.now().Add(-time.Hour)
					case "fable only":
						inventory.Windows[0].Scope = "fable"
					case "ancillary failure":
						inventory.LastError = "profile failed"
					}
					manager.usage.entries["b"] = &usageEntry{CredentialUsage: inventory}
				}
				manager.StartResetLoop()
				synctest.Wait()
				want := int32(0)
				if scenario == "all exhausted" || scenario == "ancillary failure" {
					want = 1
				}
				if executor.calls.Load() != want {
					t.Fatalf("claims=%d want=%d", executor.calls.Load(), want)
				}
				close(release)
				manager.StopResetLoop()
				synctest.Wait()
			})
		})
	}
}

func TestResetAncillaryFailureWithFreshInventory(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		for _, automatic := range []bool{false, true} {
			t.Run(provider+map[bool]string{true: "/automatic", false: "/manual"}[automatic], func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					manager, executor, clock := setupResetManager(t, provider)
					inventory := resetTestEntry(provider, 100, 5*time.Hour, 2*time.Hour)
					seedResetAuth(t, manager, "a", provider, inventory)
					executor.fetch = func(context.Context, *Auth) (UsageFetchResult, error) {
						clock.advance(time.Nanosecond)
						return UsageFetchResult{Resets: inventory.Resets, Windows: inventory.Windows, LastError: "ancillary endpoint failed"}, nil
					}
					if _, err := manager.RefreshUsage(context.Background(), "a"); err != nil {
						t.Fatal(err)
					}
					if automatic {
						manager.StartResetLoop()
						synctest.Wait()
					} else if _, _, err := manager.ApplyCredentialReset(context.Background(), "a", ""); err != nil {
						t.Fatal(err)
					}
					if executor.calls.Load() != 1 || manager.UsageSnapshot("a").LastError == "" {
						t.Fatal("ancillary failure blocked reset or lost display error")
					}
					manager.StopResetLoop()
					synctest.Wait()
					clock.advance(UsageFailureBackoff)
					if _, err := manager.RefreshUsage(context.Background(), "a"); err != nil {
						t.Fatal(err)
					}
					if resetLocked(manager.usage.resets["a"], manager.usageSnapshot("a"), clock.now()) {
						t.Fatal("fresh inventory with ancillary error did not release spend lock")
					}
				})
			})
		}
	}
}

func TestResetStaleInventoryBlockedDuringFetchCooldown(t *testing.T) {
	for _, age := range []time.Duration{time.Hour, 5 * time.Minute} {
		for _, automatic := range []bool{false, true} {
			synctest.Test(t, func(t *testing.T) {
				manager, executor, clock := setupResetManager(t, "codex")
				expiry := 2 * time.Hour
				if age == 5*time.Minute {
					expiry = 20 * time.Minute
				}
				inventory := resetTestEntry("codex", 100, 5*time.Hour, expiry)
				inventory.FetchedAt = clock.now().Add(-age)
				inventory.NextFetchAt = clock.now().Add(UsageMinFetchInterval)
				seedResetAuth(t, manager, "a", "codex", inventory)
				if automatic {
					manager.StartResetLoop()
					synctest.Wait()
				} else if _, _, err := manager.ApplyCredentialReset(context.Background(), "a", ""); !errors.Is(err, ErrResetUnavailable) {
					t.Fatalf("stale manual inventory error=%v", err)
				}
				if executor.calls.Load() != 0 || executor.fetches.Load() != 0 {
					t.Fatal("stale inventory spent or fetch cooldown bypassed")
				}
				manager.StopResetLoop()
				synctest.Wait()
			})
		}
	}
}

func TestResetLoopDiscoversNewHourlyInventoryWithoutRoutingSweep(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				manager, executor, clock := setupResetManager(t, provider)
				inventory := resetTestEntry(provider, 100, 5*time.Hour, 2*time.Hour)
				empty := inventory
				empty.Resets = &CredentialResets{}
				seedResetAuth(t, manager, "a", provider, empty)
				executor.fetch = func(context.Context, *Auth) (UsageFetchResult, error) {
					clock.advance(time.Nanosecond)
					return UsageFetchResult{Resets: inventory.Resets, Windows: inventory.Windows}, nil
				}
				manager.StartResetLoop()
				synctest.Wait()
				clock.advance(59 * time.Minute)
				manager.sweepResets(context.Background())
				synctest.Wait()
				if executor.fetches.Load() != 0 || executor.calls.Load() != 0 {
					t.Fatal("empty inventory refreshed before hourly interval")
				}
				clock.advance(time.Minute)
				manager.sweepResets(context.Background())
				synctest.Wait()
				if executor.fetches.Load() != 1 || executor.calls.Load() != 0 {
					t.Fatal("hourly refresh missing or spent in refresh tick")
				}
				manager.sweepResets(context.Background())
				synctest.Wait()
				if executor.calls.Load() != 1 || manager.usage.cancel != nil {
					t.Fatal("new reset not applied independently of routing sweep")
				}
				manager.StopResetLoop()
				synctest.Wait()
			})
		})
	}
}

func TestResetLoopNotSentBackoffWithoutSpendLock(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				manager, executor, clock := setupResetManager(t, provider)
				seedResetAuth(t, manager, "a", provider, resetTestEntry(provider, 100, 5*time.Hour, 2*time.Hour))
				executor.apply = func(context.Context, *Auth, ResetRequest) (ResetResult, error) {
					return ResetResult{Result: "unavailable", NotSent: true}, errors.New("local refusal")
				}
				manager.StartResetLoop()
				synctest.Wait()
				clock.advance(14 * time.Minute)
				manager.sweepResets(context.Background())
				synctest.Wait()
				if executor.calls.Load() != 1 || executor.fetches.Load() != 0 || !manager.usage.resets["a"].attempted.IsZero() {
					t.Fatal("local refusal retried early, refreshed, or acquired spend lock")
				}
				clock.advance(time.Minute)
				manager.sweepResets(context.Background())
				synctest.Wait()
				if executor.calls.Load() != 2 {
					t.Fatal("local refusal backoff did not expire")
				}
				manager.StopResetLoop()
				synctest.Wait()
			})
		})
	}
}

func TestResetLoopClaudeUrgencyBeforeRecovery(t *testing.T) {
	for _, expiry := range []time.Duration{10 * time.Minute, 2 * time.Hour} {
		synctest.Test(t, func(t *testing.T) {
			manager, executor, _ := setupResetManager(t, "claude")
			seedResetAuth(t, manager, "a", "claude", resetTestEntry("claude", 100, 8*time.Hour, 0))
			urgent := resetTestEntry("claude", 100, 3*time.Hour, expiry)
			if expiry == 10*time.Minute {
				urgent.Windows[0].ResetsAt = resetTestNow.Add(30 * time.Minute)
				urgent.Resets.Grants[0].UseRequiresLimit = false
			}
			seedResetAuth(t, manager, "b", "claude", urgent)
			seedResetAuth(t, manager, "c", "claude", resetTestEntry("claude", 100, 6*time.Hour, 4*time.Hour))
			manager.StartResetLoop()
			synctest.Wait()
			if executor.calls.Load() != 1 {
				t.Fatalf("claims=%d", executor.calls.Load())
			}
			if got := <-executor.ids; got != "b" {
				t.Fatalf("urgent grant starved by %s", got)
			}
			manager.StopResetLoop()
			synctest.Wait()
		})
	}
}

func TestResetCodexTargetsChosenCredit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager, executor, clock := setupResetManager(t, "codex")
		inventory := resetTestEntry("codex", 100, 5*time.Hour, 2*time.Hour)
		seedResetAuth(t, manager, "a", "codex", inventory)
		executor.fetch = func(context.Context, *Auth) (UsageFetchResult, error) {
			clock.advance(time.Nanosecond)
			return UsageFetchResult{Resets: inventory.Resets, Windows: inventory.Windows}, nil
		}
		var sent ResetRequest
		executor.apply = func(_ context.Context, _ *Auth, req ResetRequest) (ResetResult, error) {
			sent = req
			return ResetResult{Result: "reset"}, nil
		}
		if _, _, err := manager.ApplyCredentialReset(context.Background(), "a", ""); err != nil {
			t.Fatal(err)
		}
		if sent.CreditID != "credit" {
			t.Fatalf("credit not targeted: %+v", sent)
		}
	})
}

func TestResetLoopCodexLastChanceTriedOncePerCredit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hook := setupTestLoggerHook(t)
		log.SetLevel(log.InfoLevel)
		manager, executor, clock := setupResetManager(t, "codex")
		inventory := resetTestEntry("codex", 9, 5*time.Hour, 15*time.Minute)
		seedResetAuth(t, manager, "a", "codex", inventory)
		var rules []string
		executor.apply = func(_ context.Context, _ *Auth, req ResetRequest) (ResetResult, error) {
			if req.CreditID != "credit" {
				t.Errorf("credit=%q", req.CreditID)
			}
			if executor.calls.Load() == 1 {
				return ResetResult{Result: "not_limited"}, nil
			}
			return ResetResult{Result: "reset"}, nil
		}
		executor.fetch = func(context.Context, *Auth) (UsageFetchResult, error) {
			clock.advance(time.Nanosecond)
			return UsageFetchResult{Resets: inventory.Resets, Windows: inventory.Windows}, nil
		}
		manager.StartResetLoop()
		synctest.Wait()
		if executor.calls.Load() != 1 {
			t.Fatalf("calls=%d", executor.calls.Load())
		}
		for range 3 {
			clock.advance(time.Minute)
			manager.sweepResets(context.Background())
			synctest.Wait()
		}
		if executor.calls.Load() != 1 {
			t.Fatalf("last_chance repeated after not_limited: calls=%d", executor.calls.Load())
		}
		for _, entry := range hook.AllEntries() {
			if entry.Message == "auto reset not needed: credit skipped for last_chance until it expires" {
				rules = append(rules, entry.Data["rule"].(string))
				if entry.Data["credit_id"] != "credit" || entry.Data["outcome"] != "not_limited" || entry.Level != log.InfoLevel {
					t.Fatalf("log=%+v", entry.Data)
				}
			}
		}
		if len(rules) != 1 {
			t.Fatalf("refusal logs=%v", rules)
		}
		// The exhausted rule may still spend the remembered credit.
		manager.usage.mu.Lock()
		exhausted := cloneCredentialUsage(inventory)
		exhausted.Windows[0].UsedPercent = 100
		exhausted.FetchedAt = clock.now()
		manager.usage.entries["a"] = &usageEntry{CredentialUsage: exhausted, fetchStartedAt: clock.now(), windowVersions: make(map[string]uint64)}
		manager.usage.mu.Unlock()
		manager.sweepResets(context.Background())
		synctest.Wait()
		if executor.calls.Load() != 2 {
			t.Fatalf("exhausted rule blocked by last_chance refusal: calls=%d", executor.calls.Load())
		}
		clock.advance(15 * time.Minute)
		manager.sweepResets(context.Background())
		synctest.Wait()
		manager.usage.mu.RLock()
		remembered := len(manager.usage.lastChanceRefused)
		manager.usage.mu.RUnlock()
		if remembered != 0 {
			t.Fatal("expired refusal not pruned")
		}
		manager.StopResetLoop()
		synctest.Wait()
	})
}

func TestResetManualCodexNotLimitedNotRemembered(t *testing.T) {
	manager, executor, _ := setupResetManager(t, "codex")
	inventory := resetTestEntry("codex", 9, 5*time.Hour, 15*time.Minute)
	seedResetAuth(t, manager, "a", "codex", inventory)
	executor.apply = func(context.Context, *Auth, ResetRequest) (ResetResult, error) {
		return ResetResult{Result: "not_limited"}, nil
	}
	executor.fetch = func(context.Context, *Auth) (UsageFetchResult, error) {
		return UsageFetchResult{Resets: inventory.Resets, Windows: inventory.Windows}, nil
	}
	if result, _, err := manager.ApplyCredentialReset(context.Background(), "a", ""); err != nil || result.Result != "not_limited" {
		t.Fatalf("manual reset: %+v %v", result, err)
	}
	if len(manager.usage.lastChanceRefused) != 0 {
		t.Fatal("manual not_limited was remembered")
	}
}

func TestResetLoopCodexLastChanceTriesNextCreditAfterRefusal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager, executor, _ := setupResetManager(t, "codex")
		inventory := resetTestEntry("codex", 9, 5*time.Hour, 10*time.Minute)
		inventory.Resets.Credits = append(inventory.Resets.Credits, ResetCredit{ID: "second", ExpiresAt: resetTestNow.Add(14 * time.Minute)})
		seedResetAuth(t, manager, "a", "codex", inventory)
		manager.usage.mu.Lock()
		manager.usage.lastChanceRefused = map[resetCreditKey]time.Time{{"a", "credit"}: resetTestNow.Add(10 * time.Minute)}
		manager.usage.mu.Unlock()
		executor.apply = func(_ context.Context, _ *Auth, req ResetRequest) (ResetResult, error) {
			if req.CreditID != "second" {
				t.Errorf("credit=%q, want second", req.CreditID)
			}
			return ResetResult{Result: "reset"}, nil
		}
		manager.StartResetLoop()
		synctest.Wait()
		if executor.calls.Load() != 1 {
			t.Fatalf("calls=%d", executor.calls.Load())
		}
		manager.StopResetLoop()
		synctest.Wait()
	})
}
