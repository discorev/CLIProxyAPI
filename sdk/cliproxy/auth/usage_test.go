package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
)

type fakeUsageExecutor struct {
	ProviderExecutor
	provider string
	fetch    func(context.Context, *Auth) (UsageFetchResult, error)
}

func (e *fakeUsageExecutor) Identifier() string { return e.provider }
func (e *fakeUsageExecutor) FetchUsage(ctx context.Context, auth *Auth) (UsageFetchResult, error) {
	return e.fetch(ctx, auth)
}

func registerUsageAuth(t *testing.T, manager *Manager, id, provider string) *Auth {
	t.Helper()
	auth, err := manager.Register(context.Background(), &Auth{
		ID: id, Provider: provider, Metadata: map[string]interface{}{"access_token": "fake-token"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return auth
}

func TestRefreshUsageDeduplicatesAndSnapshotsAreIndependent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := NewManager(nil, nil, nil)
		registerUsageAuth(t, manager, "a", "claude")
		release := make(chan struct{})
		var calls atomic.Int32
		manager.RegisterExecutor(&fakeUsageExecutor{provider: "claude", fetch: func(context.Context, *Auth) (UsageFetchResult, error) {
			calls.Add(1)
			<-release
			return UsageFetchResult{Raw: map[string]json.RawMessage{"usage": json.RawMessage(`{"ok":true}`)}, Windows: []UsageWindow{{Kind: "7d", UsedPercent: 47}}}, nil
		}})
		type response struct {
			usage CredentialUsage
			err   error
		}
		results := make(chan response, 8)
		for range 8 {
			go func() {
				usage, err := manager.RefreshUsage(context.Background(), "a")
				results <- response{usage, err}
			}()
		}
		synctest.Wait()
		if calls.Load() != 1 || !manager.UsageSnapshot("a").Refreshing {
			t.Fatalf("calls = %d, snapshot = %+v", calls.Load(), manager.UsageSnapshot("a"))
		}
		close(release)
		synctest.Wait()
		for range 8 {
			got := <-results
			if got.err != nil || got.usage.Refreshing || len(got.usage.Windows) != 1 || got.usage.FetchedAt.IsZero() {
				t.Fatalf("refresh = %+v, %v", got.usage, got.err)
			}
			got.usage.Raw["usage"][0] = 'x'
			got.usage.Windows[0].UsedPercent = 100
		}
		got := manager.UsageSnapshot("a")
		if string(got.Raw["usage"]) != `{"ok":true}` || got.Windows[0].UsedPercent != 47 {
			t.Fatalf("snapshot was mutated: %+v", got)
		}
	})
}

func TestUsageRefreshPreservesNewerHeadersAndRemoval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := NewManager(nil, nil, nil)
		auth := registerUsageAuth(t, manager, "a", "claude")
		release := make(chan struct{})
		manager.RegisterExecutor(&fakeUsageExecutor{provider: "claude", fetch: func(context.Context, *Auth) (UsageFetchResult, error) {
			<-release
			return UsageFetchResult{Raw: map[string]json.RawMessage{"usage": json.RawMessage(`{}`)}, Windows: []UsageWindow{{Kind: "7d", UsedPercent: 40}}}, nil
		}})
		go func() { _, _ = manager.RefreshUsage(context.Background(), auth.ID) }()
		synctest.Wait()
		ctx := internallogging.WithResponseHeadersHolder(context.Background())
		internallogging.SetResponseHeaders(ctx, http.Header{"Anthropic-Ratelimit-Unified-7d-Utilization": {"0.6"}})
		manager.MarkResult(ctx, Result{AuthID: auth.ID, Provider: "claude", Success: true})
		close(release)
		synctest.Wait()
		if got := manager.UsageSnapshot(auth.ID); len(got.Windows) != 1 || got.Windows[0].UsedPercent != 60 {
			t.Fatalf("newer headers lost: %+v", got)
		}
		manager.Remove(context.Background(), auth.ID)
		if got := manager.UsageSnapshot(auth.ID); len(got.Windows) != 0 || len(got.Raw) != 0 {
			t.Fatalf("removal retained usage: %+v", got)
		}
	})
}

