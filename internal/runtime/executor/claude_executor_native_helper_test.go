package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

const (
	claudeNativeHelperSessionID = "11111111-2222-4333-8444-555555555555"
	claudeNativeHelperUserID    = `{"device_id":"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff","account_uuid":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","session_id":"11111111-2222-4333-8444-555555555555"}`
	claudeNativeHelperCoreBetas = "oauth-2025-04-20,interleaved-thinking-2025-05-14,redact-thinking-2026-02-12,thinking-token-count-2026-05-13,context-management-2025-06-27,prompt-caching-scope-2026-01-05"
)

func claudeNativeHelperHeaders(betas, compression string) http.Header {
	headers := http.Header{
		"Accept":            {"application/json"},
		"Accept-Encoding":   {compression},
		"Content-Type":      {"application/json"},
		"User-Agent":        {"claude-cli/2.1.325 (external, cli)"},
		"X-App":             {"cli"},
		"Anthropic-Beta":    {betas},
		"Anthropic-Version": {"2023-06-01"},
		"Anthropic-Dangerous-Direct-Browser-Access": {"true"},
		"X-Claude-Code-Session-Id":                  {claudeNativeHelperSessionID},
		"X-Client-Request-Id":                       {"66666666-7777-4888-8999-aaaaaaaaaaaa"},
		"X-Stainless-Lang":                          {"js"},
		"X-Stainless-Runtime":                       {"node"},
		"X-Stainless-Package-Version":               {"0.128.0"},
		"X-Stainless-Runtime-Version":               {"v26.12.1"},
		"X-Stainless-OS":                            {"MacOS"},
		"X-Stainless-Arch":                          {"arm64"},
		"X-Stainless-Retry-Count":                   {"0"},
		"X-Stainless-Timeout":                       {"600"},
	}
	canonical := make(http.Header, len(headers))
	for name, values := range headers {
		for _, value := range values {
			canonical.Add(name, value)
		}
	}
	return canonical
}

func claudeNativeHelperOAuthAuth(baseURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID: "native-helper-oauth",
		Attributes: map[string]string{
			"api_key":  "sk-ant-oat-native-helper",
			"base_url": baseURL,
		},
		Metadata: claudeOAuthTestMetadata(),
	}
}

func TestApplyClaudeHeadersPreservesCallerAsyncWithoutFingerprintOptIn(t *testing.T) {
	for _, test := range []struct {
		name      string
		confirmed bool
		profile   bool
		wantAsync string
	}{
		{name: "confirmed native", confirmed: true, wantAsync: "async"},
		{name: "unconfirmed caller default", wantAsync: "async"},
		{name: "unconfirmed caller profile", profile: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, errRequest := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", nil)
			if errRequest != nil {
				t.Fatal(errRequest)
			}
			incoming := http.Header{"X-Stainless-Async": {"async"}}
			auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-api-key"}}
			if test.profile {
				auth.Attributes["fingerprint_profile"] = "claude-code-cli"
			}
			if errHeaders := applyClaudeHeaders(
				request,
				auth,
				"test-api-key",
				true,
				nil,
				[]byte(`{"model":"claude-haiku-4-5-20251001"}`),
				&config.Config{},
				incoming,
				test.confirmed,
				claudeNativeHelperSessionID,
			); errHeaders != nil {
				t.Fatalf("applyClaudeHeaders() error = %v", errHeaders)
			}
			if got := request.Header.Get("X-Stainless-Async"); got != test.wantAsync {
				t.Fatalf("X-Stainless-Async = %q, want %q", got, test.wantAsync)
			}
		})
	}
}

func TestClaudeExecutorMinimalNativeHelperPreservesMarkerlessWire(t *testing.T) {
	var upstreamBody []byte
	var upstreamHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamBody, _ = io.ReadAll(r.Body)
		upstreamHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-4-5-20251001","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	payload := []byte(`{"model":"claude-haiku-4-5-20251001","max_tokens":1,"messages":[{"role":"user","content":"helper probe"}],"metadata":{"user_id":"` + strings.ReplaceAll(claudeNativeHelperUserID, `"`, `\"`) + `"}}`)
	headers := claudeNativeHelperHeaders(claudeNativeHelperCoreBetas, "gzip, deflate, br, zstd")
	executor := NewClaudeExecutor(&config.Config{})
	_, errExecute := executor.Execute(context.Background(), claudeNativeHelperOAuthAuth(server.URL), cliproxyexecutor.Request{
		Model:   "claude-haiku-4-5-20251001",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatClaude,
		OriginalRequest: payload,
		Headers:         headers,
	})
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}

	for _, path := range []string{"system", "stream", "context_management", "output_config"} {
		if got := gjson.GetBytes(upstreamBody, path); got.Exists() {
			t.Fatalf("helper body unexpectedly contains %s=%s: %s", path, got.Raw, upstreamBody)
		}
	}
	if bytes.Contains(upstreamBody, []byte(`"cache_control"`)) {
		t.Fatalf("helper body unexpectedly contains cache_control: %s", upstreamBody)
	}
	if got := gjson.GetBytes(upstreamBody, "messages.0.content").String(); got != "helper probe" {
		t.Fatalf("messages.0.content = %q, want preserved string", got)
	}
	if !bytes.HasPrefix(upstreamBody, []byte(`{"model":"claude-haiku-4-5-20251001","max_tokens":1,"messages":`)) {
		t.Fatalf("helper top-level order changed: %s", upstreamBody)
	}
	assertClaudeNativeHelperHeaders(t, upstreamHeaders, headers)
}

