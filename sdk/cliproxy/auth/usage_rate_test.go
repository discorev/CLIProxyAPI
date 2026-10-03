package auth

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func TestUsageRateSharedFetchFloor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager, executor, clock := setupResetManager(t, "codex")
		registerUsageAuth(t, manager, "a", "codex")
		first, err := manager.RefreshUsage(context.Background(), "a")
		if err != nil {
			t.Fatal(err)
		}
		if !first.NextFetchAt.Equal(resetTestNow.Add(UsageMinFetchInterval)) {
			t.Fatalf("next fetch=%v", first.NextFetchAt)
		}
		for range 3 {
			cached, err := manager.RefreshUsage(context.Background(), "a")
			if err != nil || cached.FetchedAt != first.FetchedAt {
				t.Fatalf("cached=%+v,%v", cached, err)
			}
			manager.sweepUsage(context.Background())
		}
		manager.StartResetLoop()
		synctest.Wait()
		if executor.fetches.Load() != 1 {
			t.Fatalf("floor bypassed: %d", executor.fetches.Load())
		}
		clock.advance(UsageMinFetchInterval)
		if _, err = manager.RefreshUsage(context.Background(), "a"); err != nil {
			t.Fatal(err)
		}
		if executor.fetches.Load() != 2 {
			t.Fatal("floor never released")
		}
		manager.StopResetLoop()
		synctest.Wait()
	})
}

func TestUsageRate429LaddersSharedWithManualRefresh(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		for _, ancillary := range []bool{false, true} {
			t.Run(provider+map[bool]string{false: "/usage", true: "/ancillary"}[ancillary], func(t *testing.T) {
				manager, executor, clock := setupResetManager(t, provider)
				registerUsageAuth(t, manager, "a", provider)
				limited := true
				executor.fetch = func(context.Context, *Auth) (UsageFetchResult, error) {
					if !limited {
						return UsageFetchResult{}, nil
					}
					failure := &UsageHTTPError{StatusCode: 429}
					if ancillary {
						return UsageFetchResult{LastError: "ancillary status 429", RateLimit: failure}, nil
					}
					return UsageFetchResult{}, failure
				}
				ladder := []time.Duration{5 * time.Minute, 15 * time.Minute, time.Hour, time.Hour}
				if provider == "claude" {
					ladder = []time.Duration{20 * time.Minute, 40 * time.Minute, time.Hour, time.Hour}
				}
				for i, delay := range ladder {
					start := clock.now()
					entry, _ := manager.RefreshUsage(context.Background(), "a")
					if !entry.CooldownUntil.Equal(start.Add(delay)) || entry.NextFetchAt != entry.CooldownUntil {
						t.Fatalf("step %d: %+v", i, entry)
					}
					clock.advance(delay - time.Nanosecond)
					cached, err := manager.RefreshUsage(context.Background(), "a")
					if err != nil || cached.CooldownUntil != entry.CooldownUntil || executor.fetches.Load() != int32(i+1) {
						t.Fatalf("manual probed during cooldown: %+v,%v calls=%d", cached, err, executor.fetches.Load())
					}
					clock.advance(time.Nanosecond)
				}
				limited = false
				entry, err := manager.RefreshUsage(context.Background(), "a")
				if err != nil || !entry.CooldownUntil.IsZero() {
					t.Fatalf("success=%+v,%v", entry, err)
				}
				clock.advance(UsageMinFetchInterval)
				limited = true
				entry, _ = manager.RefreshUsage(context.Background(), "a")
				if !entry.CooldownUntil.Equal(clock.now().Add(ladder[0])) {
					t.Fatal("success did not reset 429 ladder")
				}
			})
		}
	}
}

func TestUsageRateRetryAfterAndUrgency(t *testing.T) {
	for _, tt := range []struct {
		provider         string
		retryAfter, want time.Duration
	}{
		{"claude", 0, 20 * time.Minute}, {"claude", time.Minute, 20 * time.Minute}, {"claude", 50 * time.Minute, 50 * time.Minute},
		{"codex", 0, 5 * time.Minute}, {"codex", 40 * time.Minute, 40 * time.Minute}, {"codex", 2 * time.Hour, time.Hour},
	} {
		manager, executor, clock := setupResetManager(t, tt.provider)
		seedResetAuth(t, manager, "a", tt.provider, resetTestEntry(tt.provider, 100, time.Hour, 10*time.Minute))
		executor.fetch = func(context.Context, *Auth) (UsageFetchResult, error) {
			return UsageFetchResult{}, &UsageHTTPError{StatusCode: 429, RetryAfter: tt.retryAfter}
		}
		entry, _ := manager.RefreshUsage(context.Background(), "a")
		if !entry.CooldownUntil.Equal(clock.now().Add(tt.want)) {
			t.Fatalf("%+v: cooldown=%v", tt, entry.CooldownUntil)
		}
	}
}

