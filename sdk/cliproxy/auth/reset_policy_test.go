package auth

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

var resetTestNow = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

func resetTestEntry(provider string, used float64, recovery, expiry time.Duration) CredentialUsage {
	entry := CredentialUsage{FetchedAt: resetTestNow, Windows: []UsageWindow{{Kind: "7d", UsedPercent: used, ResetsAt: resetTestNow.Add(recovery)}}}
	if provider == "codex" {
		entry.Resets = &CredentialResets{Credits: []ResetCredit{{ID: "credit", ExpiresAt: resetTestNow.Add(expiry)}}}
	} else {
		entry.Resets = &CredentialResets{ClaudeResetStatus: &ClaudeResetStatus{
			Eligible: true, AtLimit: used >= 100,
			Grants: []ResetGrant{{ID: "grant", ResetsTotal: 1, ResetsLeft: 1, UsableNow: true, UseRequiresLimit: true,
				Clears: []string{"five_hour", "seven_day", "seven_day_overage_included"}}},
		}}
		if expiry != 0 {
			end := resetTestNow.Add(expiry)
			entry.Resets.Grants[0].EndsAt = &end
		}
	}
	return entry
}

func TestResetCodexPolicy(t *testing.T) {
	for _, tt := range []struct {
		name             string
		all              bool
		used             float64
		recovery, expiry time.Duration
		want             string
	}{
		{"all exhausted", true, 100, 4 * time.Hour, time.Hour, "all_exhausted"},
		{"all exhausted banked credit", true, 100, 4 * time.Hour, 24 * time.Hour, "all_exhausted"},
		// The old per-account rule spent here; capacity elsewhere now waits.
		{"other account capacity", false, 100, 4 * time.Hour, time.Hour, ""},
		{"other account capacity banked", false, 100, time.Hour, 4 * time.Hour, ""},
		{"recovery soon", true, 100, 59 * time.Minute, 4 * time.Hour, ""},
		{"recovery at threshold", true, 100, time.Hour, 4 * time.Hour, ""},
		{"recovery just past threshold", true, 100, time.Hour + time.Nanosecond, 4 * time.Hour, "all_exhausted"},
		{"last chance zero usage", false, 0, time.Hour, 15 * time.Minute, "last_chance"},
		{"last chance outranks all exhausted", true, 100, 4 * time.Hour, 15 * time.Minute, "last_chance"},
		{"last chance boundary", false, 0, time.Hour, 15*time.Minute + time.Nanosecond, ""},
		{"not exhausted", true, 99, 4 * time.Hour, time.Hour, ""},
		{"expired credit", true, 100, time.Hour, -time.Minute, ""},
		{"elapsed window", true, 100, -time.Minute, time.Hour, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			entry := resetTestEntry("codex", tt.used, tt.recovery, tt.expiry)
			got := codexResetChoice(entry, resetTestNow, tt.all, nil)
			if (got == nil) != (tt.want == "") || got != nil && got.rule != tt.want {
				t.Fatalf("choice = %+v, want %s", got, tt.want)
			}
		})
	}
	// Recovery is the latest exhausted window, so a weekly window recovering
	// within the hour does not block a longer one; the soonest credit is used.
	entry := resetTestEntry("codex", 100, 30*time.Minute, 4*time.Hour)
	entry.Windows = append(entry.Windows, UsageWindow{Kind: "long", UsedPercent: 100, ResetsAt: resetTestNow.Add(10 * time.Hour)})
	entry.Resets.Credits = append(entry.Resets.Credits, ResetCredit{ID: "earlier", ExpiresAt: resetTestNow.Add(2 * time.Hour)})
	if got := codexResetChoice(entry, resetTestNow, true, nil); got == nil || got.creditID != "earlier" || got.rule != "all_exhausted" {
		t.Fatalf("did not use latest recovery/soonest credit: %+v", got)
	}
	entry.Windows[0].Scope, entry.Windows[1].Scope = "fable", "fable"
	if got := codexResetChoice(entry, resetTestNow, true, nil); got != nil {
		t.Fatalf("scoped exhaustion triggered reset: %+v", got)
	}
	// A five-hour limit alone still qualifies: a Codex credit clears both the
	// five-hour and weekly windows.
	entry = resetTestEntry("codex", 40, 5*24*time.Hour, 4*time.Hour)
	entry.Windows = append(entry.Windows, UsageWindow{Kind: "5h", UsedPercent: 100, ResetsAt: resetTestNow.Add(3 * time.Hour)})
	if got := codexResetChoice(entry, resetTestNow, true, nil); got == nil || got.rule != "all_exhausted" {
		t.Fatalf("five-hour limit not a candidate: %+v", got)
	}
}