func TestClaudeExecutorStructuredNativeHelperPreservesStreamProfile(t *testing.T) {
	var upstreamBody []byte
	var upstreamHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamBody, _ = io.ReadAll(r.Body)
		upstreamHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-haiku-4-5-20251001\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer server.Close()

	betas := claudeNativeHelperCoreBetas + ",structured-outputs-2025-12-15"
	payload := []byte(`{"model":"claude-haiku-4-5-20251001","messages":[{"role":"user","content":[{"type":"text","text":"helper probe"}]}],"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.258; cc_entrypoint=cli; cch=00000;"},{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."},{"type":"text","text":"Return a short title."}],"tools":[],"metadata":{"user_id":"` + strings.ReplaceAll(claudeNativeHelperUserID, `"`, `\"`) + `"},"max_tokens":32000,"thinking":{"type":"disabled"},"temperature":1,"output_config":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"title":{"type":"string"}},"required":["title"],"additionalProperties":false}}},"stream":true}`)
	headers := claudeNativeHelperHeaders(betas, "gzip, deflate, br, zstd")
	executor := NewClaudeExecutor(&config.Config{})
	result, errStream := executor.ExecuteStream(context.Background(), claudeNativeHelperOAuthAuth(server.URL), cliproxyexecutor.Request{
		Model:   "claude-haiku-4-5-20251001",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatClaude,
		OriginalRequest: payload,
		Headers:         headers,
	})
	if errStream != nil {
		t.Fatalf("ExecuteStream() error = %v", errStream)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
	}

	if got := gjson.GetBytes(upstreamBody, "system.#").Int(); got != 3 {
		t.Fatalf("system block count = %d, want native 3: %s", got, upstreamBody)
	}
	if !bytes.HasPrefix(upstreamBody, []byte(`{"model":"claude-haiku-4-5-20251001","messages":`)) {
		t.Fatalf("structured helper top-level order changed: %s", upstreamBody)
	}
	for _, path := range []string{"context_management", "output_config.effort"} {
		if got := gjson.GetBytes(upstreamBody, path); got.Exists() {
			t.Fatalf("structured helper unexpectedly contains %s=%s: %s", path, got.Raw, upstreamBody)
		}
	}
	if bytes.Contains(upstreamBody, []byte(`"cache_control"`)) {
		t.Fatalf("structured helper unexpectedly contains cache_control: %s", upstreamBody)
	}
	if got := gjson.GetBytes(upstreamBody, "stream").Bool(); !got {
		t.Fatalf("structured helper stream = false, want true: %s", upstreamBody)
	}
	if got := gjson.GetBytes(upstreamBody, "system.0.text").String(); strings.Contains(got, "cch=00000") || !strings.Contains(got, " cch=") {
		t.Fatalf("structured helper billing CCH was not re-signed: %q", got)
	}
	assertClaudeNativeHelperHeaders(t, upstreamHeaders, headers)
}

func assertClaudeNativeHelperHeaders(t *testing.T, got, incoming http.Header) {
	t.Helper()
	if got.Get("Anthropic-Beta") != incoming.Get("Anthropic-Beta") {
		t.Fatalf("Anthropic-Beta = %q, want exact native helper profile %q", got.Get("Anthropic-Beta"), incoming.Get("Anthropic-Beta"))
	}
	if strings.Contains(got.Get("Anthropic-Beta"), claudeExtendedCacheTTLBeta) || strings.Contains(got.Get("Anthropic-Beta"), claudeCodeBeta) {
		t.Fatalf("Anthropic-Beta gained standard Claude Code cache betas: %q", got.Get("Anthropic-Beta"))
	}
	for _, name := range []string{
		"Accept",
		"Accept-Encoding",
		"Content-Type",
		"User-Agent",
		"X-App",
		"Anthropic-Version",
		"Anthropic-Dangerous-Direct-Browser-Access",
		"X-Claude-Code-Session-Id",
		"X-Client-Request-Id",
		"X-Stainless-Async",
		"X-Stainless-Lang",
		"X-Stainless-Runtime",
		"X-Stainless-Package-Version",
		"X-Stainless-Runtime-Version",
		"X-Stainless-OS",
		"X-Stainless-Arch",
		"X-Stainless-Retry-Count",
		"X-Stainless-Timeout",
	} {
		gotValue := claudeNativeHelperHeaderValue(got, name)
		wantValue := claudeNativeHelperHeaderValue(incoming, name)
		if gotValue != wantValue {
			t.Fatalf("%s = %q, want preserved %q", name, gotValue, wantValue)
		}
	}
}

