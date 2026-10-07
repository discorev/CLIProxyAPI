package executor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

// runResponsesTurns drives turns of a Responses conversation through
// ExecuteStream, echoing the output the way the Codex CLI does, and returns the
// final history. It fails the test on any downstream error.
func runResponsesTurns(t *testing.T, ctx context.Context, exec *CodexExecutor, auths []*cliproxyauth.Auth, conversation string, history []string) []string {
	t.Helper()
	for i, auth := range auths {
		result, err := exec.ExecuteStream(ctx, auth, responsesRequest(conversation, history), responsesOptions(true))
		if err != nil {
			t.Fatalf("turn %d ExecuteStream() error = %v", i+1, err)
		}
		stream := collectStream(t, result)
		for _, item := range completedOutputFromSSE(t, stream) {
			history = append(history, codexCLIEcho(t, item))
		}
		history = append(history, userInputItem(fmt.Sprintf("follow-up %d", i+1)))
	}
	return history
}

func repeatAuth(auth *cliproxyauth.Auth, n int) []*cliproxyauth.Auth {
	out := make([]*cliproxyauth.Auth, n)
	for i := range out {
		out[i] = auth
	}
	return out
}

func requireMessageModes(t *testing.T, messages []fakeCodexUpstreamMessage, want ...string) {
	t.Helper()
	if len(messages) != len(want) {
		for i, message := range messages {
			t.Logf("message %d conn=%d: %s", i, message.conn, message.payload)
		}
		t.Fatalf("upstream messages = %d, want %d", len(messages), len(want))
	}
	for i, mode := range want {
		got := "full"
		if isIncremental(messages[i].payload) {
			got = "incremental"
		}
		if got != mode {
			t.Fatalf("message %d mode = %s, want %s: %s", i, got, mode, messages[i].payload)
		}
	}
}

func TestCodexHTTPWebsocketRecoversPreviousResponseNotFound(t *testing.T) {
	upstream := newFakeCodexUpstream(t)
	upstream.script = func(_ int, index int, _ []byte) (fakeCodexReply, bool) {
		if index != 1 {
			return fakeCodexReply{}, false
		}
		// The backend lost the connection-local response the delta refers to.
		return fakeCodexReply{forget: true, events: [][]byte{[]byte(`{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"previous_response_not_found","message":"Previous response with id 'resp_c1_t1' not found."}}`)}}, true
	}
	exec, pool, _ := newPooledCodexExecutor(t, nil)
	auth := newPooledCodexOAuth("auth-a", upstream.server.URL)

	runResponsesTurns(t, context.Background(), exec, repeatAuth(auth, 2), "conv-missing", []string{userInputItem("q1")})

	upgrades, posts, messages := upstream.snapshot()
	if upgrades != 1 || posts != 0 {
		t.Fatalf("upgrades=%d posts=%d, want the same socket and no HTTP fallback", upgrades, posts)
	}
	requireMessageModes(t, messages, "full", "incremental", "full")
	if stats := pool.Stats(); stats.PreviousMissing != 1 || stats.Recoveries != 1 {
		t.Fatalf("stats = %+v, want one previous_response_not_found recovery", stats)
	}
}

func TestCodexHTTPWebsocketReconnectsWhenUpstreamClosesBeforeResponding(t *testing.T) {
	upstream := newFakeCodexUpstream(t)
	upstream.script = func(_ int, index int, _ []byte) (fakeCodexReply, bool) {
		if index == 1 {
			return fakeCodexReply{closeBefore: true}, true
		}
		return fakeCodexReply{}, false
	}
	exec, _, _ := newPooledCodexExecutor(t, nil)
	auth := newPooledCodexOAuth("auth-a", upstream.server.URL)

	runResponsesTurns(t, context.Background(), exec, repeatAuth(auth, 2), "conv-closed", []string{userInputItem("q1")})

	upgrades, posts, messages := upstream.snapshot()
	if upgrades != 2 || posts != 0 {
		t.Fatalf("upgrades=%d posts=%d, want a reconnect and no HTTP fallback", upgrades, posts)
	}
	requireMessageModes(t, messages, "full", "incremental", "full")
	if messages[2].conn != 2 {
		t.Fatalf("resend went to conn %d, want the new socket", messages[2].conn)
	}
}

