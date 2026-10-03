package cliproxy

import (
	"context"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestIntelligentFillRoutingAliases(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	for _, alias := range []string{"intelligent-fill", "intelligentfill", "if", " IF "} {
		state := normalizedRoutingRuntimeState(&internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: alias}})
		if state.strategy != "intelligent-fill" {
			t.Fatalf("strategy %q normalized to %q", alias, state.strategy)
		}
		if selector := newRoutingSelector(state, manager); selector == nil {
			t.Fatal("missing selector")
		} else if _, ok := selector.(*coreauth.IntelligentFillSelector); !ok {
			t.Fatalf("selector = %T", selector)
		}
		state.sessionAffinity = true
		selector := newRoutingSelector(state, manager)
		affinity, ok := selector.(*coreauth.SessionAffinitySelector)
		if !ok {
			t.Fatalf("affinity selector = %T", selector)
		}
		affinity.Stop()
	}
}

func TestIntelligentFillUsageSweepHotSwitch(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	defer manager.StopUsageSweep()
	service := &Service{coreManager: manager}
	for _, test := range []struct {
		strategy string
		home     bool
		running  bool
	}{
		{"round-robin", false, false},
		{"intelligent-fill", false, true},
		{"if", false, true},
		{"fill-first", false, false},
		{"intelligent-fill", true, false},
		{"intelligent-fill", false, true},
		{"intelligent-fill", true, false},
	} {
		cfg := &internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: test.strategy}}
		cfg.Home.Enabled = test.home
		if !service.applyManagerConfig(context.Background(), configCommit{cfg: cfg}) {
			t.Fatalf("apply config failed for %+v", test)
		}
		if got := manager.UsageSweepRunning(); got != test.running {
			t.Fatalf("%+v: sweep running = %v", test, got)
		}
	}
}
