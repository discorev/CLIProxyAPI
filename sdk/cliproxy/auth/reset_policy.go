package auth

import (
	"slices"
	"time"
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
	if resets == nil {
		return nil
	}
	var chosen *ResetCredit
	for i := range resets.Credits {
		credit := &resets.Credits[i]
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

func codexResetChoice(entry CredentialUsage, now time.Time) *resetChoice {
	credit := soonestResetCredit(entry.Resets, now)
	if credit == nil {
		return nil
	}
	_, recovery := exhaustedResetWindows(entry, now)
	if !recovery.IsZero() && credit.ExpiresAt.Before(recovery) {
		return &resetChoice{creditID: credit.ID, expires: credit.ExpiresAt, rule: "exhausted"}
	}
	if !credit.ExpiresAt.After(now.Add(15 * time.Minute)) {
		return &resetChoice{creditID: credit.ID, expires: credit.ExpiresAt, rule: "last_chance"}
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
