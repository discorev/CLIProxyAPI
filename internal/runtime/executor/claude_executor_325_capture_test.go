package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// claude325Capture is one sanitized Claude Code 2.1.325 main turn from
// testdata/claude_code_2_1_325_captures.json.
type claude325Capture struct {
	Name    string            `json:"name"`
	Headers map[string]string `json:"headers"`
	Body    json.RawMessage   `json:"body"`
}

type claude325Captures struct {
	MainTurns []claude325Capture `json:"main_turns"`
	MultiTurn []claude325Capture `json:"multi_turn"`
}

func loadClaude325Captures(t *testing.T) claude325Captures {
	t.Helper()
	raw, errRead := os.ReadFile("testdata/claude_code_2_1_325_captures.json")
	if errRead != nil {
		t.Fatal(errRead)
	}
	var captures claude325Captures
	if errDecode := json.Unmarshal(raw, &captures); errDecode != nil {
		t.Fatal(errDecode)
	}
	if len(captures.MainTurns) == 0 || len(captures.MultiTurn) != 3 {
		t.Fatalf("unexpected fixture shape: %d main turns, %d multi-turn requests", len(captures.MainTurns), len(captures.MultiTurn))
	}
	return captures
}

func (captures claude325Captures) all() []claude325Capture {
	return append(append([]claude325Capture(nil), captures.MainTurns...), captures.MultiTurn...)
}

func (c claude325Capture) header() http.Header {
	headers := make(http.Header, len(c.Headers))
	for name, value := range c.Headers {
		headers.Set(name, value)
	}
	return headers
}

var claude325GatewayHints = []string{
	claude1PRequestClassHeader,
	claudeRequestClassHeader,
	claudePromptIDHeader,
	claudeToolsUsedHeader,
}

// The cloaked beta header for each captured 2.1.325 main-turn body must equal
// the header real Claude Code sent with it, and the gateway hints must carry
// the captured values.
func TestClaudeCodeCLIBetas_Matches2_1_325Captures(t *testing.T) {
	captures := loadClaude325Captures(t)
	for _, capture := range captures.all() {
		t.Run(capture.Name, func(t *testing.T) {
			want := capture.Headers["anthropic-beta"]
			if got := claudeCodeCLIBetas(capture.Body, nil, true); got != want {
				t.Fatalf("claudeCodeCLIBetas() =\n%s\nwant\n%s", got, want)
			}

			const token = "sk-ant-oat-capture-fixture"
			auth := &cliproxyauth.Auth{ID: "capture-fixture", Metadata: map[string]any{"access_token": token}}
			req, errReq := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", nil)
			if errReq != nil {
				t.Fatal(errReq)
			}
			if errHeaders := applyClaudeHeaders(req, auth, token, true, nil, capture.Body, &config.Config{}, nil, false); errHeaders != nil {
				t.Fatal(errHeaders)
			}
			if got := req.Header.Get("Anthropic-Beta"); got != want {
				t.Errorf("cloaked Anthropic-Beta =\n%s\nwant\n%s", got, want)
			}
			for _, name := range claude325GatewayHints {
				if got, want := req.Header.Get(name), capture.header().Get(name); got != want {
					t.Errorf("%s = %q, want captured %q", name, got, want)
				}
			}
		})
	}
}

const claude325StreamEvents = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_325","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

// A genuine 2.1.325 client's gateway hints reach Anthropic unchanged, both on
// native passthrough and on the confirmed-client path.
func TestClaudeExecutor_Forwards2_1_325GatewayHintsUnchanged(t *testing.T) {
	captures := loadClaude325Captures(t)
	for _, passthrough := range []bool{true, false} {
		for _, capture := range captures.all() {
			t.Run(fmt.Sprintf("passthrough=%t/%s", passthrough, capture.Name), func(t *testing.T) {
				headers := capture.header()
				detection := helps.DetectClaudeCodeRequest(headers, capture.Body, false)
				if !detection.NativePassthrough || !detection.Confirmed {
					t.Fatalf("captured client is not a confirmed native client: %#v", detection)
				}
				var sentHeaders http.Header
				transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					_, _ = io.Copy(io.Discard, req.Body)
					sentHeaders = req.Header.Clone()
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(claude325StreamEvents)), Request: req}, nil
				})
				ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
				executor := NewClaudeExecutor(&config.Config{ClaudeNativePassthrough: passthrough})
				auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "local-test-key", "auth_kind": cliproxyauth.AuthKindAPIKey}}
				model := gjson.GetBytes(capture.Body, "model").String()
				result, errStream := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{Model: model, Payload: capture.Body}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, OriginalRequest: capture.Body, Headers: headers})
				if errStream != nil {
					t.Fatal(errStream)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
				}
				for _, name := range claude325GatewayHints {
					if got, want := helps.HeaderValueCaseInsensitive(sentHeaders, name), headers.Get(name); got != want {
						t.Errorf("%s = %q, want forwarded %q", name, got, want)
					}
				}
			})
		}
	}
}

