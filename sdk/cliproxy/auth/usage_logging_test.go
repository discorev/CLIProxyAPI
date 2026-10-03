package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func usageLogs(hook *logtest.Hook) []*log.Entry {
	var entries []*log.Entry
	for _, entry := range hook.AllEntries() {
		if strings.HasPrefix(entry.Message, "usage ") {
			entries = append(entries, entry)
		}
	}
	return entries
}

func assertUsageLog(t *testing.T, entry *log.Entry, message string, fields log.Fields) {
	t.Helper()
	if entry.Level != log.InfoLevel || entry.Message != message || !reflect.DeepEqual(entry.Data, fields) {
		t.Fatalf("log = %s %q %#v, want info %q %#v", entry.Level, entry.Message, entry.Data, message, fields)
	}
}

func TestUsageFetchLogsTriggersAndWindows(t *testing.T) {
	now := time.Date(2026, 10, 3, 9, 23, 0, 0, time.UTC)
	for _, tt := range []struct {
		name, caller, want string
		entry              *usageEntry
	}{
		{"initial", "sweep", "initial", nil},
		{"header only", "sweep", "initial", &usageEntry{CredentialUsage: CredentialUsage{ObservedAt: now}}},
		{"retry first failure", "sweep", "retry", &usageEntry{retryAt: now}},
		{"retry token", "sweep", "retry", &usageEntry{waitForToken: true}},
		{"reset passed", "sweep", "reset_passed", &usageEntry{CredentialUsage: CredentialUsage{FetchedAt: now.Add(-time.Minute), Windows: []UsageWindow{{ResetsAt: now}}}}},
		{"idle", "sweep", "idle", &usageEntry{CredentialUsage: CredentialUsage{FetchedAt: now.Add(-usageIdleRefreshInterval), ObservedAt: now}}},
		{"manual", "manual", "manual", nil},
		{"reset loop", "reset_loop", "reset_loop", nil},
		{"post reset", "reset", "reset", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			hook := setupTestLoggerHook(t)
			log.SetLevel(log.InfoLevel)
			manager := NewManager(nil, nil, nil)
			registerUsageAuth(t, manager, "a", "codex")
			manager.usage.now = func() time.Time { return now }
			manager.usage.entries = map[string]*usageEntry{"a": tt.entry}
			manager.usage.flights = make(map[string]*usageFlight)
			manager.RegisterExecutor(&fakeUsageExecutor{provider: "codex", fetch: func(context.Context, *Auth) (UsageFetchResult, error) {
				return UsageFetchResult{
					Raw: map[string]json.RawMessage{"usage": json.RawMessage(`{"secret":"raw-body-sentinel"}`)},
					Windows: []UsageWindow{
						{Kind: "7d", UsedPercent: 1, ResetsAt: time.Date(2026, 10, 10, 2, 8, 0, 0, time.FixedZone("offset", 7200))},
						{Kind: "5h", UsedPercent: 12.5},
						{Kind: "7d", Scope: "fable", UsedPercent: 2},
					},
				}, nil
			}})
			flight, auth, fetcher, leader, err := manager.beginUsageRefresh("a", tt.caller)
			if err != nil || !leader {
				t.Fatalf("begin refresh = %v, %v", leader, err)
			}
			manager.fetchUsage(context.Background(), auth, fetcher, flight)
			entries := usageLogs(hook)
			if len(entries) != 1 {
				t.Fatalf("logs = %#v", entries)
			}
			assertUsageLog(t, entries[0], "usage fetched", log.Fields{
				"auth_id": "a", "provider": "codex", "trigger": tt.want, "windows": 3,
				"summary": "7d=1%@2026-10-10T00:08Z, 5h=12.5%@unknown, 7d:fable=2%@unknown",
			})
		})
	}
}

