package helps

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	codexWSMaxAttempts = 3
	// codexWSMaxPeekEvents bounds how many leading telemetry frames
	// (codex.rate_limits, metadata) are held while waiting for the first frame
	// that decides between a rejection and a response.
	codexWSMaxPeekEvents = 64

	codexWSErrorPreviousResponseNotFound = "previous_response_not_found"
	codexWSErrorConnectionLimitReached   = "websocket_connection_limit_reached"
)

// CodexWSDialFunc dials one upstream Responses websocket with the given
// handshake headers.
type CodexWSDialFunc func(ctx context.Context, headers http.Header) (*websocket.Conn, *http.Response, error)

// CodexWSRoundTripConfig describes one plain-HTTP Codex Responses request that
// may be served over a pooled upstream websocket.
type CodexWSRoundTripConfig struct {
	Pool   *CodexWSPool
	Base   http.RoundTripper
	Config *config.Config

	AuthID    string
	AuthLabel string
	AuthType  string
	AuthValue string

	// Conversation is the conversation identity (already scoped to the
	// downstream client) that keys the socket group together with AuthID.
	Conversation string
	// Target identifies the upstream endpoint, including the egress proxy, so
	// sockets are only reused for the same destination.
	Target string
	// URL is the websocket URL, used for request logs.
	URL string
	// Body is the full upstream request body the HTTP path would POST.
	Body []byte
	// PrepareHeaders turns the outgoing HTTP request headers into websocket
	// handshake headers.
	PrepareHeaders func(http.Header) http.Header
	Dial           CodexWSDialFunc
}

// NewCodexWSRoundTripper returns an http.RoundTripper that serves the Codex
// Responses POST over a pooled upstream websocket, falling back to Base for
// plain HTTP whenever the websocket cannot serve the request before any output
// is committed.
//
// The response is synthesized as an SSE stream (one event: / data: frame per
// websocket message), so the existing HTTP response pipeline - usage,
// reasoning replay, multi-agent restore, bootstrap buffering, translation and
// request logging - runs unchanged. Websocket error events that arrive before
// any output become HTTP error responses with the upstream status.
func NewCodexWSRoundTripper(cfg CodexWSRoundTripConfig) http.RoundTripper {
	if cfg.Base == nil {
		cfg.Base = http.DefaultTransport
	}
	return &codexWSRoundTripper{cfg: cfg}
}

type codexWSRoundTripper struct {
	cfg CodexWSRoundTripConfig
}

func (t *codexWSRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, handled, err := t.roundTripWebsocket(req)
	if handled {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return resp, err
	}
	return t.cfg.Base.RoundTrip(req)
}

// codexWSPeekKind classifies the start of a websocket response.
type codexWSPeekKind int

const (
	codexWSPeekCommit codexWSPeekKind = iota
	codexWSPeekErrorResponse
	codexWSPeekResendFull
	codexWSPeekReconnect
	codexWSPeekContextDone
)

type codexWSPeekResult struct {
	kind   codexWSPeekKind
	events [][]byte
	err    error
}

