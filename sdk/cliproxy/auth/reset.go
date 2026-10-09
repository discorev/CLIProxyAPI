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
	authError bool
	attempted time.Time
	retryAt   time.Time
}

const resetRefusalBackoff = 15 * time.Minute

type resetCreditKey struct{ authID, creditID string }

// rememberLastChanceRefusalLocked stops repeating a Codex last_chance attempt
// that upstream answered with nothing to reset; nothing was spent. The
// all_exhausted rule may still spend the credit later. usage.mu must be held.
func (m *Manager) rememberLastChanceRefusalLocked(auth *Auth, choice resetChoice, outcome string) {
	if choice.rule != "last_chance" || outcome != "not_limited" || choice.creditID == "" ||
		!strings.EqualFold(auth.Provider, "codex") {
		return
	}
	if m.usage.lastChanceRefused == nil {
		m.usage.lastChanceRefused = make(map[resetCreditKey]time.Time)
	}
	m.usage.lastChanceRefused[resetCreditKey{auth.ID, choice.creditID}] = choice.expires
	log.WithFields(log.Fields{
		"auth_id": auth.ID, "provider": auth.Provider, "rule": choice.rule, "credit_id": choice.creditID,
		"reset_expires_at": choice.expires.UTC().Format(time.RFC3339), "outcome": outcome,
	}).Info("auto reset not needed: credit skipped for last_chance until it expires")
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
	if auth.Disabled || auth.Status == StatusDisabled ||
		(state != nil && state.authError && m.usage.timeNow().Before(state.retryAt)) {
		return nil, nil, nil, ErrResetUnavailable
	}
	if expected != nil {
		if m.resetDryRunFor(auth.Provider) || !m.automaticResetAvailable(auth, expected, state, m.usage.timeNow()) {
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
	entry, errRefresh := m.refreshUsageForReset(ctx, id, "manual", state)
	m.usage.mu.RLock()
	locked := resetLocked(state, m.usage.entries[id], m.usage.timeNow())
	m.usage.mu.RUnlock()
	if locked {
		return ResetResult{RefreshPending: true}, entry, ErrResetPendingRefresh
	}
	if errRefresh != nil || !resetInventoryFresh(entry, m.usage.timeNow()) {
		return ResetResult{}, entry, ErrResetUnavailable
	}
	choice := manualResetChoice(strings.ToLower(strings.TrimSpace(auth.Provider)), entry, strings.TrimSpace(grantID), m.usage.timeNow())
	if choice == nil {
		return ResetResult{}, entry, ErrResetUnavailable
	}
	return m.executeReset(ctx, auth, applier, state, entry, *choice)
}

// currentResetAuth returns the latest token without crossing an account lifecycle.
func (m *Manager) currentResetAuth(auth *Auth, state *resetAttempt) *Auth {
	m.mu.RLock()
	defer m.mu.RUnlock()
	m.usage.mu.RLock()
	defer m.usage.mu.RUnlock()
	current := m.auths[auth.ID]
	if m.usage.resets[auth.ID] != state || current == nil || !UsageFetchable(current) ||
		usageAccountChanged(auth, current) || current.Disabled || current.Status == StatusDisabled {
		return nil
	}
	return current.Clone()
}

func (m *Manager) executeReset(ctx context.Context, auth *Auth, applier ResetApplier, state *resetAttempt, entry CredentialUsage, choice resetChoice) (ResetResult, CredentialUsage, error) {
	// A config reload may make this provider dry-run after an automatic reset
	// was queued. Release its reservation without sending or changing
	// cooldown/backoff state.
	if choice.rule != "manual" && m.resetDryRunFor(auth.Provider) {
		return ResetResult{Result: "unavailable", NotSent: true}, m.UsageSnapshot(auth.ID), ErrResetUnavailable
	}
	current := m.currentResetAuth(auth, state)
	if ctx.Err() != nil || current == nil {
		return m.resetRefused(auth, state, choice, ResetResult{Result: "unavailable", NotSent: true})
	}
	auth = current
	request := ResetRequest{GrantID: choice.grantID, CreditID: choice.creditID}
	if choice.rule != "manual" {
		request.IdempotencyKey = resetIdempotencyKey(auth, entry, choice)
	}
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
	sent := m.usage.timeNow()
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
	// Serialize the reset marker with MarkResult's quota observation.
	m.mu.Lock()
	m.usage.mu.Lock()
	if m.usage.resets[auth.ID] != state {
		m.usage.mu.Unlock()
		m.mu.Unlock()
		return result, m.UsageSnapshot(auth.ID), nil
	}
	m.rememberLastChanceRefusalLocked(auth, choice, result.Result)
	state.authError = result.Result == "auth_error"
	if state.authError {
		// HTTP 401/403 is definitely not spent. Keep only a bounded retry delay,
		// for both providers and manual callers; no post-attempt fetch is needed.
		state.attempted = time.Time{}
		state.retryAt = finished.Add(resetRefusalBackoff)
		m.usage.mu.Unlock()
		m.mu.Unlock()
		return result, m.UsageSnapshot(auth.ID), nil
	}
	// Other outcomes retain the spend lock: transport/read failures and 5xx
	// responses cannot establish that the grant was not consumed.
	state.attempted = finished
	state.retryAt = time.Time{}
	if result.Result == "reset" && strings.EqualFold(auth.Provider, "codex") {
		anchor := result.RedeemedAt
		if anchor.IsZero() {
			anchor = sent
		}
		m.optimisticCodexResetLocked(auth.ID, finished, anchor)
	}
	if strings.EqualFold(auth.Provider, "claude") && resetNeedsBackoff(result.Result) {
		state.retryAt = resetBackoffUntil(entry.Resets, finished)
	}
	m.usage.mu.Unlock()
	m.mu.Unlock()
	if result.Result == "reset" {
		m.clearResetQuota(auth, state)
	}
	// A pre-attempt usage flight can return stale inventory. Join it first,
	// then request a new fetch after the POST has settled. Shared fetch rate
	// limits can defer it; the lock stays set until a later fetch succeeds.
	refreshed := m.refreshAfterReset(context.WithoutCancel(ctx), auth, state, finished)
	if strings.EqualFold(auth.Provider, "claude") && resetNeedsBackoff(result.Result) {
		m.usage.mu.Lock()
		if m.usage.resets[auth.ID] == state && refreshed.Resets != nil && refreshed.Resets.ClaudeResetStatus != nil &&
			refreshed.Resets.CooldownUntil != nil && refreshed.Resets.CooldownUntil.After(finished) {
			state.retryAt = *refreshed.Resets.CooldownUntil
		}
		m.usage.mu.Unlock()
	}
	m.usage.mu.RLock()
	result.RefreshPending = m.usage.resets[auth.ID] == state && resetLocked(state, m.usage.entries[auth.ID], m.usage.timeNow())
	m.usage.mu.RUnlock()
	// Attempted outcomes are data, not HTTP-handler errors. Do not expose
	// executor errors (which may contain sensitive transport details).
	return result, refreshed, nil
}

// optimisticCodexResetLocked runs under usage.mu after a confirmed reset.
func (m *Manager) optimisticCodexResetLocked(id string, finished, anchor time.Time) {
	entry := m.usage.entries[id]
	if entry == nil {
		return
	}
	next := cloneUsageEntry(entry)
	next.resetConfirmedAt = finished
	next.resetWindowAnchor = anchor
	for i, window := range next.Windows {
		if window.Scope != "" || window.Length <= 0 || (!window.ResetsAt.IsZero() && !isPreResetWindow(window, anchor)) {
			continue
		}
		delete(next.windowVersions, windowKey(window))
		next.Windows[i].UsedPercent = 0
		next.Windows[i].ResetsAt = anchor.Add(time.Duration(window.Length) * time.Second)
	}
	m.usage.entries[id] = next
}

func (m *Manager) resetRefused(auth *Auth, state *resetAttempt, choice resetChoice, result ResetResult) (ResetResult, CredentialUsage, error) {
	if choice.rule != "manual" {
		m.usage.mu.Lock()
		if m.usage.resets[auth.ID] == state {
			state.retryAt = m.usage.timeNow().Add(resetRefusalBackoff)
		}
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
	case "cooldown", "ineligible", "unavailable", "rate_limited":
		return true
	default:
		return false
	}
}

func resetBackoffUntil(resets *CredentialResets, now time.Time) time.Time {
	if resets != nil && resets.ClaudeResetStatus != nil && resets.CooldownUntil != nil && resets.CooldownUntil.After(now) {
		return *resets.CooldownUntil
	}
	return now.Add(resetRefusalBackoff)
}

func (m *Manager) refreshAfterReset(ctx context.Context, auth *Auth, state *resetAttempt, attempted time.Time) CredentialUsage {
	m.usage.mu.RLock()
	flight := m.usage.flights[auth.ID]
	current := m.usage.resets[auth.ID] == state
	m.usage.mu.RUnlock()
	if !current {
		return m.UsageSnapshot(auth.ID)
	}
	if flight != nil && flight.startedAt.Before(attempted) && m.usage.timeNow().Before(flight.startedAt.Add(UsageFlightMaxAge)) {
		<-flight.done
	}
	entry, _ := m.refreshUsageForReset(ctx, auth.ID, "reset", state)
	return entry
}
