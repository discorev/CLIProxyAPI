package auth

import (
	"context"
	"errors"
	"maps"
	"slices"
	"time"
)

// CredentialResets is the parsed, provider-specific reset inventory. A nil
// inventory means that reset availability could not be established safely.
// Claude fields are flattened to match the upstream cedar_ember block.
type CredentialResets struct {
	Credits []ResetCredit `json:"credits,omitempty"`
	*ClaudeResetStatus
}

type ResetCredit struct {
	ID        string    `json:"id"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
}

type ClaudeResetStatus struct {
	Eligible         bool         `json:"eligible"`
	IneligibleReason *string      `json:"ineligible_reason"`
	AtLimit          bool         `json:"at_limit"`
	Exhausted        []string     `json:"exhausted"`
	Grants           []ResetGrant `json:"grants"`
	NextGrantID      *string      `json:"next_grant_id"`
	WeeklyResetsAt   *time.Time   `json:"weekly_resets_at"`
	CooldownUntil    *time.Time   `json:"cooldown_until,omitzero"`
}

type ResetGrant struct {
	ID               string         `json:"id"`
	Label            string         `json:"label"`
	ResetsTotal      int            `json:"resets_total"`
	ResetsLeft       int            `json:"resets_left"`
	StartsAt         *time.Time     `json:"starts_at"`
	EndsAt           *time.Time     `json:"ends_at"`
	Clears           []string       `json:"clears"`
	Paused           bool           `json:"paused"`
	UsableNow        bool           `json:"usable_now"`
	UseRequiresLimit bool           `json:"use_requires_limit"`
	PercentUsed      map[string]int `json:"percent_used"`
}

// ResetRequest contains only claim parameters, not credentials. OrganizationID
// comes from the cached profile, falling back to credential metadata upstream.
type ResetRequest struct {
	GrantID        string
	OrganizationID string
	// IdempotencyKey is sent as the upstream request ID. Automatic resets derive
	// it from the account and the reset being spent, so separate proxy
	// instances making the same decision send the same key. Empty means random.
	IdempotencyKey string
}

type ResetResult struct {
	Result         string `json:"result"`
	RefreshPending bool   `json:"refresh_pending,omitempty"`
	// NotSent is reserved for definite local refusals before the HTTP call.
	// Unknown outcomes and HTTP refusals must still acquire the refresh lock.
	NotSent bool `json:"-"`
}

// ResetApplier is optional and is implemented only by supported OAuth executors.
type ResetApplier interface {
	ApplyReset(context.Context, *Auth, ResetRequest) (ResetResult, error)
}

var (
	ErrResetUnavailable    = errors.New("no usable reset available")
	ErrResetInFlight       = errors.New("credential reset already in flight")
	ErrResetPendingRefresh = errors.New("reset pending refresh")
)

func clonePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func cloneCredentialResets(value *CredentialResets) *CredentialResets {
	if value == nil {
		return nil
	}
	copyValue := *value
	copyValue.Credits = slices.Clone(value.Credits)
	if value.ClaudeResetStatus != nil {
		status := *value.ClaudeResetStatus
		status.IneligibleReason = clonePointer(status.IneligibleReason)
		status.NextGrantID = clonePointer(status.NextGrantID)
		status.WeeklyResetsAt = clonePointer(status.WeeklyResetsAt)
		status.CooldownUntil = clonePointer(status.CooldownUntil)
		status.Exhausted = slices.Clone(status.Exhausted)
		status.Grants = slices.Clone(status.Grants)
		for i := range status.Grants {
			grant := &status.Grants[i]
			grant.StartsAt = clonePointer(grant.StartsAt)
			grant.EndsAt = clonePointer(grant.EndsAt)
			grant.Clears = slices.Clone(grant.Clears)
			grant.PercentUsed = maps.Clone(grant.PercentUsed)
		}
		copyValue.ClaudeResetStatus = &status
	}
	return &copyValue
}
