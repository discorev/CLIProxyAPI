package auth

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func resetDecisionLogs(hook *logtest.Hook) []*log.Entry {
	var decisions []*log.Entry
	for _, entry := range hook.AllEntries() {
		if entry.Message == "auto reset would be applied" {
			decisions = append(decisions, entry)
		}
	}
	return decisions
}

func TestResetDryRunRules(t *testing.T) {
	for _, autoApply := range []bool{false, true} {
		for _, tt := range []struct {
			provider, rule   string
			used             float64
			recovery, expiry time.Duration
		}{
			{"codex", "exhausted", 100, 5 * time.Hour, 2 * time.Hour},
			{"codex", "last_chance", 0, 5 * time.Hour, 15 * time.Minute},
			{"claude", "all_exhausted", 100, 5 * time.Hour, 0},
			{"claude", "expiring_exhausted", 100, 5 * time.Hour, 2 * time.Hour},
			{"claude", "last_chance", 0, 5 * time.Hour, 15 * time.Minute},
		} {
			t.Run(fmt.Sprintf("%s/%s/auto=%v", tt.provider, tt.rule, autoApply), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					hook := setupTestLoggerHook(t)
					log.SetLevel(log.InfoLevel)
					manager, executor, clock := setupResetManager(t, tt.provider)
					manager.SetConfig(&config.Config{ResetCredits: config.ResetCreditsConfig{AutoApply: autoApply, DryRun: true}})
					inventory := resetTestEntry(tt.provider, tt.used, tt.recovery, tt.expiry)
					if tt.provider == "claude" && tt.rule == "last_chance" {
						inventory.Resets.Grants[0].UseRequiresLimit = false
					}
					auth := seedResetAuth(t, manager, "a", tt.provider, inventory)
					auth.Quota = QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: clock.now().Add(tt.recovery)}
					auth.Unavailable = true
					auth.NextRetryAfter = auth.Quota.NextRecoverAt
					if _, err := manager.Update(context.Background(), auth); err != nil {
						t.Fatal(err)
					}
					before, _ := manager.GetByID("a")
					manager.StartResetLoop()
					manager.StartResetLoop()
					synctest.Wait()
					manager.sweepResets(context.Background())
					synctest.Wait()
					if !manager.ResetLoopRunning() || executor.calls.Load() != 0 || executor.fetches.Load() != 0 {
						t.Fatal("dry-run did not run without sending/fetching fresh inventory")
					}
					if len(manager.usage.resets) != 0 {
						t.Fatal("dry-run took a spend reservation")
					}
					after, _ := manager.GetByID("a")
					if !reflect.DeepEqual(before, after) {
						t.Fatal("dry-run changed auth/cooldown state")
					}
					decisions := resetDecisionLogs(hook)
					if len(decisions) != 1 {
						t.Fatalf("decisions=%+v", decisions)
					}
					want := log.Fields{"auth_id": "a", "provider": tt.provider, "rule": tt.rule,
						"reason": resetDecisionReason(tt.rule), "reset_expires_at": nil}
					if tt.expiry != 0 {
						want["reset_expires_at"] = resetTestNow.Add(tt.expiry).Format(time.RFC3339Nano)
					}
					if tt.used >= 100 {
						want["natural_recovery"] = resetTestNow.Add(tt.recovery).Format(time.RFC3339Nano)
					}
					if tt.provider == "codex" {
						want["credit_id"] = "credit"
					} else {
						want["grant_id"] = "grant"
					}
					if decisions[0].Level != log.InfoLevel || !reflect.DeepEqual(decisions[0].Data, want) {
						t.Fatalf("log=%+v want=%+v", decisions[0], want)
					}
					announcements := 0
					for _, entry := range hook.AllEntries() {
						if entry.Message == "reset auto-apply dry-run enabled: no resets will be sent" && entry.Level == log.InfoLevel {
							announcements++
						}
					}
					if announcements != 1 {
						t.Fatalf("announcements=%d", announcements)
					}
					manager.StopResetLoop()
					synctest.Wait()
				})
			})
		}
	}
}

