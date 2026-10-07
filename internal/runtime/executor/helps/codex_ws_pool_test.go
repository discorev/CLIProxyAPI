package helps

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

const codexWSTestBaseBody = `{"model":"gpt-5.4","instructions":"be brief","tools":[{"type":"function","name":"shell"}],"reasoning":{"effort":"medium"},"store":false,"stream":true,"prompt_cache_key":"conv","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"q1"}]}]}`

var codexWSTestOutput = [][]byte{
	[]byte(`{"id":"rs_1","type":"reasoning","summary":[],"encrypted_content":"enc-1"}`),
	[]byte(`{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","annotations":[],"logprobs":[],"text":"answer"}]}`),
}

// codexWSTestNextBody echoes the output the way the Codex CLI does (no ids,
// status or empty arrays) and appends a new user message.
func codexWSTestNextBody(t *testing.T, base string, mutate func(string) string) []byte {
	t.Helper()
	echo := `{"type":"reasoning","summary":[],"encrypted_content":"enc-1"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}`
	next := `{"type":"message","role":"user","content":[{"type":"input_text","text":"q2"}]}`
	body := strings.Replace(base, `"text":"q1"}]}]`, `"text":"q1"}]},`+echo+`,`+next+`]`, 1)
	if mutate != nil {
		body = mutate(body)
	}
	return []byte(body)
}

func TestCodexWSIncrementalItems(t *testing.T) {
	baseline := newCodexWSBaseline(newCodexWSRequestShape([]byte(codexWSTestBaseBody)), codexWSTestOutput, "resp_1")
	if baseline == nil {
		t.Fatal("baseline is nil")
	}
	tests := []struct {
		name   string
		mutate func(string) string
		want   bool
	}{
		{name: "codex cli echo plus new item", want: true},
		{name: "client metadata and stream options do not matter", want: true, mutate: func(body string) string {
			return strings.Replace(body, `"store":false`, `"store":false,"client_metadata":{"x-codex-turn-metadata":"t2"},"stream_options":{"include_obfuscation":false},"type":"response.create"`, 1)
		}},
		{name: "property key order does not matter", want: true, mutate: func(body string) string {
			return strings.Replace(body, `"store":false,"stream":true`, `"stream":true,"store":false`, 1)
		}},
		{name: "instructions changed", mutate: func(body string) string {
			return strings.Replace(body, `be brief`, `be verbose`, 1)
		}},
		{name: "tools changed", mutate: func(body string) string {
			return strings.Replace(body, `"name":"shell"`, `"name":"exec"`, 1)
		}},
		{name: "reasoning effort changed", mutate: func(body string) string {
			return strings.Replace(body, `"effort":"medium"`, `"effort":"high"`, 1)
		}},
		{name: "assistant text edited", mutate: func(body string) string {
			return strings.Replace(body, `"text":"answer"`, `"text":"edited"`, 1)
		}},
		{name: "reasoning dropped from history", mutate: func(body string) string {
			return strings.Replace(body, `{"type":"reasoning","summary":[],"encrypted_content":"enc-1"},`, ``, 1)
		}},
		{name: "encrypted reasoning changed", mutate: func(body string) string {
			return strings.Replace(body, `"encrypted_content":"enc-1"`, `"encrypted_content":"enc-2"`, 1)
		}},
		{name: "first user message edited", mutate: func(body string) string {
			return strings.Replace(body, `"text":"q1"`, `"text":"q1!"`, 1)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shape := newCodexWSRequestShape(codexWSTestNextBody(t, codexWSTestBaseBody, tt.mutate))
			items, ok := codexWSIncrementalItems(shape, baseline)
			if ok != tt.want {
				t.Fatalf("incremental = %t, want %t", ok, tt.want)
			}
			if ok && (len(items) != 1 || !strings.Contains(items[0], `"text":"q2"`)) {
				t.Fatalf("incremental items = %v, want only the new user message", items)
			}
		})
	}

	t.Run("no new items", func(t *testing.T) {
		body := strings.Replace(string(codexWSTestNextBody(t, codexWSTestBaseBody, nil)), `,{"type":"message","role":"user","content":[{"type":"input_text","text":"q2"}]}`, ``, 1)
		if _, ok := codexWSIncrementalItems(newCodexWSRequestShape([]byte(body)), baseline); ok {
			t.Fatal("a request that adds nothing must be sent in full")
		}
	})
	t.Run("missing response id", func(t *testing.T) {
		if newCodexWSBaseline(newCodexWSRequestShape([]byte(codexWSTestBaseBody)), codexWSTestOutput, "") != nil {
			t.Fatal("a baseline without a response id must not be recorded")
		}
	})
	t.Run("invalid body", func(t *testing.T) {
		if shape := newCodexWSRequestShape([]byte(`{"model":"gpt-5.4","input":"text"}`)); shape.valid {
			t.Fatal("string input must not produce a comparable shape")
		}
	})
}