func TestResetClaudePolicy(t *testing.T) {
	for _, tt := range []struct {
		name   string
		all    bool
		mutate func(*CredentialUsage)
		want   string
	}{
		{"all exhausted", true, nil, "all_exhausted"},
		{"other account headroom", false, nil, ""},
		{"recovery soon", true, func(e *CredentialUsage) { e.Windows[0].ResetsAt = resetTestNow.Add(59 * time.Minute) }, ""},
		{"recovery at threshold", true, func(e *CredentialUsage) { e.Windows[0].ResetsAt = resetTestNow.Add(time.Hour) }, ""},
		{"fable only", true, func(e *CredentialUsage) { e.Windows[0].Scope = "fable" }, ""},
		{"grant incomplete", true, func(e *CredentialUsage) { e.Resets.Grants[0].Clears = []string{"five_hour"} }, ""},
		{"missing five hour coverage", true, func(e *CredentialUsage) {
			e.Windows = append(e.Windows, UsageWindow{Kind: "5h", UsedPercent: 100, ResetsAt: resetTestNow.Add(2 * time.Hour)})
			e.Resets.Grants[0].Clears = []string{"seven_day"}
		}, ""},
		{"overage not covered", true, func(e *CredentialUsage) {
			e.Resets.Exhausted = []string{"seven_day_overage_included"}
			e.Resets.Grants[0].Clears = []string{"five_hour", "seven_day"}
		}, ""},
		{"ineligible", true, func(e *CredentialUsage) { e.Resets.Eligible = false }, ""},
		{"paused", true, func(e *CredentialUsage) { e.Resets.Grants[0].Paused = true }, ""},
		{"not usable", true, func(e *CredentialUsage) { e.Resets.Grants[0].UsableNow = false }, ""},
		{"no uses", true, func(e *CredentialUsage) { e.Resets.Grants[0].ResetsLeft = 0 }, ""},
		{"future start", true, func(e *CredentialUsage) { x := resetTestNow.Add(time.Minute); e.Resets.Grants[0].StartsAt = &x }, ""},
		{"expired", true, func(e *CredentialUsage) { x := resetTestNow; e.Resets.Grants[0].EndsAt = &x }, ""},
		{"cooldown", true, func(e *CredentialUsage) { x := resetTestNow.Add(time.Minute); e.Resets.CooldownUntil = &x }, ""},
		{"expiring exhausted despite headroom elsewhere", false, func(e *CredentialUsage) { x := resetTestNow.Add(2 * time.Hour); e.Resets.Grants[0].EndsAt = &x }, "expiring_exhausted"},
		{"expiry outlives recovery", false, func(e *CredentialUsage) { x := resetTestNow.Add(10 * time.Hour); e.Resets.Grants[0].EndsAt = &x }, ""},
		{"last chance needs limit", false, func(e *CredentialUsage) {
			e.Windows[0].UsedPercent = 0
			x := resetTestNow.Add(15 * time.Minute)
			e.Resets.Grants[0].EndsAt = &x
		}, ""},
		{"last chance low usage", false, func(e *CredentialUsage) {
			e.Windows[0].UsedPercent = 0
			x := resetTestNow.Add(15 * time.Minute)
			e.Resets.Grants[0].EndsAt = &x
			e.Resets.Grants[0].UseRequiresLimit = false
		}, "last_chance"},
		{"last chance soon recovery allowed", true, func(e *CredentialUsage) {
			e.Windows[0].ResetsAt = resetTestNow.Add(20 * time.Minute)
			x := resetTestNow.Add(15 * time.Minute)
			e.Resets.Grants[0].EndsAt = &x
			e.Resets.Grants[0].UseRequiresLimit = false
		}, "last_chance"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			entry := resetTestEntry("claude", 100, 5*time.Hour, 0)
			if tt.mutate != nil {
				tt.mutate(&entry)
			}
			got := claudeResetChoice(entry, resetTestNow, tt.all)
			if (got == nil) != (tt.want == "") || got != nil && got.rule != tt.want {
				t.Fatalf("choice = %+v, want %s", got, tt.want)
			}
		})
	}
}