func TestResetDryRunDedupe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hook := setupTestLoggerHook(t)
		log.SetLevel(log.InfoLevel)
		manager, executor, clock := setupResetManager(t, "codex")
		manager.SetConfig(&config.Config{ResetCredits: config.ResetCreditsConfig{DryRun: true}})
		inventory := resetTestEntry("codex", 100, 24*time.Hour, 12*time.Hour)
		seedResetAuth(t, manager, "a", "codex", inventory)
		manager.StartResetLoop()
		synctest.Wait()
		check := func(want int) {
			t.Helper()
			// Keep the same decision fresh while advancing the fake clock.
			manager.usage.mu.Lock()
			copyEntry := *manager.usage.entries["a"]
			copyEntry.CredentialUsage = cloneCredentialUsage(inventory)
			copyEntry.FetchedAt = clock.now()
			manager.usage.entries["a"] = &copyEntry
			manager.usage.mu.Unlock()
			manager.sweepResets(context.Background())
			synctest.Wait()
			if got := len(resetDecisionLogs(hook)); got != want {
				t.Fatalf("decisions=%d want=%d", got, want)
			}
		}
		check(1)
		clock.advance(time.Hour - time.Nanosecond)
		check(1)
		clock.advance(time.Nanosecond)
		check(2)
		check(2)
		inventory.Windows[0].UsedPercent = 0
		check(2)
		if len(manager.usage.resetDecisions) != 0 {
			t.Fatal("disappeared decision retained")
		}
		inventory.Windows[0].UsedPercent = 100
		check(3)
		inventory.Resets.Credits[0].ID = "different-credit"
		check(4)
		check(4)
		// A rule change is a new decision even when the credit stays the same.
		inventory.Windows[0].UsedPercent = 0
		inventory.Resets.Credits[0].ExpiresAt = clock.now().Add(15 * time.Minute)
		check(5)
		inventory.FetchedAt = clock.now()
		seedResetAuth(t, manager, "b", "codex", cloneCredentialUsage(inventory))
		check(6)
		if executor.calls.Load() != 0 || len(manager.usage.resets) != 0 {
			t.Fatal("dedupe took a spend lock or sent a reset")
		}
		manager.StopResetLoop()
		synctest.Wait()
	})
}

func TestResetDryRunSelection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hook := setupTestLoggerHook(t)
		log.SetLevel(log.InfoLevel)
		manager, executor, _ := setupResetManager(t, "claude")
		manager.SetConfig(&config.Config{ResetCredits: config.ResetCreditsConfig{DryRun: true}})
		seedResetAuth(t, manager, "a", "claude", resetTestEntry("claude", 100, 5*time.Hour, 0))
		seedResetAuth(t, manager, "b", "claude", resetTestEntry("claude", 100, 8*time.Hour, 0))
		manager.StartResetLoop()
		synctest.Wait()
		decisions := resetDecisionLogs(hook)
		if len(decisions) != 1 || decisions[0].Data["auth_id"] != "b" || decisions[0].Data["rule"] != "all_exhausted" {
			t.Fatalf("furthest-recovery selection=%+v", decisions)
		}
		// Expiring grants outrank banked grants, regardless of recovery distance.
		seedResetAuth(t, manager, "c", "claude", resetTestEntry("claude", 100, 3*time.Hour, 2*time.Hour))
		manager.sweepResets(context.Background())
		synctest.Wait()
		decisions = resetDecisionLogs(hook)
		if len(decisions) != 2 || decisions[1].Data["auth_id"] != "c" || decisions[1].Data["rule"] != "expiring_exhausted" {
			t.Fatalf("urgent selection=%+v", decisions)
		}
		if executor.calls.Load() != 0 {
			t.Fatal("selection spent a grant")
		}
		manager.StopResetLoop()
		synctest.Wait()
	})
}