func TestBuildCodexWSIncrementalBody(t *testing.T) {
	body, err := buildCodexWSIncrementalBody(codexWSTestNextBody(t, codexWSTestBaseBody, nil), "resp_1", []string{`{"type":"message","role":"user","content":[{"type":"input_text","text":"q2"}]}`})
	if err != nil {
		t.Fatalf("buildCodexWSIncrementalBody() error = %v", err)
	}
	if got := gjson.GetBytes(body, "previous_response_id").String(); got != "resp_1" {
		t.Fatalf("previous_response_id = %q", got)
	}
	if input := gjson.GetBytes(body, "input").Array(); len(input) != 1 || input[0].Get("content.0.text").String() != "q2" {
		t.Fatalf("input = %s", gjson.GetBytes(body, "input").Raw)
	}
	if gjson.GetBytes(body, "instructions").String() != "be brief" || gjson.GetBytes(body, "prompt_cache_key").String() != "conv" {
		t.Fatalf("request properties were not preserved: %s", body)
	}
}

func TestBuildCodexWSMessageMovesTurnHeaders(t *testing.T) {
	headers := http.Header{}
	headers.Set("X-Codex-Turn-State", "state-1")
	message, err := buildCodexWSMessage([]byte(`{"model":"gpt-5.4","client_metadata":{"x-codex-turn-metadata":"from-body"},"input":[]}`), headers)
	if err != nil {
		t.Fatalf("buildCodexWSMessage() error = %v", err)
	}
	if gjson.GetBytes(message, "type").String() != "response.create" {
		t.Fatalf("type = %q", gjson.GetBytes(message, "type").String())
	}
	if gjson.GetBytes(message, "client_metadata.x-codex-turn-state").String() != "state-1" {
		t.Fatalf("turn state not moved into client_metadata: %s", message)
	}
	if gjson.GetBytes(message, "client_metadata.x-codex-turn-metadata").String() != "from-body" {
		t.Fatalf("existing client_metadata overwritten: %s", message)
	}
}

// codexWSTestServer accepts websocket upgrades and holds them open.
func codexWSTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			if _, _, errRead := conn.ReadMessage(); errRead != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	return server
}

type codexWSTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *codexWSTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *codexWSTestClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// codexWSTestLease acquires a socket for (auth, conversation), dialing the
// test server when the pool reserves a new slot.
func codexWSTestLease(t *testing.T, pool *CodexWSPool, server *httptest.Server, authID, conversation string) (*codexWSConn, bool) {
	t.Helper()
	group := codexWSGroupKey{authID: authID, conversation: conversation}
	lease, ok := pool.acquire(codexWSAcquireRequest{group: group, compat: "compat", authFP: "fp", shape: newCodexWSRequestShape([]byte(codexWSTestBaseBody))})
	if !ok {
		return nil, false
	}
	if lease.conn != nil {
		return lease.conn, true
	}
	ws, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial test server: %v", err)
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	return pool.adopt(group, ws, "compat", "fp", nil), true
}

func TestCodexWSPoolPerConversationCapFallsBackToHTTP(t *testing.T) {
	server := codexWSTestServer(t)
	settings := DefaultCodexWSPoolSettings()
	settings.MaxSocketsPerConversation = 2
	pool := NewCodexWSPool(settings, nil)
	t.Cleanup(func() { pool.CloseAll("test") })

	first, ok1 := codexWSTestLease(t, pool, server, "auth-a", "conv")
	second, ok2 := codexWSTestLease(t, pool, server, "auth-a", "conv")
	if !ok1 || !ok2 || first == second {
		t.Fatal("two concurrent requests should get two sockets")
	}
	if _, ok := codexWSTestLease(t, pool, server, "auth-a", "conv"); ok {
		t.Fatal("a third concurrent request must use HTTP instead of waiting or sharing a busy socket")
	}
	pool.release(first, nil, true, "done")
	third, ok := codexWSTestLease(t, pool, server, "auth-a", "conv")
	if !ok || third != first {
		t.Fatal("a released socket should be reused")
	}
}