func TestCodexHTTPWebsocketReconnectsOnConnectionLimit(t *testing.T) {
	upstream := newFakeCodexUpstream(t)
	upstream.script = func(_ int, index int, _ []byte) (fakeCodexReply, bool) {
		if index == 1 {
			return fakeCodexReply{events: [][]byte{[]byte(`{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"websocket_connection_limit_reached","message":"Responses websocket connection limit reached (60 minutes). Create a new websocket connection to continue."}}`)}}, true
		}
		return fakeCodexReply{}, false
	}
	exec, _, _ := newPooledCodexExecutor(t, nil)
	auth := newPooledCodexOAuth("auth-a", upstream.server.URL)

	runResponsesTurns(t, context.Background(), exec, repeatAuth(auth, 2), "conv-limit", []string{userInputItem("q1")})

	upgrades, _, messages := upstream.snapshot()
	if upgrades != 2 {
		t.Fatalf("upgrades = %d, want a fresh socket after the connection limit", upgrades)
	}
	requireMessageModes(t, messages, "full", "incremental", "full")
}

func TestCodexHTTPWebsocketAvoidsSocketsNearConnectionCap(t *testing.T) {
	upstream := newFakeCodexUpstream(t)
	cfg := &config.Config{}
	cfg.Codex.HTTPWebsocketPool.IdleTimeout = "3h"
	exec, _, clock := newPooledCodexExecutor(t, cfg)
	auth := newPooledCodexOAuth("auth-a", upstream.server.URL)

	history := runResponsesTurns(t, context.Background(), exec, repeatAuth(auth, 1), "conv-age", []string{userInputItem("q1")})
	clock.Advance(51 * time.Minute)
	runResponsesTurns(t, context.Background(), exec, repeatAuth(auth, 1), "conv-age", history)

	upgrades, _, messages := upstream.snapshot()
	if upgrades != 2 {
		t.Fatalf("upgrades = %d, want a new socket instead of one 51 minutes old", upgrades)
	}
	requireMessageModes(t, messages, "full", "full")
}

func TestCodexHTTPWebsocketIdleEvictionUsesPoolClock(t *testing.T) {
	upstream := newFakeCodexUpstream(t)
	exec, pool, clock := newPooledCodexExecutor(t, nil)
	auth := newPooledCodexOAuth("auth-a", upstream.server.URL)

	history := runResponsesTurns(t, context.Background(), exec, repeatAuth(auth, 1), "conv-idle", []string{userInputItem("q1")})
	clock.Advance(9 * time.Minute)
	if evicted := pool.EvictIdle(); evicted != 0 {
		t.Fatalf("EvictIdle() after 9m = %d, want 0", evicted)
	}
	clock.Advance(2 * time.Minute)
	if evicted := pool.EvictIdle(); evicted != 1 {
		t.Fatalf("EvictIdle() after 11m = %d, want 1", evicted)
	}
	if open := pool.Stats().OpenSockets; open != 0 {
		t.Fatalf("open sockets = %d, want 0", open)
	}
	runResponsesTurns(t, context.Background(), exec, repeatAuth(auth, 1), "conv-idle", history)
	upgrades, _, messages := upstream.snapshot()
	if upgrades != 2 {
		t.Fatalf("upgrades = %d, want 2", upgrades)
	}
	requireMessageModes(t, messages, "full", "full")
}

func TestCodexHTTPWebsocketSurvivesIdleSocketClosedByUpstream(t *testing.T) {
	upstream := newFakeCodexUpstream(t)
	exec, _, _ := newPooledCodexExecutor(t, nil)
	auth := newPooledCodexOAuth("auth-a", upstream.server.URL)

	history := runResponsesTurns(t, context.Background(), exec, repeatAuth(auth, 1), "conv-dropped", []string{userInputItem("q1")})
	upstream.closeConn(1)
	runResponsesTurns(t, context.Background(), exec, repeatAuth(auth, 2), "conv-dropped", history)

	upgrades, posts, messages := upstream.snapshot()
	if upgrades != 2 || posts != 0 {
		t.Fatalf("upgrades=%d posts=%d, want one reconnect", upgrades, posts)
	}
	// The dropped socket may be noticed before the send (full on the new
	// socket) or by the send itself (delta lost with the socket, then full);
	// either way the follow-up turn on the new socket is incremental again.
	last := messages[len(messages)-1]
	if !isIncremental(last.payload) || last.conn != 2 {
		t.Fatalf("last message conn=%d incremental=%t, want incremental on the new socket", last.conn, isIncremental(last.payload))
	}
}

