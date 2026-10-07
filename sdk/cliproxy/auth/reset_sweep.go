package auth

import (
	"context"
	"sort"
	"strings"
	"time"
)

// StartResetLoop enables automatic reset evaluation, spending only for live
// (not dry-run) providers.
// Service config opts in with auto-apply or dry-run; routing strategy is unrelated.
func (m *Manager) StartResetLoop() {
	m.usage.mu.Lock()
	defer m.usage.mu.Unlock()
	m.updateResetDryRunLocked()
	if m.usage.resetCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.usage.resetCancel = cancel
	var ticker usageTicker
	if m.usage.newTicker != nil {
		ticker = m.usage.newTicker()
	} else {
		ticker = realUsageTicker{time.NewTicker(time.Minute)}
	}
	go func() {
		defer ticker.Stop()
		m.sweepResets(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.Ticks():
				m.sweepResets(ctx)
			}
		}
	}()
}

// StopResetLoop stops scheduling and cancels active POSTs. Ambiguous canceled
// attempts still retain their lock and complete the post-attempt usage refresh.
func (m *Manager) StopResetLoop() {
	if m == nil {
		return
	}
	m.usage.mu.Lock()
	defer m.usage.mu.Unlock()
	if m.usage.resetCancel != nil {
		m.usage.resetCancel()
		m.usage.resetCancel = nil
		m.usage.resetMode = resetMode{}
		m.usage.resetDecisions = nil
	}
}

func (m *Manager) ResetLoopRunning() bool {
	m.usage.mu.RLock()
	defer m.usage.mu.RUnlock()
	return m.usage.resetCancel != nil
}

type resetCandidate struct {
	auth     *Auth
	entry    *usageEntry
	choice   resetChoice
	recovery time.Time
}

func (m *Manager) sweepResets(ctx context.Context) {
	if ctx.Err() != nil || !m.ResetLoopRunning() {
		return
	}
	now := m.usage.timeNow()
	m.usage.mu.Lock()
	for key, expires := range m.usage.lastChanceRefused {
		if !expires.After(now) {
			delete(m.usage.lastChanceRefused, key)
		}
	}
	m.usage.mu.Unlock()
	auths := m.List()
	sort.Slice(auths, func(i, j int) bool { return auths[i].ID < auths[j].ID })
	var candidates []resetCandidate
	// allExhausted is tracked per provider; a provider stays true only while
	// every enabled account of it establishes exhaustion.
	allExhausted := map[string]bool{"claude": true, "codex": true}
	for _, auth := range auths {
		if ctx.Err() != nil {
			return
		}
		if !UsageFetchable(auth) || auth.Disabled || auth.Status == StatusDisabled {
			continue
		}
		m.usage.mu.RLock()
		entry, state := m.usage.entries[auth.ID], m.usage.resets[auth.ID]
		locked := resetLocked(state, entry, now)
		busy := state != nil && state.inFlight
		backoff := state != nil && now.Before(state.retryAt)
		due := !busy && resetRefreshDue(entry, now, locked)
		m.usage.mu.RUnlock()
		var recovery time.Time
		fresh := entry != nil && resetInventoryFresh(entry.CredentialUsage, now)
		if entry != nil {
			_, recovery = exhaustedResetWindows(entry.CredentialUsage, now)
		}
		// Every enabled OAuth account of a provider must positively establish
		// exhaustion. Unknown, failed, empty or refreshing accounts are not
		// evidence that the whole pool is exhausted; ancillary errors alone are
		// immaterial.
		if !fresh || recovery.IsZero() || busy || due || locked || entry.Refreshing {
			allExhausted[resetProvider(auth)] = false
		}
		if due {
			flight, current, fetcher, leader, err := m.beginUsageRefresh(auth.ID, "reset_loop")
			if err == nil && leader {
				go m.fetchUsage(context.WithoutCancel(ctx), current, fetcher, flight)
			}
			continue
		}
		if !fresh || entry.Refreshing || busy || locked || backoff {
			continue
		}
		candidates = append(candidates, resetCandidate{auth: auth, entry: entry, recovery: recovery})
	}
	var selected []resetCandidate
	// best holds each provider's single claim for this tick: every Claude
	// rule, and Codex all_exhausted. Codex last_chance claims are independent.
	best := make(map[string]*resetCandidate, 2)
	for _, candidate := range candidates {
		provider := resetProvider(candidate.auth)
		var choice *resetChoice
		switch provider {
		case "codex":
			refused := func(creditID string) bool { return m.lastChanceRefused(candidate.auth.ID, creditID) }
			choice = codexResetChoice(candidate.entry.CredentialUsage, now, allExhausted[provider], refused)
		case "claude":
			choice = claudeResetChoice(candidate.entry.CredentialUsage, now, allExhausted[provider])
		}
		if choice == nil {
			continue
		}
		candidate.choice = *choice
		if provider == "codex" && choice.rule == "last_chance" {
			selected = append(selected, candidate)
			continue
		}
		if current := best[provider]; current == nil || resetCandidateBefore(candidate, *current) {
			copyCandidate := candidate
			best[provider] = &copyCandidate
		}
	}
	for _, provider := range []string{"codex", "claude"} {
		if candidate := best[provider]; candidate != nil {
			selected = append(selected, *candidate)
		}
	}
	var dryRun []resetCandidate
	for _, candidate := range selected {
		if m.resetDryRunFor(candidate.auth.Provider) {
			dryRun = append(dryRun, candidate)
		} else {
			m.startAutomaticReset(ctx, candidate)
		}
	}
	m.logResetDecisions(ctx, dryRun, now)
}

func (m *Manager) lastChanceRefused(authID, creditID string) bool {
	m.usage.mu.RLock()
	defer m.usage.mu.RUnlock()
	_, refused := m.usage.lastChanceRefused[resetCreditKey{authID, creditID}]
	return refused
}

func resetProvider(auth *Auth) string {
	return strings.ToLower(strings.TrimSpace(auth.Provider))
}

// resetCandidateBefore orders one provider's claims. Urgent rules (last_chance,
// expiring_exhausted) come before all_exhausted. Within each group the reset
// that would be spent expiring soonest goes first, so it is not left to lapse
// behind a longer-lived one; resets without an expiry sort last. Equal
// expiries prefer the account furthest from natural recovery, then auth ID.
func resetCandidateBefore(candidate, other resetCandidate) bool {
	urgent := candidate.choice.rule != "all_exhausted"
	otherUrgent := other.choice.rule != "all_exhausted"
	if urgent != otherUrgent {
		return urgent
	}
	if expires, otherExpires := candidate.choice.expires, other.choice.expires; !expires.Equal(otherExpires) {
		return otherExpires.IsZero() || !expires.IsZero() && expires.Before(otherExpires)
	}
	if !candidate.recovery.Equal(other.recovery) {
		return candidate.recovery.After(other.recovery)
	}
	return candidate.auth.ID < other.auth.ID
}

func (m *Manager) startAutomaticReset(ctx context.Context, candidate resetCandidate) {
	auth, applier, state, err := m.reserveReset(ctx, candidate.auth.ID, candidate.entry)
	if err != nil {
		return
	}
	go func() {
		defer m.releaseReset(state)
		_, _, _ = m.executeReset(ctx, auth, applier, state, candidate.entry.CredentialUsage, candidate.choice)
	}()
}