func TestResetClaudeGrantPreference(t *testing.T) {
	entry := resetTestEntry("claude", 100, 5*time.Hour, 3*time.Hour)
	status := entry.Resets.ClaudeResetStatus
	second := status.Grants[0]
	second.ID = "second"
	soon := resetTestNow.Add(2 * time.Hour)
	second.EndsAt = &soon
	status.Grants = append(status.Grants, second)
	if got := claudeResetChoice(entry, resetTestNow, true); got.grantID != "second" {
		t.Fatalf("did not choose soonest: %+v", got)
	}
	next := "grant"
	status.NextGrantID = &next
	if got := claudeResetChoice(entry, resetTestNow, true); got.grantID != "grant" {
		t.Fatalf("did not prefer next: %+v", got)
	}
	status.Grants[0].Clears = nil
	if got := claudeResetChoice(entry, resetTestNow, true); got.grantID != "second" {
		t.Fatalf("next bypassed coverage: %+v", got)
	}
	status.Grants[0].Clears = second.Clears
	status.Grants[0].EndsAt, status.Grants[1].EndsAt = nil, nil
	status.NextGrantID = nil
	if got := claudeResetChoice(entry, resetTestNow, true); got.grantID != "grant" {
		t.Fatalf("did not choose stable first without expiry: %+v", got)
	}
}

func TestResetRefreshFreshness(t *testing.T) {
	for _, tt := range []struct {
		name        string
		age, expiry time.Duration
		want        bool
	}{
		{"banked fresh", 59 * time.Minute, 2 * time.Hour, false},
		{"banked stale", time.Hour, 2 * time.Hour, true},
		{"near expiry fresh", 4 * time.Minute, 30 * time.Minute, false},
		{"near expiry stale", 5 * time.Minute, 30 * time.Minute, true},
	} {
		for _, provider := range []string{"codex", "claude"} {
			t.Run(provider+"/"+tt.name, func(t *testing.T) {
				entry := resetTestEntry(provider, 0, time.Hour, tt.expiry)
				entry.FetchedAt = resetTestNow.Add(-tt.age)
				entry.ObservedAt = resetTestNow
				if got := resetRefreshDue(&usageEntry{CredentialUsage: entry}, resetTestNow, false); got != tt.want {
					t.Fatalf("due = %v", got)
				}
			})
		}
	}
	if !resetRefreshDue(nil, resetTestNow, false) {
		t.Fatal("never fetched not due")
	}
	entry := &usageEntry{CredentialUsage: resetTestEntry("codex", 100, time.Hour, time.Hour), retryAt: resetTestNow.Add(time.Minute)}
	if resetRefreshDue(entry, resetTestNow, true) {
		t.Fatal("lock bypassed fetch backoff")
	}
}

