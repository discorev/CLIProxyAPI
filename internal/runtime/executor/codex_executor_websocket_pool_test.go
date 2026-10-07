package executor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	internalcache "github.com/router-for-me/CLIProxyAPI/v8/internal/cache"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// fakeCodexUpstream emulates the ChatGPT Codex Responses endpoint on both
// transports. Over the websocket it keeps the connection-local "last response"
// that previous_response_id refers to, like the real backend does with
// store=false, so a continuation sent on the wrong socket fails with
// previous_response_not_found.
type fakeCodexUpstream struct {
	t        *testing.T
	server   *httptest.Server
	upgrader websocket.Upgrader

	mu            sync.Mutex
	upgrades      int
	posts         int
	messages      []fakeCodexUpstreamMessage
	conns         map[int]*websocket.Conn
	withReasoning bool
	rejectUpgrade bool
	rateLimits    bool
	toolCalls     bool
	// script, when set, may override the reply to one websocket message.
	script func(conn int, index int, payload []byte) (fakeCodexReply, bool)
	// hold, when set, blocks the reply to a message until it is closed.
	hold func(conn int, index int, payload []byte) <-chan struct{}
}

type fakeCodexUpstreamMessage struct {
	conn    int
	payload []byte
}

type fakeCodexReply struct {
	events      [][]byte
	closeBefore bool
	closeAfter  bool
	forget      bool
	// pauseAfter events are written, then the reply waits for resume.
	pauseAfter int
	resume     <-chan struct{}
}

func newFakeCodexUpstream(t *testing.T) *fakeCodexUpstream {
	t.Helper()
	f := &fakeCodexUpstream{
		t:        t,
		upgrader: websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }},
		conns:    make(map[int]*websocket.Conn),
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeCodexUpstream) handle(w http.ResponseWriter, r *http.Request) {
	if websocket.IsWebSocketUpgrade(r) {
		f.mu.Lock()
		reject := f.rejectUpgrade
		f.mu.Unlock()
		if reject {
			f.mu.Lock()
			f.upgrades++
			f.mu.Unlock()
			http.Error(w, `{"error":{"message":"upgrade refused"}}`, http.StatusForbidden)
			return
		}
		header := http.Header{}
		header.Set("X-Codex-Primary-Used-Percent", "12")
		header.Set("X-Codex-Primary-Window-Minutes", "300")
		header.Set("X-Codex-Primary-Reset-After-Seconds", "100")
		header.Set("X-Codex-Turn-State", "handshake-turn-state")
		conn, errUpgrade := f.upgrader.Upgrade(w, r, header)
		if errUpgrade != nil {
			return
		}
		f.mu.Lock()
		f.upgrades++
		connID := f.upgrades
		f.conns[connID] = conn
		f.mu.Unlock()
		f.serveConn(connID, conn)
		return
	}
	body := new(bytes.Buffer)
	_, _ = body.ReadFrom(r.Body)
	f.mu.Lock()
	f.posts++
	n := f.posts
	f.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Codex-Primary-Used-Percent", "13")
	for _, event := range f.responseEvents(fmt.Sprintf("resp_http_%d", n), n, body.Bytes()) {
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", gjson.GetBytes(event, "type").String(), event)
	}
}

func (f *fakeCodexUpstream) serveConn(connID int, conn *websocket.Conn) {
	defer func() { _ = conn.Close() }()
	lastResponseID := ""
	turn := 0
	for {
		_, payload, errRead := conn.ReadMessage()
		if errRead != nil {
			return
		}
		f.mu.Lock()
		index := len(f.messages)
		f.messages = append(f.messages, fakeCodexUpstreamMessage{conn: connID, payload: bytes.Clone(payload)})
		script := f.script
		hold := f.hold
		f.mu.Unlock()

		if hold != nil {
			if ch := hold(connID, index, payload); ch != nil {
				<-ch
			}
		}
		var reply fakeCodexReply
		scripted := false
		if script != nil {
			reply, scripted = script(connID, index, payload)
		}
		if reply.forget {
			lastResponseID = ""
		}
		if !scripted {
			previous := gjson.GetBytes(payload, "previous_response_id").String()
			if previous != "" && previous != lastResponseID {
				reply.events = [][]byte{[]byte(`{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"previous_response_not_found","message":"Previous response not found."}}`)}
			} else {
				turn++
				lastResponseID = fmt.Sprintf("resp_c%d_t%d", connID, turn)
				reply.events = f.responseEvents(lastResponseID, turn, payload)
			}
		}
		if reply.closeBefore {
			return
		}
		for i, event := range reply.events {
			if reply.resume != nil && i == reply.pauseAfter {
				<-reply.resume
			}
			if errWrite := conn.WriteMessage(websocket.TextMessage, event); errWrite != nil {
				return
			}
		}
		if reply.closeAfter {
			return
		}
	}
}

