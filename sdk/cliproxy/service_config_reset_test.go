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
		strategy            string
		enabled, home, want bool
	}{
		{"round-robin", false, false, false},
		{"round-robin", true, false, true},
		{"fill-first", true, false, true},
		{"intelligent-fill", true, false, true},
		{"weighted-round-robin", true, false, true},
		{"intelligent-fill", false, false, false},
		{"round-robin", true, true, false},
		{"round-robin", true, false, true},
		{"round-robin", false, false, false},
	} {
		cfg := &internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: tt.strategy}, ResetCredits: internalconfig.ResetCreditsConfig{AutoApply: tt.enabled}}
		cfg.Home.Enabled = tt.home
		if !service.applyManagerConfig(context.Background(), configCommit{cfg: cfg}) {
			t.Fatalf("apply config failed: %+v", tt)
		}
		if got := manager.ResetLoopRunning(); got != tt.want {
			t.Fatalf("%+v: running=%v", tt, got)
		}
	}
}