func (t *codexWSRoundTripper) roundTripWebsocket(req *http.Request) (*http.Response, bool, error) {
	cfg := t.cfg
	pool := cfg.Pool
	if pool == nil || cfg.Dial == nil || len(cfg.Body) == 0 || strings.TrimSpace(cfg.Conversation) == "" {
		return nil, false, nil
	}
	ctx := req.Context()
	handshakeHeaders := req.Header.Clone()
	if cfg.PrepareHeaders != nil {
		handshakeHeaders = cfg.PrepareHeaders(handshakeHeaders)
	}
	shape := newCodexWSRequestShape(cfg.Body)
	group := codexWSGroupKey{authID: cfg.AuthID, conversation: cfg.Conversation}
	authFP := codexWSAuthFingerprint(handshakeHeaders)
	compat := codexWSCompatFingerprint(cfg.Target, handshakeHeaders)
	fullMessage, errFull := buildCodexWSMessage(cfg.Body, req.Header)
	if errFull != nil {
		return nil, false, nil
	}

	forceFull := false
	for attempt := 0; attempt < codexWSMaxAttempts; attempt++ {
		// A cancelled request is not sent or retried, on any transport.
		if errCtx := ctx.Err(); errCtx != nil {
			return nil, true, errCtx
		}
		lease, ok := pool.acquire(codexWSAcquireRequest{group: group, compat: compat, authFP: authFP, shape: shape, forceFull: forceFull})
		if !ok {
			return nil, false, nil
		}
		conn := lease.conn
		fresh := false
		if conn == nil {
			ws, handshake, errDial := cfg.Dial(ctx, handshakeHeaders.Clone())
			if errDial != nil {
				pool.dialFailed(group, authFP)
				status := 0
				if handshake != nil {
					status = handshake.StatusCode
					if handshake.Body != nil {
						_ = handshake.Body.Close()
					}
				}
				RecordAPIWebsocketError(ctx, cfg.Config, "pool_dial", errDial)
				log.Debugf("codex websocket pool: dial failed auth=%s status=%d, using HTTP: %v", cfg.AuthID, status, errDial)
				if errCtx := ctx.Err(); errCtx != nil {
					return nil, true, errCtx
				}
				return nil, false, nil
			}
			var handshakeHeader http.Header
			if handshake != nil {
				handshakeHeader = handshake.Header.Clone()
				if handshake.Body != nil {
					_ = handshake.Body.Close()
				}
			}
			ws.EnableWriteCompression(false)
			conn = pool.adopt(lease, group, ws, compat, authFP, handshakeHeader)
			fresh = true
			RecordAPIWebsocketHandshake(ctx, cfg.Config, http.StatusSwitchingProtocols, handshakeHeader)
		}
		// From here until release, cancelling the request closes the socket,
		// which unblocks a write to an upstream that stopped reading.
		pool.watchLease(ctx, conn)

		incremental := len(lease.incremental) > 0 && lease.responseID != ""
		message := fullMessage
		if incremental {
			delta, errDelta := buildCodexWSIncrementalBody(cfg.Body, lease.responseID, lease.incremental)
			if errDelta == nil {
				delta, errDelta = buildCodexWSMessage(delta, req.Header)
			}
			if errDelta != nil {
				incremental = false
			} else {
				message = delta
			}
		}
		pool.countDecision(incremental)
		RecordAPIWebsocketRequest(ctx, cfg.Config, UpstreamRequestLog{
			URL:       cfg.URL,
			Method:    "WEBSOCKET",
			Headers:   handshakeHeaders.Clone(),
			Body:      message,
			Provider:  "codex",
			AuthID:    cfg.AuthID,
			AuthLabel: cfg.AuthLabel,
			AuthType:  cfg.AuthType,
			AuthValue: cfg.AuthValue,
		})
		log.Debugf("codex websocket pool: send socket=%d auth=%s mode=%s fresh=%t items=%d", conn.id, cfg.AuthID, codexWSModeLabel(incremental), fresh, len(lease.incremental))

		if errCtx := ctx.Err(); errCtx != nil {
			pool.release(conn, nil, false, "context_done")
			return nil, true, errCtx
		}
		if errWrite := conn.write(message); errWrite != nil {
			pool.release(conn, nil, false, "send_error")
			if errCtx := ctx.Err(); errCtx != nil {
				return nil, true, errCtx
			}
			RecordAPIWebsocketError(ctx, cfg.Config, "pool_send", errWrite)
			pool.countRecovery(false)
			forceFull = true
			continue
		}

		peek := t.peek(ctx, conn, incremental)
		switch peek.kind {
		case codexWSPeekCommit:
			headers := codexWSResponseHeaders(conn.handshake, fresh, peek.events)
			body := &codexWSBody{
				ctx:     ctx,
				cfg:     cfg.Config,
				pool:    pool,
				conn:    conn,
				shape:   shape,
				pending: peek.events,
				outputs: make(map[int64][]byte),
			}
			return &http.Response{
				Status:        "200 OK",
				StatusCode:    http.StatusOK,
				Proto:         "HTTP/1.1",
				ProtoMajor:    1,
				ProtoMinor:    1,
				Header:        headers,
				Body:          body,
				ContentLength: -1,
				Request:       req,
			}, true, nil
		case codexWSPeekErrorResponse:
			logCodexWSDiscardedEvents(ctx, cfg.Config, peek.events)
			pool.release(conn, nil, false, "upstream_error")
			last := peek.events[len(peek.events)-1]
			return codexWSErrorResponse(req, conn.handshake, fresh, peek.events, last), true, nil
		case codexWSPeekResendFull:
			// The socket is healthy but lost the response we chained from.
			logCodexWSDiscardedEvents(ctx, cfg.Config, peek.events)
			pool.release(conn, nil, true, "previous_response_not_found")
			RecordAPIWebsocketError(ctx, cfg.Config, "pool_previous_response_not_found", peek.err)
			pool.countRecovery(true)
			forceFull = true
		case codexWSPeekReconnect:
			logCodexWSDiscardedEvents(ctx, cfg.Config, peek.events)
			pool.release(conn, nil, false, "reconnect")
			RecordAPIWebsocketError(ctx, cfg.Config, "pool_reconnect", peek.err)
			pool.countRecovery(false)
			forceFull = true
		case codexWSPeekContextDone:
			pool.release(conn, nil, false, "context_done")
			return nil, true, peek.err
		}
	}
	pool.countFallback()
	return nil, false, nil
}

