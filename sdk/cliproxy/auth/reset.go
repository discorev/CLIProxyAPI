package auth

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// resetAttempt is protected by usage.mu. The reservation covers both the POST
// and its refresh, so manual and automatic callers share the same spending guard.
type resetAttempt struct {
	inFlight  bool
	attempted time.Time
	retryAt   time.Time
}

func resetLocked(state *resetAttempt, entry *usageEntry, now time.Time) bool {
	if state == nil || state.attempted.IsZero() {
		return false
	}
	return entry == nil || !resetInventoryFresh(entry.CredentialUsage, now) ||
		!entry.FetchedAt.After(state.attempted) || entry.fetchStartedAt.Before(state.attempted)
}

// reserveReset follows the existing manager -> usage lock order. expected pins
// an automatic decision to the exact snapshot on which its rules were checked.
func (m *Manager) reserveReset(ctx context.Context, id string, expected *usageEntry) (*Auth, ResetApplier, *resetAttempt, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	auth := m.auths[id]
	if auth == nil {
		return nil, nil, nil, ErrUsageAuthNotFound
	}
	applier, ok := m.executors[executorKeyFromAuth(auth)].(ResetApplier)
	if !UsageFetchable(auth) || !ok {
		return nil, nil, nil, ErrUsageNotFetchable
	}
	m.usage.mu.Lock()
	defer m.usage.mu.Unlock()
	state := m.usage.resets[id]
	if state != nil && state.inFlight {
		return nil, nil, nil, ErrResetInFlight
	}
	if ctx.Err() != nil {
		return nil, nil, nil, ctx.Err()
	}
	if expected != nil {
		if m.resetDryRun() || !m.automaticResetAvailable(auth, expected, state, m.usage.timeNow()) {
			return nil, nil, nil, ErrResetUnavailable
		}
	}
	if m.usage.resets == nil {
		m.usage.resets = make(map[string]*resetAttempt)
	}
	if state == nil {
		state = &resetAttempt{}
		m.usage.resets[id] = state
	}
	state.inFlight = true
	return auth.Clone(), applier, state, nil
}

// automaticResetAvailable is shared by real reservations and dry-run decisions.
// The manager and usage locks must be held; this never changes spending state.
func (m *Manager) automaticResetAvailable(auth *Auth, expected *usageEntry, state *resetAttempt, now time.Time) bool {
	return m.usage.resetCancel != nil && !auth.Disabled && auth.Status != StatusDisabled &&
		m.usage.entries[auth.ID] == expected && resetInventoryFresh(expected.CredentialUsage, now) &&
		!resetLocked(state, expected, now) && (state == nil || (!state.inFlight && !now.Before(state.retryAt)))
}

func (m *Manager) releaseReset(state *resetAttempt) {
	m.usage.mu.Lock()
	state.inFlight = false
	m.usage.mu.Unlock()
}

// ApplyCredentialReset is an explicit management action. It is independent of
// auto-apply and auto backoff, but always refreshes availability before spending.
func (m *Manager) ApplyCredentialReset(ctx context.Context, id, grantID string) (ResetResult, CredentialUsage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	auth, applier, state, err := m.reserveReset(ctx, id, nil)
	if err != nil {
		return ResetResult{}, m.UsageSnapshot(id), err
	}
	defer m.releaseReset(state)
	entry, errRefresh := m.RefreshUsage(ctx, id)
	m.usage.mu.RLock()
	locked := resetLocked(state, m.usage.entries[id], m.usage.timeNow())
	m.usage.mu.RUnlock()
	if locked {
		return ResetResult{RefreshPending: true}, entry, ErrResetPendingRefresh
	}
	if errRefresh != nil || !resetInventoryFresh(entry, m.usage.timeNow()) || !m.resetAuthCurrent(auth, false) {
		return ResetResult{}, entry, ErrResetUnavailable
	}
	choice := manualResetChoice(strings.ToLower(strings.TrimSpace(auth.Provider)), entry, strings.TrimSpace(grantID), m.usage.timeNow())
	if choice == nil {
		return ResetResult{}, entry, ErrResetUnavailable
	}
	return m.executeReset(ctx, auth, applier, state, entry, *choice)
}

func (m *Manager) resetAuthCurrent(auth *Auth, automatic bool) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	current := m.auths[auth.ID]
	return current != nil && UsageFetchable(current) && !usageAccountChanged(auth, current) &&
		(!automatic || (!current.Disabled && current.Status != StatusDisabled))
}

