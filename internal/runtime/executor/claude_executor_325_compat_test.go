package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// Captured from the official Linux x64 2.1.325 binary using an isolated home and
// a mock API. The fixture removes auth, replaces generated IDs, and trims prompt
// text while retaining the request fields and client headers.
func TestClaudeExecutor325CapturedStreamingRequest(t *testing.T) {
	raw, errRead := os.ReadFile("testdata/claude_code_2_1_325.json")
	if errRead != nil {
		t.Fatal(errRead)
	}
	var fixture struct {
		Headers map[string]string
		Body    json.RawMessage
	}
	if errDecode := json.Unmarshal(raw, &fixture); errDecode != nil {
		t.Fatal(errDecode)
	}
	headers := make(http.Header)
	for key, value := range fixture.Headers {
		headers.Set(key, value)
	}
	if detection := helps.DetectClaudeCodeRequest(headers, fixture.Body, false); !detection.NativePassthrough {
		t.Fatalf("captured client is not eligible for native passthrough: %#v", detection)
	}
	const events = `event: message_start
data: {"type":"message_start","message":{"id":"msg_325","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"compatibility-ok"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}

`
	var sentBody []byte
	var sentHeaders http.Header
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		sentBody, _ = io.ReadAll(req.Body)
		sentHeaders = req.Header.Clone()
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(events)), Request: req}, nil
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
	executor := NewClaudeExecutor(&config.Config{ClaudeNativePassthrough: true})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "local-test-key", "auth_kind": cliproxyauth.AuthKindAPIKey}}
	result, errStream := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{Model: "claude-sonnet-4-6", Payload: fixture.Body}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, OriginalRequest: fixture.Body, Headers: headers})
	if errStream != nil {
		t.Fatal(errStream)
	}
	var output []byte
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		output = append(output, chunk.Payload...)
	}
	if !bytes.Contains(output, []byte("compatibility-ok")) {
		t.Fatalf("stream lost response text: %s", output)
	}
	var want, got any
	if errDecode := json.Unmarshal(fixture.Body, &want); errDecode != nil {
		t.Fatal(errDecode)
	}
	if errDecode := json.Unmarshal(sentBody, &got); errDecode != nil {
		t.Fatal(errDecode)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("captured request changed during passthrough: %s", sentBody)
	}
	for _, name := range []string{"User-Agent", "X-Stainless-Package-Version", "X-Stainless-Runtime-Version", "Anthropic-Version"} {
		if got := helps.HeaderValueCaseInsensitive(sentHeaders, name); got != headers.Get(name) {
			t.Errorf("%s = %q, want %q", name, got, headers.Get(name))
		}
	}
	if got := helps.HeaderValueCaseInsensitive(sentHeaders, "Anthropic-Beta"); got != headers.Get("Anthropic-Beta") {
		t.Errorf("caller betas changed: %q", got)
	}
}
