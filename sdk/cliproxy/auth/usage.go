package auth

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// UsageFlightMaxAge bounds shared-flight reuse, not the upstream connection.
const UsageFlightMaxAge = 5 * time.Minute

// UsageWindow is a subscription limit. Length is expressed in seconds.
type UsageWindow struct {
	Kind        string    `json:"kind"`
	Scope       string    `json:"scope"`
	UsedPercent float64   `json:"used_percent"`
	ResetsAt    time.Time `json:"resets_at,omitzero"`
	Length      int64     `json:"length"`
}

// CredentialUsage is an in-memory snapshot, never part of persisted Auth state.
type CredentialUsage struct {
	Raw           map[string]json.RawMessage `json:"raw"`
	Resets        *CredentialResets          `json:"resets"`
	FetchedAt     time.Time                  `json:"fetched_at,omitzero"`
	Windows       []UsageWindow              `json:"windows"`
	ObservedAt    time.Time                  `json:"observed_at,omitzero"`
	Refreshing    bool                       `json:"refreshing"`
	LastError     string                     `json:"last_error"`
	NextFetchAt   time.Time                  `json:"next_fetch_at,omitzero"`
	CooldownUntil time.Time                  `json:"cooldown_until,omitzero"`
}

// UsageFetchResult retains ancillary endpoint failures without discarding usage.
type UsageFetchResult struct {
	RateLimit *UsageHTTPError
	Resets    *CredentialResets
	Raw       map[string]json.RawMessage
	Windows   []UsageWindow
	LastError string
}

// UsageFetcher is optional; generation executors need not implement it.
type UsageFetcher interface {
	FetchUsage(context.Context, *Auth) (UsageFetchResult, error)
}

var (
	ErrUsageAuthNotFound = errors.New("usage credential not found")
	ErrUsageNotFetchable = errors.New("credential does not support subscription usage")
)

