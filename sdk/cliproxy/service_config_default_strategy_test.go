package cliproxy

import (
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestRoutingStrategyDefaultsToIntelligentFill(t *testing.T) {
	cases := []struct {
		name     string
		cfg      *internalconfig.Config
		strategy string
	}{
		{name: "nil config", cfg: nil, strategy: "intelligent-fill"},
		{name: "unset", cfg: &internalconfig.Config{}, strategy: "intelligent-fill"},
		{name: "unknown", cfg: &internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: "bogus"}}, strategy: "intelligent-fill"},
		{name: "explicit round-robin", cfg: &internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: "rr"}}, strategy: "round-robin"},
		{name: "explicit fill-first", cfg: &internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: "fill-first"}}, strategy: "fill-first"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := normalizedRoutingRuntimeState(tc.cfg)
			if state.strategy != tc.strategy {
				t.Fatalf("strategy = %q, want %q", state.strategy, tc.strategy)
			}
			selector := newRoutingSelector(state, nil)
			switch tc.strategy {
			case "intelligent-fill":
				if _, ok := selector.(*coreauth.IntelligentFillSelector); !ok {
					t.Fatalf("selector type = %T, want *auth.IntelligentFillSelector", selector)
				}
			case "round-robin":
				if _, ok := selector.(*coreauth.RoundRobinSelector); !ok {
					t.Fatalf("selector type = %T, want *auth.RoundRobinSelector", selector)
				}
			}
		})
	}
}
