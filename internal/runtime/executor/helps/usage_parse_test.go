package helps

import (
	"os"
	"testing"
	"time"
)

func TestParseClaudeSubscriptionUsageRealFixture(t *testing.T) {
	body, err := os.ReadFile("testdata/claude_usage_cedar_ember.json")
	if err != nil {
		t.Fatal(err)
	}
	windows, err := ParseClaudeSubscriptionUsage(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 3 {
		t.Fatalf("windows = %+v; want exactly 5h, 7d and fable (not iguana_necktie)", windows)
	}
	for i, want := range []struct {
		kind, scope string
		used        float64
		length      int64
	}{{"5h", "", 6, 18000}, {"7d", "", 47, 604800}, {"7d", "fable", 42, 604800}} {
		got := windows[i]
		if got.Kind != want.kind || got.Scope != want.scope || got.UsedPercent != want.used || got.Length != want.length || got.ResetsAt.IsZero() {
			t.Errorf("window[%d] = %+v, want %+v", i, got, want)
		}
	}
}

func TestParseClaudeSubscriptionUsageScopedAndMissingWindows(t *testing.T) {
	windows, err := ParseClaudeSubscriptionUsage([]byte(`{"five_hour":null,"limits":[
		{"kind":"weekly_scoped","percent":21,"scope":{"model":{"display_name":"FaBlE 1"}}},
		{"kind":"weekly_scoped","percent":90,"scope":{"model":{"display_name":"Sonnet"}}},
		{"kind":"session","percent":80,"scope":{"model":{"display_name":"Fable"}}}
	]}`))
	if err != nil || len(windows) != 1 || windows[0].Scope != "fable" || windows[0].UsedPercent != 21 {
		t.Fatalf("windows = %+v, error = %v", windows, err)
	}
	if _, err = ParseClaudeSubscriptionUsage([]byte(`{"five_hour":{"utilization":"bad"}}`)); err == nil {
		t.Fatal("accepted invalid utilization")
	}
}

func TestParseCodexSubscriptionUsage(t *testing.T) {
	now := time.Unix(1800000000, 0)
	for _, test := range []struct {
		name, body string
		kinds      []string
		reset      time.Time
	}{
		{"durations override slots", `{"rate_limit":{"primary_window":{"used_percent":47,"limit_window_seconds":604800,"reset_at":1800000300},"secondary_window":{"used_percent":6,"limit_window_seconds":18000,"reset_after_seconds":300}}}`, []string{"7d", "5h"}, now.Add(5 * time.Minute)},
		{"legacy slots", `{"rate_limit":{"primary_window":{"used_percent":5,"reset_after_seconds":0},"secondary_window":{"used_percent":40,"reset_at":1800000000}}}`, []string{"5h", "7d"}, now},
		{"monthly", `{"rate_limit":{"primary_window":{"used_percent":99,"limit_window_seconds":2592000,"reset_after_seconds":300}}}`, []string{"long"}, now.Add(5 * time.Minute)},
		{"weekly only", `{"rate_limit":{"primary_window":{"used_percent":50,"limit_window_seconds":604800,"reset_at":1800000300},"secondary_window":null}}`, []string{"7d"}, now.Add(5 * time.Minute)},
		{"no data", `{"rate_limit":null}`, nil, time.Time{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			windows, err := ParseCodexSubscriptionUsage([]byte(test.body), now)
			if err != nil || len(windows) != len(test.kinds) {
				t.Fatalf("windows = %+v, error = %v", windows, err)
			}
			for i, kind := range test.kinds {
				if windows[i].Kind != kind || !windows[i].ResetsAt.Equal(test.reset) || windows[i].Length <= 0 {
					t.Errorf("window[%d] = %+v, want %s reset %v", i, windows[i], kind, test.reset)
				}
			}
		})
	}
	if _, err := ParseCodexSubscriptionUsage([]byte(`not json`), now); err == nil {
		t.Fatal("accepted invalid JSON")
	}
}
