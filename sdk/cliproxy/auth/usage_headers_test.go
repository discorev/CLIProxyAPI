package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
)

func TestUsageWindowsFromClaudeHeaders(t *testing.T) {
	now := time.Unix(1800000000, 0)
	headers := http.Header{
		"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.06"},
		"Anthropic-Ratelimit-Unified-5h-Reset":       {"1800000300"},
		"Anthropic-Ratelimit-Unified-7d-Utilization": {"0.47"},
		"Anthropic-Ratelimit-Unified-7d-Status":      {"rejected"},
		"Anthropic-Ratelimit-Unified-7d_oi-Status":   {"rejected"},
		"Anthropic-Ratelimit-Unified-7d_oi-Reset":    {"1800000400"},
	}
	windows, _ := usageWindowsFromHeaders("claude", headers, nil, now)
	if len(windows) != 3 || windows[0].UsedPercent != 6 || windows[1].UsedPercent != 100 || windows[2].Scope != "fable" || windows[2].UsedPercent != 100 || !windows[2].ResetsAt.Equal(now.Add(400*time.Second)) {
		t.Fatalf("windows = %+v", windows)
	}
	allowed, _ := usageWindowsFromHeaders("claude", http.Header{"Anthropic-Ratelimit-Unified-7d_oi-Status": {"allowed"}}, windows, now)
	if len(allowed) != 1 || allowed[0].UsedPercent != 0 || !allowed[0].ResetsAt.Equal(windows[2].ResetsAt) {
		t.Fatalf("allowed status failed to clear exhaustion: %+v", allowed)
	}
	invalid, _ := usageWindowsFromHeaders("claude", http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": {"NaN"}}, nil, now)
	if len(invalid) != 0 {
		t.Fatalf("accepted NaN: %+v", invalid)
	}
}

func TestUsageWindowsFromCodexHeaders(t *testing.T) {
	now := time.Unix(1800000000, 0)
	headers := http.Header{
		"X-Codex-Primary-Used-Percent":           {"47"},
		"X-Codex-Primary-Window-Minutes":         {"10080"},
		"X-Codex-Primary-Reset-At":               {"1800000300"},
		"X-Codex-Primary-Reset-After-Seconds":    {"600"},
		"X-Codex-Secondary-Used-Percent":         {"6"},
		"X-Codex-Secondary-Window-Minutes":       {"300"},
		"X-Codex-Secondary-Reset-After-Seconds":  {"300"},
		"X-Codex-Bengalfox-Primary-Used-Percent": {"100"},
	}
	windows, _ := usageWindowsFromHeaders("codex", headers, nil, now)
	if len(windows) != 2 || windows[0].Kind != "7d" || windows[0].UsedPercent != 47 || windows[1].Kind != "5h" || windows[1].UsedPercent != 6 {
		t.Fatalf("windows = %+v", windows)
	}
	for _, window := range windows {
		if !window.ResetsAt.Equal(now.Add(5 * time.Minute)) {
			t.Fatalf("wrong reset: %+v", window)
		}
	}
	legacy, _ := usageWindowsFromHeaders("codex", http.Header{
		"X-Codex-Primary-Used-Percent": {"5"}, "X-Codex-Secondary-Used-Percent": {"40"},
	}, nil, now)
	if len(legacy) != 2 || legacy[0].Kind != "5h" || legacy[1].Kind != "7d" {
		t.Fatalf("legacy windows = %+v", legacy)
	}
}

func TestUsageWindowsFromCodexHeadersIgnoresEmptySlot(t *testing.T) {
	now := time.Unix(1791070406, 0)
	headers := http.Header{
		"X-Codex-Primary-Used-Percent":          {"9"},
		"X-Codex-Primary-Window-Minutes":        {"10080"},
		"X-Codex-Primary-Reset-At":              {"1791590904"},
		"X-Codex-Primary-Reset-After-Seconds":   {"520498"},
		"X-Codex-Secondary-Used-Percent":        {"0"},
		"X-Codex-Secondary-Window-Minutes":      {"0"},
		"X-Codex-Secondary-Reset-At":            {""},
		"X-Codex-Secondary-Reset-After-Seconds": {"0"},
	}
	previous := []UsageWindow{{Kind: "7d", Length: 604800, UsedPercent: 8, ResetsAt: time.Unix(1791590904, 0)}}
	for _, prev := range [][]UsageWindow{nil, previous} {
		windows, _ := usageWindowsFromHeaders("codex", headers, prev, now)
		if len(windows) != 1 || windows[0].Kind != "7d" || windows[0].UsedPercent != 9 || !windows[0].ResetsAt.Equal(time.Unix(1791590904, 0)) {
			t.Fatalf("windows = %+v", windows)
		}
	}
}