func claudeNativeHelperHeaderValue(headers http.Header, name string) string {
	for key, values := range headers {
		if strings.EqualFold(key, name) {
			return strings.Join(values, ",")
		}
	}
	return ""
}

// The measured minimal helper has no system field at all, so injecting a billing
// header would itself be the deviation. Keying the fallback on system presence means
// that if a payload rule later attaches a system prompt, the billing header and its
// CCH come back instead of shipping a system block native would never send unsigned.
func TestClaudeBodyNeedsBillingFallbackTracksSystemPresence(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "measured minimal helper has no system",
			body: `{"model":"claude-haiku-4-5-20251001","max_tokens":1,"messages":[{"role":"user","content":"probe"}]}`,
			want: false,
		},
		{
			name: "structured helper carries its own billing header",
			body: `{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.258; cc_entrypoint=cli; cch=00000;"}]}`,
			want: true,
		},
		{
			name: "pipeline attached a system prompt without a billing header",
			body: `{"system":[{"type":"text","text":"injected by a payload rule"}]}`,
			want: true,
		},
		{
			name: "string system prompt also needs the fallback",
			body: `{"system":"injected by a payload rule"}`,
			want: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := claudeBodyNeedsBillingFallback([]byte(test.body)); got != test.want {
				t.Fatalf("claudeBodyNeedsBillingFallback() = %v, want %v", got, test.want)
			}
		})
	}
}

// A 2.1.258 helper that reaches CPA through ANTHROPIC_BASE_URL carries no
// x-client-request-id, because the client attaches it only for a first-party base
// URL. The header must stay absent on a custom upstream instead of being
// synthesized, while a helper that did carry one keeps its own value.
func TestApplyClaudeHeadersHelperRequestIDFollowsUpstreamBase(t *testing.T) {
	const nativeRequestID = "66666666-7777-4888-8999-aaaaaaaaaaaa"
	for _, test := range []struct {
		name          string
		url           string
		incomingID    string
		wantGenerated bool
		wantID        string
	}{
		{name: "first party base without caller id", url: "https://api.anthropic.com/v1/messages?beta=true", wantGenerated: true},
		{name: "custom base without caller id", url: "https://gateway.example.com/v1/messages"},
		{name: "custom base keeps caller id", url: "https://gateway.example.com/v1/messages", incomingID: nativeRequestID, wantID: nativeRequestID},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, errRequest := http.NewRequest(http.MethodPost, test.url, nil)
			if errRequest != nil {
				t.Fatal(errRequest)
			}
			incoming := http.Header{}
			if test.incomingID != "" {
				incoming.Set("X-Client-Request-Id", test.incomingID)
			}
			if errHeaders := applyClaudeHeadersWithNativeProfile(
				request,
				&cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-api-key"}},
				"test-api-key",
				false,
				nil,
				[]byte(`{"model":"claude-haiku-4-5-20251001"}`),
				&config.Config{},
				incoming,
				true,
				true,
				claudeNativeHelperSessionID,
			); errHeaders != nil {
				t.Fatalf("applyClaudeHeadersWithNativeProfile() error = %v", errHeaders)
			}
			got := request.Header.Get("X-Client-Request-Id")
			switch {
			case test.wantID != "":
				if got != test.wantID {
					t.Fatalf("X-Client-Request-Id = %q, want caller value %q", got, test.wantID)
				}
			case test.wantGenerated:
				if _, errParse := uuid.Parse(got); errParse != nil {
					t.Fatalf("X-Client-Request-Id = %q, want a generated UUID", got)
				}
			default:
				if got != "" {
					t.Fatalf("X-Client-Request-Id = %q, want the header to stay absent", got)
				}
			}
		})
	}
}

