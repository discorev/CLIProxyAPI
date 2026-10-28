package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
	"github.com/tidwall/gjson"
)

const codexAccessProgramTestPayload = `{"model":"gpt-6-sol-daybreak-blue","access_programs":{"cyber":"standard"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`

// registerCodexAccessProgramVariant registers gpt-6-sol and its Daybreak Blue
// variant for a test credential, the way model registration does.
func registerCodexAccessProgramVariant(t *testing.T, auth *cliproxyauth.Auth) {
	t.Helper()
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{
		{ID: "gpt-6-sol"},
		{ID: "gpt-6-sol-daybreak-blue", MetadataModelID: "gpt-6-sol", AccessProgram: "daybreak_blue"},
	})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
}

// The variant's program is built-in translation: it replaces the client's
// value, and user payload rules still apply after it.
func TestCodexExecutorSendsVariantAccessProgramBeforePayloadRules(t *testing.T) {
	overrideRed := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{
		Models: []config.PayloadModelRule{{Name: "gpt-6-sol", Protocol: "codex"}},
		Params: map[string]any{"access_programs.cyber": "daybreak_red"},
	}}}}
	for _, tt := range []struct {
		name   string
		cfg    *config.Config
		stream bool
		want   string
	}{
		{name: "execute", cfg: &config.Config{}, want: "daybreak_blue"},
		{name: "stream", cfg: &config.Config{}, stream: true, want: "daybreak_blue"},
		{name: "execute payload rule wins", cfg: overrideRed, want: "daybreak_red"},
		{name: "stream payload rule wins", cfg: overrideRed, stream: true, want: "daybreak_red"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			var upstreamBody []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				upstreamBody = body
				mu.Unlock()
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"gpt-6-sol\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
			}))
			t.Cleanup(server.Close)
			auth := codexOAuthTestAuth(server.URL)
			auth.ID = "codex-access-program-http-" + tt.name
			registerCodexAccessProgramVariant(t, auth)

			executor := NewCodexExecutor(tt.cfg)
			req := cliproxyexecutor.Request{Model: "gpt-6-sol-daybreak-blue", Payload: []byte(codexAccessProgramTestPayload)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Stream: tt.stream}
			if tt.stream {
				result, errStream := executor.ExecuteStream(context.Background(), auth, req, opts)
				if errStream != nil {
					t.Fatalf("ExecuteStream: %v", errStream)
				}
				for range result.Chunks {
				}
			} else if _, errExecute := executor.Execute(context.Background(), auth, req, opts); errExecute != nil {
				t.Fatalf("Execute: %v", errExecute)
			}

			mu.Lock()
			defer mu.Unlock()
			if got := gjson.GetBytes(upstreamBody, "model").String(); got != "gpt-6-sol" {
				t.Fatalf("upstream model = %q, want the base model", got)
			}
			if got := gjson.GetBytes(upstreamBody, "access_programs.cyber").String(); got != tt.want {
				t.Fatalf("upstream access_programs.cyber = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCodexExecutorWarnsWhenAccessProgramIsDowngraded(t *testing.T) {
	hook := test.NewGlobal()
	t.Cleanup(hook.Reset)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"access_programs\":{\"cyber\":\"standard\"}}}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"gpt-6-sol\",\"access_programs\":{\"cyber\":\"standard\"},\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
	}))
	t.Cleanup(server.Close)
	auth := codexOAuthTestAuth(server.URL)
	auth.ID = "codex-access-program-downgrade-test"
	registerCodexAccessProgramVariant(t, auth)

	result, errStream := NewCodexExecutor(&config.Config{}).ExecuteStream(context.Background(), auth,
		cliproxyexecutor.Request{Model: "gpt-6-sol-daybreak-blue", Payload: []byte(codexAccessProgramTestPayload)},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Stream: true})
	if errStream != nil {
		t.Fatalf("ExecuteStream: %v", errStream)
	}
	for range result.Chunks {
	}
	warnings := 0
	for _, entry := range hook.AllEntries() {
		if entry.Level == log.WarnLevel && entry.Message == "codex access program not honoured" && entry.Data["auth_id"] == auth.ID {
			warnings++
			if entry.Data["access_program"] != "daybreak_blue" || entry.Data["served_access_program"] != "standard" {
				t.Fatalf("warning fields = %v", entry.Data)
			}
		}
	}
	if warnings != 1 {
		t.Fatalf("downgrade warnings = %d, want 1", warnings)
	}
}