// peek reads only as far as needed to tell a rejection from a response, and
// decides whether the response can be handed to the HTTP pipeline or must be
// retried. Nothing has been sent downstream yet, so retrying is always safe
// here.
//
// The upstream validates a response.create before it starts the response:
// previous_response_not_found and websocket_connection_limit_reached arrive as
// the first response frame, in place of response.created (codex-rs treats
// both as retryable errors of the request itself). Only connection telemetry
// (codex.rate_limits, metadata) may precede them, so those frames are held
// until the first other frame and everything from response.created on is
// released at once, as the plain HTTP endpoint would stream it. Holding the
// stream longer is the job of the opt-in stream-bootstrap-buffering layer.
func (t *codexWSRoundTripper) peek(ctx context.Context, conn *codexWSConn, incremental bool) codexWSPeekResult {
	var events [][]byte
	for {
		event, errNext := conn.next(ctx.Done(), ctx.Err)
		if errNext == nil {
			errNext = event.err
		}
		if errNext != nil {
			// A cancelled request closes its socket, so a read error may be
			// the cancellation itself.
			if errCtx := ctx.Err(); errCtx != nil {
				return codexWSPeekResult{kind: codexWSPeekContextDone, err: errCtx}
			}
			return codexWSPeekResult{kind: codexWSPeekReconnect, err: errNext}
		}
		payload := bytes.TrimSpace(event.payload)
		if len(payload) == 0 {
			continue
		}
		events = append(events, payload)
		eventType := gjson.GetBytes(payload, "type").String()
		switch eventType {
		case "codex.rate_limits", "codex.response.metadata", "response.metadata":
			// Connection telemetry sent ahead of the response; a rejection can
			// still follow. Their headers also become response headers.
			if len(events) < codexWSMaxPeekEvents {
				continue
			}
			return codexWSPeekResult{kind: codexWSPeekCommit, events: events}
		case "error", "response.failed":
			code := codexWSErrorCode(payload)
			switch {
			case code == codexWSErrorPreviousResponseNotFound && incremental:
				return codexWSPeekResult{kind: codexWSPeekResendFull, err: errors.New(code)}
			case code == codexWSErrorConnectionLimitReached:
				return codexWSPeekResult{kind: codexWSPeekReconnect, err: errors.New(code)}
			}
			if eventType == "error" && codexWSErrorStatus(payload) > 0 {
				return codexWSPeekResult{kind: codexWSPeekErrorResponse, events: events}
			}
			return codexWSPeekResult{kind: codexWSPeekCommit, events: events}
		default:
			return codexWSPeekResult{kind: codexWSPeekCommit, events: events}
		}
	}
}

// logCodexWSDiscardedEvents records frames that never reach the HTTP pipeline
// (which logs everything it does receive) in the websocket timeline.
func logCodexWSDiscardedEvents(ctx context.Context, cfg *config.Config, events [][]byte) {
	for _, payload := range events {
		AppendAPIWebsocketResponse(ctx, cfg, payload)
	}
}

func codexWSModeLabel(incremental bool) string {
	if incremental {
		return "incremental"
	}
	return "full"
}

