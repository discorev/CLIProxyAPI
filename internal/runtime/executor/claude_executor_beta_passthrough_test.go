package executor

import (
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// fixtureAuth builds a cloaked direct-Anthropic API-key auth for header tests.
// The key value is a placeholder that never reaches a real upstream.
func fixtureAuth() *cliproxyauth.Auth {
	attrs := map[string]string{}
	attrs[cliproxyauth.AttributeAPIKey] = strings.Join([]string{"test", "fixture", "key"}, "-")
	attrs["fingerprint_profile"] = "claude-code-cli"
	return &cliproxyauth.Auth{Attributes: attrs}
}

// claudeUnmanagedTestBeta is a beta no Claude Code profile assembles.
const claudeUnmanagedTestBeta = "future-feature-2030-01-01"

// A caller the cloak does not recognize (a Claude Code newer than the pinned
// profile) keeps betas the proxy does not manage: dropping them fails whole
// turns whose features need the missing authorization (#5738). per-turn-control
// is now assembled for fable-5-1, so this uses a beta that is still unmanaged.
func TestApplyClaudeHeaders_ForwardsUnmanagedCallerBetas(t *testing.T) {
	t.Parallel()

	auth := fixtureAuth()
	req, errReq := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", nil)
	if errReq != nil {
		t.Fatalf("NewRequest() error = %v", errReq)
	}
	incoming := http.Header{}
	incoming.Set("Anthropic-Beta", claudeUnmanagedTestBeta)
	if errHeaders := applyClaudeHeaders(req, auth, auth.Attributes[cliproxyauth.AttributeAPIKey], false, nil, []byte(`{"model":"claude-fable-5-1"}`), &config.Config{}, incoming, false); errHeaders != nil {
		t.Fatalf("applyClaudeHeaders() error = %v", errHeaders)
	}
	betas := req.Header.Get("Anthropic-Beta")
	if !strings.HasSuffix(betas, ","+claudeUnmanagedTestBeta) {
		t.Fatalf("Anthropic-Beta = %q, want unmanaged caller beta forwarded", betas)
	}
	if !strings.Contains(betas, "prompt-caching-scope-2026-01-05,per-turn-control-2026-07-01,mid-conversation-tool-changes-2026-07-01,tool-provenance-2026-10-26,thinking-token-count-2026-05-13,mid-conversation-system-clear-at-2026-08-21,effort-2025-11-24") {
		t.Fatalf("Anthropic-Beta = %q, want fable-5-1 per-turn-control in the 2.1.325 model phase", betas)
	}
}

// Managed betas stay governed by the assembled baseline on direct Anthropic:
// one whose gating excludes the request (effort on a Haiku model) is not
// reinstated just because the caller asked for it.
func TestApplyClaudeHeaders_StillGatesManagedCallerBetas(t *testing.T) {
	t.Parallel()

	auth := fixtureAuth()
	req, errReq := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", nil)
	if errReq != nil {
		t.Fatalf("NewRequest() error = %v", errReq)
	}
	incoming := http.Header{}
	incoming.Set("Anthropic-Beta", "effort-2025-11-24")
	if errHeaders := applyClaudeHeaders(req, auth, auth.Attributes[cliproxyauth.AttributeAPIKey], false, nil, []byte(`{"model":"claude-haiku-4-5"}`), &config.Config{}, incoming, false); errHeaders != nil {
		t.Fatalf("applyClaudeHeaders() error = %v", errHeaders)
	}
	if betas := req.Header.Get("Anthropic-Beta"); strings.Contains(betas, "effort-2025-11-24") {
		t.Fatalf("Anthropic-Beta = %q, want gated effort beta kept off the Haiku request", betas)
	}
}

func TestApplyClaudeHeaders_PreservesNativeGatewayHintsOnly(t *testing.T) {
	for _, tt := range []struct {
		name         string
		requestClass string
		confirmed    bool
	}{
		{name: "confirmed main", requestClass: "main", confirmed: true},
		{name: "confirmed auxiliary", requestClass: "auxiliary", confirmed: true},
		{name: "unconfirmed caller", requestClass: "auxiliary"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
			if err != nil {
				t.Fatal(err)
			}
			incoming := http.Header{
				"x-claude-code-request-class":       {tt.requestClass},
				"x-claude-code-agent-type":          {"builtin"},
				"x-claude-code-prev-tool-durations": {"5"},
				"x-claude-code-compaction":          {"false"},
				"x-claude-code-context-compacted":   {"false"},
			}
			auth := fixtureAuth()
			if err := applyClaudeHeaders(req, auth, auth.Attributes[cliproxyauth.AttributeAPIKey], false, nil,
				[]byte(`{"model":"claude-opus-5-5"}`), &config.Config{}, incoming, tt.confirmed); err != nil {
				t.Fatal(err)
			}
			for key, values := range incoming {
				got := req.Header.Get(key)
				if tt.confirmed && got != values[0] {
					t.Errorf("%s = %q, want %q", key, got, values[0])
				}
				// The cloak derives its own request class for an unconfirmed
				// caller (a main turn here); every other hint is dropped.
				if !tt.confirmed && key == "x-claude-code-request-class" {
					if got != "main" {
						t.Errorf("unconfirmed caller request class = %q, want derived main", got)
					}
					continue
				}
				if !tt.confirmed && got != "" {
					t.Errorf("unconfirmed caller forwarded %s = %q", key, got)
				}
			}
		})
	}
}

