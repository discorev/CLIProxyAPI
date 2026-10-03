package auth

import (
	"context"
	"sort"
	"strings"
	"time"
)

// StartResetLoop enables automatic reset evaluation, spending only outside dry-run.
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
		m.usage.resetDryRun = false
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
	auths := m.List()
	sort.Slice(auths, func(i, j int) bool { return auths[i].ID < auths[j].ID })
	var candidates []resetCandidate
	allClaudeExhausted := true
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
		// Every enabled OAuth account must positively establish exhaustion.
		// Unknown, failed, empty or refreshing accounts are not evidence that
		// the whole pool is exhausted; ancillary errors alone are immaterial.
		if strings.EqualFold(auth.Provider, "claude") &&
			(!fresh || recovery.IsZero() || busy || due || locked || entry.Refreshing) {
			allClaudeExhausted = false
		}
		if due {
			flight, current, fetcher, leader, err := m.beginUsageRefresh(auth.ID, false)
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
	var claude *resetCandidate
	for _, candidate := range candidates {
		if strings.EqualFold(candidate.auth.Provider, "codex") {
			if choice := codexResetChoice(candidate.entry.CredentialUsage, now); choice != nil {
				candidate.choice = *choice
				selected = append(selected, candidate)
			}
			continue
		}
		choice := claudeResetChoice(candidate.entry.CredentialUsage, now, allClaudeExhausted)
		if choice == nil {
			continue
		}
		candidate.choice = *choice
		// Spend expiring grants before banked ones, then prefer the account
		// furthest from natural relief. There is still only one claim per tick.
		if claude == nil || resetCandidateBefore(candidate, *claude) {
			copyCandidate := candidate
			claude = &copyCandidate
		}
	}
	if claude != nil {
		selected = append(selected, *claude)
	}
	if m.resetDryRun() {
		m.logResetDecisions(ctx, selected, now)
		return
	}
	for _, candidate := range selected {
		m.startAutomaticReset(ctx, candidate)
	}
}

func resetCandidateBefore(candidate, other resetCandidate) bool {
	urgent := candidate.choice.rule != "all_exhausted"
	otherUrgent := other.choice.rule != "all_exhausted"
	if urgent != otherUrgent {
		return urgent
	}
	if urgent && !candidate.choice.expires.Equal(other.choice.expires) {
		return candidate.choice.expires.Before(other.choice.expires)
	}
	return candidate.recovery.After(other.recovery)
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