func TestCodexHTTPWebsocketConcurrentRequestsUseSeparateSockets(t *testing.T) {
	upstream := newFakeCodexUpstream(t)
	received := make(chan struct{})
	release := make(chan struct{})
	upstream.hold = func(_ int, index int, _ []byte) <-chan struct{} {
		if index == 0 {
			close(received)
			return release
		}
		return nil
	}
	exec, _, _ := newPooledCodexExecutor(t, nil)
	auth := newPooledCodexOAuth("auth-a", upstream.server.URL)
	history := []string{userInputItem("q1")}

	var wg sync.WaitGroup
	var firstErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		result, err := exec.ExecuteStream(context.Background(), auth, responsesRequest("conv-concurrent", history), responsesOptions(true))
		if err != nil {
			firstErr = err
			return
		}
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				firstErr = chunk.Err
			}
		}
	}()
	<-received

	// The first response is still in flight on socket 1; this must not wait for it.
	result, err := exec.ExecuteStream(context.Background(), auth, responsesRequest("conv-concurrent", history), responsesOptions(true))
	if err != nil {
		t.Fatalf("concurrent ExecuteStream() error = %v", err)
	}
	collectStream(t, result)
	close(release)
	wg.Wait()
	if firstErr != nil {
		t.Fatalf("first request error = %v", firstErr)
	}

	upgrades, posts, messages := upstream.snapshot()
	if upgrades != 2 || posts != 0 || len(messages) != 2 {
		t.Fatalf("upgrades=%d posts=%d messages=%d, want two sockets serving one request each", upgrades, posts, len(messages))
	}
	if messages[0].conn == messages[1].conn {
		t.Fatal("concurrent requests shared one socket")
	}
}

func TestCodexHTTPWebsocketCredentialSwitchUsesNewCredentialSocket(t *testing.T) {
	upstream := newFakeCodexUpstream(t)
	exec, pool, _ := newPooledCodexExecutor(t, nil)
	authA := newPooledCodexOAuth("auth-a", upstream.server.URL)
	authB := newPooledCodexOAuth("auth-b", upstream.server.URL)

	runResponsesTurns(t, context.Background(), exec, []*cliproxyauth.Auth{authA, authB, authB}, "conv-switch", []string{userInputItem("q1")})

	upgrades, _, messages := upstream.snapshot()
	if upgrades != 2 {
		t.Fatalf("upgrades = %d, want one socket per credential", upgrades)
	}
	requireMessageModes(t, messages, "full", "full", "incremental")
	if messages[0].conn == messages[1].conn || messages[1].conn != messages[2].conn {
		t.Fatalf("conns = %d,%d,%d, want the switch to open the new credential's socket", messages[0].conn, messages[1].conn, messages[2].conn)
	}
	if !strings.Contains(string(messages[1].payload), `"prompt_cache_key":"conv-switch"`) {
		t.Fatalf("switched request lost the conversation key: %s", messages[1].payload)
	}
	if open := pool.Stats().OpenSockets; open != 1 {
		t.Fatalf("open sockets = %d, want the previous credential's socket evicted", open)
	}
}

func TestCodexHTTPWebsocketCloseAuthClosesSockets(t *testing.T) {
	upstream := newFakeCodexUpstream(t)
	exec, pool, _ := newPooledCodexExecutor(t, nil)
	auth := newPooledCodexOAuth("auth-a", upstream.server.URL)
	other := newPooledCodexOAuth("auth-b", upstream.server.URL)

	runResponsesTurns(t, context.Background(), exec, repeatAuth(auth, 1), "conv-remove-a", []string{userInputItem("q1")})
	runResponsesTurns(t, context.Background(), exec, repeatAuth(other, 1), "conv-remove-b", []string{userInputItem("q1")})
	if open := pool.Stats().OpenSockets; open != 2 {
		t.Fatalf("open sockets = %d, want 2", open)
	}
	pool.CloseAuth("auth-a", "auth_removed")
	if open := pool.Stats().OpenSockets; open != 1 {
		t.Fatalf("open sockets after CloseAuth = %d, want only auth-b's socket", open)
	}
}

func TestCloseCodexWebsocketSessionsForAuthIDClosesDefaultPoolSockets(t *testing.T) {
	upstream := newFakeCodexUpstream(t)
	exec := NewCodexExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := newPooledCodexOAuth("auth-default-pool-removal", upstream.server.URL)

	runResponsesTurns(t, context.Background(), exec, repeatAuth(auth, 1), "conv-default-pool", []string{userInputItem("q1")})
	before := defaultPoolOpenSockets()
	CloseCodexWebsocketSessionsForAuthID(auth.ID, "auth_removed")
	if after := defaultPoolOpenSockets(); after != before-1 {
		t.Fatalf("default pool open sockets %d -> %d, want the removed credential's socket closed", before, after)
	}
}