func TestCodexWSPoolCapsEvictIdleSocketsOnly(t *testing.T) {
	server := codexWSTestServer(t)
	clock := &codexWSTestClock{now: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)}
	settings := DefaultCodexWSPoolSettings()
	settings.MaxSocketsPerAuth = 2
	settings.MaxSockets = 3
	pool := NewCodexWSPool(settings, clock.Now)
	t.Cleanup(func() { pool.CloseAll("test") })

	busy, _ := codexWSTestLease(t, pool, server, "auth-a", "conv-1")
	clock.Advance(time.Second)
	idle, _ := codexWSTestLease(t, pool, server, "auth-a", "conv-2")
	pool.release(idle, nil, true, "done")
	clock.Advance(time.Second)

	// auth-a is at its cap: the idle socket is evicted, the busy one is kept.
	third, ok := codexWSTestLease(t, pool, server, "auth-a", "conv-3")
	if !ok || third == nil {
		t.Fatal("expected room after evicting the idle socket")
	}
	if !codexWSTestClosed(pool, idle) || codexWSTestClosed(pool, busy) {
		t.Fatalf("idle closed=%t busy closed=%t, want only the idle socket evicted", codexWSTestClosed(pool, idle), codexWSTestClosed(pool, busy))
	}
	// auth-a is full of busy sockets now.
	if _, ok := codexWSTestLease(t, pool, server, "auth-a", "conv-4"); ok {
		t.Fatal("no idle socket to evict, the request must use HTTP")
	}
	other, ok := codexWSTestLease(t, pool, server, "auth-b", "conv-1")
	if !ok || other == nil {
		t.Fatal("another credential has its own allowance")
	}
	if stats := pool.Stats(); stats.OpenSockets != 3 {
		t.Fatalf("open sockets = %d, want 3", stats.OpenSockets)
	}
	if _, ok := codexWSTestLease(t, pool, server, "auth-c", "conv-1"); ok {
		t.Fatal("global cap reached with only busy sockets, the request must use HTTP")
	}
}

func TestCodexWSPoolConfigureDisabledClosesIdleAndRetiresBusy(t *testing.T) {
	server := codexWSTestServer(t)
	pool := NewCodexWSPool(DefaultCodexWSPoolSettings(), nil)
	idle, _ := codexWSTestLease(t, pool, server, "auth-a", "conv-1")
	pool.release(idle, nil, true, "done")
	busy, _ := codexWSTestLease(t, pool, server, "auth-a", "conv-2")

	disabled := DefaultCodexWSPoolSettings()
	disabled.Enabled = false
	pool.Configure(disabled)
	if !codexWSTestClosed(pool, idle) {
		t.Fatal("idle socket should close when the pool is disabled")
	}
	select {
	case <-busy.closing:
		t.Fatal("in-flight socket must finish its response before closing")
	default:
	}
	pool.release(busy, nil, true, "done")
	select {
	case <-busy.closing:
	default:
		t.Fatal("retired socket should close on release")
	}
	if _, ok := codexWSTestLease(t, pool, server, "auth-a", "conv-3"); ok {
		t.Fatal("a disabled pool must not serve requests")
	}
}

func codexWSTestClosed(pool *CodexWSPool, c *codexWSConn) bool {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	return c.closed
}