var claude325BillingPattern = regexp.MustCompile(`^x-anthropic-billing-header: cc_version=2\.1\.325\.[0-9a-f]{3}; cc_entrypoint=cli; cch=[0-9a-f]{5};` +
	`(?: cc_prev_req=(req_[A-Za-z0-9_]+);)? cc_prompt_id=([0-9a-f-]{36}); cc_turn_origin=human; cc_prompt_index=(\d+); cc_turn_index=(\d+);$`)

type claude325BillingFields struct {
	prevReq, promptID, promptIndex, turnIndex string
}

func parseClaude325Billing(t *testing.T, text string) claude325BillingFields {
	t.Helper()
	match := claude325BillingPattern.FindStringSubmatch(text)
	if match == nil {
		t.Fatalf("billing header does not follow the 2.1.325 field order: %s", text)
	}
	return claude325BillingFields{prevReq: match[1], promptID: match[2], promptIndex: match[3], turnIndex: match[4]}
}

// A cloaked client replaying the captured multi-turn transcript gets the same
// turn positions, prompt-id chaining and gateway hints as real 2.1.325: the
// tool loop keeps prompt 1/1 with cc_prev_req, the second prompt moves to 2/2.
func TestClaudeExecutor_Cloaked2_1_325MultiTurnBillingAndGatewayHints(t *testing.T) {
	captures := loadClaude325Captures(t)

	var sentBodies [][]byte
	var sentHeaders []http.Header
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		sentBodies = append(sentBodies, body)
		sentHeaders = append(sentHeaders, req.Header.Clone())
		n := len(sentBodies)
		response := fmt.Sprintf(`{"id":"msg_cloaked_%d","type":"message","model":"claude-opus-5-5","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`, n)
		header := http.Header{"Content-Type": {"application/json"}, "Request-Id": {fmt.Sprintf("req_cloaked_turn_%d", n)}}
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(response)), Request: req}, nil
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
	testID := uuid.NewString()
	auth := &cliproxyauth.Auth{
		ID:         "cloaked-325-" + testID,
		Attributes: map[string]string{"api_key": "sk-ant-oat-cloaked-325", "cloak_mode": "always"},
		Metadata:   claudeOAuthTestMetadata(),
	}
	executor := NewClaudeExecutor(&config.Config{})
	options := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatClaude,
		Metadata:     map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: "cloaked-325-" + testID},
	}

	for _, capture := range captures.MultiTurn {
		// A third-party client sends only its own transcript: the user and
		// assistant turns, without Claude Code's system prompt or role=system turns.
		payload := []byte(`{"model":"claude-opus-5-5","max_tokens":1024,"messages":[]}`)
		gjson.GetBytes(capture.Body, "messages").ForEach(func(_, message gjson.Result) bool {
			if role := message.Get("role").String(); role == "user" || role == "assistant" {
				payload, _ = sjson.SetRawBytes(payload, "messages.-1", []byte(message.Raw))
			}
			return true
		})
		if _, errExecute := executor.Execute(ctx, auth, cliproxyexecutor.Request{Model: "claude-opus-5-5", Payload: payload}, options); errExecute != nil {
			t.Fatalf("%s: %v", capture.Name, errExecute)
		}
	}
	if len(sentBodies) != len(captures.MultiTurn) {
		t.Fatalf("sent %d requests, want %d", len(sentBodies), len(captures.MultiTurn))
	}

	var got []claude325BillingFields
	for i, capture := range captures.MultiTurn {
		fields := parseClaude325Billing(t, gjson.GetBytes(sentBodies[i], "system.0.text").String())
		captured := parseClaude325Billing(t, gjson.GetBytes(capture.Body, "system.0.text").String())
		if fields.promptIndex != captured.promptIndex || fields.turnIndex != captured.turnIndex {
			t.Errorf("%s: cc_prompt_index/cc_turn_index = %s/%s, want captured %s/%s", capture.Name, fields.promptIndex, fields.turnIndex, captured.promptIndex, captured.turnIndex)
		}
		if (fields.prevReq == "") != (captured.prevReq == "") {
			t.Errorf("%s: cc_prev_req = %q, captured presence %q", capture.Name, fields.prevReq, captured.prevReq)
		}
		for _, name := range []string{claude1PRequestClassHeader, claudeRequestClassHeader, claudeToolsUsedHeader} {
			if got, want := helps.HeaderValueCaseInsensitive(sentHeaders[i], name), capture.header().Get(name); got != want {
				t.Errorf("%s: %s = %q, want captured %q", capture.Name, name, got, want)
			}
		}
		if got := helps.HeaderValueCaseInsensitive(sentHeaders[i], claudePromptIDHeader); got != fields.promptID {
			t.Errorf("%s: %s = %q, want the billing cc_prompt_id %q", capture.Name, claudePromptIDHeader, got, fields.promptID)
		}
		got = append(got, fields)
	}
	if got[1].prevReq != "req_cloaked_turn_1" || got[2].prevReq != "req_cloaked_turn_2" {
		t.Errorf("cc_prev_req chain = %q, %q; want req_cloaked_turn_1, req_cloaked_turn_2", got[1].prevReq, got[2].prevReq)
	}
	if got[0].promptID != got[1].promptID {
		t.Errorf("tool loop changed the prompt id: %s -> %s", got[0].promptID, got[1].promptID)
	}
	if got[2].promptID == got[1].promptID {
		t.Errorf("second prompt reused prompt id %s", got[2].promptID)
	}
}

