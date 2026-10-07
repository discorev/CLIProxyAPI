package auth

import (
	"context"
	"slices"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

type resetDecisionKey struct {
	authID, rule, grantID, creditID string
}

// resetMode says which providers spend automatic resets; the rest are only
// logged (dry-run). The zero value means every provider is live.
type resetMode struct {
	allDryRun bool
	// live, when set, lists the only live providers: sorted, deduplicated and
	// lowercase. Empty means all providers are live.
	live []string
}

func (r resetMode) dryRunFor(provider string) bool {
	return r.allDryRun || (len(r.live) > 0 && !slices.Contains(r.live, strings.ToLower(strings.TrimSpace(provider))))
}

func (r resetMode) equal(other resetMode) bool {
	return r.allDryRun == other.allDryRun && slices.Equal(r.live, other.live)
}

// currentResetMode derives the reset mode from config. A provider is live only
// with auto-apply on, dry-run off and, when reset-credits.providers is set, the
// provider listed.
func (m *Manager) currentResetMode() resetMode {
	cfg := m.runtimeConfigSnapshot()
	switch {
	case cfg == nil:
		return resetMode{}
	case cfg.ResetCredits.DryRun || !cfg.ResetCredits.AutoApply:
		return resetMode{allDryRun: true}
	}
	var live []string
	for _, provider := range cfg.ResetCredits.Providers {
		if provider = strings.ToLower(strings.TrimSpace(provider)); provider != "" {
			live = append(live, provider)
		}
	}
	slices.Sort(live)
	return resetMode{live: slices.Compact(live)}
}

// resetDryRunFor reports whether automatic resets for provider are only logged.
func (m *Manager) resetDryRunFor(provider string) bool {
	return m.currentResetMode().dryRunFor(provider)
}

// updateResetDryRunLocked announces each dry-run transition, including a hot
// reload of an already running loop. usage.mu must be held.
func (m *Manager) updateResetDryRunLocked() {
	mode := m.currentResetMode()
	if m.usage.resetMode.equal(mode) {
		return
	}
	m.usage.resetMode = mode
	m.usage.resetDecisions = nil
	switch {
	case mode.allDryRun:
		log.Info("reset auto-apply dry-run enabled: no resets will be sent")
	case len(mode.live) > 0:
		log.WithField("providers", strings.Join(mode.live, ",")).Info("reset auto-apply limited to listed providers: other providers are dry-run")
	}
}

// logResetDecisions uses the same reservation eligibility checks without taking
// a spend reservation. Only the chosen candidates count as present this sweep.
func (m *Manager) logResetDecisions(ctx context.Context, candidates []resetCandidate, now time.Time) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	m.usage.mu.Lock()
	defer m.usage.mu.Unlock()
	if ctx.Err() != nil || m.usage.resetCancel == nil {
		return
	}
	m.updateResetDryRunLocked()
	present := make(map[resetDecisionKey]time.Time, len(candidates))
	for _, candidate := range candidates {
		auth := m.auths[candidate.auth.ID]
		if auth == nil || !UsageFetchable(auth) || !m.resetDryRunFor(auth.Provider) {
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
	case "all_exhausted":
		return "all enabled accounts of this provider exhausted; recovery more than one hour away"
	case "expiring_exhausted":
		return "grant expires before exhausted windows recover"
	case "last_chance":
		return "reset expires within fifteen minutes"
	default:
		return "reset policy matched"
	}
}