func TestResetDryRunFetchCooldown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hook := setupTestLoggerHook(t)
		log.SetLevel(log.InfoLevel)
		manager, executor, clock := setupResetManager(t, "codex")
		manager.SetConfig(&config.Config{ResetCredits: config.ResetCreditsConfig{DryRun: true}})
		registerUsageAuth(t, manager, "a", "codex")
		inventory := resetTestEntry("codex", 100, 5*time.Hour, 2*time.Hour)
		executor.fetch = func(context.Context, *Auth) (UsageFetchResult, error) {
			if executor.fetches.Load() == 1 {
				return UsageFetchResult{}, &UsageHTTPError{StatusCode: 429}
			}
			return UsageFetchResult{Windows: inventory.Windows, Resets: inventory.Resets}, nil
		}
		manager.StartResetLoop()
		synctest.Wait()
		if executor.fetches.Load() != 1 {
			t.Fatal("dry-run alone did not fetch inventory")
		}
		cooldown := manager.UsageSnapshot("a").CooldownUntil
		clock.advance(cooldown.Sub(clock.now()) - time.Nanosecond)
		manager.sweepResets(context.Background())
		synctest.Wait()
		if executor.fetches.Load() != 1 || !manager.UsageSnapshot("a").CooldownUntil.Equal(cooldown) || len(resetDecisionLogs(hook)) != 0 {
			t.Fatal("dry-run bypassed/cleared fetch cooldown or logged unverified inventory")
		}
		clock.advance(time.Nanosecond)
		manager.sweepResets(context.Background())
		synctest.Wait()
		manager.sweepResets(context.Background())
		synctest.Wait()
		if executor.fetches.Load() != 2 || executor.calls.Load() != 0 || len(resetDecisionLogs(hook)) != 1 || len(manager.usage.resets) != 0 {
			t.Fatal("fresh inventory not evaluated without spending")
		}
		manager.StopResetLoop()
		synctest.Wait()
	})
}

func TestResetDryRunManualUnaffected(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			manager, executor, _ := setupResetManager(t, provider)
			manager.SetConfig(&config.Config{ResetCredits: config.ResetCreditsConfig{AutoApply: true, DryRun: true}})
			inventory := resetTestEntry(provider, 100, 5*time.Hour, 2*time.Hour)
			seedResetAuth(t, manager, "a", provider, inventory)
			executor.fetch = func(context.Context, *Auth) (UsageFetchResult, error) {
				return UsageFetchResult{Windows: inventory.Windows, Resets: inventory.Resets}, nil
			}
			result, _, err := manager.ApplyCredentialReset(context.Background(), "a", "")
			if err != nil || result.Result != "reset" || executor.calls.Load() != 1 {
				t.Fatalf("manual reset blocked by dry-run: %+v %v calls=%d", result, err, executor.calls.Load())
			}
		})
	}
}

func TestResetDryRunReloadGuards(t *testing.T) {
	manager, executor, clock := setupResetManager(t, "codex")
	hook := setupTestLoggerHook(t)
	log.SetLevel(log.InfoLevel)
	manager.SetConfig(&config.Config{ResetCredits: config.ResetCreditsConfig{DryRun: true}})
	inventory := resetTestEntry("codex", 100, 5*time.Hour, 2*time.Hour)
	seedResetAuth(t, manager, "a", "codex", inventory)
	// Mark the loop running without a goroutine to deterministically model a
	// config switch between selection, reservation, and execution.
	_, cancel := context.WithCancel(context.Background())
	manager.usage.resetCancel = cancel
	entry := manager.usage.entries["a"]
	if _, _, _, err := manager.reserveReset(context.Background(), "a", entry); !errors.Is(err, ErrResetUnavailable) || len(manager.usage.resets) != 0 {
		t.Fatalf("dry-run reservation was not refused: %v", err)
	}
	manager.SetConfig(&config.Config{ResetCredits: config.ResetCreditsConfig{AutoApply: true}})
	auth, applier, state, err := manager.reserveReset(context.Background(), "a", entry)
	if err != nil {
		t.Fatal(err)
	}
	manager.SetConfig(&config.Config{ResetCredits: config.ResetCreditsConfig{AutoApply: true, DryRun: true}})
	manager.StartResetLoop()
	manager.StartResetLoop()
	choice := codexResetChoice(inventory, clock.now())
	result, _, err := manager.executeReset(context.Background(), auth, applier, state, inventory, *choice)
	manager.releaseReset(state)
	if !errors.Is(err, ErrResetUnavailable) || !result.NotSent || executor.calls.Load() != 0 || !state.attempted.IsZero() || !state.retryAt.IsZero() {
		t.Fatalf("queued reset sent or changed backoff after dry-run reload: %+v %v %+v", result, err, state)
	}
	announcements := 0
	for _, entry := range hook.AllEntries() {
		if entry.Message == "reset auto-apply dry-run enabled: no resets will be sent" {
			announcements++
		}
	}
	if announcements != 1 {
		t.Fatalf("reload announcements=%d", announcements)
	}
}
