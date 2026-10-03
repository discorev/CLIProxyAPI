package auth

import (
	"context"
	"time"

	log "github.com/sirupsen/logrus"
)

type resetDecisionKey struct {
	authID, rule, grantID, creditID string
}

func (m *Manager) resetDryRun() bool {
	cfg := m.runtimeConfigSnapshot()
	return cfg != nil && cfg.ResetCredits.DryRun
}

// updateResetDryRunLocked announces each transition into dry-run, including a
// hot reload of an already running loop. usage.mu must be held.
func (m *Manager) updateResetDryRunLocked() {
	dryRun := m.resetDryRun()
	if m.usage.resetDryRun == dryRun {
		return
	}
	m.usage.resetDryRun = dryRun
	m.usage.resetDecisions = nil
	if dryRun {
		log.Info("reset auto-apply dry-run enabled: no resets will be sent")
	}
}

// logResetDecisions uses the same reservation eligibility checks without taking
// a spend reservation. Only the chosen candidates count as present this sweep.
func (m *Manager) logResetDecisions(ctx context.Context, candidates []resetCandidate, now time.Time) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	m.usage.mu.Lock()
	defer m.usage.mu.Unlock()
	if ctx.Err() != nil || m.usage.resetCancel == nil || !m.resetDryRun() {
		return
	}
	m.updateResetDryRunLocked()
	present := make(map[resetDecisionKey]time.Time, len(candidates))
	for _, candidate := range candidates {
		auth := m.auths[candidate.auth.ID]
		if auth == nil || !UsageFetchable(auth) {
			continue
		}
		if _, ok := m.executors[executorKeyFromAuth(auth)].(ResetApplier); !ok {
			continue
		}
		if !m.automaticResetAvailable(auth, candidate.entry, m.usage.resets[auth.ID], now) {
			continue
		}
		choice := candidate.choice
		key := resetDecisionKey{authID: auth.ID, rule: choice.rule, grantID: choice.grantID, creditID: choice.creditID}
		last, seen := m.usage.resetDecisions[key]
		if !seen || !now.Before(last.Add(time.Hour)) {
			fields := log.Fields{
				"auth_id": auth.ID, "provider": auth.Provider, "rule": choice.rule,
				"reset_expires_at": nil, "reason": resetDecisionReason(choice.rule),
			}
			if choice.grantID != "" {
				fields["grant_id"] = choice.grantID
			}
			if choice.creditID != "" {
				fields["credit_id"] = choice.creditID
			}
			if !choice.expires.IsZero() {
				fields["reset_expires_at"] = choice.expires.UTC().Format(time.RFC3339Nano)
			}
			if !candidate.recovery.IsZero() {
				fields["natural_recovery"] = candidate.recovery.UTC().Format(time.RFC3339Nano)
			}
			log.WithFields(fields).Info("auto reset would be applied")
			last = now
		}
		present[key] = last
	}
	m.usage.resetDecisions = present
}

func resetDecisionReason(rule string) string {
	switch rule {
	case "exhausted":
		return "credit expires before exhausted windows recover"
	case "all_exhausted":
		return "all enabled Claude accounts exhausted; recovery more than one hour away"
	case "expiring_exhausted":
		return "grant expires before exhausted windows recover"
	case "last_chance":
		return "reset expires within fifteen minutes"
	default:
		return "reset policy matched"
	}
}