// UsageFetchable excludes API keys even if they also carry OAuth metadata.
func UsageFetchable(auth *Auth) bool {
	if auth == nil || strings.TrimSpace(auth.Attributes["api_key"]) != "" {
		return false
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	token, _ := auth.Metadata["access_token"].(string)
	return (provider == "claude" || provider == "codex") && strings.TrimSpace(token) != ""
}

type usageEntry struct {
	CredentialUsage
	windowVersions     map[string]uint64
	headerVersion      uint64
	retryAt            time.Time
	fetchStartedAt     time.Time
	resetRetryExpiry   time.Time
	lastFetchStartedAt time.Time
	rawFetchedAt       map[string]time.Time
	rateLimitLevel     int
	waitForToken       bool
	failedTokenHash    [32]byte
}

type usageFlight struct {
	trigger   string
	startedAt time.Time
	done      chan struct{}
	snapshot  CredentialUsage
	err       error
}

type usageTicker interface {
	Ticks() <-chan time.Time
	Stop()
}

type realUsageTicker struct{ *time.Ticker }

func (t realUsageTicker) Ticks() <-chan time.Time { return t.C }

type usageCache struct {
	resetCancel    context.CancelFunc
	resetDryRun    bool
	resetDecisions map[resetDecisionKey]time.Time
	resets         map[string]*resetAttempt
	mu             sync.RWMutex
	entries        map[string]*usageEntry
	flights        map[string]*usageFlight
	cancel         context.CancelFunc
	now            func() time.Time
	newTicker      func() usageTicker
}

func (u *usageCache) timeNow() time.Time {
	if u.now != nil {
		return u.now()
	}
	return time.Now()
}

func emptyCredentialUsage() CredentialUsage {
	return CredentialUsage{Raw: make(map[string]json.RawMessage), Windows: []UsageWindow{}}
}

func cloneCredentialUsage(s CredentialUsage) CredentialUsage {
	s.Raw = maps.Clone(s.Raw)
	if s.Raw == nil {
		s.Raw = make(map[string]json.RawMessage)
	}
	for name, raw := range s.Raw {
		s.Raw[name] = append(json.RawMessage(nil), raw...)
	}
	s.Windows = append([]UsageWindow{}, s.Windows...)
	s.Resets = cloneCredentialResets(s.Resets)
	return s
}

func cloneUsageEntry(s *usageEntry) *usageEntry {
	if s == nil {
		return &usageEntry{CredentialUsage: emptyCredentialUsage(), windowVersions: make(map[string]uint64)}
	}
	next := *s
	// Raw bodies are immutable internally; only public snapshots deep-copy them.
	next.Windows = append([]UsageWindow{}, s.Windows...)
	next.windowVersions = maps.Clone(s.windowVersions)
	next.rawFetchedAt = maps.Clone(s.rawFetchedAt)
	return &next
}

func windowKey(w UsageWindow) string { return w.Kind + ":" + w.Scope }

// usageSnapshot is read-only. Writers always replace the whole entry.
func (m *Manager) usageSnapshot(id string) *usageEntry {
	m.usage.mu.RLock()
	entry := m.usage.entries[id]
	m.usage.mu.RUnlock()
	return entry
}

// UsageSnapshot returns an independent copy for callers outside the selector.
func (m *Manager) UsageSnapshot(id string) CredentialUsage {
	if m != nil {
		if entry := m.usageSnapshot(id); entry != nil {
			return cloneCredentialUsage(entry.CredentialUsage)
		}
	}
	return emptyCredentialUsage()
}

// RefreshUsage shares one fetch per credential, independent of caller cancellation.
// Each caller, including the one starting the fetch, can cancel its own wait.
func (m *Manager) RefreshUsage(ctx context.Context, authID string) (CredentialUsage, error) {
	return m.refreshUsage(ctx, authID, "manual")
}

func (m *Manager) refreshUsage(ctx context.Context, authID, trigger string) (CredentialUsage, error) {
	return m.refreshUsageForReset(ctx, authID, trigger, nil)
}

// A reset reservation pins its refresh to the same credential generation.
func (m *Manager) refreshUsageForReset(ctx context.Context, authID, trigger string, state *resetAttempt) (CredentialUsage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	flight, auth, fetcher, leader, err := m.beginUsageRefreshForReset(authID, trigger, state)
	if err != nil {
		return emptyCredentialUsage(), err
	}
	if flight == nil {
		return m.UsageSnapshot(authID), nil
	}
	if leader {
		go m.fetchUsage(context.WithoutCancel(ctx), auth, fetcher, flight)
	}
	select {
	case <-flight.done:
		return cloneCredentialUsage(flight.snapshot), flight.err
	case <-ctx.Done():
		return m.UsageSnapshot(authID), ctx.Err()
	}
}

func (m *Manager) beginUsageRefresh(id, trigger string) (*usageFlight, *Auth, UsageFetcher, bool, error) {
	return m.beginUsageRefreshForReset(id, trigger, nil)
}

func (m *Manager) beginUsageRefreshForReset(id, trigger string, state *resetAttempt) (*usageFlight, *Auth, UsageFetcher, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	auth := m.auths[id]
	if auth == nil {
		return nil, nil, nil, false, ErrUsageAuthNotFound
	}
	if !UsageFetchable(auth) {
		return nil, nil, nil, false, ErrUsageNotFetchable
	}
	fetcher, ok := m.executors[executorKeyFromAuth(auth)].(UsageFetcher)
	if !ok {
		return nil, nil, nil, false, ErrUsageNotFetchable
	}
	m.usage.mu.Lock()
	defer m.usage.mu.Unlock()
	if state != nil && m.usage.resets[id] != state {
		return nil, nil, nil, false, ErrResetUnavailable
	}
	now := m.usage.timeNow()
	previous := m.usage.flights[id]
	if previous != nil && now.Before(previous.startedAt.Add(UsageFlightMaxAge)) {
		return previous, nil, nil, false, nil
	}
	entry := m.usage.entries[id]
	if usageFetchBlocked(entry, auth, now) {
		return nil, nil, nil, false, nil
	}
	if trigger == "sweep" {
		trigger = usageRefreshTrigger(entry, now)
		if auth.Disabled || auth.Status == StatusDisabled || trigger == "" {
			return nil, nil, nil, false, nil
		}
	}
	if m.usage.entries == nil {
		m.usage.entries = make(map[string]*usageEntry)
		m.usage.flights = make(map[string]*usageFlight)
	}
	flight := &usageFlight{done: make(chan struct{}), startedAt: now, trigger: trigger}
	// Replacing the pointer detaches the old flight. Its connection is untouched,
	// but its eventual completion can no longer publish usage or retry state.
	m.usage.flights[id] = flight
	if previous != nil {
		log.WithFields(log.Fields{
			"auth_id": id, "provider": auth.Provider,
			"started_at": previous.startedAt.UTC().Format(time.RFC3339),
		}).Info("usage fetch abandoned")
	}
	next := cloneUsageEntry(entry)
	next.Refreshing = true
	next.lastFetchStartedAt = flight.startedAt
	next.NextFetchAt = flight.startedAt.Add(UsageMinFetchInterval)
	m.usage.entries[id] = next
	return flight, auth.Clone(), fetcher, true, nil
}

func (m *Manager) fetchUsage(ctx context.Context, auth *Auth, fetcher UsageFetcher, flight *usageFlight) {
	var headerVersion uint64
	if entry := m.usageSnapshot(auth.ID); entry != nil {
		headerVersion = entry.headerVersion
		ctx = context.WithValue(ctx, usageFetchOptionsKey{}, usageFetchOptions(entry, flight.startedAt))
	}
	if rt := m.roundTripperFor(auth); rt != nil {
		ctx = context.WithValue(ctx, roundTripperContextKey{}, rt)
		ctx = context.WithValue(ctx, "cliproxy.roundtripper", rt)
	}
	result, errFetch := fetcher.FetchUsage(ctx, auth)
	finished := m.usage.timeNow()
	canceled := errFetch != nil && (ctx.Err() != nil || errors.Is(errFetch, context.Canceled))
	m.usage.mu.Lock()
	next := cloneUsageEntry(m.usage.entries[auth.ID])
	next.Refreshing = false
	retryDelay := usageFailureRetryDelay(next, finished)
	switch {
	case canceled:
		// Cancellation is not an upstream failure; retain existing error/retry state.
	case errFetch != nil:
		next.LastError = errFetch.Error()
		next.Resets = nil
	default:
		next.Raw = maps.Clone(next.Raw)
		if next.Raw == nil {
			next.Raw = make(map[string]json.RawMessage)
		}
		if next.rawFetchedAt == nil {
			next.rawFetchedAt = make(map[string]time.Time)
		}
		for name, raw := range result.Raw {
			next.rawFetchedAt[name] = finished
			next.Raw[name] = append(json.RawMessage(nil), raw...)
		}
		next.FetchedAt = finished
		next.fetchStartedAt = flight.startedAt
		// Only successful inventory parsing establishes spendable resets;
		// unrelated ancillary errors remain display-only for reset eligibility.
		next.Resets = cloneCredentialResets(result.Resets)
		next.LastError = result.LastError
		windows := append([]UsageWindow{}, result.Windows...)
		versions := make(map[string]uint64, len(windows))
		// A response observed while these HTTP calls were in flight is newer
		// than the fetched view, including windows absent from that view.
		for _, w := range next.Windows {
			key := windowKey(w)
			if version := next.windowVersions[key]; version > headerVersion {
				windows = mergeUsageWindow(windows, w)
				versions[key] = version
			}
		}
		next.Windows, next.windowVersions = windows, versions
	}
	if !canceled {
		applyUsageFetchRate(next, auth, result, errFetch, finished, retryDelay)
	}
	// Removal invalidates a flight. A late response cannot resurrect the entry
	// or overwrite a new credential registered under the same ID.
	stored := m.usage.flights[auth.ID] == flight
	if stored {
		m.usage.entries[auth.ID] = next
		delete(m.usage.flights, auth.ID)
		if !canceled {
			// Log before publishing completion or permitting invalidation so the
			// lifecycle lines follow the order of cache mutations.
			logUsageFetch(auth, flight.trigger, next, errFetch)
		}
	}
	flight.snapshot = next.CredentialUsage
	flight.err = errFetch
	close(flight.done)
	m.usage.mu.Unlock()
}

func mergeUsageWindow(windows []UsageWindow, window UsageWindow) []UsageWindow {
	for i, existing := range windows {
		if windowKey(existing) == windowKey(window) {
			windows[i] = window
			return windows
		}
	}
	return append(windows, window)
}