func TestUsageFetchFailureLogs(t *testing.T) {
	for _, tt := range []struct {
		name   string
		result UsageFetchResult
		err    error
		want   []string
	}{
		{"failure", UsageFetchResult{}, errors.New("codex usage: request failed"), []string{"usage fetch failed"}},
		{"partial", UsageFetchResult{LastError: "codex subscription: upstream status 503"}, nil, []string{"usage fetched", "usage fetch failed"}},
		{"429", UsageFetchResult{}, &UsageHTTPError{StatusCode: 429}, []string{"usage fetch rate-limited"}},
		{"ancillary 429", UsageFetchResult{LastError: "codex subscription: upstream status 429", RateLimit: &UsageHTTPError{StatusCode: 429}}, nil, []string{"usage fetch rate-limited"}},
		{"canceled", UsageFetchResult{}, context.Canceled, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			hook := setupTestLoggerHook(t)
			log.SetLevel(log.InfoLevel)
			manager := NewManager(nil, nil, nil)
			registerUsageAuth(t, manager, "a", "codex")
			now := time.Date(2026, 10, 3, 9, 23, 0, 0, time.UTC)
			manager.usage.now = func() time.Time { return now }
			manager.RegisterExecutor(&fakeUsageExecutor{provider: "codex", fetch: func(context.Context, *Auth) (UsageFetchResult, error) {
				return tt.result, tt.err
			}})
			_, _ = manager.RefreshUsage(context.Background(), "a")
			entries := usageLogs(hook)
			if len(entries) != len(tt.want) {
				t.Fatalf("got %d logs, want %v: %#v", len(entries), tt.want, entries)
			}
			for i, message := range tt.want {
				fields := log.Fields{"auth_id": "a", "provider": "codex", "trigger": "manual"}
				switch message {
				case "usage fetched":
					fields["windows"], fields["summary"] = 0, ""
				case "usage fetch failed":
					fields["error"] = tt.result.LastError
					if tt.err != nil {
						fields["error"] = tt.err.Error()
					}
					fields["retry_at"] = now.Add(UsageFailureBackoff).Format(time.RFC3339)
				case "usage fetch rate-limited":
					delete(fields, "trigger")
					fields["cooldown_until"] = now.Add(UsageCodex429Initial).Format(time.RFC3339)
				}
				assertUsageLog(t, entries[i], message, fields)
			}
			// The shared floor suppresses another call and all logs, not just HTTP.
			_, _ = manager.RefreshUsage(context.Background(), "a")
			if len(usageLogs(hook)) != len(tt.want) {
				t.Fatal("blocked refresh logged again")
			}
		})
	}
}