func TestCodexHTTPWebsocketOptOuts(t *testing.T) {
	disabled := false
	tests := []struct {
		name          string
		cfg           func(*config.Config)
		auth          func(baseURL string) *cliproxyauth.Auth
		wantWebsocket bool
	}{
		{
			name:          "oauth default",
			auth:          func(baseURL string) *cliproxyauth.Auth { return newPooledCodexOAuth("auth-a", baseURL) },
			wantWebsocket: true,
		},
		{
			name: "pool disabled in config",
			cfg:  func(cfg *config.Config) { cfg.Codex.HTTPWebsocketPool.Enabled = &disabled },
			auth: func(baseURL string) *cliproxyauth.Auth { return newPooledCodexOAuth("auth-a", baseURL) },
		},
		{
			name: "credential websockets false",
			auth: func(baseURL string) *cliproxyauth.Auth {
				auth := newPooledCodexOAuth("auth-a", baseURL)
				auth.Metadata["websockets"] = false
				return auth
			},
		},
		{
			name: "api key keeps opt-in",
			auth: func(baseURL string) *cliproxyauth.Auth {
				return &cliproxyauth.Auth{ID: "key-a", Provider: "codex", Attributes: map[string]string{"api_key": "sk-test", "base_url": baseURL}}
			},
		},
		{
			name: "api key explicitly enabled",
			auth: func(baseURL string) *cliproxyauth.Auth {
				return &cliproxyauth.Auth{ID: "key-a", Provider: "codex", Attributes: map[string]string{"api_key": "sk-test", "base_url": baseURL, "websockets": "true"}}
			},
			wantWebsocket: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := newFakeCodexUpstream(t)
			cfg := &config.Config{}
			if tt.cfg != nil {
				tt.cfg(cfg)
			}
			exec, _, _ := newPooledCodexExecutor(t, cfg)
			runResponsesTurns(t, context.Background(), exec, repeatAuth(tt.auth(upstream.server.URL), 2), "conv-opt", []string{userInputItem("q1")})
			upgrades, posts, _ := upstream.snapshot()
			if tt.wantWebsocket && (upgrades != 1 || posts != 0) {
				t.Fatalf("upgrades=%d posts=%d, want websocket", upgrades, posts)
			}
			if !tt.wantWebsocket && (upgrades != 0 || posts != 2) {
				t.Fatalf("upgrades=%d posts=%d, want plain HTTP", upgrades, posts)
			}
		})
	}
}

// countingRoundTripper stands in for an SDK caller's RoundTripperProvider
// transport, such as a private gateway or an egress policy.
type countingRoundTripper struct {
	mu    sync.Mutex
	calls int
}

func (c *countingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return http.DefaultTransport.RoundTrip(req)
}

func TestCodexHTTPWebsocketKeepsInjectedTransport(t *testing.T) {
	upstream := newFakeCodexUpstream(t)
	exec, _, _ := newPooledCodexExecutor(t, nil)
	auth := newPooledCodexOAuth("auth-a", upstream.server.URL)
	rt := &countingRoundTripper{}
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(rt))
	runResponsesTurns(t, ctx, exec, repeatAuth(auth, 2), "conv-rt", []string{userInputItem("q1")})
	if upgrades, posts, _ := upstream.snapshot(); upgrades != 0 || posts != 2 {
		t.Fatalf("upgrades=%d posts=%d, want plain HTTP through the injected transport", upgrades, posts)
	}
	rt.mu.Lock()
	calls := rt.calls
	rt.mu.Unlock()
	if calls != 2 {
		t.Fatalf("injected transport calls = %d, want 2", calls)
	}

	// A proxy URL takes precedence over the context transport on the HTTP path,
	// and the websocket dialer honours it, so the pool stays eligible.
	cfg := &config.Config{}
	cfg.ProxyURL = "http://proxy.invalid:1"
	if codexHTTPWebsocketCustomTransport(ctx, cfg, auth) {
		t.Fatal("custom transport reported although a proxy URL takes precedence")
	}
	if codexHTTPWebsocketCustomTransport(context.Background(), nil, auth) {
		t.Fatal("custom transport reported without one in the context")
	}
}

func TestCodexHTTPWebsocketWithoutConversationUsesHTTP(t *testing.T) {
	upstream := newFakeCodexUpstream(t)
	exec, _, _ := newPooledCodexExecutor(t, nil)
	auth := newPooledCodexOAuth("auth-a", upstream.server.URL)
	req := responsesRequest("", []string{userInputItem("q1")})
	req.Payload = []byte(strings.Replace(string(req.Payload), `"prompt_cache_key":"",`, "", 1))
	if gjson.GetBytes(req.Payload, "prompt_cache_key").Exists() {
		t.Fatalf("test payload still has prompt_cache_key: %s", req.Payload)
	}
	result, err := exec.ExecuteStream(context.Background(), auth, req, responsesOptions(true))
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	collectStream(t, result)
	if upgrades, posts, _ := upstream.snapshot(); upgrades != 0 || posts != 1 {
		t.Fatalf("upgrades=%d posts=%d, want plain HTTP without a conversation identity", upgrades, posts)
	}
}

