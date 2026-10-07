package auth

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

type resetChoice struct {
	grantID  string
	creditID string
	expires  time.Time
	rule     string
}

// exhaustedResetWindows deliberately excludes model-scoped/Fable limits and
// elapsed windows. Recovery is when every credential-wide exhausted limit ends.
func exhaustedResetWindows(entry CredentialUsage, now time.Time) ([]string, time.Time) {
	var windows []string
	var recovery time.Time
	for _, window := range entry.Windows {
		if window.Scope != "" || window.UsedPercent < 100 || !window.ResetsAt.After(now) {
			continue
		}
		var name string
		switch window.Kind {
		case "5h":
			name = "five_hour"
		case "7d", "long":
			name = "seven_day"
		default:
			continue
		}
		if !slices.Contains(windows, name) {
			windows = append(windows, name)
		}
		if window.ResetsAt.After(recovery) {
			recovery = window.ResetsAt
		}
	}
	return windows, recovery
}

func soonestResetCredit(resets *CredentialResets, now time.Time) *ResetCredit {
	return soonestResetCreditExcept(resets, now, nil)
}

// soonestResetCreditExcept ignores credits for which skip reports true; a nil
// skip considers every unexpired credit.
func soonestResetCreditExcept(resets *CredentialResets, now time.Time, skip func(creditID string) bool) *ResetCredit {
	if resets == nil {
		return nil
	}
	var chosen *ResetCredit
	for i := range resets.Credits {
		credit := &resets.Credits[i]
		if skip != nil && skip(credit.ID) {
			continue
		}
		if credit.ExpiresAt.After(now) && (chosen == nil || credit.ExpiresAt.Before(chosen.ExpiresAt)) {
			chosen = credit
		}
	}
	return chosen
}

func usableResetGrant(status *ClaudeResetStatus, grant ResetGrant, now time.Time) bool {
	return status != nil && status.Eligible && !grant.Paused && grant.UsableNow && grant.ResetsLeft > 0 &&
		(grant.StartsAt == nil || !grant.StartsAt.After(now)) && (grant.EndsAt == nil || grant.EndsAt.After(now)) &&
		(status.CooldownUntil == nil || !status.CooldownUntil.After(now))
}

func coveringResetGrant(grant ResetGrant, exhausted []string, status *ClaudeResetStatus) bool {
	for _, window := range exhausted {
		if !slices.Contains(grant.Clears, window) {
			return false
		}
	}
	// The block can also report the unscoped overage window, which is not a
	// routing limit. Do not claim a grant that would leave that block in place.
	if len(exhausted) > 0 {
		for _, window := range status.Exhausted {
			if !slices.Contains(grant.Clears, window) {
				return false
			}
		}
	}
	return true
}

func chooseResetGrant(status *ClaudeResetStatus, now time.Time, accept func(ResetGrant) bool) *ResetGrant {
	if status == nil {
		return nil
	}
	var chosen *ResetGrant
	for i := range status.Grants {
		grant := &status.Grants[i]
		if !usableResetGrant(status, *grant, now) || !accept(*grant) {
			continue
		}
		if status.NextGrantID != nil && grant.ID == *status.NextGrantID {
			return grant
		}
		if chosen == nil || (grant.EndsAt != nil && (chosen.EndsAt == nil || grant.EndsAt.Before(*chosen.EndsAt))) {
			chosen = grant
		}
	}
	return chosen
}

func grantResetChoice(grant *ResetGrant, rule string) *resetChoice {
	if grant == nil {
		return nil
	}
	choice := &resetChoice{grantID: grant.ID, rule: rule}
	if grant.EndsAt != nil {
		choice.expires = *grant.EndsAt
	}
	return choice
}

// codexResetChoice has two rules. last_chance spends the soonest credit within
// 15 minutes of expiry, skipping credits whose last_chance attempt was already
// refused (refused may be nil). all_exhausted spends the soonest credit only
// when every enabled Codex account is exhausted (allExhausted) and this
// account's natural recovery is more than an hour away; refusals never block
// it. A Codex reset restarts the weekly window, so spending while another
// account has capacity makes intelligent-fill rank this account last and its
// fresh window runs down unused. Upstream refuses a credit unless its own
// account is limited, so waiting never loses a credit that last_chance can
// still spend.
func codexResetChoice(entry CredentialUsage, now time.Time, allExhausted bool, refused func(creditID string) bool) *resetChoice {
	credit := soonestResetCreditExcept(entry.Resets, now, refused)
	if credit != nil && !credit.ExpiresAt.After(now.Add(15*time.Minute)) {
		return &resetChoice{creditID: credit.ID, expires: credit.ExpiresAt, rule: "last_chance"}
	}
	if !allExhausted {
		return nil
	}
	_, recovery := exhaustedResetWindows(entry, now)
	if credit = soonestResetCredit(entry.Resets, now); credit != nil && recovery.After(now.Add(time.Hour)) {
		return &resetChoice{creditID: credit.ID, expires: credit.ExpiresAt, rule: "all_exhausted"}
	}
	return nil
}

