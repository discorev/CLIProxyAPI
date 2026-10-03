package helps

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

var resetGrantIDPattern = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)

var resetWindows = []string{"five_hour", "seven_day", "seven_day_overage_included"}

// ParseCodexResetCredits retains only available Codex credits with a valid
// expiry. Counts alone never authorize a redemption.
func ParseCodexResetCredits(body []byte) *cliproxyauth.CredentialResets {
	var payload struct {
		Credits []json.RawMessage `json:"credits"`
	}
	if json.Unmarshal(body, &payload) != nil || payload.Credits == nil {
		return nil
	}
	result := &cliproxyauth.CredentialResets{Credits: []cliproxyauth.ResetCredit{}}
	for _, raw := range payload.Credits {
		var credit struct {
			ID          json.RawMessage `json:"id"`
			ResetType   string          `json:"reset_type"`
			ResetTypeV2 string          `json:"resetType"`
			Status      string          `json:"status"`
			ExpiresAt   string          `json:"expires_at"`
			ExpiresAtV2 string          `json:"expiresAt"`
		}
		if json.Unmarshal(raw, &credit) != nil {
			continue
		}
		if credit.ResetType == "" {
			credit.ResetType = credit.ResetTypeV2
		}
		if strings.TrimSpace(credit.ResetType) != "codex_rate_limits" || strings.TrimSpace(credit.Status) != "available" {
			continue
		}
		if credit.ExpiresAt == "" {
			credit.ExpiresAt = credit.ExpiresAtV2
		}
		expires, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(credit.ExpiresAt))
		if err != nil {
			continue
		}
		var id string
		if json.Unmarshal(credit.ID, &id) != nil {
			var number json.Number
			if json.Unmarshal(credit.ID, &number) == nil {
				id = number.String()
			}
		}
		result.Credits = append(result.Credits, cliproxyauth.ResetCredit{ID: strings.TrimSpace(id), ExpiresAt: expires})
	}
	return result
}

// ParseClaudeResetGrants mirrors the strict cedar_ember contract: one malformed
// grant, duplicate ID, invalid timestamp or boolean rejects the entire block.
func ParseClaudeResetGrants(body []byte) *cliproxyauth.CredentialResets {
	var payload struct {
		Block *struct {
			Eligible         *bool             `json:"eligible"`
			IneligibleReason *string           `json:"ineligible_reason"`
			AtLimit          *bool             `json:"at_limit"`
			Exhausted        []json.RawMessage `json:"exhausted"`
			Grants           []json.RawMessage `json:"grants"`
			NextGrantID      json.RawMessage   `json:"next_grant_id"`
			WeeklyResetsAt   *time.Time        `json:"weekly_resets_at"`
			CooldownUntil    *time.Time        `json:"cooldown_until"`
		} `json:"cedar_ember"`
	}
	if json.Unmarshal(body, &payload) != nil || payload.Block == nil || payload.Block.Eligible == nil {
		return nil
	}
	block := payload.Block
	status := &cliproxyauth.ClaudeResetStatus{
		Eligible: *block.Eligible, IneligibleReason: block.IneligibleReason,
		WeeklyResetsAt: block.WeeklyResetsAt, CooldownUntil: block.CooldownUntil,
		Exhausted: parseResetWindows(block.Exhausted), Grants: []cliproxyauth.ResetGrant{},
	}
	if block.AtLimit != nil {
		status.AtLimit = *block.AtLimit
	}
	if status.IneligibleReason != nil && !slices.Contains([]string{
		"config_off", "tier", "seat", "mobile", "surface", "cli_version", "no_grant", "tenure", "other_experiment", "unavailable", "unknown",
	}, *status.IneligibleReason) {
		reason := "unknown"
		status.IneligibleReason = &reason
	}
	seen := make(map[string]bool)
	for _, raw := range block.Grants {
		grant, ok := parseClaudeResetGrant(raw)
		if !ok || seen[grant.ID] {
			return nil
		}
		seen[grant.ID] = true
		status.Grants = append(status.Grants, grant)
	}
	var next string
	if json.Unmarshal(block.NextGrantID, &next) == nil && seen[next] {
		status.NextGrantID = &next
	}
	return &cliproxyauth.CredentialResets{ClaudeResetStatus: status}
}

func parseClaudeResetGrant(raw []byte) (cliproxyauth.ResetGrant, bool) {
	var wire struct {
		ID               string            `json:"id"`
		Label            json.RawMessage   `json:"label"`
		ResetsTotal      *int              `json:"resets_total"`
		ResetsLeft       *int              `json:"resets_left"`
		StartsAt         *time.Time        `json:"starts_at"`
		EndsAt           *time.Time        `json:"ends_at"`
		Clears           []json.RawMessage `json:"clears"`
		Paused           *bool             `json:"paused"`
		UsableNow        *bool             `json:"usable_now"`
		UseRequiresLimit *bool             `json:"use_requires_limit"`
		PercentUsed      json.RawMessage   `json:"percent_used"`
	}
	var grant cliproxyauth.ResetGrant
	if json.Unmarshal(raw, &wire) != nil || !resetGrantIDPattern.MatchString(wire.ID) ||
		wire.ResetsTotal == nil || wire.ResetsLeft == nil || *wire.ResetsTotal < 0 || *wire.ResetsLeft < 0 || *wire.ResetsLeft > *wire.ResetsTotal {
		return grant, false
	}
	grant = cliproxyauth.ResetGrant{
		ID: wire.ID, ResetsTotal: *wire.ResetsTotal, ResetsLeft: *wire.ResetsLeft,
		StartsAt: wire.StartsAt, EndsAt: wire.EndsAt, Clears: parseResetWindows(wire.Clears),
		UseRequiresLimit: true, PercentUsed: make(map[string]int),
	}
	var label string
	_ = json.Unmarshal(wire.Label, &label)
	label = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, label)
	labelRunes := []rune(strings.Join(strings.Fields(label), " "))
	grant.Label = string(labelRunes[:min(len(labelRunes), 120)])
	if wire.Paused != nil {
		grant.Paused = *wire.Paused
	}
	if wire.UsableNow != nil {
		grant.UsableNow = *wire.UsableNow
	}
	if wire.UseRequiresLimit != nil {
		grant.UseRequiresLimit = *wire.UseRequiresLimit
	}
	var percents map[string]json.RawMessage
	if json.Unmarshal(wire.PercentUsed, &percents) == nil {
		for _, window := range resetWindows {
			var percent *int
			if json.Unmarshal(percents[window], &percent) == nil && percent != nil && *percent >= 0 && *percent <= 100 {
				grant.PercentUsed[window] = *percent
			}
		}
	}
	return grant, true
}

func parseResetWindows(raw []json.RawMessage) []string {
	windows := []string{}
	for _, known := range resetWindows {
		for _, value := range raw {
			var name string
			if json.Unmarshal(value, &name) == nil && name == known {
				windows = append(windows, known)
				break
			}
		}
	}
	return windows
}