func TestCodexHTTPWebsocketDialRejectionFallsBackWithBackoff(t *testing.T) {
	upstream := newFakeCodexUpstream(t)
	upstream.rejectUpgrade = true
	exec, pool, clock := newPooledCodexExecutor(t, nil)
	auth := newPooledCodexOAuth("auth-a", upstream.server.URL)

	history := runResponsesTurns(t, context.Background(), exec, repeatAuth(auth, 2), "conv-reject", []string{userInputItem("q1")})
	if upgrades, posts, _ := upstream.snapshot(); upgrades != 1 || posts != 2 {
		t.Fatalf("upgrades=%d posts=%d, want one refused upgrade then HTTP during the backoff", upgrades, posts)
	}
	clock.Advance(3 * time.Minute)
	upstream.mu.Lock()
	upstream.rejectUpgrade = false
	upstream.mu.Unlock()
	runResponsesTurns(t, context.Background(), exec, repeatAuth(auth, 1), "conv-reject", history)
	if upgrades, posts, _ := upstream.snapshot(); upgrades != 2 || posts != 2 {
		t.Fatalf("upgrades=%d posts=%d, want the websocket retried after the backoff", upgrades, posts)
	}
	if stats := pool.Stats(); stats.DialFailures != 1 {
		t.Fatalf("stats = %+v, want one dial failure", stats)
	}
}

func TestCodexHTTPWebsocketErrorEventBecomesHTTPError(t *testing.T) {
	upstream := newFakeCodexUpstream(t)
	upstream.script = func(int, int, []byte) (fakeCodexReply, bool) {
		return fakeCodexReply{events: [][]byte{[]byte(`{"type":"error","status":429,"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_in_seconds":120},"headers":{"x-codex-primary-used-percent":"100","x-codex-primary-window-minutes":"300","x-codex-primary-reset-after-seconds":"120"}}`)}}, true
	}
	exec, pool, _ := newPooledCodexExecutor(t, nil)
	auth := newPooledCodexOAuth("auth-a", upstream.server.URL)
	ctx := logging.WithResponseHeadersHolder(context.Background())

	_, err := exec.ExecuteStream(ctx, auth, responsesRequest("conv-429", []string{userInputItem("q1")}), responsesOptions(true))
	if err == nil {
		t.Fatal("ExecuteStream() error = nil, want 429")
	}
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) || status.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("error = %v, want status 429", err)
	}
	if got := logging.GetResponseHeaders(ctx).Get("X-Codex-Primary-Used-Percent"); got != "100" {
		t.Fatalf("response quota header = %q, want 100 from the error frame", got)
	}
	if _, posts, _ := upstream.snapshot(); posts != 0 {
		t.Fatalf("posts = %d, want the websocket error surfaced without an HTTP retry", posts)
	}
	if open := pool.Stats().OpenSockets; open != 0 {
		t.Fatalf("open sockets = %d, want the errored socket closed", open)
	}
}

func TestCodexHTTPWebsocketFeedsQuotaHeaders(t *testing.T) {
	upstream := newFakeCodexUpstream(t)
	upstream.rateLimits = true
	exec, _, _ := newPooledCodexExecutor(t, nil)
	auth := newPooledCodexOAuth("auth-a", upstream.server.URL)

	ctx := logging.WithResponseHeadersHolder(context.Background())
	history := runResponsesTurns(t, ctx, exec, repeatAuth(auth, 1), "conv-quota", []string{userInputItem("q1")})
	if got := logging.GetResponseHeaders(ctx).Get("X-Codex-Primary-Used-Percent"); got != "42" {
		t.Fatalf("turn 1 quota header = %q, want 42 from codex.rate_limits", got)
	}

	// A reused socket must not report the handshake's quota snapshot again.
	upstream.mu.Lock()
	upstream.rateLimits = false
	upstream.mu.Unlock()
	ctx = logging.WithResponseHeadersHolder(context.Background())
	runResponsesTurns(t, ctx, exec, repeatAuth(auth, 1), "conv-quota", history)
	if got := logging.GetResponseHeaders(ctx).Get("X-Codex-Primary-Used-Percent"); got != "" {
		t.Fatalf("turn 2 quota header = %q, want no stale handshake value", got)
	}
	if got := logging.GetResponseHeaders(ctx).Get("X-Codex-Turn-State"); got != "" {
		t.Fatalf("turn 2 turn-state header = %q, want no stale handshake value", got)
	}
}