func TestCodexWSItemDigestNormalizesOnlyKnownMetadata(t *testing.T) {
	digest := func(raw string) codexWSDigest {
		t.Helper()
		d, err := codexWSItemDigest([]byte(raw))
		if err != nil {
			t.Fatalf("codexWSItemDigest(%s) error = %v", raw, err)
		}
		return d
	}
	equal := []struct {
		name string
		a, b string
	}{
		{
			name: "item id and status",
			a:    `{"id":"fc_1","type":"function_call","status":"completed","call_id":"c1","name":"shell","arguments":"{}"}`,
			b:    `{"type":"function_call","call_id":"c1","name":"shell","arguments":"{}"}`,
		},
		{
			name: "empty annotations and logprobs on output_text",
			a:    `{"type":"message","role":"assistant","content":[{"type":"output_text","annotations":[],"logprobs":[],"text":"hi"}]}`,
			b:    `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}`,
		},
		{
			name: "null content on reasoning",
			a:    `{"id":"rs_1","type":"reasoning","summary":[],"encrypted_content":"enc"}`,
			b:    `{"type":"reasoning","summary":[],"content":null,"encrypted_content":"enc"}`,
		},
		{
			name: "key order",
			a:    `{"type":"message","role":"user","content":[{"type":"input_text","text":"q"}]}`,
			b:    `{"content":[{"text":"q","type":"input_text"}],"role":"user","type":"message"}`,
		},
	}
	for _, tt := range equal {
		t.Run("equal/"+tt.name, func(t *testing.T) {
			if digest(tt.a) != digest(tt.b) {
				t.Fatalf("digests differ:\n%s\n%s", tt.a, tt.b)
			}
		})
	}

	different := []struct {
		name string
		a, b string
	}{
		{
			name: "null removed inside a tool schema",
			a:    `{"type":"tool_search_output","call_id":"ts1","execution":"client","tools":[{"type":"function","name":"pick","parameters":{"type":"object","properties":{"mode":{"const":null}}}}]}`,
			b:    `{"type":"tool_search_output","call_id":"ts1","execution":"client","tools":[{"type":"function","name":"pick","parameters":{"type":"object","properties":{"mode":{}}}}]}`,
		},
		{
			name: "empty array removed inside a tool schema",
			a:    `{"type":"tool_search_output","call_id":"ts1","tools":[{"type":"function","name":"pick","parameters":{"type":"object","required":[],"properties":{}}}]}`,
			b:    `{"type":"tool_search_output","call_id":"ts1","tools":[{"type":"function","name":"pick","parameters":{"type":"object","properties":{}}}]}`,
		},
		{
			name: "empty tools list removed from tool_search_output",
			a:    `{"type":"tool_search_output","call_id":"ts1","tools":[]}`,
			b:    `{"type":"tool_search_output","call_id":"ts1"}`,
		},
		{
			name: "null output on a function call output",
			a:    `{"type":"function_call_output","call_id":"c1","output":null}`,
			b:    `{"type":"function_call_output","call_id":"c1"}`,
		},
		{
			name: "non-empty annotations",
			a:    `{"type":"message","role":"assistant","content":[{"type":"output_text","annotations":[{"type":"url_citation","url":"https://example.com"}],"text":"hi"}]}`,
			b:    `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}`,
		},
		{
			name: "empty annotations outside output_text",
			a:    `{"type":"message","role":"user","content":[{"type":"input_text","annotations":[],"text":"hi"}]}`,
			b:    `{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`,
		},
		{
			name: "nested id is content",
			a:    `{"type":"custom_tool_call_output","call_id":"c1","output":{"id":"x","status":"done"}}`,
			b:    `{"type":"custom_tool_call_output","call_id":"c1","output":{}}`,
		},
		{
			name: "reasoning summary dropped",
			a:    `{"type":"reasoning","summary":[],"encrypted_content":"enc"}`,
			b:    `{"type":"reasoning","encrypted_content":"enc"}`,
		},
	}
	for _, tt := range different {
		t.Run("different/"+tt.name, func(t *testing.T) {
			if digest(tt.a) == digest(tt.b) {
				t.Fatalf("digests are equal, a real change would be dropped from the delta:\n%s\n%s", tt.a, tt.b)
			}
		})
	}
}

// TestCodexWSIncrementalItemsKeepsNestedHistoryChanges guards the delta
// against hiding an edit deep inside a history item: the upstream would keep
// the old version.
func TestCodexWSIncrementalItemsKeepsNestedHistoryChanges(t *testing.T) {
	toolSearch := `{"type":"tool_search_output","call_id":"ts1","execution":"client","status":"completed","tools":[{"type":"function","name":"pick","parameters":{"type":"object","properties":{"mode":{"const":null}}}}]}`
	withHistory := func(items ...string) []byte {
		return []byte(strings.Replace(codexWSTestBaseBody, `"text":"q1"}]}]`, `"text":"q1"}]},`+strings.Join(items, ",")+`]`, 1))
	}
	baseline := newCodexWSBaseline(newCodexWSRequestShape(withHistory(toolSearch)), codexWSTestOutput, "resp_1")
	if baseline == nil {
		t.Fatal("baseline is nil")
	}
	echo := []string{
		`{"type":"reasoning","summary":[],"encrypted_content":"enc-1"}`,
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}`,
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"q2"}]}`,
	}

	unchanged := withHistory(append([]string{strings.Replace(toolSearch, `"status":"completed",`, ``, 1)}, echo...)...)
	if items, ok := codexWSIncrementalItems(newCodexWSRequestShape(unchanged), baseline); !ok || len(items) != 1 {
		t.Fatalf("unchanged history = (%d items, %t), want an incremental request with the new message", len(items), ok)
	}

	changed := withHistory(append([]string{strings.Replace(toolSearch, `"mode":{"const":null}`, `"mode":{}`, 1)}, echo...)...)
	if _, ok := codexWSIncrementalItems(newCodexWSRequestShape(changed), baseline); ok {
		t.Fatal("removing const:null from a tool schema in history must send the full request")
	}
}
