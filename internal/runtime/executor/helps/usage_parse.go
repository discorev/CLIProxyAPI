package helps

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func ParseClaudeSubscriptionUsage(body []byte) ([]cliproxyauth.UsageWindow, error) {
	type window struct {
		Utilization *float64  `json:"utilization"`
		ResetsAt    time.Time `json:"resets_at"`
	}
	var payload struct {
		FiveHour *window `json:"five_hour"`
		SevenDay *window `json:"seven_day"`
		Limits   []struct {
			Kind     string    `json:"kind"`
			Percent  *float64  `json:"percent"`
			ResetsAt time.Time `json:"resets_at"`
			Scope    struct {
				Model struct {
					DisplayName string `json:"display_name"`
				} `json:"model"`
			} `json:"scope"`
		} `json:"limits"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parse claude usage: %w", err)
	}
	windows := make([]cliproxyauth.UsageWindow, 0, 3)
	for i, w := range []*window{payload.FiveHour, payload.SevenDay} {
		if w == nil || w.Utilization == nil {
			continue
		}
		windows = append(windows, cliproxyauth.UsageWindow{
			Kind: []string{"5h", "7d"}[i], UsedPercent: *w.Utilization,
			ResetsAt: w.ResetsAt, Length: []int64{18000, 604800}[i],
		})
	}
	for _, limit := range payload.Limits {
		if limit.Kind == "weekly_scoped" && limit.Percent != nil &&
			strings.HasPrefix(strings.ToLower(strings.TrimSpace(limit.Scope.Model.DisplayName)), "fable") {
			windows = append(windows, cliproxyauth.UsageWindow{
				Kind: "7d", Scope: "fable", UsedPercent: *limit.Percent,
				ResetsAt: limit.ResetsAt, Length: 604800,
			})
		}
	}
	return windows, nil
}

func ParseCodexSubscriptionUsage(body []byte, now time.Time) ([]cliproxyauth.UsageWindow, error) {
	type window struct {
		UsedPercent        *float64 `json:"used_percent"`
		LimitWindowSeconds int64    `json:"limit_window_seconds"`
		ResetAfterSeconds  *int64   `json:"reset_after_seconds"`
		ResetAt            int64    `json:"reset_at"`
	}
	var payload struct {
		RateLimit struct {
			Primary   *window `json:"primary_window"`
			Secondary *window `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parse codex usage: %w", err)
	}
	windows := make([]cliproxyauth.UsageWindow, 0, 2)
	for i, w := range []*window{payload.RateLimit.Primary, payload.RateLimit.Secondary} {
		if w == nil || w.UsedPercent == nil {
			continue
		}
		length := w.LimitWindowSeconds
		kind := cliproxyauth.UsageWindowKind(length, []string{"5h", "7d"}[i])
		if length <= 0 {
			length = []int64{18000, 604800}[i]
		}
		var reset time.Time
		if w.ResetAt > 0 {
			reset = time.Unix(w.ResetAt, 0)
		} else if w.ResetAfterSeconds != nil && *w.ResetAfterSeconds >= 0 {
			reset = now.Add(time.Duration(*w.ResetAfterSeconds) * time.Second)
		}
		windows = append(windows, cliproxyauth.UsageWindow{
			Kind: kind, UsedPercent: *w.UsedPercent, ResetsAt: reset, Length: length,
		})
	}
	return windows, nil
}
