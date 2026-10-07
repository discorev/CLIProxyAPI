package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// round-robin is no longer the default, so an explicit choice must be written
// even when the file has no routing section; intelligent-fill is elided.
func TestSaveConfigPreserveComments_RoutingStrategyDefault(t *testing.T) {
	cases := []struct {
		strategy string
		written  bool
	}{
		{strategy: "round-robin", written: true},
		{strategy: "intelligent-fill", written: false},
	}
	for _, tc := range cases {
		t.Run(tc.strategy, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			if errWrite := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); errWrite != nil {
				t.Fatalf("os.WriteFile() error = %v", errWrite)
			}
			cfg := &Config{Routing: RoutingConfig{Strategy: tc.strategy}}
			cfg.Port = 8317
			if errSave := SaveConfigPreserveComments(configPath, cfg); errSave != nil {
				t.Fatalf("SaveConfigPreserveComments() error = %v", errSave)
			}
			saved, errRead := os.ReadFile(configPath)
			if errRead != nil {
				t.Fatalf("os.ReadFile() error = %v", errRead)
			}
			if got := strings.Contains(string(saved), "strategy: "+tc.strategy); got != tc.written {
				t.Fatalf("strategy written = %v, want %v; got:\n%s", got, tc.written, saved)
			}
		})
	}
}
