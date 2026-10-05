package diff

import (
	"slices"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestResetCreditsConfigDiff(t *testing.T) {
	before := &config.Config{}
	after := &config.Config{ResetCredits: config.ResetCreditsConfig{AutoApply: true, DryRun: true}}
	for _, field := range []string{"auto-apply", "dry-run"} {
		for _, tt := range []struct {
			old, current *config.Config
			want         string
		}{
			{before, after, "false -> true"},
			{after, before, "true -> false"},
		} {
			changes := BuildConfigChangeDetails(tt.old, tt.current)
			if !slices.Contains(changes, "reset-credits."+field+": "+tt.want) {
				t.Fatalf("missing diff: %v", changes)
			}
		}
		if changes := BuildConfigChangeDetails(after, after); slices.Contains(changes, "reset-credits."+field+": true -> true") {
			t.Fatalf("spurious diff: %v", changes)
		}
	}
	limited := &config.Config{ResetCredits: config.ResetCreditsConfig{Providers: []string{"codex", "claude"}}}
	if changes := BuildConfigChangeDetails(before, limited); !slices.Contains(changes, "reset-credits.providers: [] -> [codex, claude]") {
		t.Fatalf("missing providers diff: %v", changes)
	}
	if changes := BuildConfigChangeDetails(limited, limited); len(changes) != 0 {
		t.Fatalf("spurious providers diff: %v", changes)
	}
}