func TestUsageRemovalInvalidatesInFlightFetch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := NewManager(nil, nil, nil)
		registerUsageAuth(t, manager, "a", "claude")
		release := make(chan struct{})
		var calls atomic.Int32
		manager.RegisterExecutor(&fakeUsageExecutor{provider: "claude", fetch: func(context.Context, *Auth) (UsageFetchResult, error) {
			if calls.Add(1) == 1 {
				<-release
				return UsageFetchResult{Windows: []UsageWindow{{Kind: "7d", UsedPercent: 100}}}, nil
			}
			return UsageFetchResult{Windows: []UsageWindow{{Kind: "7d", UsedPercent: 2}}}, nil
		}})
		go func() { _, _ = manager.RefreshUsage(context.Background(), "a") }()
		synctest.Wait()
		manager.Remove(context.Background(), "a")
		registerUsageAuth(t, manager, "a", "claude")
		if _, err := manager.RefreshUsage(context.Background(), "a"); err != nil {
			t.Fatal(err)
		}
		close(release)
		synctest.Wait()
		if got := manager.UsageSnapshot("a"); got.Windows[0].UsedPercent != 2 {
			t.Fatalf("stale flight replaced new credential: %+v", got)
		}
	})
}

func TestRefreshUsageFailureBackoffAndValidation(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	registerUsageAuth(t, manager, "a", "codex")
	now := time.Unix(1800000000, 0)
	manager.usage.now = func() time.Time { return now }
	failure := false
	manager.RegisterExecutor(&fakeUsageExecutor{provider: "codex", fetch: func(context.Context, *Auth) (UsageFetchResult, error) {
		if failure {
			return UsageFetchResult{}, errors.New("upstream status 429")
		}
		return UsageFetchResult{Windows: []UsageWindow{{Kind: "7d", UsedPercent: 40}}, Raw: map[string]json.RawMessage{"usage": json.RawMessage(`{}`)}}, nil
	}})
	if _, err := manager.RefreshUsage(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	failure = true
	now = now.Add(UsageMinFetchInterval)
	got, err := manager.RefreshUsage(context.Background(), "a")
	if err == nil || got.LastError != "upstream status 429" || len(got.Windows) != 1 || len(got.Raw) != 1 || got.Refreshing {
		t.Fatalf("failure discarded usage: %+v, %v", got, err)
	}
	if usageRefreshDue(manager.usageSnapshot("a"), now.Add(14*time.Minute)) || !usageRefreshDue(manager.usageSnapshot("a"), now.Add(15*time.Minute)) {
		t.Fatal("failed fetch did not back off for exactly 15 minutes")
	}
	if _, err = manager.RefreshUsage(context.Background(), "missing"); !errors.Is(err, ErrUsageAuthNotFound) {
		t.Fatalf("unknown auth: %v", err)
	}
	apiKey := registerUsageAuth(t, manager, "key", "codex")
	apiKey.Attributes = map[string]string{"api_key": "fake-key"}
	if _, err = manager.Update(context.Background(), apiKey); err != nil {
		t.Fatal(err)
	}
	if _, err = manager.RefreshUsage(context.Background(), "key"); !errors.Is(err, ErrUsageNotFetchable) {
		t.Fatalf("API key accepted: %v", err)
	}
}

func TestUsageRefreshDue(t *testing.T) {
	now := time.Unix(1800000000, 0)
	for _, test := range []struct {
		name  string
		entry *usageEntry
		want  bool
	}{
		{"new", nil, true},
		{"no data", &usageEntry{}, true},
		{"in flight", &usageEntry{CredentialUsage: CredentialUsage{Refreshing: true}}, false},
		{"backoff", &usageEntry{retryAt: now.Add(time.Minute)}, false},
		{"stale", &usageEntry{CredentialUsage: CredentialUsage{FetchedAt: now.Add(-15 * time.Minute)}}, true},
		{"recent fetch", &usageEntry{CredentialUsage: CredentialUsage{FetchedAt: now.Add(-time.Minute)}}, false},
		{"recent observation does not defer stale fetch", &usageEntry{CredentialUsage: CredentialUsage{FetchedAt: now.Add(-15 * time.Minute), ObservedAt: now}}, true},
		{"recent fetch despite old observation", &usageEntry{CredentialUsage: CredentialUsage{FetchedAt: now.Add(-14 * time.Minute), ObservedAt: now.Add(-time.Hour)}}, false},
		{"header only", &usageEntry{CredentialUsage: CredentialUsage{Windows: []UsageWindow{{Kind: "7d"}}, ObservedAt: now}}, true},
		{"header only in backoff", &usageEntry{CredentialUsage: CredentialUsage{Windows: []UsageWindow{{Kind: "7d"}}, ObservedAt: now}, retryAt: now.Add(time.Minute)}, false},
		{"retry despite fresh headers", &usageEntry{CredentialUsage: CredentialUsage{FetchedAt: now.Add(-time.Minute), ObservedAt: now}, retryAt: now}, true},
		{"reset passed", &usageEntry{CredentialUsage: CredentialUsage{FetchedAt: now.Add(-time.Minute), ObservedAt: now, Windows: []UsageWindow{{ResetsAt: now}}}}, true},
		{"reset already fetched", &usageEntry{CredentialUsage: CredentialUsage{FetchedAt: now, Windows: []UsageWindow{{ResetsAt: now.Add(-time.Minute)}}}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := usageRefreshDue(test.entry, now); got != test.want {
				t.Fatalf("due = %v, want %v", got, test.want)
			}
		})
	}
}

type fakeUsageTicker struct {
	ticks   chan time.Time
	stopped chan struct{}
}

func (t *fakeUsageTicker) Ticks() <-chan time.Time { return t.ticks }
func (t *fakeUsageTicker) Stop()                   { close(t.stopped) }

func TestUsageSweepClockAndLifecycle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := NewManager(nil, nil, nil)
		now := time.Unix(1800000000, 0)
		manager.usage.now = func() time.Time { return now }
		ticker := &fakeUsageTicker{ticks: make(chan time.Time), stopped: make(chan struct{})}
		manager.usage.newTicker = func() usageTicker { return ticker }
		registerUsageAuth(t, manager, "active", "claude")
		disabled := registerUsageAuth(t, manager, "disabled", "claude")
		disabled.Disabled = true
		if _, err := manager.Update(context.Background(), disabled); err != nil {
			t.Fatal(err)
		}
		var calls atomic.Int32
		manager.RegisterExecutor(&fakeUsageExecutor{provider: "claude", fetch: func(_ context.Context, auth *Auth) (UsageFetchResult, error) {
			if auth.ID != "active" {
				t.Errorf("fetched disabled auth %s", auth.ID)
			}
			calls.Add(1)
			return UsageFetchResult{Windows: []UsageWindow{{Kind: "7d", ResetsAt: now.Add(time.Hour)}}}, nil
		}})
		manager.StartUsageSweep()
		manager.StartUsageSweep()
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatalf("initial calls = %d", calls.Load())
		}
		now = now.Add(time.Minute)
		ticker.ticks <- now
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatal("fresh data fetched again")
		}
		now = now.Add(14 * time.Minute)
		ticker.ticks <- now
		synctest.Wait()
		if calls.Load() != 2 {
			t.Fatal("stale data not fetched")
		}
		manager.StopUsageSweep()
		synctest.Wait()
		<-ticker.stopped
		if manager.UsageSweepRunning() {
			t.Fatal("sweep still running")
		}
	})
}