func TestClaudeExecutor_Native280HaikuTitleHelperPreservesServerSideFallbackBeta(t *testing.T) {
	var seenHeaders http.Header
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		seenHeaders = req.Header.Clone()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"msg_helper","type":"message","model":"claude-haiku-4-5-20251001","role":"assistant","content":[{"type":"text","text":"ok"}]}`)),
			Request:    req,
		}, nil
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
	const titleBetas = claudeNativeHelperCoreBetas + ",structured-outputs-2025-12-15,server-side-fallback-2026-06-01,fallback-credit-2026-06-01,cache-diagnosis-2026-04-07"
	headers := claudeNativeHelperHeaders(titleBetas, "gzip, deflate, br, zstd")
	payload := []byte(`{"model":"claude-haiku-4-5-20251001","max_tokens":80,"messages":[{"role":"user","content":"generate title"}],"metadata":{"user_id":"` + strings.ReplaceAll(claudeNativeHelperUserID, `"`, `\"`) + `"},"output_config":{"format":{"type":"json_schema","schema":{"type":"object"}}}}`)

	_, err := NewClaudeExecutor(&config.Config{}).Execute(ctx, claudeNativeHelperOAuthAuth("https://api.anthropic.com"), cliproxyexecutor.Request{
		Model:   "claude-haiku-4-5-20251001",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatClaude,
		Headers:      headers,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	outBeta := helps.HeaderValueCaseInsensitive(seenHeaders, "Anthropic-Beta")
	if !strings.Contains(outBeta, "server-side-fallback-2026-06-01") {
		t.Fatalf("Anthropic-Beta = %q, want server-side-fallback-2026-06-01 preserved for 2.1.280 title helper", outBeta)
	}
}

// claude325TitleHelperBetas is the anthropic-beta a captured Claude Code
// 2.1.325 session-title helper (claude-haiku-5-5) sent.
const claude325TitleHelperBetas = "oauth-2025-04-20,interleaved-thinking-2025-05-14,redact-thinking-2026-02-12," +
	"context-management-2025-06-27,prompt-caching-scope-2026-01-05,per-turn-control-2026-07-01," +
	"mid-conversation-tool-changes-2026-07-01,tool-provenance-2026-10-26,thinking-token-count-2026-05-13," +
	"mid-conversation-system-clear-at-2026-08-21,claude-code-side-query-2026-09-29,effort-2025-11-24," +
	"structured-outputs-2025-12-15,cache-diagnosis-2026-04-07"

// claude325TitleHelperPayload is a sanitized body with the captured 2.1.325
// title helper's shape: claude-haiku-5-5, the session effort in output_config,
// no thinking, max_tokens 128000.
func claude325TitleHelperPayload() []byte {
	return []byte(`{"model":"claude-haiku-5-5","messages":[{"role":"user","content":[{"type":"text","text":"<session>placeholder</session>"}]}],"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.325.000; cc_entrypoint=cli; cch=00000;"},{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."},{"type":"text","text":"Return a short title."}],"tools":[],"metadata":{"user_id":"` + strings.ReplaceAll(claudeNativeHelperUserID, `"`, `\"`) + `"},"max_tokens":128000,"output_config":{"effort":"medium","format":{"type":"json_schema","schema":{"type":"object","properties":{"title":{"type":"string"}},"required":["title"],"additionalProperties":false}}},"stream":true}`)
}

// A genuine 2.1.325 title helper is confirmed by its measured helper profile;
// with native passthrough off its beta header must still reach Anthropic
// unchanged, effort-2025-11-24 included.
func TestClaudeExecutor_Confirmed325TitleHelperKeepsCapturedBetas(t *testing.T) {
	var upstreamHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		upstreamHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-haiku-5-5\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer server.Close()

	payload := claude325TitleHelperPayload()
	headers := claudeNativeHelperHeaders(claude325TitleHelperBetas, "gzip, deflate, br, zstd")
	if detection := helps.DetectClaudeCodeRequest(headers, payload, false); !detection.Confirmed || !detection.HelperProfile {
		t.Fatalf("detection = %+v, want a confirmed helper profile", detection)
	}
	cfg := &config.Config{}
	cfg.ClaudeNativePassthrough = false
	result, errStream := NewClaudeExecutor(cfg).ExecuteStream(context.Background(), claudeNativeHelperOAuthAuth(server.URL), cliproxyexecutor.Request{
		Model:   "claude-haiku-5-5",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatClaude,
		OriginalRequest: payload,
		Headers:         headers,
	})
	if errStream != nil {
		t.Fatalf("ExecuteStream() error = %v", errStream)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
	}
	if got := upstreamHeaders.Get("Anthropic-Beta"); got != claude325TitleHelperBetas {
		t.Fatalf("Anthropic-Beta =\n%s\nwant captured\n%s", got, claude325TitleHelperBetas)
	}
}
