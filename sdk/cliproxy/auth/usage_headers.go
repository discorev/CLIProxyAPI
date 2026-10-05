package auth

import (
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// UsageWindowKind classifies provider windows by duration, with a legacy slot
// fallback for responses that omit durations.
func UsageWindowKind(seconds int64, fallback string) string {
	switch {
	case seconds > 7*24*60*60:
		return "long"
	case seconds == 7*24*60*60:
		return "7d"
	case seconds > 0:
		return "5h"
	default:
		return fallback
	}
}

func usageHeaderNumber(headers http.Header, key string) (float64, bool) {
	value, err := strconv.ParseFloat(strings.TrimSpace(headers.Get(key)), 64)
	return value, err == nil && !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

// observeUsageHeadersLocked runs under m.mu so removal cannot race observation.
func (m *Manager) observeUsageHeadersLocked(auth *Auth, headers http.Header, now time.Time) {
	if !UsageFetchable(auth) {
		return
	}
	// Normalize the already-allowlisted headers, including lowercase wire keys.
	signals := collectQuotaSignals(auth.Provider, headers)
	if len(signals) == 0 {
		return
	}
	normalized := make(http.Header, len(signals))
	for key, value := range signals {
		normalized.Set(key, value)
	}
	m.usage.mu.Lock()
	defer m.usage.mu.Unlock()
	next := cloneUsageEntry(m.usage.entries[auth.ID])
	windows, complete := usageWindowsFromHeaders(auth.Provider, normalized, next.Windows, now)
	if len(windows) == 0 {
		return
	}
	next.headerVersion++
	if complete {
		// The headers listed every credential-wide window, so a cached one
		// they omit (e.g. a now-empty slot) is stale. Scoped windows come only
		// from usage fetches and are kept.
		next.Windows = slices.DeleteFunc(next.Windows, func(cached UsageWindow) bool {
			stale := cached.Scope == "" && !slices.ContainsFunc(windows, func(w UsageWindow) bool { return windowKey(w) == windowKey(cached) })
			if stale {
				delete(next.windowVersions, windowKey(cached))
			}
			return stale
		})
	}
	for _, window := range windows {
		next.Windows = mergeUsageWindow(next.Windows, window)
		next.windowVersions[windowKey(window)] = next.headerVersion
	}
	next.ObservedAt = now
	if m.usage.entries == nil {
		m.usage.entries = make(map[string]*usageEntry)
		m.usage.flights = make(map[string]*usageFlight)
	}
	m.usage.entries[auth.ID] = next
}

// usageWindowsFromHeaders also reports whether the windows are the complete
// credential-wide set. That holds only for Codex headers whose slots all carry
// window-minutes; older shapes without it are merged as partial updates.
func usageWindowsFromHeaders(provider string, headers http.Header, previous []UsageWindow, now time.Time) ([]UsageWindow, bool) {
	var windows []UsageWindow
	complete := false
	base := func(kind, scope string, length int64) UsageWindow {
		for _, window := range previous {
			if window.Kind == kind && window.Scope == scope {
				return window
			}
		}
		return UsageWindow{Kind: kind, Scope: scope, Length: length}
	}
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude":
		for _, slot := range []struct {
			name, kind, scope string
			length            int64
		}{{"5h", "5h", "", 18000}, {"7d", "7d", "", 604800}, {"7d_oi", "7d", "fable", 604800}} {
			prefix := "anthropic-ratelimit-unified-" + slot.name + "-"
			window := base(slot.kind, slot.scope, slot.length)
			used, hasUsed := usageHeaderNumber(headers, prefix+"utilization")
			reset, hasReset := usageHeaderNumber(headers, prefix+"reset")
			status := strings.ToLower(strings.TrimSpace(headers.Get(prefix + "status")))
			if !hasUsed && !hasReset && status != "rejected" && status != "allowed" {
				continue
			}
			if hasUsed {
				window.UsedPercent = used * 100
			}
			if status == "rejected" {
				window.UsedPercent = 100
			} else if status == "allowed" && !hasUsed && window.UsedPercent >= 100 {
				window.UsedPercent = 0
			}
			if hasReset && reset > 0 {
				window.ResetsAt = time.Unix(int64(reset), 0)
			}
			windows = append(windows, window)
		}
	case "codex":
		described := 0
		for i, slot := range []string{"primary", "secondary"} {
			prefix := "x-codex-" + slot + "-"
			used, hasUsed := usageHeaderNumber(headers, prefix+"used-percent")
			reset, hasReset := usageHeaderNumber(headers, prefix+"reset-at")
			after, hasAfter := usageHeaderNumber(headers, prefix+"reset-after-seconds")
			minutes, hasMinutes := usageHeaderNumber(headers, prefix+"window-minutes")
			// Codex sends an all-zero slot when the plan has no such window. An
			// explicit zero-length window is absent, not a freshly reset one.
			if hasMinutes && minutes == 0 {
				described++
				continue
			}
			if !hasUsed && !hasReset && !hasAfter {
				continue
			}
			if hasMinutes {
				described++
			}
			length := int64(minutes * 60)
			kind := UsageWindowKind(length, []string{"5h", "7d"}[i])
			if length <= 0 {
				length = []int64{18000, 604800}[i]
			}
			window := base(kind, "", length)
			if hasUsed {
				window.UsedPercent = used
			}
			window.Length = length
			if hasReset && reset > 0 {
				window.ResetsAt = time.Unix(int64(reset), 0)
			} else if hasAfter {
				window.ResetsAt = now.Add(time.Duration(after * float64(time.Second)))
			}
			windows = append(windows, window)
		}
		complete = described == 2
	}
	return windows, complete
}
