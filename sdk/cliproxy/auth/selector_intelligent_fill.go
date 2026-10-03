package auth

import (
	"context"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// IntelligentFillSelector fills the subscription whose weekly reset is soonest.
// It deliberately uses the custom-selector path, not the built-in scheduler.
type IntelligentFillSelector struct {
	manager *Manager
}

func NewIntelligentFillSelector(manager *Manager) *IntelligentFillSelector {
	return &IntelligentFillSelector{manager: manager}
}

func (s *IntelligentFillSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	now := time.Now()
	if s.manager != nil {
		now = s.manager.usage.timeNow()
	}
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	var best, fallback *Auth
	var bestRank, fallbackRank usageRank
	for _, auth := range available {
		var entry *usageEntry
		requestModel := model
		if s.manager != nil {
			entry = s.manager.usageSnapshot(auth.ID)
			requestModel = s.manager.selectionModelForAuth(auth, model)
		}
		rank, exhausted := rankUsage(entry, requestModel, now)
		if fallback == nil || rank.less(fallbackRank, auth.ID, fallback.ID) {
			fallback, fallbackRank = auth, rank
		}
		if !exhausted && (best == nil || rank.less(bestRank, auth.ID, best.ID)) {
			best, bestRank = auth, rank
		}
	}
	if best != nil {
		return best, nil
	}
	// Cached exhaustion is only advisory; upstream decides when all are gated.
	return fallback, nil
}

type usageRank struct {
	known bool
	reset time.Time
}

func (r usageRank) less(other usageRank, id, otherID string) bool {
	if r.known != other.known {
		return r.known
	}
	if r.reset.IsZero() != other.reset.IsZero() {
		return !r.reset.IsZero()
	}
	if !r.reset.Equal(other.reset) {
		return r.reset.Before(other.reset)
	}
	return id < otherID
}

func rankUsage(entry *usageEntry, model string, now time.Time) (usageRank, bool) {
	if entry == nil || len(entry.Windows) == 0 {
		return usageRank{}, false
	}
	rank := usageRank{known: true}
	exhausted := false
	var ranking *UsageWindow
	for i := range entry.Windows {
		window := &entry.Windows[i]
		if window.Scope != "" && (window.Scope != "fable" || !strings.Contains(strings.ToLower(canonicalModelKey(model)), "fable")) {
			continue
		}
		rolledOver := !window.ResetsAt.IsZero() && !window.ResetsAt.After(now)
		if !rolledOver && window.UsedPercent >= 100 {
			exhausted = true
		}
		// Scoped limits and the short window gate, but do not displace the
		// global weekly window in ranking. The longest is the fallback.
		if window.Scope != "" {
			continue
		}
		if ranking == nil || (window.Kind == "7d" && ranking.Kind != "7d") ||
			(ranking.Kind != "7d" && window.Length > ranking.Length) {
			ranking = window
		}
	}
	if ranking != nil && ranking.ResetsAt.After(now) {
		rank.reset = ranking.ResetsAt
	}
	return rank, exhausted
}
