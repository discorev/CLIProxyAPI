package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCodexHTTPWebsocketPoolConfigDefaults(t *testing.T) {
	var pool CodexHTTPWebsocketPoolConfig
	if !pool.IsEnabled() {
		t.Fatal("pool should default to enabled")
	}
	if got := pool.IdleTimeoutDuration(); got != 0 {
		t.Fatalf("unset idle timeout = %v, want 0 (caller default)", got)
	}
	for raw, want := range map[string]time.Duration{"3m": 3 * time.Minute, "90": 90 * time.Second, "bogus": 0, "-1m": 0} {
		pool.IdleTimeout = raw
		if got := pool.IdleTimeoutDuration(); got != want {
			t.Fatalf("IdleTimeoutDuration(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestCodexHTTPWebsocketPoolConfigParsesBothLayouts(t *testing.T) {
	for name, raw := range map[string]string{
		"upstream": "upstream:\n  codex:\n    http-websocket-pool: {enabled: false, idle-timeout: 4m, max-sockets: 9, max-sockets-per-auth: 3}\n",
		"legacy":   "codex:\n  http-websocket-pool: {enabled: false, idle-timeout: 4m, max-sockets: 9, max-sockets-per-auth: 3}\n",
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := ParseConfigBytes([]byte(raw))
			if err != nil {
				t.Fatalf("ParseConfigBytes() error = %v", err)
			}
			pool := cfg.Codex.HTTPWebsocketPool
			if pool.IsEnabled() || pool.IdleTimeoutDuration() != 4*time.Minute || pool.MaxSockets != 9 || pool.MaxSocketsPerAuth != 3 {
				t.Fatalf("pool config = %+v", pool)
			}
			if api := cfg.ForAPIKey(); api.Codex.HTTPWebsocketPool.IsEnabled() {
				t.Fatal("http-websocket-pool is a shared upstream setting and must apply to API-key routes too")
			}
		})
	}
}

// An explicit opt-out must survive a save: dropping enabled: false (or the
// mappings above it) as a zero value would turn the pool back on at reload.
func TestSaveConfigPreserveCommentsKeepsHTTPWebsocketPoolOptOut(t *testing.T) {
	for name, original := range map[string]string{
		"no codex section":     "port: 8317\n",
		"v8 layout":            "port: 8317\nupstream:\n  codex:\n    http-websocket-pool:\n      enabled: false\n",
		"v8 layout, no pool":   "port: 8317\nupstream:\n  codex:\n    response-steering: false\n",
		"legacy layout":        "port: 8317\ncodex:\n  http-websocket-pool:\n    enabled: false\n",
		"legacy layout, empty": "port: 8317\ncodex: {}\n",
	} {
		t.Run(name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			if errWrite := os.WriteFile(configPath, []byte(original), 0o600); errWrite != nil {
				t.Fatalf("os.WriteFile() error = %v", errWrite)
			}
			cfg, errLoad := LoadConfig(configPath)
			if errLoad != nil {
				t.Fatalf("LoadConfig() error = %v", errLoad)
			}
			disabled := false
			cfg.Codex.HTTPWebsocketPool.Enabled = &disabled
			for round := 1; round <= 2; round++ {
				if errSave := SaveConfigPreserveComments(configPath, cfg); errSave != nil {
					t.Fatalf("round %d SaveConfigPreserveComments() error = %v", round, errSave)
				}
				saved, errRead := os.ReadFile(configPath)
				if errRead != nil {
					t.Fatalf("os.ReadFile() error = %v", errRead)
				}
				reloaded, errReload := LoadConfig(configPath)
				if errReload != nil {
					t.Fatalf("round %d LoadConfig() error = %v", round, errReload)
				}
				if reloaded.Codex.HTTPWebsocketPool.IsEnabled() {
					t.Fatalf("round %d: pool re-enabled after save; saved config:\n%s", round, saved)
				}
				cfg = reloaded
			}
		})
	}

	t.Run("default stays elided", func(t *testing.T) {
		configPath := filepath.Join(t.TempDir(), "config.yaml")
		if errWrite := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); errWrite != nil {
			t.Fatalf("os.WriteFile() error = %v", errWrite)
		}
		cfg, errLoad := LoadConfig(configPath)
		if errLoad != nil {
			t.Fatalf("LoadConfig() error = %v", errLoad)
		}
		if errSave := SaveConfigPreserveComments(configPath, cfg); errSave != nil {
			t.Fatalf("SaveConfigPreserveComments() error = %v", errSave)
		}
		saved, _ := os.ReadFile(configPath)
		if strings.Contains(string(saved), "http-websocket-pool") {
			t.Fatalf("an unset pool setting must not be written:\n%s", saved)
		}
	})
}
