package auth

import (
	"context"
	"reflect"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

func TestIntelligentFillSelector(t *testing.T) {
	now := time.Unix(1800000000, 0)
	weekly := func(hours int, used float64) UsageWindow {
		return UsageWindow{Kind: "7d", UsedPercent: used, ResetsAt: now.Add(time.Duration(hours) * time.Hour), Length: 604800}
	}
	short := UsageWindow{Kind: "5h", UsedPercent: 100, ResetsAt: now.Add(time.Hour), Length: 18000}
	fable := weekly(1, 100)
	fable.Scope = "fable"
	monthly := UsageWindow{Kind: "long", UsedPercent: 50, ResetsAt: now.Add(2 * time.Hour), Length: 2592000}
	for _, test := range []struct {
		name, model, want string
		a, b              []UsageWindow
		priorityA         string
	}{
		{"rank soonest", "sonnet", "b", []UsageWindow{weekly(5, 1)}, []UsageWindow{weekly(2, 99)}, ""},
		{"5h gate", "sonnet", "b", []UsageWindow{weekly(1, 1), short}, []UsageWindow{weekly(2, 1)}, ""},
		{"weekly gate", "sonnet", "b", []UsageWindow{weekly(1, 100)}, []UsageWindow{weekly(2, 1)}, ""},
		{"fable gate", "claude-fable-5(high)", "b", []UsageWindow{weekly(1, 1), fable}, []UsageWindow{weekly(2, 1)}, ""},
		{"fable irrelevant to sonnet", "claude-sonnet", "a", []UsageWindow{weekly(1, 1), fable}, []UsageWindow{weekly(2, 1)}, ""},
		{"scoped reset not ranked", "fable", "b", []UsageWindow{weekly(5, 1), {Kind: "7d", Scope: "fable", ResetsAt: now.Add(time.Minute)}}, []UsageWindow{weekly(2, 1)}, ""},
		{"rollover opens gate", "fable", "a", []UsageWindow{weekly(0, 100)}, []UsageWindow{weekly(2, 100)}, ""},
		{"rollover reset unknown", "sonnet", "b", []UsageWindow{weekly(0, 100)}, []UsageWindow{weekly(2, 1)}, ""},
		{"unknown last", "sonnet", "b", nil, []UsageWindow{weekly(2, 1)}, ""},
		{"no data fill first", "other", "a", nil, nil, ""},
		{"all gated fallback", "fable", "b", []UsageWindow{weekly(5, 100)}, []UsageWindow{weekly(2, 100)}, ""},
		{"tie by ID", "sonnet", "a", []UsageWindow{weekly(2, 1)}, []UsageWindow{weekly(2, 2)}, ""},
		{"priority before gate", "sonnet", "a", []UsageWindow{weekly(5, 100)}, []UsageWindow{weekly(2, 1)}, "10"},
		{"longest fallback", "gpt", "b", []UsageWindow{monthly}, []UsageWindow{weekly(1, 20)}, ""},
		{"weekly before monthly", "gpt", "b", []UsageWindow{weekly(5, 20), monthly}, []UsageWindow{weekly(3, 20)}, ""},
		{"monthly gate", "gpt", "b", []UsageWindow{{Kind: "long", UsedPercent: 100, ResetsAt: now.Add(time.Hour), Length: 2592000}}, []UsageWindow{weekly(3, 20)}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			manager.usage.now = func() time.Time { return now }
			manager.usage.entries = map[string]*usageEntry{
				"a": {CredentialUsage: CredentialUsage{Windows: test.a}},
				"b": {CredentialUsage: CredentialUsage{Windows: test.b}},
			}
			selector := NewIntelligentFillSelector(manager)
			auths := []*Auth{{ID: "b"}, {ID: "a", Attributes: map[string]string{"priority": test.priorityA}}}
			got, err := selector.Pick(context.Background(), "claude", test.model, cliproxyexecutor.Options{}, auths)
			if err != nil || got == nil || got.ID != test.want {
				t.Fatalf("Pick = %+v, %v; want %s", got, err, test.want)
			}
		})
	}
}