func (f *fakeCodexUpstream) responseEvents(responseID string, turn int, request []byte) [][]byte {
	f.mu.Lock()
	withReasoning := f.withReasoning
	rateLimits := f.rateLimits
	toolCalls := f.toolCalls
	f.mu.Unlock()
	var items []string
	if withReasoning {
		// Like the real backend, reasoning summaries are only produced when the
		// request asks for them.
		summary := "[]"
		if mode := gjson.GetBytes(request, "reasoning.summary").String(); mode != "" && mode != "none" {
			summary = fmt.Sprintf(`[{"type":"summary_text","text":"thinking %d"}]`, turn)
		}
		items = append(items, fmt.Sprintf(`{"id":"rs_%s","type":"reasoning","summary":%s,"encrypted_content":"%s"}`, responseID, summary, validCodexReasoningEncryptedContentForTestSeed(byte(turn))))
	}
	if toolCalls {
		arguments := strconv.Quote(fmt.Sprintf(`{"cmd":"ls %d"}`, turn))
		items = append(items, fmt.Sprintf(`{"id":"fc_%s","type":"function_call","status":"completed","arguments":%s,"call_id":"call_%s","name":"shell"}`, responseID, arguments, responseID))
	} else {
		items = append(items, fmt.Sprintf(`{"id":"msg_%s","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","annotations":[],"logprobs":[],"text":"answer %d"}]}`, responseID, turn))
	}
	var events [][]byte
	if rateLimits {
		events = append(events, []byte(`{"type":"codex.rate_limits","rate_limits":{"allowed":true,"limit_reached":false,"primary":{"used_percent":42,"window_minutes":300,"reset_after_seconds":600}}}`))
	}
	events = append(events, []byte(fmt.Sprintf(`{"type":"response.created","response":{"id":"%s","status":"in_progress","output":[]}}`, responseID)))
	for i, item := range items {
		events = append(events, []byte(fmt.Sprintf(`{"type":"response.output_item.added","output_index":%d,"item":%s}`, i, item)))
		if strings.Contains(item, `"type":"message"`) {
			events = append(events, []byte(fmt.Sprintf(`{"type":"response.output_text.delta","output_index":%d,"content_index":0,"delta":"answer %d"}`, i, turn)))
		}
		events = append(events, []byte(fmt.Sprintf(`{"type":"response.output_item.done","output_index":%d,"item":%s}`, i, item)))
	}
	events = append(events, []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"%s","status":"completed","output":[%s],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`, responseID, strings.Join(items, ","))))
	return events
}

func (f *fakeCodexUpstream) snapshot() (upgrades int, posts int, messages []fakeCodexUpstreamMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.upgrades, f.posts, append([]fakeCodexUpstreamMessage(nil), f.messages...)
}

func (f *fakeCodexUpstream) closeConn(connID int) {
	f.mu.Lock()
	conn := f.conns[connID]
	f.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

type fakeCodexClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeCodexClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeCodexClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newPooledCodexExecutor(t *testing.T, cfg *config.Config) (*CodexExecutor, *helps.CodexWSPool, *fakeCodexClock) {
	t.Helper()
	if cfg == nil {
		cfg = &config.Config{}
	}
	cfg.DisableImageGeneration = config.DisableImageGenerationAll
	clock := &fakeCodexClock{now: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)}
	pool := helps.NewCodexWSPool(codexHTTPWebsocketPoolSettings(cfg), clock.Now)
	t.Cleanup(func() { pool.CloseAll("test_cleanup") })
	exec := NewCodexExecutor(cfg)
	exec.wsPool = pool
	return exec, pool, clock
}

func newPooledCodexOAuth(id string, baseURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:         id,
		Provider:   "codex",
		Attributes: map[string]string{"base_url": baseURL},
		Metadata:   map[string]any{"access_token": "token-" + id, "email": id + "@example.com", "account_id": "acct-" + id},
	}
}

func responsesRequest(promptCacheKey string, input []string) cliproxyexecutor.Request {
	payload := []byte(`{"model":"gpt-5.4","stream":true,"instructions":"be brief","reasoning":{"effort":"medium","summary":"auto"},"include":["reasoning.encrypted_content"],"store":false}`)
	payload, _ = sjson.SetBytes(payload, "prompt_cache_key", promptCacheKey)
	payload, _ = sjson.SetRawBytes(payload, "input", []byte("["+strings.Join(input, ",")+"]"))
	return cliproxyexecutor.Request{Model: "gpt-5.4", Payload: payload}
}

func responsesOptions(stream bool) cliproxyexecutor.Options {
	return cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Stream: stream}
}

func userInputItem(text string) string {
	return fmt.Sprintf(`{"type":"message","role":"user","content":[{"type":"input_text","text":%q}]}`, text)
}

// codexCLIEcho mimics how the Codex CLI serializes server output items back
// into history: no item ids or status, and output_text parts carry only text.
func codexCLIEcho(t *testing.T, item string) string {
	t.Helper()
	out := []byte(item)
	out, _ = sjson.DeleteBytes(out, "id")
	out, _ = sjson.DeleteBytes(out, "status")
	if gjson.GetBytes(out, "type").String() == "message" {
		for i := range gjson.GetBytes(out, "content").Array() {
			out, _ = sjson.DeleteBytes(out, fmt.Sprintf("content.%d.annotations", i))
			out, _ = sjson.DeleteBytes(out, fmt.Sprintf("content.%d.logprobs", i))
		}
	}
	return string(out)
}

func collectStream(t *testing.T, result *cliproxyexecutor.StreamResult) []byte {
	t.Helper()
	var out bytes.Buffer
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
		out.Write(chunk.Payload)
		out.WriteByte('\n')
	}
	return out.Bytes()
}

// completedOutputFromSSE returns the output items of the response.completed
// event in a downstream Responses SSE stream.
func completedOutputFromSSE(t *testing.T, stream []byte) []string {
	t.Helper()
	scanner := bufio.NewScanner(bytes.NewReader(stream))
	scanner.Buffer(nil, 1<<20)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[5:])
		if gjson.GetBytes(data, "type").String() != "response.completed" {
			continue
		}
		var items []string
		for _, item := range gjson.GetBytes(data, "response.output").Array() {
			items = append(items, item.Raw)
		}
		return items
	}
	t.Fatalf("no response.completed in stream: %s", stream)
	return nil
}

func isIncremental(payload []byte) bool {
	return gjson.GetBytes(payload, "previous_response_id").String() != ""
}

func TestCodexHTTPWebsocketResponsesStreamSendsIncrementalTurns(t *testing.T) {
	upstream := newFakeCodexUpstream(t)
	upstream.withReasoning = true
	exec, pool, _ := newPooledCodexExecutor(t, nil)
	auth := newPooledCodexOAuth("auth-a", upstream.server.URL)
	ctx := context.Background()

	history := []string{userInputItem("q1")}
	for turn := 1; turn <= 3; turn++ {
		result, err := exec.ExecuteStream(ctx, auth, responsesRequest("conv-stream", history), responsesOptions(true))
		if err != nil {
			t.Fatalf("turn %d ExecuteStream() error = %v", turn, err)
		}
		stream := collectStream(t, result)
		if !strings.Contains(string(stream), fmt.Sprintf("answer %d", turn)) {
			t.Fatalf("turn %d stream missing answer: %s", turn, stream)
		}
		for _, item := range completedOutputFromSSE(t, stream) {
			history = append(history, codexCLIEcho(t, item))
		}
		history = append(history, userInputItem(fmt.Sprintf("q%d", turn+1)))
	}

	upgrades, posts, messages := upstream.snapshot()
	if upgrades != 1 || posts != 0 {
		t.Fatalf("upgrades=%d posts=%d, want one websocket and no HTTP POST", upgrades, posts)
	}
	if len(messages) != 3 {
		t.Fatalf("upstream messages = %d, want 3", len(messages))
	}
	if isIncremental(messages[0].payload) {
		t.Fatalf("first turn must be a full request: %s", messages[0].payload)
	}
	for i := 1; i < 3; i++ {
		payload := messages[i].payload
		if !isIncremental(payload) {
			t.Fatalf("turn %d was not incremental: %s", i+1, payload)
		}
		if got := len(gjson.GetBytes(payload, "input").Array()); got != 1 {
			t.Fatalf("turn %d incremental input has %d items, want only the new user message: %s", i+1, got, payload)
		}
		if gjson.GetBytes(payload, "type").String() != "response.create" {
			t.Fatalf("turn %d type = %q", i+1, gjson.GetBytes(payload, "type").String())
		}
		if gjson.GetBytes(payload, "instructions").String() != "be brief" || gjson.GetBytes(payload, "prompt_cache_key").String() != "conv-stream" {
			t.Fatalf("turn %d dropped request properties: %s", i+1, payload)
		}
	}
	stats := pool.Stats()
	if stats.Incremental != 2 || stats.Full != 1 || stats.Dials != 1 {
		t.Fatalf("stats = %+v, want 2 incremental, 1 full, 1 dial", stats)
	}
}

// codexHTTPWebsocketTurn runs one non-stream turn through Execute and returns
// the downstream response payload.
func codexHTTPWebsocketTurn(t *testing.T, exec *CodexExecutor, auth *cliproxyauth.Auth, format string, payload []byte, headers http.Header) []byte {
	t.Helper()
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString(format), Headers: headers}
	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{Model: "gpt-5.4", Payload: payload}, opts)
	if err != nil {
		t.Fatalf("Execute(%s) error = %v", format, err)
	}
	return resp.Payload
}

// TestCodexHTTPWebsocketDeltaHitRateBySourceFormat measures, for each
// downstream format, how many follow-up turns are sent as previous_response_id
// continuations when the client echoes its history the way that format's
// clients do. It documents the expected hit rate rather than forcing one: a
// format whose history cannot round-trip the upstream output exactly must fall
// back to full requests.
func TestCodexHTTPWebsocketDeltaHitRateBySourceFormat(t *testing.T) {
	type scenario struct {
		name          string
		format        string
		withReasoning bool
		toolCalls     bool
		thinking      string // Claude thinking config
		wantDeltas    int    // of 2 follow-up turns
	}
	scenarios := []scenario{
		{name: "responses/reasoning", format: "openai-response", withReasoning: true, wantDeltas: 2},
		{name: "responses/plain", format: "openai-response", wantDeltas: 2},
		{name: "responses/tools", format: "openai-response", withReasoning: true, toolCalls: true, wantDeltas: 2},
		{name: "chat-completions/reasoning", format: "openai", withReasoning: true, wantDeltas: 0},
		{name: "chat-completions/plain", format: "openai", wantDeltas: 2},
		{name: "chat-completions/tools", format: "openai", toolCalls: true, wantDeltas: 2},
		{name: "chat-completions/tools-reasoning", format: "openai", withReasoning: true, toolCalls: true, wantDeltas: 0},
		{name: "claude/reasoning", format: "claude", withReasoning: true, wantDeltas: 2},
		{name: "claude/reasoning-thinking-enabled", format: "claude", withReasoning: true, thinking: `{"type":"enabled","budget_tokens":4096}`, wantDeltas: 2},
		{name: "claude/reasoning-summary", format: "claude", withReasoning: true, thinking: `{"type":"enabled","budget_tokens":4096,"display":"summarized"}`, wantDeltas: 0},
		{name: "claude/plain", format: "claude", wantDeltas: 2},
		{name: "claude/tools", format: "claude", withReasoning: true, toolCalls: true, wantDeltas: 2},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			internalcache.ClearCodexReasoningReplayCache()
			t.Cleanup(internalcache.ClearCodexReasoningReplayCache)
			upstream := newFakeCodexUpstream(t)
			upstream.withReasoning = sc.withReasoning
			upstream.toolCalls = sc.toolCalls
			exec, _, _ := newPooledCodexExecutor(t, nil)
			auth := newPooledCodexOAuth("auth-"+strings.ReplaceAll(sc.name, "/", "-"), upstream.server.URL)

			switch sc.format {
			case "openai-response":
				history := []string{userInputItem("q1")}
				for turn := 1; turn <= 3; turn++ {
					payload := responsesRequest("conv-"+sc.name, history).Payload
					if sc.toolCalls {
						payload, _ = sjson.SetRawBytes(payload, "tools", []byte(`[{"type":"function","name":"shell","description":"run","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}]`))
					}
					out := codexHTTPWebsocketTurn(t, exec, auth, sc.format, payload, nil)
					for _, item := range gjson.GetBytes(out, "output").Array() {
						history = append(history, codexCLIEcho(t, item.Raw))
						if item.Get("type").String() == "function_call" {
							history = append(history, fmt.Sprintf(`{"type":"function_call_output","call_id":%q,"output":"ok"}`, item.Get("call_id").String()))
						}
					}
					if !sc.toolCalls {
						history = append(history, userInputItem(fmt.Sprintf("q%d", turn+1)))
					}
				}
			case "openai":
				messages := []string{`{"role":"user","content":"q1"}`}
				for turn := 1; turn <= 3; turn++ {
					payload := []byte(`{"model":"gpt-5.4","prompt_cache_key":"conv-chat"}`)
					if sc.toolCalls {
						payload, _ = sjson.SetRawBytes(payload, "tools", []byte(`[{"type":"function","function":{"name":"shell","description":"run","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}}]`))
					}
					payload, _ = sjson.SetRawBytes(payload, "messages", []byte("["+strings.Join(messages, ",")+"]"))
					out := codexHTTPWebsocketTurn(t, exec, auth, sc.format, payload, nil)
					message := gjson.GetBytes(out, "choices.0.message")
					assistant := []byte(`{"role":"assistant"}`)
					if content := message.Get("content"); content.Type == gjson.String {
						assistant, _ = sjson.SetBytes(assistant, "content", content.String())
					} else {
						assistant, _ = sjson.SetRawBytes(assistant, "content", []byte("null"))
					}
					calls := message.Get("tool_calls").Array()
					if len(calls) > 0 {
						assistant, _ = sjson.SetRawBytes(assistant, "tool_calls", []byte(message.Get("tool_calls").Raw))
					}
					messages = append(messages, string(assistant))
					for _, call := range calls {
						messages = append(messages, fmt.Sprintf(`{"role":"tool","tool_call_id":%q,"content":"ok"}`, call.Get("id").String()))
					}
					if len(calls) == 0 {
						messages = append(messages, fmt.Sprintf(`{"role":"user","content":"q%d"}`, turn+1))
					}
				}
			case "claude":
				headers := http.Header{}
				headers.Set("X-Claude-Code-Session-Id", uuidForScenario(sc.name))
				messages := []string{`{"role":"user","content":[{"type":"text","text":"q1"}]}`}
				for turn := 1; turn <= 3; turn++ {
					payload := []byte(`{"model":"gpt-5.4","max_tokens":1024}`)
					if sc.thinking != "" {
						payload, _ = sjson.SetRawBytes(payload, "thinking", []byte(sc.thinking))
					}
					if sc.toolCalls {
						payload, _ = sjson.SetRawBytes(payload, "tools", []byte(`[{"name":"shell","description":"run","input_schema":{"type":"object","properties":{"cmd":{"type":"string"}}}}]`))
					}
					payload, _ = sjson.SetRawBytes(payload, "messages", []byte("["+strings.Join(messages, ",")+"]"))
					out := codexHTTPWebsocketTurn(t, exec, auth, sc.format, payload, headers)
					assistant := []byte(`{"role":"assistant"}`)
					assistant, _ = sjson.SetRawBytes(assistant, "content", []byte(gjson.GetBytes(out, "content").Raw))
					messages = append(messages, string(assistant))
					var blocks []string
					for _, block := range gjson.GetBytes(out, "content").Array() {
						if block.Get("type").String() == "tool_use" {
							blocks = append(blocks, fmt.Sprintf(`{"type":"tool_result","tool_use_id":%q,"content":"ok"}`, block.Get("id").String()))
						}
					}
					if len(blocks) == 0 {
						blocks = append(blocks, fmt.Sprintf(`{"type":"text","text":"q%d"}`, turn+1))
					}
					messages = append(messages, `{"role":"user","content":[`+strings.Join(blocks, ",")+`]}`)
				}
			}

			upgrades, posts, messages := upstream.snapshot()
			if posts != 0 || upgrades != 1 || len(messages) != 3 {
				t.Fatalf("upgrades=%d posts=%d messages=%d, want 1/0/3", upgrades, posts, len(messages))
			}
			deltas := 0
			for _, message := range messages[1:] {
				if isIncremental(message.payload) {
					deltas++
				}
			}
			t.Logf("delta hit rate %s: %d/2 follow-up turns incremental", sc.name, deltas)
			if deltas != sc.wantDeltas {
				for i, message := range messages {
					t.Logf("upstream message %d: %s", i, message.payload)
				}
				t.Fatalf("incremental follow-ups = %d, want %d", deltas, sc.wantDeltas)
			}
		})
	}
}

func uuidForScenario(name string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("codex-ws-pool-test:"+name)).String()
}
