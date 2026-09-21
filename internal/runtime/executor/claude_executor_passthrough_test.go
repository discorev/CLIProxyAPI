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

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

const nativePassthroughCallerUserID = `{"device_id":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","account_uuid":"dddddddd-dddd-4ddd-8ddd-dddddddddddd","session_id":"11111111-2222-4333-8444-555555555555"}`

func nativePassthroughHeaders() http.Header {
	return http.Header{
		"User-Agent":                  {"claude-cli/2.1.280 (external, cli)"},
		"X-App":                       {"cli"},
		"Anthropic-Beta":              {"dangerous-tool-use-2026-09-03,caller-open-list-beta"},
		"Anthropic-Version":           {"2023-06-01"},
		"X-Stainless-Package-Version": {"0.999.0"},
		"X-Stainless-Runtime-Version": {"v99.1.2"},
		"X-Stainless-Os":              {"Linux"},
		"X-Stainless-Arch":            {"x64"},
		"X-Stainless-Custom":          {"caller-owned"},
		"X-Client-Request-Id":         {"66666666-7777-4888-8999-aaaaaaaaaaaa"},
		"X-Management-Key":            {"downstream-secret"},
		"Proxy-Authorization":         {"Bearer downstream-proxy-secret"},
		"Connection":                  {"keep-alive, X-Hop-Only"},
		"X-Hop-Only":                  {"remove-me"},
	}
}

func nativePassthroughPayload(stream bool) []byte {
	return []byte(fmt.Sprintf(`{"model":"caller-model","system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.280; cc_entrypoint=cli; cch=00000;","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral","ttl":"5m"}}]}],"metadata":{"user_id":%q,"caller_metadata":"preserve"},"safeguards":{"mode":"auto","open_field":{"future":true}},"thinking":{"type":"adaptive"},"caller_extension":{"nested":[1,2,3]},"stream":%t}`, nativePassthroughCallerUserID, stream))
}

func nativePassthroughOAuthAuth(baseURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID: "native-passthrough-oauth",
		Attributes: map[string]string{
			"api_key":  "sk-ant-oat-native-passthrough",
			"base_url": baseURL,
		},
		Metadata: map[string]any{
			"account_uuid":      "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
			"claude_device_ids": []string{"0000000000000000000000000000000000000000000000000000000000000000"},
		},
	}
}

func assertNativePassthroughOAuthRequest(t *testing.T, body []byte, headers http.Header, wantStream bool) {
	t.Helper()
	if got := headers.Get("User-Agent"); got != "claude-cli/2.1.280 (external, cli)" {
		t.Fatalf("User-Agent = %q, want caller version", got)
	}
	if got := headers.Get("X-Stainless-Package-Version"); got != "0.999.0" {
		t.Fatalf("X-Stainless-Package-Version = %q, want caller value", got)
	}
	if got := headers.Get("X-Stainless-Custom"); got != "caller-owned" {
		t.Fatalf("X-Stainless-Custom = %q, want caller value", got)
	}
	if got := headers.Get("Authorization"); got != "Bearer sk-ant-oat-native-passthrough" {
		t.Fatalf("Authorization = %q, want selected OAuth bearer", got)
	}
	if got := headers.Get("X-Api-Key"); got != "" {
		t.Fatalf("X-Api-Key = %q, want absent for OAuth", got)
	}
	if got := headers.Get("Anthropic-Beta"); got != "dangerous-tool-use-2026-09-03,caller-open-list-beta,oauth-2025-04-20,extended-cache-ttl-2025-04-11" {
		t.Fatalf("Anthropic-Beta = %q, want caller values followed by credential betas", got)
	}
	for _, name := range []string{"X-Management-Key", "Proxy-Authorization", "X-Hop-Only", "Connection"} {
		if got := headers.Get(name); got != "" {
			t.Fatalf("%s = %q, want filtered", name, got)
		}
	}
	if got := headers.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json fallback", got)
	}

	if got := gjson.GetBytes(body, "model").String(); got != "routed-model" {
		t.Fatalf("model = %q, want routed-model", got)
	}
	if got := gjson.GetBytes(body, "safeguards.mode").String(); got != "auto" {
		t.Fatalf("safeguards.mode = %q, want preserved", got)
	}
	if !gjson.GetBytes(body, "safeguards.open_field.future").Bool() {
		t.Fatalf("safeguards open field changed: %s", body)
	}
	if got := gjson.GetBytes(body, "thinking.type").String(); got != "adaptive" {
		t.Fatalf("thinking.type = %q, want adaptive", got)
	}
	if got := gjson.GetBytes(body, "system.0.cache_control.ttl").String(); got != "1h" {
		t.Fatalf("system cache ttl = %q, want 1h", got)
	}
	if got := gjson.GetBytes(body, "messages.0.content.0.cache_control.ttl").String(); got != "5m" {
		t.Fatalf("message cache ttl = %q, want 5m", got)
	}
	if got := gjson.GetBytes(body, "caller_extension.nested.2").Int(); got != 3 {
		t.Fatalf("caller extension changed: %s", body)
	}
	if got := gjson.GetBytes(body, "stream").Bool(); got != wantStream {
		t.Fatalf("stream = %t, want %t", got, wantStream)
	}
	for _, absent := range []string{"max_tokens", "context_management", "diagnostics", "betas"} {
		if got := gjson.GetBytes(body, absent); got.Exists() {
			t.Fatalf("unexpected synthesized %s = %s", absent, got.Raw)
		}
	}
	userID := gjson.GetBytes(body, "metadata.user_id").String()
	if got := gjson.Get(userID, "account_uuid").String(); got != "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" {
		t.Fatalf("metadata account_uuid = %q, want selected credential identity", got)
	}
	if got := gjson.Get(userID, "device_id").String(); got != strings.Repeat("0", 64) {
		t.Fatalf("metadata device_id = %q, want selected credential device", got)
	}
	if got := gjson.GetBytes(body, "metadata.caller_metadata").String(); got != "preserve" {
		t.Fatalf("metadata.caller_metadata = %q, want preserved", got)
	}
	billing := gjson.GetBytes(body, "system.0.text").String()
	if strings.Contains(billing, "cch=00000;") || !strings.Contains(billing, "cch=") {
		t.Fatalf("billing header was not signed: %q", billing)
	}
	resigned, errResign := finalizeAnthropicMessagesBodyCCH(body, "")
	if errResign != nil {
		t.Fatalf("re-finalize CCH: %v", errResign)
	}
	if !bytes.Equal(resigned, body) {
		t.Fatal("CCH was not calculated over the final passthrough body")
	}
}