func TestUsageSweepRefetchesActiveCredentialDespiteHeaders(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := NewManager(nil, nil, nil)
		now := time.Unix(1800000000, 0)
		manager.usage.now = func() time.Time { return now }
		ticker := &fakeUsageTicker{ticks: make(chan time.Time), stopped: make(chan struct{})}
		manager.usage.newTicker = func() usageTicker { return ticker }
		auth := registerUsageAuth(t, manager, "active", "claude")
		var calls atomic.Int32
		manager.RegisterExecutor(&fakeUsageExecutor{provider: "claude", fetch: func(context.Context, *Auth) (UsageFetchResult, error) {
			calls.Add(1)
			return UsageFetchResult{Raw: map[string]json.RawMessage{"usage": json.RawMessage(`{}`)}}, nil
		}})
		manager.StartUsageSweep()
		defer manager.StopUsageSweep()
		synctest.Wait()
		// Proxied traffic observes headers every minute; the raw body must
		// still be re-fetched once the last successful fetch is 15 minutes old.
		for minute := 1; minute <= 15; minute++ {
			now = now.Add(time.Minute)
			manager.mu.Lock()
			manager.observeUsageHeadersLocked(auth, http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.1"}}, now)
			manager.mu.Unlock()
			ticker.ticks <- now
			synctest.Wait()
			want := int32(1)
			if minute == 15 {
				want = 2
			}
			if got := calls.Load(); got != want {
				t.Fatalf("minute %d: fetches = %d, want %d", minute, got, want)
			}
		}
	})
}