func TestUsageRateExpiredClaudeTokenWaitsForRotation(t *testing.T) {
	manager, executor, clock := setupResetManager(t, "claude")
	auth := registerUsageAuth(t, manager, "a", "claude")
	auth.Metadata["expired"] = clock.now().Add(-time.Minute).Format(time.RFC3339)
	if _, err := manager.Update(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	limited := true
	executor.fetch = func(context.Context, *Auth) (UsageFetchResult, error) {
		if limited {
			return UsageFetchResult{}, &UsageHTTPError{StatusCode: 429}
		}
		return UsageFetchResult{}, nil
	}
	entry, _ := manager.RefreshUsage(context.Background(), "a")
	if !entry.CooldownUntil.IsZero() || !manager.usageSnapshot("a").waitForToken {
		t.Fatalf("expired token became a rate cooldown: %+v", entry)
	}
	clock.advance(2 * time.Hour)
	_, _ = manager.RefreshUsage(context.Background(), "a")
	if executor.fetches.Load() != 1 {
		t.Fatal("retried expired token")
	}
	auth.Metadata["access_token"] = "rotated-fake-token"
	auth.Metadata["expired"] = clock.now().Add(time.Hour).Format(time.RFC3339)
	if _, err := manager.Update(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	limited = false
	entry, err := manager.RefreshUsage(context.Background(), "a")
	if err != nil || executor.fetches.Load() != 2 || entry.LastError != "" || manager.usageSnapshot("a").waitForToken {
		t.Fatalf("rotation did not recover: %+v,%v", entry, err)
	}
}

func TestUsageRateAncillaryCacheTTL(t *testing.T) {
	manager, executor, clock := setupResetManager(t, "codex")
	registerUsageAuth(t, manager, "a", "codex")
	var options []UsageFetchOptions
	executor.fetch = func(ctx context.Context, _ *Auth) (UsageFetchResult, error) {
		opts := UsageFetchOptionsFromContext(ctx)
		options = append(options, opts)
		raw := map[string]json.RawMessage{"usage": json.RawMessage(`{}`), "reset_credits": json.RawMessage(`{"credits":[]}`)}
		if !opts.SkipProfile {
			raw["profile"] = json.RawMessage(`{"cached":true}`)
		}
		if !opts.SkipSubscription {
			raw["subscription"] = json.RawMessage(`{"cached":true}`)
		}
		return UsageFetchResult{Raw: raw}, nil
	}
	_, _ = manager.RefreshUsage(context.Background(), "a")
	clock.advance(UsageMinFetchInterval)
	entry, _ := manager.RefreshUsage(context.Background(), "a")
	if options[0].SkipProfile || options[0].SkipSubscription || !options[1].SkipProfile || !options[1].SkipSubscription || string(entry.Raw["profile"]) != `{"cached":true}` {
		t.Fatalf("TTL options/cache=%+v %+v", options, entry)
	}
	clock.advance(UsageAncillaryTTL - UsageMinFetchInterval)
	_, _ = manager.RefreshUsage(context.Background(), "a")
	if options[2].SkipProfile || options[2].SkipSubscription {
		t.Fatal("ancillary TTL extended by skipped fetch")
	}
}

func TestResetPostAttemptFloorRetainsSafetyLock(t *testing.T) {
	manager, executor, clock := setupResetManager(t, "codex")
	inventory := resetTestEntry("codex", 0, 5*time.Hour, 2*time.Hour)
	seedResetAuth(t, manager, "a", "codex", inventory)
	executor.fetch = func(context.Context, *Auth) (UsageFetchResult, error) {
		clock.advance(time.Nanosecond)
		return UsageFetchResult{Resets: inventory.Resets, Windows: inventory.Windows}, nil
	}
	result, entry, err := manager.ApplyCredentialReset(context.Background(), "a", "")
	if err != nil || result.Result != "reset" || executor.fetches.Load() != 1 || entry.NextFetchAt.IsZero() {
		t.Fatalf("reset=%+v,%+v,%v", result, entry, err)
	}
	if _, _, err = manager.ApplyCredentialReset(context.Background(), "a", ""); !errors.Is(err, ErrResetPendingRefresh) || executor.calls.Load() != 1 {
		t.Fatal("manual reused stale inventory")
	}
	clock.advance(UsageMinFetchInterval)
	_, _ = manager.RefreshUsage(context.Background(), "a")
	if resetLocked(manager.usage.resets["a"], manager.usageSnapshot("a"), clock.now()) {
		t.Fatal("fresh later fetch did not unlock")
	}
}