// Client websocket turns, including client-sent previous_response_id
// continuations, carry the variant's program and the base model.
func TestCodexWebsocketsSendVariantAccessProgram(t *testing.T) {
	var mu sync.Mutex
	var frames []string
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, errUpgrade := upgrader.Upgrade(w, r, nil)
		if errUpgrade != nil {
			t.Errorf("upgrade websocket: %v", errUpgrade)
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			_, payload, errRead := conn.ReadMessage()
			if errRead != nil {
				return
			}
			mu.Lock()
			frames = append(frames, gjson.GetBytes(payload, "model").String()+" "+gjson.GetBytes(payload, "access_programs.cyber").String())
			mu.Unlock()
			completed := []byte(`{"type":"response.completed","response":{"id":"resp-1","object":"response","status":"completed","model":"gpt-6-sol","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)
			if errWrite := conn.WriteMessage(websocket.TextMessage, completed); errWrite != nil {
				return
			}
		}
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	const executionSessionID = "ws-access-program"
	t.Cleanup(func() { exec.CloseExecutionSession(executionSessionID) })
	auth := codexOAuthTestAuth(server.URL)
	auth.ID = "codex-access-program-ws-test"
	registerCodexAccessProgramVariant(t, auth)
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Metadata:     map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: executionSessionID},
	}
	req := cliproxyexecutor.Request{Model: "gpt-6-sol-daybreak-blue", Payload: []byte(codexAccessProgramTestPayload)}
	if _, errExecute := exec.Execute(context.Background(), auth, req, opts); errExecute != nil {
		t.Fatalf("Execute: %v", errExecute)
	}
	req.Payload = []byte(`{"model":"gpt-6-sol-daybreak-blue","previous_response_id":"resp-1","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"again"}]}]}`)
	opts.Stream = true
	result, errStream := exec.ExecuteStream(context.Background(), auth, req, opts)
	if errStream != nil {
		t.Fatalf("ExecuteStream: %v", errStream)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(frames) != 2 || frames[0] != "gpt-6-sol daybreak_blue" || frames[1] != "gpt-6-sol daybreak_blue" {
		t.Fatalf("frames = %q, want both turns on gpt-6-sol with daybreak_blue", frames)
	}
}

// runCodexAccessProgramDuplex opens a duplex session for model with initial,
// sends followUp after the first response completes, and returns the frames
// upstream received ("model program") and the stream error, if any.
func runCodexAccessProgramDuplex(t *testing.T, authID, model, initial, followUp string) ([]string, error) {
	t.Helper()
	var mu sync.Mutex
	var frames []string
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, errUpgrade := upgrader.Upgrade(w, r, nil)
		if errUpgrade != nil {
			t.Errorf("upgrade websocket: %v", errUpgrade)
			return
		}
		defer func() { _ = conn.Close() }()
		for turn := 1; ; turn++ {
			_, payload, errRead := conn.ReadMessage()
			if errRead != nil {
				return
			}
			mu.Lock()
			frames = append(frames, gjson.GetBytes(payload, "model").String()+" "+gjson.GetBytes(payload, "access_programs.cyber").String())
			mu.Unlock()
			id := fmt.Sprintf("resp-%d", turn)
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"`+id+`","output":[]}}`))
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"`+id+`","output":[]}}`))
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	input := make(chan cliproxyexecutor.WebsocketInput, 1)
	ctx = cliproxyexecutor.WithWebsocketInput(cliproxyexecutor.WithDownstreamWebsocket(ctx), input)
	exec := NewCodexWebsocketsExecutor(&config.Config{Codex: config.CodexConfig{ResponseSteering: true}})
	exec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
	auth := &cliproxyauth.Auth{ID: authID, Provider: "codex", Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL, "websockets": "true"}}
	registerCodexAccessProgramVariant(t, auth)
	opts := cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("codex"),
		OriginalRequest: []byte(initial),
		Metadata:        map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: authID},
	}
	result, errStream := exec.ExecuteStream(ctx, auth, cliproxyexecutor.Request{Model: model, Payload: []byte(initial)}, opts)
	if errStream != nil {
		t.Fatalf("ExecuteStream: %v", errStream)
	}
	var streamErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
			break
		}
		switch gjson.GetBytes(chunk.Payload, "type").String() + " " + gjson.GetBytes(chunk.Payload, "response.id").String() {
		case "response.completed resp-1":
			input <- cliproxyexecutor.WebsocketInput{Payload: []byte(followUp)}
		case "response.completed resp-2":
			cancel()
		}
	}
	for range result.Chunks {
	}
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), frames...), streamErr
}

// A duplex follow-up asking for a program the session's credential was not
// selected for is replayed, so the handler routes it to a credential that lists it.
func TestCodexDuplexFollowUpWithNewAccessProgramRequiresReplay(t *testing.T) {
	frames, errStream := runCodexAccessProgramDuplex(t, "codex-access-program-duplex-base", "gpt-6-sol",
		`{"model":"gpt-6-sol","input":[]}`,
		`{"type":"response.create","model":"gpt-6-sol","previous_response_id":"resp-1","access_programs":{"cyber":"daybreak_blue"},"input":[]}`)
	if !cliproxyexecutor.IsUpstreamWebsocketReplayRequired(errStream) {
		t.Fatalf("stream error = %v, want replay required", errStream)
	}
	if len(frames) != 1 || frames[0] != "gpt-6-sol " {
		t.Fatalf("frames = %q, want only the standard initial turn", frames)
	}
}

// On a variant session, follow-ups for the base model keep the variant's program.
func TestCodexDuplexVariantFollowUpKeepsAccessProgram(t *testing.T) {
	frames, errStream := runCodexAccessProgramDuplex(t, "codex-access-program-duplex-variant", "gpt-6-sol-daybreak-blue",
		`{"model":"gpt-6-sol","access_programs":{"cyber":"daybreak_blue"},"input":[]}`,
		`{"type":"response.create","model":"gpt-6-sol","previous_response_id":"resp-1","access_programs":{"cyber":"daybreak_blue"},"input":[]}`)
	if errStream != nil && !errors.Is(errStream, context.Canceled) {
		t.Fatalf("stream error = %v", errStream)
	}
	if len(frames) != 2 || frames[0] != "gpt-6-sol daybreak_blue" || frames[1] != "gpt-6-sol daybreak_blue" {
		t.Fatalf("frames = %q, want both turns on gpt-6-sol with daybreak_blue", frames)
	}
}