func TestUsageCacheClearedLogsIdentityAndRemoval(t *testing.T) {
	for _, operation := range []string{"register", "update", "refresh", "load"} {
		for _, tt := range []struct{ provider, key, before, after, change string }{
			{"codex", "account_id", "", "new-secret", "<empty> -> <set>"},
			{"codex", "account_id", "old-secret", "", "<set> -> <empty>"},
			{"codex", "account_id", "old-secret", "new-secret", "<set> -> <changed>"},
			{"codex", "email", "old-secret", "new-secret", "<set> -> <changed>"},
			{"claude", "email", "old-secret", "new-secret", "<set> -> <changed>"},
			{"claude", "account_uuid", "old-secret", "new-secret", "<set> -> <changed>"},
			{"claude", "organization_uuid", "old-secret", "new-secret", "<set> -> <changed>"},
			{"codex", "provider", "codex", "claude", ""},
			{"codex", "removed", "", "", ""},
		} {
			t.Run(operation+"/"+tt.provider+"/"+tt.key+"/"+tt.change, func(t *testing.T) {
				hook := setupTestLoggerHook(t)
				log.SetLevel(log.InfoLevel)
				store := &schedulerLoadStore{}
				manager := NewManager(store, nil, nil)
				auth := registerUsageAuth(t, manager, "a", tt.provider)
				auth.Metadata[tt.key] = tt.before
				if _, err := manager.Update(context.Background(), auth); err != nil {
					t.Fatal(err)
				}
				manager.usage.entries = map[string]*usageEntry{"a": {}}
				resetState := &resetAttempt{inFlight: true, attempted: time.Now(), retryAt: time.Now().Add(time.Hour), authError: true}
				manager.usage.resets = map[string]*resetAttempt{"a": resetState}
				decision := resetDecisionKey{authID: "a", rule: "last_chance"}
				otherDecision := resetDecisionKey{authID: "other", rule: "last_chance"}
				manager.usage.resetDecisions = map[resetDecisionKey]time.Time{decision: time.Now(), otherDecision: time.Now()}
				replace := func(next *Auth) {
					t.Helper()
					var err error
					switch operation {
					case "register":
						_, err = manager.Register(context.Background(), next)
					case "update":
						_, err = manager.Update(context.Background(), next)
					case "refresh":
						base, _ := manager.GetByID("a")
						_, err = manager.UpdateRefreshedAuth(context.Background(), base, next)
					case "load":
						store.auths = []*Auth{next}
						err = manager.Load(context.Background())
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				// Rotating a token is not an identity change or a clear event.
				auth.Metadata["access_token"] = "rotated-secret-token"
				replace(auth.Clone())
				if len(usageLogs(hook)) != 0 || manager.usageSnapshot("a") == nil || manager.usage.resets["a"] != resetState || len(manager.usage.resetDecisions) != 2 {
					t.Fatal("token-only rotation cleared/logged usage or reset state")
				}
				fields := log.Fields{"auth_id": "a", "provider": tt.provider, "reason": "identity_changed", "identity_changes": tt.key + ": " + tt.change}
				switch tt.key {
				case "removed":
					fields["reason"] = "removed"
					delete(fields, "identity_changes")
					if operation == "load" {
						store.auths = nil
						if err := manager.Load(context.Background()); err != nil {
							t.Fatal(err)
						}
					} else {
						manager.Remove(context.Background(), "a")
					}
				case "provider":
					fields["reason"] = "provider_changed"
					delete(fields, "identity_changes")
					auth.Provider = tt.after
					replace(auth)
				default:
					auth.Metadata[tt.key] = tt.after
					replace(auth)
				}
				entries := usageLogs(hook)
				if operation == "refresh" && tt.key == "provider" {
					// Refresh merging deliberately preserves the current provider.
					if len(entries) != 0 || manager.usageSnapshot("a") == nil {
						t.Fatal("ignored provider change cleared/logged cache")
					}
					return
				}
				if len(entries) != 1 || manager.usageSnapshot("a") != nil {
					t.Fatalf("clear logs = %#v, cache = %+v", entries, manager.usageSnapshot("a"))
				}
				assertUsageLog(t, entries[0], "usage cache cleared", fields)
				if manager.usage.resets["a"] != nil || len(manager.usage.resetDecisions) != 1 || manager.usage.resetDecisions[otherDecision].IsZero() {
					t.Fatal("account change retained reset state or cleared another account's dedupe")
				}
				if strings.Contains(fmt.Sprint(entries[0].Data), "secret") {
					t.Fatal("identity/token values leaked")
				}
				manager.Remove(context.Background(), "a")
				if len(usageLogs(hook)) != 1 {
					t.Fatal("empty cache removal logged")
				}
			})
		}
	}
}

func TestUsageSweepLogsOncePerLifecycle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hook := setupTestLoggerHook(t)
		log.SetLevel(log.InfoLevel)
		manager := NewManager(nil, nil, nil)
		for cycle := range 2 {
			ticker := &fakeUsageTicker{ticks: make(chan time.Time), stopped: make(chan struct{})}
			manager.usage.newTicker = func() usageTicker { return ticker }
			manager.StartUsageSweep()
			manager.StartUsageSweep()
			synctest.Wait()
			manager.StopUsageSweep()
			manager.StopUsageSweep()
			synctest.Wait()
			<-ticker.stopped
			entries := usageLogs(hook)
			if len(entries) != (cycle+1)*2 {
				t.Fatalf("logs = %#v", entries)
			}
			assertUsageLog(t, entries[cycle*2], "usage sweep started", log.Fields{})
			assertUsageLog(t, entries[cycle*2+1], "usage sweep stopped", log.Fields{"reason": "stop_requested"})
		}
	})
}

