package config

import (
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