func TestUsageRefreshRespectsFloorAtSameClockTick(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	registerUsageAuth(t, manager, "a", "claude")
	now := time.Unix(1800000000, 0)
	manager.usage.now = func() time.Time { return now }
	used := float64(40)
	manager.RegisterExecutor(&fakeUsageExecutor{provider: "claude", fetch: func(context.Context, *Auth) (UsageFetchResult, error) {
		return UsageFetchResult{Windows: []UsageWindow{{Kind: "7d", UsedPercent: used}}}, nil
	}})
	if _, err := manager.RefreshUsage(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	used = 20
	if _, err := manager.RefreshUsage(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if got := manager.UsageSnapshot("a"); got.Windows[0].UsedPercent != 40 {
		t.Fatalf("same-tick refresh bypassed floor: %+v", got)
	}
	now = now.Add(UsageMinFetchInterval)
	if got, err := manager.RefreshUsage(context.Background(), "a"); err != nil || got.Windows[0].UsedPercent != 20 {
		t.Fatalf("elapsed floor did not fetch: %+v, %v", got, err)
	}
}

func TestUsageCanceledFetchRemainsDueAfterFloor(t *testing.T) {
	for _, previousError := range []string{"", "previous upstream failure"} {
		t.Run(previousError, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			registerUsageAuth(t, manager, "a", "claude")
			manager.RegisterExecutor(&fakeUsageExecutor{provider: "claude", fetch: func(ctx context.Context, _ *Auth) (UsageFetchResult, error) {
				return UsageFetchResult{}, ctx.Err()
			}})
			manager.usage.entries = map[string]*usageEntry{"a": {CredentialUsage: CredentialUsage{LastError: previousError}}}
			manager.usage.flights = make(map[string]*usageFlight)
			flight, auth, fetcher, leader, err := manager.beginUsageRefresh("a", "sweep")
			if err != nil || !leader {
				t.Fatalf("begin refresh = %v, %v", leader, err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			manager.fetchUsage(ctx, auth, fetcher, flight)
			<-flight.done
			entry := manager.usageSnapshot("a")
			if !errors.Is(flight.err, context.Canceled) || entry.LastError != previousError || entry.Refreshing || !entry.retryAt.IsZero() || usageRefreshDue(entry, time.Now()) || !usageRefreshDue(entry, entry.NextFetchAt) {
				t.Fatalf("cancellation became a failure: entry = %+v, err = %v", entry, flight.err)
			}
			if manager.usage.flights["a"] != nil {
				t.Fatal("canceled flight retained")
			}
		})
	}
}

func TestUsageSharedFetchSurvivesCallerAndSweepCancellation(t *testing.T) {
	for _, startWithSweep := range []bool{false, true} {
		name := "caller"
		if startWithSweep {
			name = "sweep"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				manager := NewManager(nil, nil, nil)
				registerUsageAuth(t, manager, "a", "claude")
				release := make(chan struct{})
				var calls atomic.Int32
				manager.RegisterExecutor(&fakeUsageExecutor{provider: "claude", fetch: func(ctx context.Context, _ *Auth) (UsageFetchResult, error) {
					calls.Add(1)
					select {
					case <-ctx.Done():
						return UsageFetchResult{}, ctx.Err()
					case <-release:
						return UsageFetchResult{Windows: []UsageWindow{{Kind: "7d", UsedPercent: 25}}}, nil
					}
				}})
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				canceledWaiter := make(chan error, 1)
				var ticker *fakeUsageTicker
				if startWithSweep {
					ticker = &fakeUsageTicker{ticks: make(chan time.Time), stopped: make(chan struct{})}
					manager.usage.newTicker = func() usageTicker { return ticker }
					manager.StartUsageSweep()
					synctest.Wait()
				}
				go func() {
					_, err := manager.RefreshUsage(ctx, "a")
					canceledWaiter <- err
				}()
				synctest.Wait()
				otherWaiter := make(chan error, 1)
				go func() {
					_, err := manager.RefreshUsage(context.Background(), "a")
					otherWaiter <- err
				}()
				synctest.Wait()
				cancel()
				if startWithSweep {
					manager.StopUsageSweep()
				}
				synctest.Wait()
				if err := <-canceledWaiter; !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled waiter returned %v", err)
				}
				if startWithSweep {
					<-ticker.stopped
				}
				if got := manager.UsageSnapshot("a"); !got.Refreshing || got.LastError != "" || calls.Load() != 1 {
					t.Fatalf("shared flight canceled: %+v, calls = %d", got, calls.Load())
				}
				select {
				case err := <-otherWaiter:
					t.Fatalf("other waiter returned before fetch completed: %v", err)
				default:
				}
				close(release)
				synctest.Wait()
				if err := <-otherWaiter; err != nil {
					t.Fatal(err)
				}
				if got := manager.UsageSnapshot("a"); got.Refreshing || len(got.Windows) != 1 || got.Windows[0].UsedPercent != 25 || got.FetchedAt.IsZero() {
					t.Fatalf("shared fetch lost: %+v", got)
				}
			})
		})
	}
}