func TestUsageInvalidatedFlightDoesNotLogFetched(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hook := setupTestLoggerHook(t)
		log.SetLevel(log.InfoLevel)
		manager := NewManager(nil, nil, nil)
		registerUsageAuth(t, manager, "a", "codex")
		release := make(chan struct{})
		manager.RegisterExecutor(&fakeUsageExecutor{provider: "codex", fetch: func(context.Context, *Auth) (UsageFetchResult, error) {
			<-release
			return UsageFetchResult{}, nil
		}})
		go func() { _, _ = manager.RefreshUsage(context.Background(), "a") }()
		synctest.Wait()
		manager.Remove(context.Background(), "a")
		close(release)
		synctest.Wait()
		entries := usageLogs(hook)
		if len(entries) != 1 {
			t.Fatalf("invalidated flight logged completion: %#v", entries)
		}
		assertUsageLog(t, entries[0], "usage cache cleared", log.Fields{"auth_id": "a", "provider": "codex", "reason": "removed"})
	})
}

func TestUsageParseFailureLogsSafeInfoAndVerbatimDebug(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		for _, level := range []log.Level{log.InfoLevel, log.DebugLevel} {
			t.Run(provider+"/"+level.String(), func(t *testing.T) {
				hook := setupTestLoggerHook(t)
				log.SetLevel(level)
				manager := NewManager(nil, nil, nil)
				registerUsageAuth(t, manager, "a", provider)
				now := time.Date(2026, 10, 3, 9, 23, 0, 0, time.UTC)
				manager.usage.now = func() time.Time { return now }
				parseErr := fmt.Errorf("parse %s usage: %w", provider, &time.ParseError{Layout: time.RFC3339, Value: "private-response-value", LayoutElem: "2006", ValueElem: "private-response-value"})
				manager.RegisterExecutor(&fakeUsageExecutor{provider: provider, fetch: func(context.Context, *Auth) (UsageFetchResult, error) {
					return UsageFetchResult{}, parseErr
				}})
				snapshot, err := manager.RefreshUsage(context.Background(), "a")
				if err != parseErr || snapshot.LastError != parseErr.Error() {
					t.Fatalf("verbatim diagnostic changed: err=%v last_error=%q", err, snapshot.LastError)
				}
				entries := usageLogs(hook)
				want := 1
				if level == log.DebugLevel {
					want = 2
					if len(entries) != want || entries[0].Level != log.DebugLevel || entries[0].Message != "usage fetch parse failed" || entries[0].Data["error"] != parseErr.Error() {
						t.Fatalf("missing verbatim debug diagnostic: %#v", entries)
					}
				}
				if len(entries) != want {
					t.Fatalf("got %d logs, want %d", len(entries), want)
				}
				assertUsageLog(t, entries[want-1], "usage fetch failed", log.Fields{
					"auth_id": "a", "provider": provider, "trigger": "manual",
					"error": "invalid " + provider + " usage response", "retry_at": now.Add(UsageFailureBackoff).Format(time.RFC3339),
				})
			})
		}
	}
}

func TestUsageAccountChangeReportsAllFieldsWithoutValues(t *testing.T) {
	previous := &Auth{Provider: "codex", Metadata: map[string]interface{}{"account_id": "old-secret", "email": "old-email"}}
	current := &Auth{Provider: "codex", Metadata: map[string]interface{}{"account_id": "new-secret", "email": ""}}
	reason, changes := usageAccountChange(previous, current)
	if reason != "identity_changed" || changes != "account_id: <set> -> <changed>, email: <set> -> <empty>" {
		t.Fatalf("reason=%q changes=%q", reason, changes)
	}
	current = previous.Clone()
	current.Provider = " CODEX "
	current.Metadata["account_id"] = " old-secret "
	if reason, changes = usageAccountChange(previous, current); reason != "" || changes != "" {
		t.Fatalf("normalization changed identity: reason=%q changes=%q", reason, changes)
	}
}