func TestCodexHTTPWebsocketAbandonedStreamClosesSocket(t *testing.T) {
	upstream := newFakeCodexUpstream(t)
	upstream.script = func(_ int, index int, _ []byte) (fakeCodexReply, bool) {
		if index != 0 {
			return fakeCodexReply{}, false
		}
		return fakeCodexReply{events: [][]byte{
			[]byte(`{"type":"response.created","response":{"id":"resp_partial","status":"in_progress","output":[]}}`),
			[]byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_partial","type":"message","status":"in_progress","role":"assistant","content":[]}}`),
			[]byte(`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"partial"}`),
		}}, true
	}
	exec, pool, _ := newPooledCodexExecutor(t, nil)
	auth := newPooledCodexOAuth("auth-a", upstream.server.URL)

	ctx, cancel := context.WithCancel(context.Background())
	result, err := exec.ExecuteStream(ctx, auth, responsesRequest("conv-abandon", []string{userInputItem("q1")}), responsesOptions(true))
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	cancel()
	for range result.Chunks {
	}
	if open := pool.Stats().OpenSockets; open != 0 {
		t.Fatalf("open sockets = %d, want the abandoned socket closed", open)
	}

	runResponsesTurns(t, context.Background(), exec, repeatAuth(auth, 1), "conv-abandon", []string{userInputItem("q1")})
	upgrades, _, messages := upstream.snapshot()
	if upgrades != 2 {
		t.Fatalf("upgrades = %d, want a fresh socket after the abandoned one", upgrades)
	}
	requireMessageModes(t, messages, "full", "full")
}

func TestCodexHTTPWebsocketMovesTurnHeadersIntoClientMetadata(t *testing.T) {
	upstream := newFakeCodexUpstream(t)
	exec, _, _ := newPooledCodexExecutor(t, nil)
	auth := newPooledCodexOAuth("auth-a", upstream.server.URL)
	opts := responsesOptions(true)
	opts.Headers = http.Header{}
	opts.Headers.Set("X-Codex-Turn-State", "turn-state-1")
	opts.Headers.Set("X-Codex-Turn-Metadata", `{"turn_id":"t1"}`)

	result, err := exec.ExecuteStream(context.Background(), auth, responsesRequest("conv-turn", []string{userInputItem("q1")}), opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	collectStream(t, result)
	_, _, messages := upstream.snapshot()
	if len(messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(messages))
	}
	payload := messages[0].payload
	if got := gjson.GetBytes(payload, `client_metadata.x-codex-turn-state`).String(); got != "turn-state-1" {
		t.Fatalf("client_metadata turn state = %q: %s", got, payload)
	}
	if got := gjson.GetBytes(payload, `client_metadata.x-codex-turn-metadata`).String(); got != `{"turn_id":"t1"}` {
		t.Fatalf("client_metadata turn metadata = %q: %s", got, payload)
	}
}

func TestCodexHTTPWebsocketChatCompletionsStream(t *testing.T) {
	upstream := newFakeCodexUpstream(t)
	exec, _, _ := newPooledCodexExecutor(t, nil)
	auth := newPooledCodexOAuth("auth-a", upstream.server.URL)
	opts := cliproxyexecutor.Options{SourceFormat: "openai", Stream: true}

	payload := []byte(`{"model":"gpt-5.4","stream":true,"prompt_cache_key":"conv-chat-stream","messages":[{"role":"user","content":"q1"}]}`)
	result, err := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{Model: "gpt-5.4", Payload: payload}, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	stream := collectStream(t, result)
	if !strings.Contains(string(stream), "answer 1") || !strings.Contains(string(stream), "chat.completion.chunk") {
		t.Fatalf("chat stream missing translated answer: %s", stream)
	}
	if upgrades, posts, _ := upstream.snapshot(); upgrades != 1 || posts != 0 {
		t.Fatalf("upgrades=%d posts=%d, want the websocket", upgrades, posts)
	}
}

func defaultPoolOpenSockets() int {
	return helps.DefaultCodexWSPool().Stats().OpenSockets
}

