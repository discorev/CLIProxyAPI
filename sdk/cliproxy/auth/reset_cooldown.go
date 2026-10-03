package auth

import (
	"context"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

// clearResetQuota clears only limits restored by the confirmed upstream reset.
// It does not turn a usage reset into a general authentication/error reset.
func (m *Manager) clearResetQuota(attemptAuth *Auth) {
	now := m.usage.timeNow()
	m.mu.Lock()
	auth := m.auths[attemptAuth.ID]
	if auth == nil || usageAccountChanged(attemptAuth, auth) {
		m.mu.Unlock()
		return
	}
	codex := strings.EqualFold(auth.Provider, "codex")
	changed := false
	for _, state := range auth.ModelStates {
		if state == nil || !resetClearsQuota(state.Quota, codex) {
			continue
		}
		if state.Status == StatusDisabled || hasNonQuotaResetError(state.LastError) {
			applyCooldownFields(&state.Quota, QuotaState{})
		} else {
			resetModelState(state, now)
		}
		changed = true
	}
	preserveAuthError := hasNonQuotaResetError(auth.LastError)
	oldUnavailable, oldRetry := auth.Unavailable, auth.NextRetryAfter
	if resetClearsQuota(auth.Quota, true) {
		applyCooldownFields(&auth.Quota, QuotaState{})
		if !preserveAuthError {
			auth.Unavailable = false
			auth.NextRetryAfter = time.Time{}
		}
		changed = true
	}
	if !changed {
		m.mu.Unlock()
		return
	}
	if len(auth.ModelStates) > 0 {
		updateAggregatedAvailability(auth, now)
	}
	if preserveAuthError {
		auth.Unavailable, auth.NextRetryAfter = oldUnavailable, oldRetry
	} else if !auth.Disabled && auth.Status != StatusDisabled && !hasModelError(auth, now) {
		auth.LastError = nil
		auth.StatusMessage = ""
		auth.Status = StatusActive
	}
	auth.Generation++
	auth.UpdatedAt = now
	snapshot := auth.Clone()
	m.mu.Unlock()

	// Match ResetQuota's registry generation/epoch projection and scheduler sync.
	reg := registry.GetGlobalRegistry()
	models, epoch := reg.GetModelsAndEpochForClient(snapshot.ID)
	projections := make([]registry.ClientModelProjection, 0, len(models))
	for _, model := range models {
		if model != nil && strings.TrimSpace(model.ID) != "" {
			projections = append(projections, m.clientModelProjectionForAuth(snapshot, model.ID, now))
		}
	}
	reg.ApplyClientModelProjections(snapshot.ID, epoch, snapshot.Generation, projections)
	if m.scheduler != nil {
		m.scheduler.upsertAuth(snapshot)
	}
	m.persistCooldownStates(context.Background())
}

func resetClearsQuota(quota QuotaState, modelQuota bool) bool {
	return quota.Exceeded && (quota.Reason == "credential_quota" || modelQuota && quota.Reason == "quota")
}

func hasNonQuotaResetError(err *Error) bool {
	return err != nil && err.HTTPStatus != 429
}