func codexWSErrorCode(payload []byte) string {
	for _, path := range []string{"error.code", "response.error.code", "body.error.code", "error.type", "code"} {
		if value := strings.TrimSpace(gjson.GetBytes(payload, path).String()); value != "" {
			if value == codexWSErrorPreviousResponseNotFound || value == codexWSErrorConnectionLimitReached {
				return value
			}
		}
	}
	return ""
}

func codexWSErrorStatus(payload []byte) int {
	status := int(gjson.GetBytes(payload, "status").Int())
	if status == 0 {
		status = int(gjson.GetBytes(payload, "status_code").Int())
	}
	return status
}

// buildCodexWSMessage turns an HTTP request body into a response.create
// message. Per-turn routing headers that a websocket cannot resend on an open
// connection travel in client_metadata, as the Codex CLI does on its websocket.
func buildCodexWSMessage(body []byte, headers http.Header) ([]byte, error) {
	out, err := sjson.SetBytes(body, "type", "response.create")
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"x-codex-turn-state", "x-codex-turn-metadata"} {
		value := strings.TrimSpace(headers.Get(name))
		if value == "" {
			continue
		}
		path := "client_metadata." + name
		if gjson.GetBytes(out, path).Exists() {
			continue
		}
		if updated, errSet := sjson.SetBytes(out, path, value); errSet == nil {
			out = updated
		}
	}
	return out, nil
}

// codexWSVolatileHeader reports per-request handshake response headers that a
// reused socket must not report again: quota snapshots and per-turn routing
// state would be stale.
func codexWSVolatileHeader(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	switch lower {
	case "x-codex-turn-state", "x-request-id", "x-oai-request-id", "openai-request-id", "cf-ray", "date", "set-cookie":
		return true
	}
	return isCodexQuotaHeaderName(lower)
}

func codexWSHopByHopHeader(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "connection", "upgrade", "sec-websocket-accept", "sec-websocket-extensions", "sec-websocket-protocol", "content-length", "transfer-encoding", "keep-alive":
		return true
	}
	return false
}

// codexWSResponseHeaders synthesizes the HTTP response headers: the handshake
// headers (minus per-request values when the socket is reused), quota headers
// from codex.rate_limits frames and headers carried by metadata frames.
func codexWSResponseHeaders(handshake http.Header, fresh bool, events [][]byte) http.Header {
	headers := make(http.Header)
	for key, values := range handshake {
		if codexWSHopByHopHeader(key) || (!fresh && codexWSVolatileHeader(key)) {
			continue
		}
		headers[key] = append([]string(nil), values...)
	}
	for _, payload := range events {
		switch gjson.GetBytes(payload, "type").String() {
		case "codex.rate_limits":
			mergeCodexWSHeaders(headers, ParseCodexQuotaEventHeaders(payload))
		case "codex.response.metadata", "response.metadata", "error":
			mergeCodexWSHeaders(headers, codexWSEventHeaders(payload))
		}
	}
	headers.Set("Content-Type", "text/event-stream")
	return headers
}

func mergeCodexWSHeaders(dst, src http.Header) {
	for key, values := range src {
		dst[http.CanonicalHeaderKey(key)] = append([]string(nil), values...)
	}
}

func codexWSEventHeaders(payload []byte) http.Header {
	node := gjson.GetBytes(payload, "headers")
	if !node.IsObject() {
		return nil
	}
	headers := make(http.Header)
	node.ForEach(func(key, value gjson.Result) bool {
		name := strings.TrimSpace(key.String())
		if name == "" || codexWSHopByHopHeader(name) {
			return true
		}
		switch value.Type {
		case gjson.String:
			if v := strings.TrimSpace(value.String()); v != "" && !strings.ContainsAny(v, "\r\n") {
				headers.Set(name, v)
			}
		case gjson.Number, gjson.True, gjson.False:
			headers.Set(name, strings.TrimSpace(value.Raw))
		}
		return true
	})
	return headers
}