func claudeResetChoice(entry CredentialUsage, now time.Time, allExhausted bool) *resetChoice {
	if entry.Resets == nil || entry.Resets.ClaudeResetStatus == nil {
		return nil
	}
	status := entry.Resets.ClaudeResetStatus
	exhausted, recovery := exhaustedResetWindows(entry, now)
	if recovery.After(now.Add(time.Hour)) {
		grant := chooseResetGrant(status, now, func(g ResetGrant) bool {
			return g.EndsAt != nil && g.EndsAt.Before(recovery) && coveringResetGrant(g, exhausted, status)
		})
		if grant != nil {
			return grantResetChoice(grant, "expiring_exhausted")
		}
	}
	grant := chooseResetGrant(status, now, func(g ResetGrant) bool {
		return !g.UseRequiresLimit && g.EndsAt != nil && !g.EndsAt.After(now.Add(15*time.Minute)) &&
			coveringResetGrant(g, exhausted, status)
	})
	if grant != nil {
		return grantResetChoice(grant, "last_chance")
	}
	if allExhausted && recovery.After(now.Add(time.Hour)) {
		grant = chooseResetGrant(status, now, func(g ResetGrant) bool { return coveringResetGrant(g, exhausted, status) })
		return grantResetChoice(grant, "all_exhausted")
	}
	return nil
}

func manualResetChoice(provider string, entry CredentialUsage, grantID string, now time.Time) *resetChoice {
	if provider == "codex" {
		if grantID != "" {
			return nil
		}
		if credit := soonestResetCredit(entry.Resets, now); credit != nil {
			return &resetChoice{creditID: credit.ID, expires: credit.ExpiresAt, rule: "manual"}
		}
		return nil
	}
	if entry.Resets == nil || entry.Resets.ClaudeResetStatus == nil {
		return nil
	}
	status := entry.Resets.ClaudeResetStatus
	grant := chooseResetGrant(status, now, func(g ResetGrant) bool {
		return (grantID == "" || g.ID == grantID) && (!g.UseRequiresLimit || status.AtLimit)
	})
	return grantResetChoice(grant, "manual")
}

// resetRefreshDue is independent of traffic header freshness: reset inventories
// can expire or be consumed elsewhere even while routing observations are fresh.
func resetRefreshDue(entry *usageEntry, now time.Time, locked bool) bool {
	if entry == nil {
		return true
	}
	if usageRefreshInFlight(entry, now) || now.Before(entry.NextFetchAt) || now.Before(entry.retryAt) {
		return false
	}
	if entry.Refreshing || entry.FetchedAt.IsZero() || entry.waitForToken || locked || !entry.retryAt.IsZero() {
		return true
	}
	// Empty inventories also need hourly checks to discover newly granted resets.
	return !now.Before(entry.FetchedAt.Add(resetFreshnessInterval(entry.CredentialUsage, now)))
}

func resetInventoryFresh(entry CredentialUsage, now time.Time) bool {
	return entry.Resets != nil && !entry.FetchedAt.IsZero() &&
		now.Before(entry.FetchedAt.Add(resetFreshnessInterval(entry, now)))
}

func resetFreshnessInterval(entry CredentialUsage, now time.Time) time.Duration {
	if entry.Resets != nil {
		if credit := soonestResetCredit(entry.Resets, now); credit != nil && !credit.ExpiresAt.After(now.Add(UsageNearExpiryWindow)) {
			return 5 * time.Minute
		}
		if status := entry.Resets.ClaudeResetStatus; status != nil {
			for _, grant := range status.Grants {
				if grant.ResetsLeft > 0 && grant.EndsAt != nil && grant.EndsAt.After(now) && !grant.EndsAt.After(now.Add(UsageNearExpiryWindow)) {
					return 5 * time.Minute
				}
			}
		}
	}
	return time.Hour
}

// Remember urgency separately from spendable inventory: an inventory failure
// clears Resets, but repeated failures must not revert to a 15-minute backoff
// while a previously observed reset is about to expire.
func usageFailureRetryDelay(entry *usageEntry, now time.Time) time.Duration {
	remember := func(expiry time.Time) {
		if expiry.After(now) && !expiry.After(now.Add(UsageNearExpiryWindow)) &&
			(!entry.resetRetryExpiry.After(now) || expiry.Before(entry.resetRetryExpiry)) {
			entry.resetRetryExpiry = expiry
		}
	}
	if entry.Resets != nil {
		for _, credit := range entry.Resets.Credits {
			remember(credit.ExpiresAt)
		}
		if status := entry.Resets.ClaudeResetStatus; status != nil {
			for _, grant := range status.Grants {
				if grant.ResetsLeft > 0 && grant.EndsAt != nil {
					remember(*grant.EndsAt)
				}
			}
		}
	}
	if entry.resetRetryExpiry.After(now) {
		return UsageMinFetchInterval
	}
	return UsageFailureBackoff
}

// resetIdempotencyKey identifies one automatic spend of one reset. It covers
// the account, the credit or grant, and, for multi-use grants, how many uses
// remain, so a later legitimate use of the same grant gets a new key. It is
// deliberately independent of rule, auth ID and time, so two instances that
// share credentials derive the same key for the same decision.
func resetIdempotencyKey(auth *Auth, entry CredentialUsage, choice resetChoice) string {
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	parts := []string{"cliproxy-reset", provider}
	var identityKeys []string
	switch provider {
	case "claude":
		identityKeys = []string{"account_uuid", "organization_uuid", "email"}
	case "codex":
		identityKeys = []string{"account_id", "email"}
	}
	for _, key := range identityKeys {
		value, _ := auth.Metadata[key].(string)
		parts = append(parts, strings.TrimSpace(value))
	}
	if choice.grantID != "" {
		left := -1
		if entry.Resets != nil && entry.Resets.ClaudeResetStatus != nil {
			for _, grant := range entry.Resets.Grants {
				if grant.ID == choice.grantID {
					left = grant.ResetsLeft
				}
			}
		}
		parts = append(parts, "grant", choice.grantID, strconv.Itoa(left))
	} else {
		parts = append(parts, "credit", choice.creditID, choice.expires.UTC().Format(time.RFC3339))
	}
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(strings.Join(parts, "\x00"))).String()
}