func TestUsageFetchDoesNotChangeObservedAt(t *testing.T) {
	for _, observedAt := range []time.Time{{}, time.Unix(1800000000, 0)} {
		t.Run(observedAt.String(), func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			registerUsageAuth(t, manager, "a", "claude")
			manager.usage.now = func() time.Time { return time.Unix(1800000300, 0) }
			manager.usage.entries = map[string]*usageEntry{"a": {CredentialUsage: CredentialUsage{ObservedAt: observedAt}}}
			manager.usage.flights = make(map[string]*usageFlight)
			manager.RegisterExecutor(&fakeUsageExecutor{provider: "claude", fetch: func(context.Context, *Auth) (UsageFetchResult, error) {
				return UsageFetchResult{}, nil
			}})
			got, err := manager.RefreshUsage(context.Background(), "a")
			if err != nil || !got.ObservedAt.Equal(observedAt) || got.FetchedAt.IsZero() {
				t.Fatalf("fetch changed header observation timestamp: %+v, %v", got, err)
			}
		})
	}
}

func TestUsageAccountReplacementInvalidatesCacheAndFlight(t *testing.T) {
	for _, operation := range []string{"register", "update", "load"} {
		for _, identity := range []struct{ provider, key string }{
			{"claude", "email"}, {"claude", "account_uuid"}, {"codex", "account_id"}, {"codex", "email"},
		} {
			t.Run(operation+"/"+identity.provider+"/"+identity.key, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					store := &schedulerLoadStore{}
					manager := NewManager(store, nil, nil)
					now := time.Now()
					manager.usage.now = func() time.Time { return now }
					auth, err := manager.Register(context.Background(), &Auth{
						ID: "a", Provider: identity.provider,
						Metadata: map[string]interface{}{"access_token": "fake-token", identity.key: "old-account"},
					})
					if err != nil {
						t.Fatal(err)
					}
					replace := func(next *Auth) {
						t.Helper()
						var err error
						switch operation {
						case "register":
							_, err = manager.Register(context.Background(), next)
						case "update":
							_, err = manager.Update(context.Background(), next)
						case "load":
							store.auths = []*Auth{next}
							err = manager.Load(context.Background())
						}
						if err != nil {
							t.Fatal(err)
						}
					}
					release := make(chan struct{})
					var calls atomic.Int32
					manager.RegisterExecutor(&fakeUsageExecutor{provider: identity.provider, fetch: func(_ context.Context, credential *Auth) (UsageFetchResult, error) {
						if calls.Add(1) == 2 {
							<-release
						}
						used := float64(90)
						if credential.Metadata[identity.key] == "new-account" {
							used = 2
						}
						return UsageFetchResult{Windows: []UsageWindow{{Kind: "7d", UsedPercent: used}}}, nil
					}})
					if _, err := manager.RefreshUsage(context.Background(), "a"); err != nil {
						t.Fatal(err)
					}
					now = now.Add(UsageMinFetchInterval)
					go func() { _, _ = manager.RefreshUsage(context.Background(), "a") }()
					synctest.Wait()
					oldFlight := manager.usage.flights["a"]
					auth.Metadata["access_token"] = "rotated-fake-token"
					replace(auth)
					if got := manager.UsageSnapshot("a"); len(got.Windows) != 1 || got.Windows[0].UsedPercent != 90 || !got.Refreshing || manager.usage.flights["a"] != oldFlight {
						t.Fatalf("token rotation discarded usage: %+v", got)
					}
					auth.Metadata[identity.key] = "new-account"
					replace(auth)
					if manager.usageSnapshot("a") != nil || manager.usage.flights["a"] != nil {
						t.Fatal("new account inherited old account cache/flight")
					}
					if _, err := manager.RefreshUsage(context.Background(), "a"); err != nil {
						t.Fatal(err)
					}
					close(release)
					synctest.Wait()
					if got := manager.UsageSnapshot("a"); len(got.Windows) != 1 || got.Windows[0].UsedPercent != 2 {
						t.Fatalf("old flight replaced new account usage: %+v", got)
					}
				})
			})
		}
	}
}

