package management

import (
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestAuthWebsocketsValueReportsEffectiveSetting(t *testing.T) {
	tests := []struct {
		name       string
		auth       *coreauth.Auth
		wantValue  bool
		wantReport bool
	}{
		{name: "codex oauth default on", auth: &coreauth.Auth{Provider: "codex", Metadata: map[string]any{"access_token": "t"}}, wantValue: true, wantReport: true},
		{name: "codex oauth explicit off", auth: &coreauth.Auth{Provider: "codex", Metadata: map[string]any{"access_token": "t", "websockets": false}}, wantReport: true},
		{name: "codex api key unset", auth: &coreauth.Auth{Provider: "codex", Attributes: map[string]string{"api_key": "sk-test"}}},
		{name: "xai unset", auth: &coreauth.Auth{Provider: "xai", Metadata: map[string]any{"access_token": "t"}}},
		{name: "xai explicit on", auth: &coreauth.Auth{Provider: "xai", Metadata: map[string]any{"websockets": true}}, wantValue: true, wantReport: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value, reported := authWebsocketsValue(tt.auth)
			if value != tt.wantValue || reported != tt.wantReport {
				t.Fatalf("authWebsocketsValue() = (%t, %t), want (%t, %t)", value, reported, tt.wantValue, tt.wantReport)
			}
		})
	}
}
