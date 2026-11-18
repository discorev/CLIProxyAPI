package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

// The cloak adds Claude Code's refusal fallback only when
// claude-cloak-refusal-fallback is on, and the fallback betas follow the final
// body: no fallbacks field means none of them. Payload rules still run after
// the injection and have the last word.
func TestClaudeExecutor_CloakedOpus55RefusalFallbackIsOptIn(t *testing.T) {
	fallbackBetas := []string{claudeServerSideFallbackBeta, claudeServerSideFallbackDefaultBeta, claudeFallbackCreditBeta}
	filterFallbacks := config.PayloadConfig{Filter: []config.PayloadFilterRule{{
		Models: []config.PayloadModelRule{{Name: "claude-opus-5-5"}},
		Params: []string{"fallbacks"},
	}}}
	for _, tt := range []struct {
		name         string
		cfg          *config.Config
		wantFallback string
		wantBetas    []string
	}{
		{name: "default off", cfg: &config.Config{}},
		{name: "opt in", cfg: &config.Config{ClaudeCloakRefusalFallback: true}, wantFallback: "claude-opus-4-8",
			wantBetas: []string{claudeServerSideFallbackBeta, claudeFallbackCreditBeta}},
		{name: "opt in, payload filter wins", cfg: &config.Config{ClaudeCloakRefusalFallback: true, Payload: filterFallbacks}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var sentBody []byte
			var sentHeaders http.Header
			transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				sentBody, _ = io.ReadAll(req.Body)
				sentHeaders = req.Header.Clone()
				response := `{"id":"msg_1","type":"message","model":"claude-opus-5-5","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(response)), Request: req}, nil
			})
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
			auth := &cliproxyauth.Auth{ID: "refusal-fallback-" + tt.name, Metadata: claudeOAuthTestMetadata(), Attributes: map[string]string{
				"api_key": "sk-ant-oat-refusal-fallback", "cloak_mode": "always",
			}}
			payload := []byte(`{"model":"claude-opus-5-5","thinking":{"type":"adaptive"},"messages":[{"role":"user","content":"test"}]}`)
			if _, errExecute := NewClaudeExecutor(tt.cfg).Execute(ctx, auth, cliproxyexecutor.Request{Model: "claude-opus-5-5", Payload: payload},
				cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}); errExecute != nil {
				t.Fatal(errExecute)
			}
			if !strings.Contains(gjson.GetBytes(sentBody, "system.0.text").String(), "cc_turn_origin=human;") {
				t.Fatalf("request was not cloaked: %s", sentBody)
			}
			if got := gjson.GetBytes(sentBody, "fallbacks"); tt.wantFallback == "" && got.Exists() {
				t.Errorf("fallbacks = %s, want none", got.Raw)
			} else if tt.wantFallback != "" && (len(got.Array()) != 1 || got.Get("0.model").String() != tt.wantFallback) {
				t.Errorf("fallbacks = %s, want [{model:%s}]", got.Raw, tt.wantFallback)
			}
			sentBetas := helps.HeaderValueCaseInsensitive(sentHeaders, "Anthropic-Beta")
			if !strings.Contains(sentBetas, claudeCodeBeta) {
				t.Fatalf("Anthropic-Beta = %q, want the cloaked set", sentBetas)
			}
			betas := strings.Split(sentBetas, ",")
			for _, beta := range fallbackBetas {
				want := false
				for _, wanted := range tt.wantBetas {
					want = want || wanted == beta
				}
				has := false
				for _, sent := range betas {
					has = has || sent == beta
				}
				if has != want {
					t.Errorf("%s present = %t, want %t in %q", beta, has, want, sentBetas)
				}
			}
		})
	}
}