// 2.1.325 sends a subagent's parent prompt id both in the billing block and in
// x-claude-code-prompt-id, so a cloaked subagent carries the header too, with
// the billing value.
func TestApplyClaudeCodeGatewayHints_SubagentCarriesBillingPromptID(t *testing.T) {
	const promptID = "00000000-0000-4000-8000-000000000002"
	body := []byte(`{"model":"claude-opus-5-5","system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.325.000; cc_entrypoint=cli; cch=00000; cc_is_subagent=true; cc_prompt_id=` + promptID + `;"}],"messages":[{"role":"user","content":"task"}]}`)
	headers := http.Header{}
	headers.Set(claudePromptIDHeader, "stale")
	applyClaudeCodeGatewayHints(headers, nil, body)
	if got := headers.Get(claudeRequestClassHeader) + "/" + headers.Get(claude1PRequestClassHeader); got != "subagent/blocking-subagent" {
		t.Fatalf("request classes = %q, want subagent/blocking-subagent", got)
	}
	if got := headers.Get(claudePromptIDHeader); got != promptID {
		t.Fatalf("%s = %q, want billing cc_prompt_id %q", claudePromptIDHeader, got, promptID)
	}

	noPromptID := []byte(`{"model":"claude-opus-5-5","system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.325.000; cc_entrypoint=cli; cch=00000; cc_is_subagent=true;"}],"messages":[{"role":"user","content":"task"}]}`)
	applyClaudeCodeGatewayHints(headers, nil, noPromptID)
	if got := headers.Get(claudePromptIDHeader); got != "" {
		t.Fatalf("%s = %q, want none without a billing cc_prompt_id", claudePromptIDHeader, got)
	}
}

// 2.1.325 Haiku side requests (the quota probe on claude-haiku-4-5 and the
// session-title helper on claude-haiku-5-5) carry no claude-code-20250219;
// only an agentic Haiku query gets it in the Haiku slot.
func TestClaudeCodeCLIBetas_HaikuProbesAndHelpersOmitClaudeCodeBeta(t *testing.T) {
	const titleSchema = `"output_config":{"effort":"medium","format":{"type":"json_schema","schema":{"type":"object","properties":{"title":{"type":"string"}},"required":["title"],"additionalProperties":false}}}`
	for _, tt := range []struct {
		name string
		body string
		want bool
	}{
		{name: "quota probe", body: `{"model":"claude-haiku-4-5-20251001","max_tokens":1,"messages":[{"role":"user","content":"quota"}]}`},
		{name: "title helper", body: `{"model":"claude-haiku-5-5","messages":[{"role":"user","content":[{"type":"text","text":"<session>placeholder</session>"}]}],"system":[{"type":"text","text":"Return a short title."}],"tools":[],"max_tokens":128000,` + titleSchema + `,"stream":true}`},
		{name: "agentic query", body: `{"model":"claude-haiku-5-5","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`, want: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := claudeCodeCLIBetas([]byte(tt.body), nil, true)
			if has := strings.Contains(got, claudeCodeBeta); has != tt.want {
				t.Fatalf("claude-code beta present = %t, want %t: %s", has, tt.want, got)
			}
		})
	}
}

