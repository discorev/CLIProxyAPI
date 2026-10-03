package diff

import (
	"slices"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestResetCreditsConfigDiff(t *testing.T) {
	before := &config.Config{}
	after := &config.Config{ResetCredits: config.ResetCreditsConfig{AutoApply: true}}
	changes := BuildConfigChangeDetails(before, after)
	if !slices.Contains(changes, "reset-credits.auto-apply: false -> true") {
		t.Fatalf("missing diff: %v", changes)
	}
	if changes := BuildConfigChangeDetails(after, after); slices.Contains(changes, "reset-credits.auto-apply: true -> true") {
		t.Fatalf("spurious diff: %v", changes)
	}
}