func TestAppendClaudeOAuthCredentialBetasPreservesCallerList(t *testing.T) {
	if got, want := appendClaudeOAuthCredentialBetas(" first ,oauth-2025-04-20,,first ", true), " first ,oauth-2025-04-20,,first ,extended-cache-ttl-2025-04-11"; got != want {
		t.Fatalf("appendClaudeOAuthCredentialBetas() = %q, want %q", got, want)
	}
	if got, want := appendClaudeOAuthCredentialBetas("", false), "oauth-2025-04-20"; got != want {
		t.Fatalf("appendClaudeOAuthCredentialBetas(empty) = %q, want %q", got, want)
	}
}

func TestPrepareClaudeNativePassthroughRequestSignsExistingBillingBlockWithoutCCH(t *testing.T) {
	executor := NewClaudeExecutor(&config.Config{})
	payload := []byte(`{"model":"caller-model","system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.2.0; cc_entrypoint=cli;"}],"messages":[]}`)
	body, _, errPrepare := executor.prepareClaudeNativePassthroughRequest(
		nativePassthroughOAuthAuth("https://api.anthropic.com"),
		"sk-ant-oat-native-passthrough",
		"https://api.anthropic.com/v1/messages",
		"",
		payload,
		nativePassthroughHeaders(),
		"",
		false,
		true,
	)
	if errPrepare != nil {
		t.Fatalf("prepareClaudeNativePassthroughRequest() error = %v", errPrepare)
	}
	billing := gjson.GetBytes(body, "system.0.text").String()
	if !strings.Contains(billing, "cch=") || strings.Contains(billing, "cch=00000;") {
		t.Fatalf("billing header = %q, want inserted and signed CCH", billing)
	}
	resigned, errResign := finalizeAnthropicMessagesBodyCCH(body, "")
	if errResign != nil {
		t.Fatalf("re-finalize CCH: %v", errResign)
	}
	if !bytes.Equal(resigned, body) {
		t.Fatal("inserted CCH was not signed over the final passthrough body")
	}
}

func TestPrepareClaudeNativePassthroughRequestResignsExistingCCH(t *testing.T) {
	executor := NewClaudeExecutor(&config.Config{})
	payload := []byte(`{"model":"caller-model","system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.2.0; cc_entrypoint=cli; cch=12345;"}],"messages":[]}`)
	want, errWant := finalizeAnthropicMessagesBodyCCH(payload, "")
	if errWant != nil {
		t.Fatalf("finalize expected CCH: %v", errWant)
	}
	body, _, errPrepare := executor.prepareClaudeNativePassthroughRequest(
		nativePassthroughOAuthAuth("https://api.anthropic.com"),
		"sk-ant-oat-native-passthrough",
		"https://api.anthropic.com/v1/messages",
		"",
		payload,
		nativePassthroughHeaders(),
		"",
		false,
		true,
	)
	if errPrepare != nil {
		t.Fatalf("prepareClaudeNativePassthroughRequest() error = %v", errPrepare)
	}
	if !bytes.Equal(body, want) {
		t.Fatalf("prepared body did not re-sign existing CCH\n got: %s\nwant: %s", body, want)
	}
}