func (m *Manager) executeReset(ctx context.Context, auth *Auth, applier ResetApplier, state *resetAttempt, entry CredentialUsage, choice resetChoice) (ResetResult, CredentialUsage, error) {
	// A config reload may enable dry-run after an automatic reset was queued.
	// Release its reservation without sending or changing cooldown/backoff state.
	if choice.rule != "manual" && m.resetDryRun() {
		return ResetResult{Result: "unavailable", NotSent: true}, m.UsageSnapshot(auth.ID), ErrResetUnavailable
	}
	if ctx.Err() != nil || !m.resetAuthCurrent(auth, choice.rule != "manual") {
		return m.resetRefused(auth, state, choice, ResetResult{Result: "unavailable", NotSent: true})
	}
	request := ResetRequest{GrantID: choice.grantID}
	var profile struct {
		Organization struct {
			UUID string `json:"uuid"`
		} `json:"organization"`
	}
	if json.Unmarshal(entry.Raw["profile"], &profile) == nil {
		request.OrganizationID = profile.Organization.UUID
	}
	if rt := m.roundTripperFor(auth); rt != nil {
		ctx = context.WithValue(ctx, roundTripperContextKey{}, rt)
		ctx = context.WithValue(ctx, "cliproxy.roundtripper", rt)
	}
	result, _ := applier.ApplyReset(ctx, auth, request)
	if result.Result == "" {
		result.Result = "unknown"
	}
	if result.NotSent {
		return m.resetRefused(auth, state, choice, result)
	}
	finished := m.usage.timeNow()
	log.WithFields(log.Fields{
		"auth_id": auth.ID, "provider": auth.Provider, "rule": choice.rule,
		"grant_id": choice.grantID, "reset_expires_at": choice.expires, "outcome": result.Result,
	}).Info("subscription reset attempt")
	if result.Result == "reset" {
		m.clearResetQuota(auth)
	}
	// Even a known upstream refusal takes this lock. A transport/read failure
	// is not evidence that nothing was spent and must never trigger a retry.
	m.usage.mu.Lock()
	state.attempted = finished
	state.retryAt = time.Time{}
	if strings.EqualFold(auth.Provider, "claude") && resetNeedsBackoff(result.Result) {
		state.retryAt = resetBackoffUntil(entry.Resets, finished)
	}
	m.usage.mu.Unlock()
	// A pre-attempt usage flight can return stale inventory. Join it first,
	// then request a new fetch after the POST has settled. Shared fetch rate
	// limits can defer it; the lock stays set until a later fetch succeeds.
	refreshed := m.refreshAfterReset(context.WithoutCancel(ctx), auth, finished)
	if strings.EqualFold(auth.Provider, "claude") && resetNeedsBackoff(result.Result) {
		m.usage.mu.Lock()
		if refreshed.Resets != nil && refreshed.Resets.ClaudeResetStatus != nil &&
			refreshed.Resets.CooldownUntil != nil && refreshed.Resets.CooldownUntil.After(finished) {
			state.retryAt = *refreshed.Resets.CooldownUntil
		}
		m.usage.mu.Unlock()
	}
	m.usage.mu.RLock()
	result.RefreshPending = resetLocked(state, m.usage.entries[auth.ID], m.usage.timeNow())
	m.usage.mu.RUnlock()
	// Attempted outcomes are data, not HTTP-handler errors. Do not expose
	// executor errors (which may contain sensitive transport details).
	return result, refreshed, nil
}

func (m *Manager) resetRefused(auth *Auth, state *resetAttempt, choice resetChoice, result ResetResult) (ResetResult, CredentialUsage, error) {
	if choice.rule != "manual" {
		m.usage.mu.Lock()
		state.retryAt = m.usage.timeNow().Add(15 * time.Minute)
		m.usage.mu.Unlock()
	}
	log.WithFields(log.Fields{
		"auth_id": auth.ID, "provider": auth.Provider, "rule": choice.rule,
		"grant_id": choice.grantID, "outcome": result.Result,
	}).Debug("subscription reset refused before send")
	return result, m.UsageSnapshot(auth.ID), ErrResetUnavailable
}

func resetNeedsBackoff(outcome string) bool {
	switch outcome {
	case "cooldown", "ineligible", "unavailable", "rate_limited", "auth_error":
		return true
	default:
		return false
	}
}

func resetBackoffUntil(resets *CredentialResets, now time.Time) time.Time {
	if resets != nil && resets.ClaudeResetStatus != nil && resets.CooldownUntil != nil && resets.CooldownUntil.After(now) {
		return *resets.CooldownUntil
	}
	return now.Add(15 * time.Minute)
}

func (m *Manager) refreshAfterReset(ctx context.Context, auth *Auth, attempted time.Time) CredentialUsage {
	m.usage.mu.RLock()
	flight := m.usage.flights[auth.ID]
	m.usage.mu.RUnlock()
	if flight != nil && flight.startedAt.Before(attempted) {
		<-flight.done
	}
	if !m.resetAuthCurrent(auth, false) {
		return m.UsageSnapshot(auth.ID)
	}
	entry, _ := m.RefreshUsage(ctx, auth.ID)
	return entry
}
