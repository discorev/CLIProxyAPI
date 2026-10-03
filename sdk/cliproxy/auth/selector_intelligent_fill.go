package auth

import (
	"context"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

// IntelligentFillSelector fills the subscription whose weekly reset is soonest.
// It deliberately uses the custom-selector path, not the built-in scheduler.
type IntelligentFillSelector struct {
	manager *Manager

	// lastPick maps provider + canonical model to the last chosen auth ID so
	// routing changes are logged once, not per request.
	mu       sync.Mutex
	lastPick map[string]string
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
		s.notePick(provider, model, best, bestRank, false, available, now)
		return best, nil
	}
	// Cached exhaustion is only advisory; upstream decides when all are gated.
	s.notePick(provider, model, fallback, fallbackRank, true, available, now)
	return fallback, nil
}

// notePick logs when the chosen credential for a provider/model changes. The
// unchanged path is one map lookup under a mutex; details are built only on
// change.
func (s *IntelligentFillSelector) notePick(provider, model string, chosen *Auth, chosenRank usageRank, fallback bool, available []*Auth, now time.Time) {
	if chosen == nil {
		return
	}
	modelKey := canonicalModelKey(model)
	key := provider + "\x00" + modelKey
	s.mu.Lock()
	previous, seen := s.lastPick[key]
	if seen && previous == chosen.ID {
		s.mu.Unlock()
		return
	}
	if s.lastPick == nil {
		s.lastPick = make(map[string]string)
	}
	s.lastPick[key] = chosen.ID
	s.mu.Unlock()

	weeklyReset := "unknown"
	if !chosenRank.reset.IsZero() {
		weeklyReset = chosenRank.reset.UTC().Format(time.RFC3339)
	}
	fields := log.Fields{
		"provider": provider, "model": modelKey, "auth_id": chosen.ID,
		"previous_auth_id": previous, "weekly_reset": weeklyReset,
	}
	if skipped := s.skippedCandidates(model, chosen, chosenRank, available, now); skipped != "" {
		fields["skipped"] = skipped
	}
	if fallback {
		fields["fallback"] = true
	}
	log.WithFields(fields).Info("intelligent-fill routing changed")
}

// skippedCandidates explains candidates ranked ahead of the chosen one that
// were gated, plus candidates passed over for lacking usage data.
func (s *IntelligentFillSelector) skippedCandidates(model string, chosen *Auth, chosenRank usageRank, available []*Auth, now time.Time) string {
	var skipped []string
	for _, auth := range available {
		if auth.ID == chosen.ID {
			continue
		}
		var entry *usageEntry
		requestModel := model
		if s.manager != nil {
			entry = s.manager.usageSnapshot(auth.ID)
			requestModel = s.manager.selectionModelForAuth(auth, model)
		}
		if entry == nil || len(entry.Windows) == 0 {
			skipped = append(skipped, auth.ID+": no usage data")
			continue
		}
		if rank, _ := rankUsage(entry, requestModel, now); !rank.less(chosenRank, auth.ID, chosen.ID) {
			continue
		}
		var gates []string
		for _, window := range entry.Windows {
			if !usageWindowApplies(window, requestModel) || !usageWindowGates(window, now) {
				continue
			}
			label := window.Kind
			if window.Scope != "" {
				label = window.Scope
			}
			gate := label + " exhausted"
			if !window.ResetsAt.IsZero() {
				gate += " until " + window.ResetsAt.UTC().Format(time.RFC3339)
			}
			gates = append(gates, gate)
		}
		if len(gates) > 0 {
			skipped = append(skipped, auth.ID+": "+strings.Join(gates, ", "))
		}
	}
	return strings.Join(skipped, "; ")
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
		if !usageWindowApplies(*window, model) {
			continue
		}
		if usageWindowGates(*window, now) {
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

// usageWindowApplies reports whether a window limits requests for model.
// Scoped windows apply only to their model family.
func usageWindowApplies(window UsageWindow, model string) bool {
	return window.Scope == "" || (window.Scope == "fable" && strings.Contains(strings.ToLower(canonicalModelKey(model)), "fable"))
}

// usageWindowGates reports whether a window is exhausted and not rolled over.
func usageWindowGates(window UsageWindow, now time.Time) bool {
	rolledOver := !window.ResetsAt.IsZero() && !window.ResetsAt.After(now)
	return !rolledOver && window.UsedPercent >= 100
}