func TestPrepareClaudeNativePassthroughRequestDoesNotSynthesizeBillingBlock(t *testing.T) {
	executor := NewClaudeExecutor(&config.Config{})
	for _, test := range []struct {
		name    string
		payload []byte
	}{
		{name: "no system", payload: []byte(`{"model":"caller-model","messages":[]}`)},
		{name: "non-billing system", payload: []byte(`{"model":"caller-model","system":[{"type":"text","text":"caller system"}],"messages":[]}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			body, _, errPrepare := executor.prepareClaudeNativePassthroughRequest(
				nativePassthroughOAuthAuth("https://api.anthropic.com"),
				"sk-ant-oat-native-passthrough",
				"https://api.anthropic.com/v1/messages",
				"",
				test.payload,
				nativePassthroughHeaders(),
				"",
				false,
				true,
			)
			if errPrepare != nil {
				t.Fatalf("prepareClaudeNativePassthroughRequest() error = %v", errPrepare)
			}
			if !bytes.Equal(body, test.payload) {
				t.Fatalf("body changed without a billing block\n got: %s\nwant: %s", body, test.payload)
			}
		})
	}
}

func TestClaudeExecutorNativePassthroughExecute(t *testing.T) {
	var upstreamBody []byte
	var upstreamHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamBody, _ = io.ReadAll(r.Body)
		upstreamHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_passthrough","type":"message","model":"routed-model","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{ClaudeNativePassthrough: true})
	executor.upstreamModelNormalizer = func(string) string { return "routed-model" }
	payload := nativePassthroughPayload(false)
	_, errExecute := executor.Execute(context.Background(), nativePassthroughOAuthAuth(server.URL), cliproxyexecutor.Request{
		Model:   "caller-model",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatClaude,
		OriginalRequest: payload,
		Headers:         nativePassthroughHeaders(),
	})
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	assertNativePassthroughOAuthRequest(t, upstreamBody, upstreamHeaders, false)
}

func TestClaudeExecutorNativePassthroughExecuteStream(t *testing.T) {
	var upstreamBody []byte
	var upstreamHeaders http.Header
	const safeguardDelta = `{"type":"message_delta","delta":{"stop_reason":"end_turn","safeguard_results":{"dangerous_tool_use":{"allowed":true}}},"usage":{"output_tokens":1}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamBody, _ = io.ReadAll(r.Body)
		upstreamHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_start\n" + `data: {"type":"message_start","message":{"id":"msg_stream","type":"message","model":"routed-model","role":"assistant","content":[],"usage":{"input_tokens":2,"output_tokens":0}}}` + "\n\n"))
		_, _ = w.Write([]byte("event: message_delta\ndata: " + safeguardDelta + "\n\n"))
		_, _ = w.Write([]byte("event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n"))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{ClaudeNativePassthrough: true})
	executor.upstreamModelNormalizer = func(string) string { return "routed-model" }
	payload := nativePassthroughPayload(true)
	result, errStream := executor.ExecuteStream(context.Background(), nativePassthroughOAuthAuth(server.URL), cliproxyexecutor.Request{
		Model:   "caller-model",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatClaude,
		OriginalRequest: payload,
		Headers:         nativePassthroughHeaders(),
	})
	if errStream != nil {
		t.Fatalf("ExecuteStream() error = %v", errStream)
	}
	var output []byte
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		output = append(output, chunk.Payload...)
	}
	if !bytes.Contains(output, []byte(safeguardDelta)) {
		t.Fatalf("stream safeguard delta changed\n got: %s\nwant event data: %s", output, safeguardDelta)
	}
	assertNativePassthroughOAuthRequest(t, upstreamBody, upstreamHeaders, true)
}

func TestClaudeExecutorNativePassthroughCountTokens(t *testing.T) {
	var upstreamBody []byte
	var upstreamHeaders http.Header
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		upstreamBody, _ = io.ReadAll(req.Body)
		upstreamHeaders = req.Header.Clone()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"input_tokens":17}`)),
			Request:    req,
		}, nil
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
	headers := nativePassthroughHeaders()
	payload := []byte(`{"model":"caller-model","messages":[{"role":"user","content":"hello"}],"tools":[],"safeguards":{"mode":"auto"},"thinking":{"type":"adaptive"},"caller_extension":{"future":true}}`)
	executor := NewClaudeExecutor(&config.Config{ClaudeNativePassthrough: true})
	executor.upstreamModelNormalizer = func(string) string { return "routed-model" }
	_, errCount := executor.countTokensUpstream(ctx, nativePassthroughOAuthAuth(""), cliproxyexecutor.Request{
		Model:   "caller-model",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatClaude,
		OriginalRequest: payload,
		Headers:         headers,
	})
	if errCount != nil {
		t.Fatalf("countTokensUpstream() error = %v", errCount)
	}
	if got := gjson.GetBytes(upstreamBody, "model").String(); got != "routed-model" {
		t.Fatalf("count_tokens model = %q, want routed-model", got)
	}
	for path, want := range map[string]string{
		"safeguards.mode":         "auto",
		"thinking.type":           "adaptive",
		"caller_extension.future": "true",
	} {
		got := gjson.GetBytes(upstreamBody, path)
		if got.String() != want {
			t.Fatalf("count_tokens %s = %s, want %s", path, got.Raw, want)
		}
	}
	for _, absent := range []string{"metadata", "system", "context_management", "diagnostics", "betas"} {
		if got := gjson.GetBytes(upstreamBody, absent); got.Exists() {
			t.Fatalf("count_tokens unexpected %s = %s", absent, got.Raw)
		}
	}
	if got := strings.Join(upstreamHeaders["anthropic-beta"], ","); got != "dangerous-tool-use-2026-09-03,caller-open-list-beta,oauth-2025-04-20" {
		t.Fatalf("count_tokens Anthropic-Beta = %q", got)
	}
	if got := upstreamHeaders.Get("User-Agent"); got != headers.Get("User-Agent") {
		t.Fatalf("count_tokens User-Agent = %q, want %q", got, headers.Get("User-Agent"))
	}
	if got := upstreamHeaders.Get("X-Stainless-Custom"); got != "caller-owned" {
		t.Fatalf("count_tokens X-Stainless-Custom = %q, want caller value", got)
	}
}

func TestClaudeExecutorNativePassthroughDisabledUsesExistingPipeline(t *testing.T) {
	var upstreamHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		upstreamHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_disabled","type":"message","model":"caller-model","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	payload := nativePassthroughPayload(false)
	executor := NewClaudeExecutor(&config.Config{ClaudeNativePassthrough: false})
	_, errExecute := executor.Execute(context.Background(), nativePassthroughOAuthAuth(server.URL), cliproxyexecutor.Request{
		Model:   "caller-model",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatClaude,
		OriginalRequest: payload,
		Headers:         nativePassthroughHeaders(),
	})
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	if got := upstreamHeaders.Get("User-Agent"); got != "claude-cli/2.1.258 (external, cli)" {
		t.Fatalf("disabled passthrough User-Agent = %q, want baseline profile", got)
	}
}

func TestClaudeExecutorNativePassthroughAPIKeyKeepsBodyIdentity(t *testing.T) {
	var upstreamBody []byte
	var upstreamHeaders http.Header
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		upstreamBody, _ = io.ReadAll(req.Body)
		upstreamHeaders = req.Header.Clone()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"msg_api_key","type":"message","model":"caller-model","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)),
			Request:    req,
		}, nil
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
	headers := nativePassthroughHeaders()
	payload := nativePassthroughPayload(false)
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":   "direct-anthropic-api-key",
		"auth_kind": cliproxyauth.AuthKindAPIKey,
	}}
	executor := NewClaudeExecutor(&config.Config{ClaudeNativePassthrough: true})
	_, errExecute := executor.Execute(ctx, auth, cliproxyexecutor.Request{Model: "caller-model", Payload: payload}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatClaude,
		OriginalRequest: payload,
		Headers:         headers,
	})
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	if got := upstreamHeaders.Get("X-Api-Key"); got != "direct-anthropic-api-key" {
		t.Fatalf("X-Api-Key = %q, want selected API key", got)
	}
	if got := upstreamHeaders.Get("Authorization"); got != "" {
		t.Fatalf("Authorization = %q, want absent", got)
	}
	if got := strings.Join(upstreamHeaders["anthropic-beta"], ","); got != headers.Get("Anthropic-Beta") {
		t.Fatalf("Anthropic-Beta = %q, want caller list %q", got, headers.Get("Anthropic-Beta"))
	}
	if got := gjson.GetBytes(upstreamBody, "metadata.user_id").String(); got != nativePassthroughCallerUserID {
		t.Fatalf("metadata.user_id = %q, want caller identity unchanged", got)
	}
	if got := gjson.GetBytes(upstreamBody, "system.0.text").String(); !strings.Contains(got, "cch=00000;") {
		t.Fatalf("API-key CCH changed unexpectedly: %q", got)
	}
}