// 2.1.325 gives Haiku 5.5 the per-turn-control, clear-at and effort betas (its
// catalog lists effort and per_turn_effort), so a cloaked title-helper-shaped
// body carries them; the quota probe on claude-haiku-4-5 carries none.
func TestClaudeCodeCLIBetas_Haiku55TitleHelperGates(t *testing.T) {
	const titleSchema = `"output_config":{"effort":"medium","format":{"type":"json_schema","schema":{"type":"object","properties":{"title":{"type":"string"}},"required":["title"],"additionalProperties":false}}}`
	title := `{"model":"claude-haiku-5-5","messages":[{"role":"user","content":[{"type":"text","text":"<session>placeholder</session>"}]}],"system":[{"type":"text","text":"Return a short title."}],"tools":[],"max_tokens":128000,` + titleSchema + `,"stream":true}`
	// The captured helper also sends claude-code-side-query and cache-diagnosis,
	// which the cloak only adds when the caller asks for them.
	const wantTitle = "oauth-2025-04-20,interleaved-thinking-2025-05-14,redact-thinking-2026-02-12," +
		"context-management-2025-06-27,prompt-caching-scope-2026-01-05,per-turn-control-2026-07-01," +
		"mid-conversation-tool-changes-2026-07-01,tool-provenance-2026-10-26,thinking-token-count-2026-05-13," +
		"mid-conversation-system-clear-at-2026-08-21,effort-2025-11-24,structured-outputs-2025-12-15"
	if got := claudeCodeCLIBetas([]byte(title), nil, true); got != wantTitle {
		t.Fatalf("title helper betas =\n%s\nwant\n%s", got, wantTitle)
	}

	probe := `{"model":"claude-haiku-4-5-20251001","max_tokens":1,"messages":[{"role":"user","content":"quota"}]}`
	got := claudeCodeCLIBetas([]byte(probe), nil, true)
	for _, beta := range []string{claudePerTurnControlBeta, claudeMidConvSystemClearAtBeta, claudeEffortBeta} {
		if strings.Contains(got, beta) {
			t.Errorf("quota probe betas = %s, want no %s", got, beta)
		}
	}
	if claudeIncludePerTurnTiming([]byte(`{"model":"claude-haiku-5-5","output_config":{"timing":{}}}`), nil) {
		t.Error("claude-haiku-5-5 got timing-2026-09-09; its catalog entry lacks per_turn_timing")
	}
}

// The 2.1.325 catalog lists per_turn_effort and per_turn_timing for Sonnet 5.5,
// Fable 5.2 and Mythos 5.2, so they always carry per-turn-control and carry
// timing-2026-09-09 only when the body sets output_config.timing.
func TestClaudeCodeCLIBetas_PerTurnEffortCatalogModels(t *testing.T) {
	for _, model := range []string{"claude-sonnet-5-5", "claude-fable-5-2", "claude-mythos-5-2"} {
		for _, tt := range []struct {
			name       string
			body       string
			wantTiming bool
		}{
			{name: "no timing", body: `{"model":"` + model + `","thinking":{"type":"adaptive"}}`},
			{name: "timing", body: `{"model":"` + model + `","thinking":{"type":"adaptive"},"output_config":{"timing":{}}}`, wantTiming: true},
		} {
			t.Run(model+"/"+tt.name, func(t *testing.T) {
				got := claudeCodeCLIBetas([]byte(tt.body), nil, true)
				if !strings.Contains(got, claudePerTurnControlBeta) {
					t.Errorf("betas = %s, want %s", got, claudePerTurnControlBeta)
				}
				if has := strings.Contains(got, claudePerTurnTimingBeta); has != tt.wantTiming {
					t.Errorf("timing beta present = %t, want %t: %s", has, tt.wantTiming, got)
				}
			})
		}
	}
}

// Pre-5 Haiku never gets effort-2025-11-24, including under a provider-prefixed
// id; Haiku 5.5 under the same prefix still does.
func TestClaudeRequestSupportsEffort_ProviderPrefixedHaiku(t *testing.T) {
	for model, want := range map[string]bool{
		"us.anthropic.claude-haiku-4-5-20251001-v1:0": false,
		"us.anthropic.claude-haiku-5-5":               true,
	} {
		body := []byte(`{"model":"` + model + `","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`)
		if got := claudeRequestSupportsEffort(body, nil); got != want {
			t.Errorf("claudeRequestSupportsEffort(%s) = %t, want %t", model, got, want)
		}
	}
}

// A cloaked quota-probe-shaped body gets exactly the captured 2.1.325 quota
// probe's betas: no tool-provenance and no thinking-token-count.
func TestClaudeCodeCLIBetas_QuotaProbeMatches2_1_325Capture(t *testing.T) {
	const capturedProbeBetas = "oauth-2025-04-20,interleaved-thinking-2025-05-14,redact-thinking-2026-02-12," +
		"context-management-2025-06-27,prompt-caching-scope-2026-01-05"
	probe := `{"model":"claude-haiku-4-5-20251001","max_tokens":1,"messages":[{"role":"user","content":"quota"}],"metadata":{"user_id":"{}"}}`
	if got := claudeCodeCLIBetas([]byte(probe), nil, true); got != capturedProbeBetas {
		t.Fatalf("quota probe betas =\n%s\nwant captured\n%s", got, capturedProbeBetas)
	}
}