// codexWSErrorResponse converts a websocket error frame into the HTTP error
// response the plain HTTP endpoint would have returned.
func codexWSErrorResponse(req *http.Request, handshake http.Header, fresh bool, events [][]byte, payload []byte) *http.Response {
	status := codexWSErrorStatus(payload)
	headers := codexWSResponseHeaders(handshake, fresh, events)
	headers.Set("Content-Type", "application/json")
	body := []byte(`{}`)
	if node := gjson.GetBytes(payload, "error"); node.Exists() {
		body, _ = sjson.SetRawBytes(body, "error", []byte(node.Raw))
	} else if node = gjson.GetBytes(payload, "body.error"); node.Exists() {
		body, _ = sjson.SetRawBytes(body, "error", []byte(node.Raw))
	} else {
		body, _ = sjson.SetBytes(body, "error.type", "server_error")
		body, _ = sjson.SetBytes(body, "error.message", http.StatusText(status))
	}
	body, _ = sjson.SetBytes(body, "status", status)
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		StatusCode:    status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        headers,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

func codexWSAuthFingerprint(headers http.Header) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(headers.Get("Authorization")) + "\x00" + strings.TrimSpace(headers.Get("Chatgpt-Account-Id"))))
	return hex.EncodeToString(sum[:8])
}

// codexWSCompatFingerprint identifies the handshake a socket was opened with.
// Headers that legitimately change per request are excluded; any other header
// change (model routing hint, beta features, cloaking identity) needs a new
// socket because a websocket cannot change its handshake.
func codexWSCompatFingerprint(target string, headers http.Header) string {
	rendered := sortedHeaderFingerprint(headers, func(key string) bool {
		switch strings.ToLower(key) {
		case "x-client-request-id", "x-codex-turn-state", "x-codex-turn-metadata":
			return true
		}
		return false
	})
	sum := sha256.Sum256([]byte(target + "\x00" + rendered))
	return hex.EncodeToString(sum[:])
}

// codexWSBody streams one websocket response as SSE frames and returns the
// socket to the pool once the response completes.
type codexWSBody struct {
	ctx   context.Context
	cfg   *config.Config
	pool  *CodexWSPool
	conn  *codexWSConn
	shape codexWSRequestShape

	mu       sync.Mutex
	pending  [][]byte
	current  []byte
	finished bool
	err      error

	outputs     map[int64][]byte
	outputOrder [][]byte
	responseID  string

	releaseOnce sync.Once
}

func (b *codexWSBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for len(b.current) == 0 {
		if b.err != nil {
			return 0, b.err
		}
		if b.finished {
			return 0, io.EOF
		}
		var payload []byte
		if len(b.pending) > 0 {
			payload = b.pending[0]
			b.pending = b.pending[1:]
		} else {
			event, errNext := b.conn.next(b.ctx.Done(), b.ctx.Err)
			if errNext == nil && event.err != nil {
				errNext = event.err
			}
			if errNext != nil {
				// A cancelled request closes its socket; report the
				// cancellation rather than the resulting connection error.
				if errCtx := b.ctx.Err(); errCtx != nil {
					errNext = errCtx
				}
				b.err = errNext
				b.release(nil, false, "read_error")
				return 0, errNext
			}
			payload = bytes.TrimSpace(event.payload)
			if len(payload) == 0 {
				continue
			}
		}
		b.current = b.process(payload)
	}
	n := copy(p, b.current)
	b.current = b.current[n:]
	return n, nil
}