// TestCodexHTTPWebsocketReleasesStreamAtResponseCreated checks that the
// websocket transport hands response.created to the client as soon as it
// arrives, like the plain HTTP endpoint, instead of holding the stream start
// until generation begins.
func TestCodexHTTPWebsocketReleasesStreamAtResponseCreated(t *testing.T) {
	upstream := newFakeCodexUpstream(t)
	upstream.rateLimits = true
	resume := make(chan struct{})
	var resumeOnce sync.Once
	resumeUpstream := func() { resumeOnce.Do(func() { close(resume) }) }
	t.Cleanup(resumeUpstream)
	upstream.script = func(_ int, index int, payload []byte) (fakeCodexReply, bool) {
		if index != 0 {
			return fakeCodexReply{}, false
		}
		events := upstream.responseEvents("resp_slow", 1, payload)
		// codex.rate_limits, response.created and repeated in_progress
		// keepalives, then nothing until the test resumes the upstream.
		inProgress := []byte(`{"type":"response.in_progress","response":{"id":"resp_slow","status":"in_progress","output":[]}}`)
		head := append([][]byte{}, events[:2]...)
		head = append(head, inProgress, inProgress, inProgress)
		return fakeCodexReply{events: append(head, events[2:]...), pauseAfter: len(head), resume: resume}, true
	}
	exec, _, _ := newPooledCodexExecutor(t, nil)
	auth := newPooledCodexOAuth("auth-a", upstream.server.URL)

	type started struct {
		result *cliproxyexecutor.StreamResult
		err    error
	}
	startedCh := make(chan started, 1)
	go func() {
		result, err := exec.ExecuteStream(context.Background(), auth, responsesRequest("conv-early", []string{userInputItem("q1")}), responsesOptions(true))
		startedCh <- started{result: result, err: err}
	}()

	var result *cliproxyexecutor.StreamResult
	select {
	case got := <-startedCh:
		if got.err != nil {
			t.Fatalf("ExecuteStream() error = %v", got.err)
		}
		result = got.result
	case <-time.After(10 * time.Second):
		t.Fatal("ExecuteStream blocked until generation started; response.created was held back")
	}
	var early strings.Builder
	for !strings.Contains(early.String(), "response.in_progress") {
		select {
		case chunk, ok := <-result.Chunks:
			if !ok {
				t.Fatalf("stream ended early: %s", early.String())
			}
			if chunk.Err != nil {
				t.Fatalf("stream chunk error: %v", chunk.Err)
			}
			early.Write(chunk.Payload)
		case <-time.After(10 * time.Second):
			t.Fatalf("early stream events were held back; received so far: %s", early.String())
		}
	}
	if !strings.Contains(early.String(), "response.created") {
		t.Fatalf("response.created not delivered before generation: %s", early.String())
	}

	resumeUpstream()
	rest := collectStream(t, result)
	if !strings.Contains(string(rest), "answer 1") {
		t.Fatalf("stream missing answer after resume: %s", rest)
	}
}

// TestCodexHTTPWebsocketRecoversRejectionAfterTelemetry checks that a
// rejected continuation is still resent invisibly when connection telemetry
// precedes the error frame.
func TestCodexHTTPWebsocketRecoversRejectionAfterTelemetry(t *testing.T) {
	upstream := newFakeCodexUpstream(t)
	upstream.script = func(_ int, index int, _ []byte) (fakeCodexReply, bool) {
		if index != 1 {
			return fakeCodexReply{}, false
		}
		return fakeCodexReply{forget: true, events: [][]byte{
			[]byte(`{"type":"codex.rate_limits","rate_limits":{"allowed":true,"limit_reached":false,"primary":{"used_percent":42,"window_minutes":300,"reset_after_seconds":600}}}`),
			[]byte(`{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"previous_response_not_found","message":"Previous response not found."}}`),
		}}, true
	}
	exec, pool, _ := newPooledCodexExecutor(t, nil)
	auth := newPooledCodexOAuth("auth-a", upstream.server.URL)

	runResponsesTurns(t, context.Background(), exec, repeatAuth(auth, 2), "conv-telemetry", []string{userInputItem("q1")})

	_, posts, messages := upstream.snapshot()
	if posts != 0 {
		t.Fatalf("posts = %d, want no HTTP fallback", posts)
	}
	requireMessageModes(t, messages, "full", "incremental", "full")
	if stats := pool.Stats(); stats.PreviousMissing != 1 {
		t.Fatalf("stats = %+v, want one previous_response_not_found recovery", stats)
	}
}

