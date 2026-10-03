package auth

import (
	"context"
	"time"

	log "github.com/sirupsen/logrus"
)

// usageIdleRefreshInterval re-fetches raw usage bodies even while proxied
// traffic keeps header-derived windows fresh.
const usageIdleRefreshInterval = 15 * time.Minute

func usageRefreshInFlight(entry *usageEntry, now time.Time) bool {
	return entry != nil && entry.Refreshing &&
		(entry.lastFetchStartedAt.IsZero() || now.Before(entry.lastFetchStartedAt.Add(UsageFlightMaxAge)))
}

func usageRefreshDue(entry *usageEntry, now time.Time) bool {
	return usageRefreshTrigger(entry, now) != ""
}

// usageRefreshTrigger is shared by scheduling and logging, so the reported
// trigger describes the condition that actually made a sweep fetch due.
func usageRefreshTrigger(entry *usageEntry, now time.Time) string {
	if entry == nil {
		return "initial"
	}
	if usageRefreshInFlight(entry, now) || now.Before(entry.NextFetchAt) || now.Before(entry.retryAt) {
		return ""
	}
	if entry.Refreshing {
		return "retry" // A stale flight must not suppress another sweep fetch.
	}
	// An elapsed retry is due even if traffic keeps header observations fresh.
	if entry.waitForToken || !entry.retryAt.IsZero() {
		return "retry"
	}
	if entry.FetchedAt.IsZero() {
		return "initial"
	}
	for _, window := range entry.Windows {
		if !window.ResetsAt.IsZero() && !window.ResetsAt.After(now) && window.ResetsAt.After(entry.FetchedAt) {
			return "reset_passed"
		}
	}
	// Header observations refresh windows but not the raw bodies (plan,
	// credits, grants), so only a successful fetch defers the idle refresh.
	if !now.Before(entry.FetchedAt.Add(usageIdleRefreshInterval)) {
		return "idle"
	}
	return ""
}

// StartUsageSweep starts the routing-only refresh loop. It is idempotent and
// uses a private lifetime, independent of short-lived config-update contexts.
func (m *Manager) StartUsageSweep() {
	m.usage.mu.Lock()
	defer m.usage.mu.Unlock()
	if m.usage.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.usage.cancel = cancel
	var ticker usageTicker
	if m.usage.newTicker != nil {
		ticker = m.usage.newTicker()
	} else {
		ticker = realUsageTicker{time.NewTicker(time.Minute)}
	}
	log.Info("usage sweep started")
	go func() {
		defer ticker.Stop()
		m.sweepUsage(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.Ticks():
				m.sweepUsage(ctx)
			}
		}
	}()
}

// StopUsageSweep stops scheduling refreshes without canceling shared fetches.
func (m *Manager) StopUsageSweep() {
	if m == nil {
		return
	}
	m.usage.mu.Lock()
	defer m.usage.mu.Unlock()
	if m.usage.cancel != nil {
		m.usage.cancel()
		m.usage.cancel = nil
		log.WithField("reason", "stop_requested").Info("usage sweep stopped")
	}
}

// UsageSweepRunning reports whether routing refresh is enabled.
func (m *Manager) UsageSweepRunning() bool {
	m.usage.mu.RLock()
	defer m.usage.mu.RUnlock()
	return m.usage.cancel != nil
}

func (m *Manager) sweepUsage(ctx context.Context) {
	for _, auth := range m.List() {
		if ctx.Err() != nil {
			return
		}
		if !UsageFetchable(auth) || auth.Disabled || auth.Status == StatusDisabled {
			continue
		}
		flight, current, fetcher, leader, err := m.beginUsageRefresh(auth.ID, "sweep")
		if err == nil && leader {
			// Each credential has its own flight: a hung upstream cannot stall
			// the sweep or repeatedly spawn workers for that credential.
			go m.fetchUsage(context.WithoutCancel(ctx), current, fetcher, flight)
		}
	}
}