// process records what the pool needs from one frame and renders it as SSE.
func (b *codexWSBody) process(payload []byte) []byte {
	eventType := gjson.GetBytes(payload, "type").String()
	if eventType == "response.done" {
		if updated, errSet := sjson.SetBytes(payload, "type", "response.completed"); errSet == nil {
			payload = updated
			eventType = "response.completed"
		}
	}
	switch eventType {
	case "codex.rate_limits":
		logging.MergeResponseHeaders(b.ctx, ParseCodexQuotaEventHeaders(payload))
	case "response.created":
		if id := gjson.GetBytes(payload, "response.id").String(); id != "" {
			b.responseID = id
		}
	case "response.output_item.done":
		item := gjson.GetBytes(payload, "item")
		if item.IsObject() {
			if index := gjson.GetBytes(payload, "output_index"); index.Exists() {
				b.outputs[index.Int()] = []byte(item.Raw)
			} else {
				b.outputOrder = append(b.outputOrder, []byte(item.Raw))
			}
		}
	case "response.completed":
		if id := gjson.GetBytes(payload, "response.id").String(); id != "" {
			b.responseID = id
		}
		b.finished = true
		b.release(newCodexWSBaseline(b.shape, b.outputItems(payload), b.responseID), true, "completed")
	case "error":
		// Quota and retry headers ride on websocket error frames; the HTTP
		// path sees them as response headers.
		logging.MergeResponseHeaders(b.ctx, codexWSEventHeaders(payload))
		payload = normalizeCodexWSErrorFrame(payload)
		b.finished = true
		b.release(nil, false, eventType)
	case "response.incomplete", "response.failed":
		b.finished = true
		b.release(nil, false, eventType)
	}

	var out bytes.Buffer
	out.Grow(len(payload) + len(eventType) + 16)
	if eventType != "" {
		out.WriteString("event: ")
		out.WriteString(eventType)
		out.WriteByte('\n')
	}
	out.WriteString("data: ")
	out.Write(payload)
	out.WriteString("\n\n")
	return out.Bytes()
}

// normalizeCodexWSErrorFrame rewrites a websocket error envelope into the
// shape the HTTP SSE pipeline reads terminal errors from. The websocket wraps
// the HTTP error as {"type":"error","status":401,"body":{"error":{...}},
// "headers":{...}}, while the SSE parser only looks at the event's error object
// and takes the status from error.status_code. Without this the details and
// the status are lost and the failure surfaces as a generic 502, so a 401
// would miss the credential's unauthorized handling. The wrapped body error is
// preferred, as the downstream-websocket executor does
// (buildCodexWebsocketErrorPayload), and the explicit status overrides one the
// error object may carry. Frames without an explicit status are left alone.
func normalizeCodexWSErrorFrame(payload []byte) []byte {
	status := codexWSErrorStatus(payload)
	if status <= 0 {
		return payload
	}
	out := payload
	errorNode := gjson.GetBytes(payload, "body.error")
	if !errorNode.Exists() {
		errorNode = gjson.GetBytes(payload, "error")
	}
	var errorObject []byte
	switch {
	case errorNode.IsObject():
		errorObject = []byte(errorNode.Raw)
	case errorNode.Exists() && strings.TrimSpace(errorNode.String()) != "":
		errorObject, _ = sjson.SetBytes([]byte(`{}`), "message", strings.TrimSpace(errorNode.String()))
	default:
		errorObject = []byte(`{}`)
		errorObject, _ = sjson.SetBytes(errorObject, "type", "server_error")
		errorObject, _ = sjson.SetBytes(errorObject, "message", http.StatusText(status))
	}
	errorObject, _ = sjson.SetBytes(errorObject, "status_code", status)
	if updated, errSet := sjson.SetRawBytes(out, "error", errorObject); errSet == nil {
		out = updated
	}
	return out
}

// outputItems returns the response's output items in output order, falling
// back to the completed response's output array.
func (b *codexWSBody) outputItems(completed []byte) [][]byte {
	if len(b.outputs) > 0 {
		indexes := make([]int64, 0, len(b.outputs))
		for index := range b.outputs {
			indexes = append(indexes, index)
		}
		sort.Slice(indexes, func(i, j int) bool { return indexes[i] < indexes[j] })
		items := make([][]byte, 0, len(indexes)+len(b.outputOrder))
		for _, index := range indexes {
			items = append(items, b.outputs[index])
		}
		return append(items, b.outputOrder...)
	}
	if len(b.outputOrder) > 0 {
		return b.outputOrder
	}
	var items [][]byte
	gjson.GetBytes(completed, "response.output").ForEach(func(_, item gjson.Result) bool {
		items = append(items, []byte(item.Raw))
		return true
	})
	return items
}

func (b *codexWSBody) release(baseline *codexWSBaseline, healthy bool, reason string) {
	b.releaseOnce.Do(func() {
		b.pool.release(b.conn, baseline, healthy, reason)
	})
}

// Close abandons the response if it has not completed; the socket is then in
// an unknown state and is closed rather than reused.
func (b *codexWSBody) Close() error {
	b.release(nil, false, "abandoned")
	return nil
}