func TestUsagePartialFailureRetainsRawAndRetriesDespiteHeaders(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := NewManager(nil, nil, nil)
		auth := registerUsageAuth(t, manager, "a", "claude")
		now := time.Unix(1800000000, 0)
		manager.usage.now = func() time.Time { return now }
		var calls atomic.Int32
		manager.RegisterExecutor(&fakeUsageExecutor{provider: "claude", fetch: func(context.Context, *Auth) (UsageFetchResult, error) {
			switch calls.Add(1) {
			case 1:
				return UsageFetchResult{Raw: map[string]json.RawMessage{
					"usage": json.RawMessage(`{"version":1}`), "profile": json.RawMessage(`{"version":1}`),
					"subscription": json.RawMessage(`{"version":1}`), "reset_credits": json.RawMessage(`{"version":1}`),
				}}, nil
			case 2:
				return UsageFetchResult{Raw: map[string]json.RawMessage{"usage": json.RawMessage(`{"version":2}`)}, LastError: "profile, subscription, reset_credits unavailable"}, nil
			default:
				return UsageFetchResult{Raw: map[string]json.RawMessage{
					"usage": json.RawMessage(`{"version":3}`), "profile": json.RawMessage(`{"version":3}`),
					"subscription": json.RawMessage(`{"version":3}`), "reset_credits": json.RawMessage(`{"version":3}`),
				}}, nil
			}
		}})
		if _, err := manager.RefreshUsage(context.Background(), "a"); err != nil {
			t.Fatal(err)
		}
		previous := manager.usageSnapshot("a")
		now = now.Add(UsageMinFetchInterval)
		partial, err := manager.RefreshUsage(context.Background(), "a")
		if err != nil || partial.LastError == "" || !partial.FetchedAt.Equal(now) || string(partial.Raw["usage"]) != `{"version":2}` || string(previous.Raw["usage"]) != `{"version":1}` {
			t.Fatalf("partial fetch not merged independently: %+v, %v", partial, err)
		}
		for _, endpoint := range []string{"profile", "subscription", "reset_credits"} {
			if string(partial.Raw[endpoint]) != `{"version":1}` {
				t.Fatalf("partial fetch discarded %s: %s", endpoint, partial.Raw[endpoint])
			}
		}
		if got := manager.usageSnapshot("a").retryAt; !got.Equal(now.Add(15 * time.Minute)) {
			t.Fatalf("retryAt = %v", got)
		}
		for minute := 1; minute <= 15; minute++ {
			now = now.Add(time.Minute)
			manager.mu.Lock()
			manager.observeUsageHeadersLocked(auth, http.Header{"Anthropic-Ratelimit-Unified-7d-Utilization": {"0.6"}}, now)
			manager.mu.Unlock()
			manager.sweepUsage(context.Background())
			synctest.Wait()
			wantCalls := int32(2)
			if minute == 15 {
				wantCalls = 3
			}
			if calls.Load() != wantCalls {
				t.Fatalf("minute %d: calls = %d, want %d", minute, calls.Load(), wantCalls)
			}
		}
		got := manager.UsageSnapshot("a")
		if got.LastError != "" || !manager.usageSnapshot("a").retryAt.IsZero() || !got.ObservedAt.Equal(now) {
			t.Fatalf("retry did not recover: %+v", got)
		}
		for _, endpoint := range []string{"usage", "profile", "subscription", "reset_credits"} {
			if string(got.Raw[endpoint]) != `{"version":3}` {
				t.Fatalf("retry did not update %s: %s", endpoint, got.Raw[endpoint])
			}
		}
	})
}
