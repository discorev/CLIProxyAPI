package auth

import (
	"fmt"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// usageAccountChanged compares account metadata, not rotating access tokens.
func usageAccountChanged(previous, current *Auth) bool {
	reason, _ := usageAccountChange(previous, current)
	return reason != ""
}

// usageAccountChange returns only field names and presence/change indicators,
// never account identifiers, email addresses, or tokens.
func usageAccountChange(previous, current *Auth) (reason, changes string) {
	if previous == nil || current == nil {
		return "", ""
	}
	provider := strings.ToLower(strings.TrimSpace(previous.Provider))
	if provider != strings.ToLower(strings.TrimSpace(current.Provider)) {
		return "provider_changed", ""
	}
	var keys []string
	switch provider {
	case "claude":
		keys = []string{"email", "account_uuid", "organization_uuid"}
	case "codex":
		keys = []string{"account_id", "email"}
	default:
		return "", ""
	}
	var changed []string
	for _, key := range keys {
		before, _ := previous.Metadata[key].(string)
		after, _ := current.Metadata[key].(string)
		before, after = strings.TrimSpace(before), strings.TrimSpace(after)
		if before == after {
			continue
		}
		change := "<set> -> <changed>"
		if before == "" {
			change = "<empty> -> <set>"
		} else if after == "" {
			change = "<set> -> <empty>"
		}
		changed = append(changed, key+": "+change)
	}
	if len(changed) == 0 {
		return "", ""
	}
	return "identity_changed", strings.Join(changed, ", ")
}

// invalidateUsageLocked and removeUsageLocked run under m.mu, matching the
// refresh/header lock ordering. Only an actual cache/flight removal is logged.
func (m *Manager) invalidateUsageLocked(previous, current *Auth) {
	if reason, changes := usageAccountChange(previous, current); reason != "" {
		m.removeUsageLocked(previous, reason, changes)
	}
}

func (m *Manager) removeUsageLocked(auth *Auth, reason, changes string) {
	if auth == nil {
		return
	}
	id := auth.ID
	m.usage.mu.Lock()
	defer m.usage.mu.Unlock()
	cleared := m.usage.entries[id] != nil || m.usage.flights[id] != nil
	delete(m.usage.entries, id)
	delete(m.usage.flights, id)
	if state := m.usage.resets[id]; state != nil && !state.inFlight {
		delete(m.usage.resets, id)
	}
	if cleared {
		fields := log.Fields{"auth_id": id, "provider": auth.Provider, "reason": reason}
		if changes != "" {
			fields["identity_changes"] = changes
		}
		log.WithFields(fields).Info("usage cache cleared")
	}
}

// logUsageFetch runs while the cache mutation is locked. Invalidated flights
// and cancellations never claim to have populated the cache or failed upstream.
func logUsageFetch(auth *Auth, trigger string, entry *usageEntry, errFetch error) {
	fields := log.Fields{"auth_id": auth.ID, "provider": auth.Provider}
	// Each 429 cooldown has one existing log line, including ancillary 429s.
	if !entry.CooldownUntil.IsZero() {
		fields["cooldown_until"] = entry.CooldownUntil.UTC().Format(time.RFC3339)
		log.WithFields(fields).Info("usage fetch rate-limited")
		return
	}
	fields["trigger"] = trigger
	if errFetch == nil {
		log.WithFields(fields).WithFields(log.Fields{
			"windows": len(entry.Windows), "summary": usageWindowSummary(entry.Windows),
		}).Info("usage fetched")
	}
	if errFetch != nil || entry.LastError != "" {
		fields["error"] = entry.LastError
		// Transport/HTTP errors are sanitised by the fetchers. Parser errors
		// retain verbatim diagnostics in the cache and at opt-in DEBUG only:
		// their error strings can include values from the response.
		if errFetch != nil {
			for _, provider := range []string{"claude", "codex"} {
				if strings.HasPrefix(errFetch.Error(), "parse "+provider+" usage:") {
					log.WithFields(fields).Debug("usage fetch parse failed")
					fields["error"] = "invalid " + provider + " usage response"
					break
				}
			}
		}
		fields["retry_at"] = entry.NextFetchAt.UTC().Format(time.RFC3339)
		log.WithFields(fields).Info("usage fetch failed")
	}
}

func usageWindowSummary(windows []UsageWindow) string {
	parts := make([]string, 0, len(windows))
	for _, window := range windows {
		name := window.Kind
		if window.Scope != "" && window.Scope != "all" {
			name += ":" + window.Scope
		}
		reset := "unknown"
		if !window.ResetsAt.IsZero() {
			reset = window.ResetsAt.UTC().Format("2006-01-02T15:04Z")
		}
		parts = append(parts, fmt.Sprintf("%s=%.3g%%@%s", name, window.UsedPercent, reset))
	}
	return strings.Join(parts, ", ")
}