// TestCodexHTTPWebsocketCancelledStreamClosesSocket checks that a client that
// goes away mid-response closes the upstream socket even while the upstream is
// silent, and that the socket is not reused.
func TestCodexHTTPWebsocketCancelledStreamClosesSocket(t *testing.T) {
	upstream := newFakeCodexUpstream(t)
	resume := make(chan struct{})
	t.Cleanup(func() { close(resume) })
	upstream.script = func(_ int, index int, payload []byte) (fakeCodexReply, bool) {
		if index != 0 {
			return fakeCodexReply{}, false
		}
		// Generation has started (response.created, output_item.added, a
		// text delta), then the upstream goes silent.
		return fakeCodexReply{events: upstream.responseEvents("resp_stall", 1, payload), pauseAfter: 3, resume: resume}, true
	}
	exec, pool, _ := newPooledCodexExecutor(t, nil)
	auth := newPooledCodexOAuth("auth-a", upstream.server.URL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result, err := exec.ExecuteStream(ctx, auth, responsesRequest("conv-cancel", []string{userInputItem("q1")}), responsesOptions(true))
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	cancel()
	for range result.Chunks {
	}
	if open := pool.Stats().OpenSockets; open != 0 {
		t.Fatalf("open sockets = %d, want the cancelled socket closed", open)
	}
}

// TestCodexHTTPWebsocketErrorAfterReleaseKeepsStatusAndHeaders covers error
// frames that arrive after the stream was handed to the HTTP pipeline: the
// websocket envelope's status, wrapped error and headers must reach the
// executor as they would on HTTP, so a 401 still triggers the credential's
// unauthorized handling and quota headers reach the usage cache.
func TestCodexHTTPWebsocketErrorAfterReleaseKeepsStatusAndHeaders(t *testing.T) {
	tests := []struct {
		name           string
		frame          string
		wantStatus     int
		wantMessage    string
		wantCredential bool
		wantHeader     string
	}{
		{
			name:        "wrapped body error",
			frame:       `{"type":"error","status":401,"body":{"error":{"type":"authentication_error","message":"expired token"}},"headers":{"x-codex-primary-used-percent":"55"}}`,
			wantStatus:  http.StatusUnauthorized,
			wantMessage: "expired token",
			wantHeader:  "55",
		},
		{
			name:        "status overrides a generic error type",
			frame:       `{"type":"error","status":403,"error":{"type":"server_error","message":"workspace disabled"}}`,
			wantStatus:  http.StatusForbidden,
			wantMessage: "workspace disabled",
		},
		{
			name:           "usage limit with quota headers",
			frame:          `{"type":"error","status":429,"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_in_seconds":120},"headers":{"x-codex-primary-used-percent":"100"}}`,
			wantStatus:     http.StatusTooManyRequests,
			wantMessage:    "usage limit",
			wantCredential: true,
			wantHeader:     "100",
		},
	}
	for _, tt := range tests {
		for _, stream := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/stream=%t", tt.name, stream), func(t *testing.T) {
				upstream := newFakeCodexUpstream(t)
				upstream.script = func(int, int, []byte) (fakeCodexReply, bool) {
					return fakeCodexReply{events: [][]byte{
						[]byte(`{"type":"response.created","response":{"id":"resp_err","status":"in_progress","output":[]}}`),
						[]byte(tt.frame),
					}}, true
				}
				exec, _, _ := newPooledCodexExecutor(t, nil)
				auth := newPooledCodexOAuth("auth-a", upstream.server.URL)
				ctx := logging.WithResponseHeadersHolder(context.Background())

				var err error
				if stream {
					result, errStream := exec.ExecuteStream(ctx, auth, responsesRequest("conv-late-error", []string{userInputItem("q1")}), responsesOptions(true))
					if errStream != nil {
						t.Fatalf("ExecuteStream() error = %v, want the stream to start at response.created", errStream)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							err = chunk.Err
						}
					}
				} else {
					_, err = exec.Execute(ctx, auth, responsesRequest("conv-late-error", []string{userInputItem("q1")}), responsesOptions(false))
				}
				var status interface{ StatusCode() int }
				if !errors.As(err, &status) || status.StatusCode() != tt.wantStatus {
					t.Fatalf("error = %v, want status %d", err, tt.wantStatus)
				}
				if !strings.Contains(err.Error(), tt.wantMessage) {
					t.Fatalf("error = %v, want the upstream message %q", err, tt.wantMessage)
				}
				var scoped interface{ IsCredentialScoped() bool }
				if errors.As(err, &scoped) && scoped.IsCredentialScoped() != tt.wantCredential {
					t.Fatalf("credential scoped = %t, want %t", scoped.IsCredentialScoped(), tt.wantCredential)
				}
				if tt.wantHeader != "" {
					if got := logging.GetResponseHeaders(ctx).Get("X-Codex-Primary-Used-Percent"); got != tt.wantHeader {
						t.Fatalf("response quota header = %q, want %q from the error frame", got, tt.wantHeader)
					}
				}
				if _, posts, _ := upstream.snapshot(); posts != 0 {
					t.Fatalf("posts = %d, want no HTTP retry", posts)
				}
			})
		}
	}
}