func TestResetIdempotencyKeyIsSharedAcrossInstances(t *testing.T) {
	expires := time.Date(2026, 10, 4, 3, 24, 0, 0, time.UTC)
	codex := func(id string) *Auth {
		return &Auth{ID: id, Provider: "codex", Metadata: map[string]any{"account_id": "acct-1", "email": "a@example.com", "access_token": "t-" + id}}
	}
	credit := resetChoice{creditID: "credit-1", expires: expires, rule: "last_chance"}
	// Two instances may name the credential differently, hold different tokens
	// and reach the decision via different rules: the key must still match.
	first := resetIdempotencyKey(codex("instance-a.json"), CredentialUsage{}, credit)
	second := resetIdempotencyKey(codex("instance-b.json"), CredentialUsage{}, resetChoice{creditID: "credit-1", expires: expires, rule: "all_exhausted"})
	if first != second {
		t.Fatalf("same decision produced different keys: %s vs %s", first, second)
	}
	if other := resetIdempotencyKey(codex("instance-a.json"), CredentialUsage{}, resetChoice{creditID: "credit-2", expires: expires, rule: "last_chance"}); other == first {
		t.Fatal("a different credit reused the key")
	}
	otherAccount := codex("instance-a.json")
	otherAccount.Metadata["account_id"] = "acct-2"
	if resetIdempotencyKey(otherAccount, CredentialUsage{}, credit) == first {
		t.Fatal("a different account reused the key")
	}

	claude := &Auth{ID: "c.json", Provider: "claude", Metadata: map[string]any{"account_uuid": "u-1", "organization_uuid": "o-1"}}
	usage := func(left int) CredentialUsage {
		return CredentialUsage{Resets: &CredentialResets{ClaudeResetStatus: &ClaudeResetStatus{Grants: []ResetGrant{{ID: "grant-1", ResetsLeft: left}}}}}
	}
	grant := resetChoice{grantID: "grant-1", rule: "all_exhausted"}
	twoLeft := resetIdempotencyKey(claude, usage(2), grant)
	if twoLeft != resetIdempotencyKey(claude, usage(2), resetChoice{grantID: "grant-1", rule: "last_chance"}) {
		t.Fatal("same grant use produced different keys")
	}
	// A multi-use grant's next legitimate use must not be deduplicated upstream.
	if twoLeft == resetIdempotencyKey(claude, usage(1), grant) {
		t.Fatal("next use of a multi-use grant reused the key")
	}
	if _, err := uuid.Parse(twoLeft); err != nil {
		t.Fatalf("key is not a UUID: %q", twoLeft)
	}
}

func TestResetCodexLastChanceSkipsRefusedCredits(t *testing.T) {
	refusedFirst := func(id string) bool { return id == "first" }
	for _, tt := range []struct {
		name          string
		all           bool
		used          float64
		second        time.Duration
		want, wantFor string
	}{
		{"second inside 15 minutes", false, 0, 14 * time.Minute, "last_chance", "second"},
		{"second outside 15 minutes", false, 0, 16 * time.Minute, "", ""},
		{"exhausted with capacity elsewhere", false, 100, 16 * time.Minute, "", ""},
		{"last chance before all exhausted", true, 100, 14 * time.Minute, "last_chance", "second"},
		{"all exhausted ignores refusals", true, 100, 16 * time.Minute, "all_exhausted", "first"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			entry := resetTestEntry("codex", tt.used, 4*time.Hour, 10*time.Minute)
			entry.Resets.Credits = []ResetCredit{
				{ID: "second", ExpiresAt: resetTestNow.Add(tt.second)},
				{ID: "first", ExpiresAt: resetTestNow.Add(10 * time.Minute)},
			}
			got := codexResetChoice(entry, resetTestNow, tt.all, refusedFirst)
			if (got == nil) != (tt.want == "") || got != nil && (got.rule != tt.want || got.creditID != tt.wantFor) {
				t.Fatalf("choice = %+v, want %s %s", got, tt.want, tt.wantFor)
			}
		})
	}
}