func TestIntelligentFillSelectorAvailabilityAndWebsocket(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	selector := NewIntelligentFillSelector(manager)
	now := time.Now()
	auths := []*Auth{
		{ID: "a", Disabled: true},
		{ID: "b", Unavailable: true, NextRetryAfter: now.Add(time.Hour)},
		{ID: "c"},
		{ID: "d", Attributes: map[string]string{"websockets": "true"}},
	}
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	got, err := selector.Pick(ctx, "codex", "", cliproxyexecutor.Options{}, auths)
	if err != nil || got == nil || got.ID != "d" {
		t.Fatalf("Pick = %+v, %v; want websocket credential d", got, err)
	}
}

func TestIntelligentFillLegacyPickResolvesAliasesWithoutRecheckingCooldowns(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetSelector(NewIntelligentFillSelector(manager))
	if manager.useSchedulerFastPath() {
		t.Fatal("intelligent-fill must use the legacy selection path")
	}
	manager.RegisterExecutor(&fakeUsageExecutor{provider: "claude"})
	manager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{
		"claude": {{Name: "claude-fable", Alias: "friendly"}},
	})
	now := time.Now()
	for _, id := range []string{"if-legacy-a", "if-legacy-b"} {
		auth := registerUsageAuth(t, manager, id, "claude")
		// A stale alias cooldown is unrelated to the resolved model. The
		// manager has already checked that canonical model before Pick.
		auth.ModelStates = map[string]*ModelState{"friendly": {Unavailable: true, NextRetryAfter: now.Add(time.Hour)}}
		if _, err := manager.Update(context.Background(), auth); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: "friendly"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	manager.usage.entries = map[string]*usageEntry{
		"if-legacy-a": {CredentialUsage: CredentialUsage{Windows: []UsageWindow{
			{Kind: "7d", ResetsAt: now.Add(time.Hour)}, {Kind: "7d", Scope: "fable", UsedPercent: 100, ResetsAt: now.Add(time.Hour)},
		}}},
		"if-legacy-b": {CredentialUsage: CredentialUsage{Windows: []UsageWindow{{Kind: "7d", ResetsAt: now.Add(2 * time.Hour)}}}},
	}
	got, _, err := manager.pickNext(context.Background(), "claude", "friendly", cliproxyexecutor.Options{}, nil)
	if err != nil || got == nil || got.ID != "if-legacy-b" {
		t.Fatalf("legacy Pick = %+v, %v; want if-legacy-b", got, err)
	}
}

