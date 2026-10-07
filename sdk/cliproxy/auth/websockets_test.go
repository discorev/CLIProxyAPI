package auth

import (
	"context"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestWebsocketsSetting(t *testing.T) {
	tests := []struct {
		name         string
		auth         *Auth
		wantEnabled  bool
		wantExplicit bool
	}{
		{name: "nil", auth: nil},
		{
			name:        "codex oauth file defaults on",
			auth:        &Auth{Provider: "codex", Attributes: map[string]string{"path": "/auths/codex.json"}, Metadata: map[string]any{"access_token": "t"}},
			wantEnabled: true,
		},
		{
			name:        "codex auth without kind hints defaults on",
			auth:        &Auth{Provider: "Codex"},
			wantEnabled: true,
		},
		{
			name:         "codex oauth explicit metadata false opts out",
			auth:         &Auth{Provider: "codex", Metadata: map[string]any{"access_token": "t", "websockets": false}},
			wantExplicit: true,
		},
		{
			name:         "codex oauth explicit string attribute false opts out",
			auth:         &Auth{Provider: "codex", Attributes: map[string]string{"websockets": "false"}, Metadata: map[string]any{"websockets": true}},
			wantExplicit: true,
		},
		{
			name:         "codex oauth explicit metadata string true",
			auth:         &Auth{Provider: "codex", Metadata: map[string]any{"websockets": " true "}},
			wantEnabled:  true,
			wantExplicit: true,
		},
		{
			name:        "invalid explicit value falls back to default",
			auth:        &Auth{Provider: "codex", Metadata: map[string]any{"websockets": "maybe"}},
			wantEnabled: true,
		},
		{
			name: "codex api key stays opt-in",
			auth: &Auth{Provider: "codex", Attributes: map[string]string{"api_key": "sk-test", "source": "config:codex[abc]"}},
		},
		{
			name: "codex base-url-only config entry stays opt-in",
			auth: &Auth{Provider: "codex", Attributes: map[string]string{"base_url": "https://example.com", "source": "config:codex[abc]"}},
		},
		{
			name:         "codex api key explicit opt-in",
			auth:         &Auth{Provider: "codex", Attributes: map[string]string{"api_key": "sk-test", "websockets": "true"}},
			wantEnabled:  true,
			wantExplicit: true,
		},
		{
			name: "codex auth kind apikey stays opt-in",
			auth: &Auth{Provider: "codex", Attributes: map[string]string{"auth_kind": "apikey"}},
		},
		{
			name: "xai oauth stays opt-in",
			auth: &Auth{Provider: "xai", Metadata: map[string]any{"access_token": "t"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			enabled, explicit := WebsocketsSetting(tt.auth)
			if enabled != tt.wantEnabled || explicit != tt.wantExplicit {
				t.Fatalf("WebsocketsSetting() = (%t, %t), want (%t, %t)", enabled, explicit, tt.wantEnabled, tt.wantExplicit)
			}
			if got := WebsocketsEnabled(tt.auth); got != tt.wantEnabled {
				t.Fatalf("WebsocketsEnabled() = %t, want %t", got, tt.wantEnabled)
			}
			if got := authWebsocketsEnabled(tt.auth); got != tt.wantEnabled {
				t.Fatalf("authWebsocketsEnabled() = %t, want %t", got, tt.wantEnabled)
			}
		})
	}
}

func TestPreferCodexWebsocketAuthsKeepsDefaultOAuthCandidates(t *testing.T) {
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	a := &Auth{ID: "a", Provider: "codex", Metadata: map[string]any{"access_token": "t"}}
	b := &Auth{ID: "b", Provider: "codex", Metadata: map[string]any{"access_token": "t"}}
	explicitOn := &Auth{ID: "c", Provider: "codex", Metadata: map[string]any{"access_token": "t", "websockets": true}}

	got := preferCodexWebsocketAuths(ctx, "codex", []*Auth{a, b, explicitOn})
	if len(got) != 3 {
		t.Fatalf("preferCodexWebsocketAuths() kept %d candidates, want all 3 (default-on OAuth credentials must not be filtered out)", len(got))
	}
}

func TestPreferCodexWebsocketAuthsDropsExplicitOptOut(t *testing.T) {
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	optedOut := &Auth{ID: "off", Provider: "codex", Metadata: map[string]any{"access_token": "t", "websockets": false}}
	defaultOn := &Auth{ID: "on", Provider: "codex", Metadata: map[string]any{"access_token": "t"}}
	apiKey := &Auth{ID: "key", Provider: "codex", Attributes: map[string]string{"api_key": "sk-test"}}

	got := preferCodexWebsocketAuths(ctx, "codex", []*Auth{optedOut, defaultOn, apiKey})
	if len(got) != 1 || got[0].ID != "on" {
		ids := make([]string, 0, len(got))
		for _, candidate := range got {
			ids = append(ids, candidate.ID)
		}
		t.Fatalf("preferCodexWebsocketAuths() = %v, want [on]", ids)
	}
}