func TestUsageStaleFlightSupersededWithoutDeadline(t *testing.T) {
	for _, trigger := range []string{"manual", "sweep", "reset_loop"} {
		t.Run(trigger, func(t *testing.T) {
			hook := setupTestLoggerHook(t)
			log.SetLevel(log.InfoLevel)
			manager, executor, clock := setupResetManager(t, "codex")
			registerUsageAuth(t, manager, "a", "codex")
			executor.fetch = func(ctx context.Context, _ *Auth) (UsageFetchResult, error) {
				if _, deadline := ctx.Deadline(); deadline || ctx.Err() != nil {
					t.Fatal("stale-flight recovery added a network deadline or cancellation")
				}
				if executor.fetches.Load() == 2 {
					return UsageFetchResult{}, &UsageHTTPError{StatusCode: 429}
				}
				return UsageFetchResult{Windows: []UsageWindow{{Kind: "7d", UsedPercent: 12}}}, nil
			}
			old, auth, fetcher, leader, err := manager.beginUsageRefresh("a", trigger)
			if err != nil || !leader {
				t.Fatalf("initial flight: leader=%v err=%v", leader, err)
			}
			clock.advance(UsageFlightMaxAge - time.Nanosecond)
			joined, _, _, leader, err := manager.beginUsageRefresh("a", trigger)
			if err != nil || leader || joined != old || usageRefreshDue(manager.usageSnapshot("a"), clock.now()) || resetRefreshDue(manager.usageSnapshot("a"), clock.now(), false) {
				t.Fatal("live flight was not shared")
			}
			clock.advance(time.Nanosecond)
			if !usageRefreshDue(manager.usageSnapshot("a"), clock.now()) || !resetRefreshDue(manager.usageSnapshot("a"), clock.now(), false) {
				t.Fatal("stale flight suppressed sweep due-ness")
			}
			next, current, currentFetcher, leader, err := manager.beginUsageRefresh("a", trigger)
			if err != nil || !leader || next == old {
				t.Fatalf("stale flight not superseded: leader=%v err=%v", leader, err)
			}
			joined, _, _, leader, err = manager.beginUsageRefresh("a", trigger)
			if err != nil || leader || joined != next {
				t.Fatal("replacement flight not shared")
			}
			entries := usageLogs(hook)
			if len(entries) != 1 {
				t.Fatalf("abandonment logs=%d", len(entries))
			}
			assertUsageLog(t, entries[0], "usage fetch abandoned", log.Fields{
				"auth_id": "a", "provider": "codex", "started_at": old.startedAt.UTC().Format(time.RFC3339),
			})
			manager.fetchUsage(context.Background(), current, currentFetcher, next)
			cached := manager.usageSnapshot("a")
			// The superseded request eventually returns a 429. Neither its data,
			// retry state nor failure log may replace the newer successful fetch.
			manager.fetchUsage(context.Background(), auth, fetcher, old)
			if manager.usageSnapshot("a") != cached || len(usageLogs(hook)) != 2 || cached.Windows[0].UsedPercent != 12 {
				t.Fatal("late stale completion changed current usage or logs")
			}
			if next, _, _, leader, err := manager.beginUsageRefresh("a", "manual"); err != nil || leader || next != nil {
				t.Fatal("replacement fetch bypassed the three-minute floor")
			}
		})
	}
}

func TestUsageStaleFlightHonorsCooldown(t *testing.T) {
	manager, _, clock := setupResetManager(t, "claude")
	registerUsageAuth(t, manager, "a", "claude")
	old, _, _, _, _ := manager.beginUsageRefresh("a", "manual")
	clock.advance(UsageFlightMaxAge)
	entry := cloneUsageEntry(manager.usageSnapshot("a"))
	entry.NextFetchAt = clock.now().Add(UsageClaude429Initial)
	entry.CooldownUntil, entry.retryAt = entry.NextFetchAt, entry.NextFetchAt
	manager.usage.entries["a"] = entry
	for _, trigger := range []string{"manual", "sweep", "reset_loop"} {
		flight, _, _, leader, err := manager.beginUsageRefresh("a", trigger)
		if err != nil || flight != nil || leader || usageRefreshDue(entry, clock.now()) || resetRefreshDue(entry, clock.now(), false) {
			t.Fatalf("%s joined stale flight or bypassed cooldown", trigger)
		}
	}
	clock.advance(UsageClaude429Initial)
	flight, _, _, leader, err := manager.beginUsageRefresh("a", "sweep")
	if err != nil || !leader || flight == old {
		t.Fatal("elapsed cooldown did not allow stale flight replacement")
	}
}