// TestApplyClaudeHeaders_ForwardsUnmanagedCallerBetas_OAuth verifies the exact
// scenario reported in #5738: an OAuth credential with an unconfirmed client
// sending a per-turn effort directive turn.
func TestApplyClaudeHeaders_ForwardsUnmanagedCallerBetas_OAuth(t *testing.T) {
	t.Parallel()

	auth := &cliproxyauth.Auth{
		ID:       "claude-oauth-fixture",
		Metadata: map[string]any{"access_token": "sk-ant-oat-fixture"},
	}
	req, errReq := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", nil)
	if errReq != nil {
		t.Fatalf("NewRequest() error = %v", errReq)
	}
	incoming := http.Header{}
	incoming.Set("Anthropic-Beta", claudeUnmanagedTestBeta+",mid-conversation-system-2026-04-07")
	body := []byte(`{
		"model": "claude-fable-5-1",
		"max_tokens": 16,
		"messages": [
			{"role": "user",   "content": "hi"},
			{"role": "system", "content": [], "output_config": {"effort": "low"}},
			{"role": "user",   "content": "say ok"}
		]
	}`)
	if errHeaders := applyClaudeHeaders(req, auth, "sk-ant-oat-fixture", false, nil, body, &config.Config{}, incoming, false); errHeaders != nil {
		t.Fatalf("applyClaudeHeaders() error = %v", errHeaders)
	}
	betas := req.Header.Get("Anthropic-Beta")
	if !strings.Contains(betas, claudeUnmanagedTestBeta) {
		t.Fatalf("Anthropic-Beta = %q, want unmanaged caller beta forwarded on OAuth", betas)
	}
	if strings.Contains(betas, claudeMidConvSystemBeta) {
		t.Fatalf("Anthropic-Beta = %q, want headerless mid-conversation-system dropped like 2.1.325", betas)
	}
	if !strings.Contains(betas, "prompt-caching-scope-2026-01-05,per-turn-control-2026-07-01,mid-conversation-tool-changes-2026-07-01") {
		t.Fatalf("Anthropic-Beta = %q, want fable-5-1 per-turn-control assembled", betas)
	}
}

// Probes and Haiku requests without fallbacks never carry a server-side
// fallback beta, neither the list (2026-06-01) nor the default (2026-07-01)
// variant, nor fallback-credit-2026-06-01, even when a caller on a custom
// gateway asks for them.
func TestApplyClaudeHeaders_StripsBothServerSideFallbackBetasFromProbesAndHaiku(t *testing.T) {
	t.Parallel()

	for name, body := range map[string]string{
		"probe": `{"model":"claude-opus-5-5","max_tokens":1,"messages":[{"role":"user","content":"quota"}]}`,
		"haiku": `{"model":"claude-haiku-4-5-20251001","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			auth := fixtureAuth()
			req, errReq := http.NewRequest(http.MethodPost, "https://gateway.example.invalid/v1/messages", nil)
			if errReq != nil {
				t.Fatalf("NewRequest() error = %v", errReq)
			}
			incoming := http.Header{}
			incoming.Set("Anthropic-Beta", claudeServerSideFallbackBeta+","+claudeServerSideFallbackDefaultBeta+","+claudeFallbackCreditBeta)
			if errHeaders := applyClaudeHeaders(req, auth, auth.Attributes[cliproxyauth.AttributeAPIKey], false, nil, []byte(body), &config.Config{}, incoming, false); errHeaders != nil {
				t.Fatalf("applyClaudeHeaders() error = %v", errHeaders)
			}
			betas := req.Header.Get("Anthropic-Beta")
			for _, beta := range []string{claudeServerSideFallbackBeta, claudeServerSideFallbackDefaultBeta, claudeFallbackCreditBeta} {
				if strings.Contains(betas, beta) {
					t.Errorf("Anthropic-Beta = %q, want %s stripped", betas, beta)
				}
			}
		})
	}
}
