package cliproxy

import (
	"context"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestResetLoopConfigHotReload(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	defer manager.StopResetLoop()
	defer manager.StopUsageSweep()
	service := &Service{coreManager: manager}
	for _, tt := range []struct {
		strategy                    string
		enabled, dryRun, home, want bool
	}{
		{"round-robin", false, false, false, false},
		{"round-robin", true, false, false, true},
		{"fill-first", true, false, false, true},
		{"intelligent-fill", true, false, false, true},
		{"weighted-round-robin", true, false, false, true},
		{"intelligent-fill", false, false, false, false},
		{"round-robin", true, false, true, false},
		{"round-robin", true, false, false, true},
		{"round-robin", false, false, false, false},
		{"round-robin", false, true, false, true},
		{"round-robin", true, true, false, true},
		{"round-robin", true, false, false, true},
		{"round-robin", false, true, false, true},
		{"round-robin", false, true, true, false},
		{"round-robin", true, true, true, false},
		{"round-robin", false, true, false, true},
		{"round-robin", false, false, false, false},
	} {
		cfg := &internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: tt.strategy}, ResetCredits: internalconfig.ResetCreditsConfig{AutoApply: tt.enabled, DryRun: tt.dryRun}}
		cfg.Home.Enabled = tt.home
		if !service.applyManagerConfig(context.Background(), configCommit{cfg: cfg}) {
			t.Fatalf("apply config failed: %+v", tt)
		}
		if got := manager.ResetLoopRunning(); got != tt.want {
			t.Fatalf("%+v: running=%v", tt, got)
		}
	}
}