func TestMarkResultMergesUsageWindowsWithoutChangingRaw(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth := registerUsageAuth(t, manager, "claude", "claude")
	manager.RegisterExecutor(&fakeUsageExecutor{provider: "claude", fetch: func(context.Context, *Auth) (UsageFetchResult, error) {
		return UsageFetchResult{
			Raw:     map[string]json.RawMessage{"usage": json.RawMessage(`{"five_hour":{"utilization":1}}`), "profile": json.RawMessage(`{}`)},
			Windows: []UsageWindow{{Kind: "5h", UsedPercent: 1}, {Kind: "7d", UsedPercent: 47}, {Kind: "7d", Scope: "fable", UsedPercent: 42}},
		}, nil
	}})
	if _, err := manager.RefreshUsage(context.Background(), auth.ID); err != nil {
		t.Fatal(err)
	}
	before := manager.usageSnapshot(auth.ID)
	ctx := internallogging.WithResponseHeadersHolder(context.Background())
	internallogging.SetResponseHeaders(ctx, http.Header{"anthropic-ratelimit-unified-5h-utilization": {"0.06"}})
	manager.MarkResult(ctx, Result{AuthID: auth.ID, Provider: "claude", Success: true})
	after := manager.UsageSnapshot(auth.ID)
	if len(after.Windows) != 3 || after.Windows[0].UsedPercent != 6 || after.Windows[1].UsedPercent != 47 || after.Windows[2].UsedPercent != 42 || len(after.Raw) != 2 || !after.FetchedAt.Equal(before.FetchedAt) {
		t.Fatalf("merge lost fetched data: %+v", after)
	}
	if before.Windows[0].UsedPercent != 1 {
		t.Fatal("header merge mutated published snapshot")
	}
	internallogging.SetResponseHeaders(ctx, http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.99"}})
	manager.MarkResult(ctx, Result{AuthID: auth.ID, Provider: "claude", Success: true, SkipQuotaObservation: true})
	if got := manager.UsageSnapshot(auth.ID); got.Windows[0].UsedPercent != 6 {
		t.Fatal("count tokens request changed usage")
	}
}

func TestObserveCodexHeadersDropsWindowOfEmptySlot(t *testing.T) {
	now := time.Unix(1791070406, 0)
	weekly := time.Unix(1791590904, 0)
	observed := http.Header{
		"X-Codex-Primary-Used-Percent":          {"9"},
		"X-Codex-Primary-Window-Minutes":        {"10080"},
		"X-Codex-Primary-Reset-At":              {"1791590904"},
		"X-Codex-Primary-Reset-After-Seconds":   {"520498"},
		"X-Codex-Secondary-Used-Percent":        {"0"},
		"X-Codex-Secondary-Window-Minutes":      {"0"},
		"X-Codex-Secondary-Reset-At":            {""},
		"X-Codex-Secondary-Reset-After-Seconds": {"0"},
	}
	cached := []UsageWindow{
		{Kind: "7d", Length: 604800, UsedPercent: 8, ResetsAt: weekly},
		{Kind: "5h", Length: 18000, UsedPercent: 100, ResetsAt: now.Add(time.Hour)},
		{Kind: "7d", Scope: "fable", Length: 604800, UsedPercent: 100, ResetsAt: weekly},
	}
	for _, tt := range []struct {
		name    string
		headers http.Header
		want    []UsageWindow
	}{
		{"empty secondary", observed, []UsageWindow{
			{Kind: "7d", Length: 604800, UsedPercent: 9, ResetsAt: weekly},
			{Kind: "7d", Scope: "fable", Length: 604800, UsedPercent: 100, ResetsAt: weekly},
		}},
		// Without window-minutes the headers are not known to be complete.
		{"older shape", http.Header{"X-Codex-Primary-Used-Percent": {"40"}}, []UsageWindow{
			{Kind: "7d", Length: 604800, UsedPercent: 8, ResetsAt: weekly},
			{Kind: "5h", Length: 18000, UsedPercent: 40, ResetsAt: now.Add(time.Hour)},
			{Kind: "7d", Scope: "fable", Length: 604800, UsedPercent: 100, ResetsAt: weekly},
		}},
		{"older shape both slots", http.Header{"X-Codex-Primary-Used-Percent": {"40"}, "X-Codex-Secondary-Used-Percent": {"10"}}, []UsageWindow{
			{Kind: "7d", Length: 604800, UsedPercent: 10, ResetsAt: weekly},
			{Kind: "5h", Length: 18000, UsedPercent: 40, ResetsAt: now.Add(time.Hour)},
			{Kind: "7d", Scope: "fable", Length: 604800, UsedPercent: 100, ResetsAt: weekly},
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			auth := registerUsageAuth(t, manager, "codex", "codex")
			manager.usage.mu.Lock()
			manager.usage.entries = map[string]*usageEntry{"codex": {
				CredentialUsage: CredentialUsage{Windows: append([]UsageWindow{}, cached...)},
				windowVersions:  map[string]uint64{"5h:": 0, "7d:": 0, "7d:fable": 0},
			}}
			manager.usage.mu.Unlock()
			manager.mu.Lock()
			manager.observeUsageHeadersLocked(auth, tt.headers, now)
			manager.mu.Unlock()
			got := manager.usageSnapshot("codex")
			if len(got.Windows) != len(tt.want) {
				t.Fatalf("windows = %+v, want %+v", got.Windows, tt.want)
			}
			for i, want := range tt.want {
				w := got.Windows[i]
				if w.Kind != want.Kind || w.Scope != want.Scope || w.Length != want.Length || w.UsedPercent != want.UsedPercent || !w.ResetsAt.Equal(want.ResetsAt) {
					t.Fatalf("window %d = %+v, want %+v", i, w, want)
				}
			}
			if _, kept := got.windowVersions["5h:"]; kept != (len(tt.want) == 3) {
				t.Fatalf("5h window version kept=%t", kept)
			}
		})
	}
}
