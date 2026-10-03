package auth

import (
	"context"
	"time"
)

func usageRefreshDue(entry *usageEntry, now time.Time) bool {
	if entry == nil {
		return true
	}
	if entry.Refreshing || now.Before(entry.retryAt) {
		return false
	}
	// An elapsed retry is due even if traffic keeps header observations fresh.
	if entry.FetchedAt.IsZero() || !entry.retryAt.IsZero() {
		return true
	}
	for _, window := range entry.Windows {
		if !window.ResetsAt.IsZero() && !window.ResetsAt.After(now) && window.ResetsAt.After(entry.FetchedAt) {
			return true
		}
	}
	freshest := entry.FetchedAt
	if entry.ObservedAt.After(freshest) {
		freshest = entry.ObservedAt
	}
	return !now.Before(freshest.Add(15 * time.Minute))
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
		flight, current, fetcher, leader, err := m.beginUsageRefresh(auth.ID, true)
		if err == nil && leader {
			// Each credential has its own flight: a hung upstream cannot stall
			// the sweep or repeatedly spawn workers for that credential.
			go m.fetchUsage(context.WithoutCancel(ctx), current, fetcher, flight)
		}
	}
}