func TestIntelligentFillSelectorLogsRoutingChanges(t *testing.T) {
	hook := setupTestLoggerHook(t)
	log.SetLevel(log.InfoLevel)
	now := time.Unix(1800000000, 0).UTC()
	at := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }
	manager := NewManager(nil, nil, nil)
	manager.usage.now = func() time.Time { return now }
	weeklyA := UsageWindow{Kind: "7d", UsedPercent: 10, ResetsAt: now.Add(time.Hour), Length: 604800}
	weeklyB := UsageWindow{Kind: "7d", UsedPercent: 10, ResetsAt: now.Add(2 * time.Hour), Length: 604800}
	setWindows := func(id string, windows ...UsageWindow) {
		manager.usage.mu.Lock()
		manager.usage.entries[id] = &usageEntry{CredentialUsage: CredentialUsage{Windows: windows}}
		manager.usage.mu.Unlock()
	}
	manager.usage.entries = map[string]*usageEntry{}
	setWindows("a", weeklyA)
	setWindows("b", weeklyB)
	selector := NewIntelligentFillSelector(manager)
	auths := []*Auth{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	routingLogs := func() []*log.Entry {
		var entries []*log.Entry
		for _, entry := range hook.AllEntries() {
			if entry.Message == "intelligent-fill routing changed" {
				entries = append(entries, entry)
			}
		}
		hook.Reset()
		return entries
	}
	pick := func(model, want string, wantLog log.Fields) {
		t.Helper()
		got, err := selector.Pick(context.Background(), "claude", model, cliproxyexecutor.Options{}, auths)
		if err != nil || got == nil || got.ID != want {
			t.Fatalf("Pick = %+v, %v; want %s", got, err, want)
		}
		logs := routingLogs()
		if wantLog == nil {
			if len(logs) != 0 {
				t.Fatalf("unexpected routing log: %+v", logs[0].Data)
			}
			return
		}
		if len(logs) != 1 || logs[0].Level != log.InfoLevel || !reflect.DeepEqual(logs[0].Data, wantLog) {
			var data []log.Fields
			for _, entry := range logs {
				data = append(data, entry.Data)
			}
			t.Fatalf("routing logs = %+v, want %v", data, wantLog)
		}
	}

	// First pick after start logs; c has never been fetched.
	pick("claude-sonnet-4(high)", "a", log.Fields{
		"provider": "claude", "model": "claude-sonnet-4", "auth_id": "a", "previous_auth_id": "",
		"weekly_reset": at(time.Hour), "skipped": "c: no usage data",
	})
	// Unchanged picks never log, including for the same canonical model.
	pick("claude-sonnet-4", "a", nil)
	pick("claude-sonnet-4(low)", "a", nil)

	setWindows("c", weeklyB)
	setWindows("a", weeklyA, UsageWindow{Kind: "5h", UsedPercent: 100, ResetsAt: now.Add(30 * time.Minute), Length: 18000})
	pick("claude-sonnet-4", "b", log.Fields{
		"provider": "claude", "model": "claude-sonnet-4", "auth_id": "b", "previous_auth_id": "a",
		"weekly_reset": at(2 * time.Hour), "skipped": "a: 5h exhausted until " + at(30*time.Minute),
	})

	// Each model key is tracked independently; the fable scope gates only Fable.
	setWindows("a", weeklyA)
	setWindows("b", weeklyB, UsageWindow{Kind: "7d", Scope: "fable", UsedPercent: 100, ResetsAt: now.Add(2 * time.Hour), Length: 604800})
	setWindows("c", UsageWindow{Kind: "7d", UsedPercent: 10, ResetsAt: now.Add(3 * time.Hour), Length: 604800})
	setWindows("a", weeklyA, UsageWindow{Kind: "7d", Scope: "fable", UsedPercent: 100, ResetsAt: now.Add(time.Hour), Length: 604800})
	pick("claude-fable-5", "c", log.Fields{
		"provider": "claude", "model": "claude-fable-5", "auth_id": "c", "previous_auth_id": "",
		"weekly_reset": at(3 * time.Hour),
		"skipped":      "a: fable exhausted until " + at(time.Hour) + "; b: fable exhausted until " + at(2*time.Hour),
	})
	pick("claude-sonnet-4", "a", log.Fields{
		"provider": "claude", "model": "claude-sonnet-4", "auth_id": "a", "previous_auth_id": "b",
		"weekly_reset": at(time.Hour),
	})

	// All gated: the soonest reset is used as an advisory fallback.
	setWindows("c", UsageWindow{Kind: "7d", Scope: "fable", UsedPercent: 100, ResetsAt: now.Add(3 * time.Hour), Length: 604800}, UsageWindow{Kind: "7d", UsedPercent: 10, ResetsAt: now.Add(3 * time.Hour), Length: 604800})
	pick("claude-fable-5", "a", log.Fields{
		"provider": "claude", "model": "claude-fable-5", "auth_id": "a", "previous_auth_id": "c",
		"weekly_reset": at(time.Hour), "fallback": true,
	})
	pick("claude-fable-5", "a", nil)
}
