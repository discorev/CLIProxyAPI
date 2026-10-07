package helps

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// Defaults for the pool of upstream Codex Responses websockets that serve
// plain-HTTP downstream requests.
const (
	DefaultCodexWSPoolIdleTimeout               = 10 * time.Minute
	DefaultCodexWSPoolMaxSockets                = 512
	DefaultCodexWSPoolMaxSocketsPerAuth         = 64
	DefaultCodexWSPoolMaxSocketsPerConversation = 4
	// DefaultCodexWSPoolMaxAge keeps new responses off sockets approaching the
	// upstream's 60-minute connection cap (websocket_connection_limit_reached),
	// leaving room for a long response to finish on the socket it started on.
	DefaultCodexWSPoolMaxAge = 50 * time.Minute
	// DefaultCodexWSPoolDialBackoff pauses websocket dials for a credential after
	// a failed handshake, so an upstream that refuses upgrades costs one extra
	// round trip per backoff window rather than one per request.
	DefaultCodexWSPoolDialBackoff = 2 * time.Minute

	codexWSPoolJanitorInterval = 30 * time.Second
	codexWSEventBuffer         = 1024
)

// CodexWSPoolSettings tunes the HTTP-to-websocket pool.
type CodexWSPoolSettings struct {
	Enabled                   bool
	IdleTimeout               time.Duration
	MaxSockets                int
	MaxSocketsPerAuth         int
	MaxSocketsPerConversation int
	MaxAge                    time.Duration
	DialBackoff               time.Duration
}

// DefaultCodexWSPoolSettings returns the built-in pool settings.
func DefaultCodexWSPoolSettings() CodexWSPoolSettings {
	return CodexWSPoolSettings{
		Enabled:                   true,
		IdleTimeout:               DefaultCodexWSPoolIdleTimeout,
		MaxSockets:                DefaultCodexWSPoolMaxSockets,
		MaxSocketsPerAuth:         DefaultCodexWSPoolMaxSocketsPerAuth,
		MaxSocketsPerConversation: DefaultCodexWSPoolMaxSocketsPerConversation,
		MaxAge:                    DefaultCodexWSPoolMaxAge,
		DialBackoff:               DefaultCodexWSPoolDialBackoff,
	}
}

func (s CodexWSPoolSettings) normalized() CodexWSPoolSettings {
	defaults := DefaultCodexWSPoolSettings()
	if s.IdleTimeout <= 0 {
		s.IdleTimeout = defaults.IdleTimeout
	}
	if s.MaxSockets <= 0 {
		s.MaxSockets = defaults.MaxSockets
	}
	if s.MaxSocketsPerAuth <= 0 {
		s.MaxSocketsPerAuth = defaults.MaxSocketsPerAuth
	}
	if s.MaxSocketsPerConversation <= 0 {
		s.MaxSocketsPerConversation = defaults.MaxSocketsPerConversation
	}
	if s.MaxAge <= 0 {
		s.MaxAge = defaults.MaxAge
	}
	if s.DialBackoff < 0 {
		s.DialBackoff = 0
	}
	return s
}

// CodexWSPoolStats counts pool decisions. It is used by tests and debug logs.
type CodexWSPoolStats struct {
	Dials           int
	DialFailures    int
	Incremental     int
	Full            int
	Recoveries      int
	HTTPFallbacks   int
	Evicted         int
	OpenSockets     int
	PreviousMissing int
}

type codexWSGroupKey struct {
	authID       string
	conversation string
}

// CodexWSPool keeps upstream Codex Responses websockets warm between plain-HTTP
// requests of one conversation on one credential, so follow-up turns can send
// only new input with previous_response_id.
//
// The pool never chooses credentials: the conductor selects one per request and
// the pool only looks up sockets already open for that (credential,
// conversation) pair. A socket carries at most one in-flight response.
type CodexWSPool struct {
	mu       sync.Mutex
	settings CodexWSPoolSettings
	now      func() time.Time

	conns   map[*codexWSConn]struct{}
	groups  map[codexWSGroupKey][]*codexWSConn
	authsBy map[string]map[string]struct{} // conversation -> auth IDs with sockets
	pending map[codexWSGroupKey]int
	backoff map[string]time.Time

	pendingTotal  int
	pendingByAuth map[string]int

	// generation and authGeneration advance whenever sockets are invalidated
	// (pool disabled or closed, credential removed). A dial reserves its slot
	// under one generation; adopt compares it, so a handshake that completes
	// after the invalidation serves its own request but never joins the pool.
	generation     uint64
	authGeneration map[string]uint64

	nextID         uint64
	janitorRunning bool
	stats          CodexWSPoolStats
}

// NewCodexWSPool creates a pool. A nil clock uses time.Now.
func NewCodexWSPool(settings CodexWSPoolSettings, now func() time.Time) *CodexWSPool {
	if now == nil {
		now = time.Now
	}
	return &CodexWSPool{
		settings:       settings.normalized(),
		now:            now,
		conns:          make(map[*codexWSConn]struct{}),
		groups:         make(map[codexWSGroupKey][]*codexWSConn),
		authsBy:        make(map[string]map[string]struct{}),
		pending:        make(map[codexWSGroupKey]int),
		backoff:        make(map[string]time.Time),
		pendingByAuth:  make(map[string]int),
		authGeneration: make(map[string]uint64),
	}
}

var defaultCodexWSPool = NewCodexWSPool(DefaultCodexWSPoolSettings(), nil)

// DefaultCodexWSPool returns the process-wide pool used by the Codex executor.
func DefaultCodexWSPool() *CodexWSPool {
	return defaultCodexWSPool
}

// Configure applies new settings. Disabling the pool closes every idle socket
// and retires in-flight ones. Lowered caps close the least recently used idle
// sockets at once; in-flight sockets still over a cap close as their responses
// finish (see release).
func (p *CodexWSPool) Configure(settings CodexWSPoolSettings) {
	if p == nil {
		return
	}
	settings = settings.normalized()
	p.mu.Lock()
	p.settings = settings
	var toClose []*codexWSConn
	if !settings.Enabled {
		p.generation++
		toClose = p.retireLocked(func(*codexWSConn) bool { return true })
	} else {
		toClose = p.evictLocked(p.now())
		toClose = append(toClose, p.enforceCapsLocked()...)
	}
	p.mu.Unlock()
	closeCodexWSConns(toClose, "pool_reconfigured")
}

// Settings returns the active settings.
func (p *CodexWSPool) Settings() CodexWSPoolSettings {
	if p == nil {
		return CodexWSPoolSettings{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.settings
}

// Stats returns a snapshot of the pool counters.
func (p *CodexWSPool) Stats() CodexWSPoolStats {
	if p == nil {
		return CodexWSPoolStats{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	stats := p.stats
	stats.OpenSockets = len(p.conns)
	return stats
}

// CloseAuth closes idle sockets of authID and retires in-flight ones so they
// close as soon as their response completes.
func (p *CodexWSPool) CloseAuth(authID string, reason string) {
	authID = strings.TrimSpace(authID)
	if p == nil || authID == "" {
		return
	}
	p.mu.Lock()
	p.authGeneration[authID]++
	toClose := p.retireLocked(func(c *codexWSConn) bool { return c.group.authID == authID })
	for key := range p.backoff {
		if strings.HasPrefix(key, authID+"\x00") {
			delete(p.backoff, key)
		}
	}
	p.mu.Unlock()
	closeCodexWSConns(toClose, reason)
}

// CloseAll closes idle sockets and retires in-flight ones.
func (p *CodexWSPool) CloseAll(reason string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.generation++
	toClose := p.retireLocked(func(*codexWSConn) bool { return true })
	p.mu.Unlock()
	closeCodexWSConns(toClose, reason)
}

// EvictIdle closes sockets that have been idle past the idle timeout or are
// too old to start another response. It returns the number closed.
func (p *CodexWSPool) EvictIdle() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	toClose := p.evictLocked(p.now())
	p.mu.Unlock()
	closeCodexWSConns(toClose, "idle_evicted")
	return len(toClose)
}

// retireLocked detaches matching idle sockets for closing and marks matching
// busy sockets to close on release.
func (p *CodexWSPool) retireLocked(match func(*codexWSConn) bool) []*codexWSConn {
	var toClose []*codexWSConn
	for c := range p.conns {
		if match(c) {
			p.retireConnLocked(c, &toClose)
		}
	}
	return toClose
}

// retireConnLocked takes one socket out of service. An idle socket, or a busy
// one whose request has already been cancelled, is detached and queued for
// closing now; a busy socket still serving its request finishes the response
// and closes on release.
func (p *CodexWSPool) retireConnLocked(c *codexWSConn, toClose *[]*codexWSConn) {
	if c.busy && !c.leaseCancelledLocked() {
		c.retired = true
		return
	}
	p.removeLocked(c)
	*toClose = append(*toClose, c)
}

// enforceCapsLocked closes least recently used idle sockets until every
// conversation, every credential and the pool as a whole are within their
// caps. Busy sockets are never interrupted; they still count, so a scope that
// is over its cap with only busy sockets left shrinks as they are released.
func (p *CodexWSPool) enforceCapsLocked() []*codexWSConn {
	var toClose []*codexWSConn
	evictWhile := func(over func() bool, match func(*codexWSConn) bool) {
		for over() {
			victim := p.evictLRULocked(match)
			if victim == nil {
				return
			}
			toClose = append(toClose, victim)
		}
	}
	groups := make([]codexWSGroupKey, 0, len(p.groups))
	auths := make(map[string]struct{})
	for key := range p.groups {
		groups = append(groups, key)
		auths[key.authID] = struct{}{}
	}
	for _, key := range groups {
		evictWhile(
			func() bool { return len(p.groups[key])+p.pending[key] > p.settings.MaxSocketsPerConversation },
			func(c *codexWSConn) bool { return c.group == key },
		)
	}
	for authID := range auths {
		evictWhile(
			func() bool { return p.authSocketCountLocked(authID) > p.settings.MaxSocketsPerAuth },
			func(c *codexWSConn) bool { return c.group.authID == authID },
		)
	}
	evictWhile(
		func() bool { return len(p.conns)+p.pendingTotal > p.settings.MaxSockets },
		func(*codexWSConn) bool { return true },
	)
	return toClose
}

func (p *CodexWSPool) evictLocked(now time.Time) []*codexWSConn {
	var toClose []*codexWSConn
	for c := range p.conns {
		if c.busy {
			continue
		}
		if now.Sub(c.lastUsed) >= p.settings.IdleTimeout || now.Sub(c.created) >= p.settings.MaxAge {
			p.removeLocked(c)
			p.stats.Evicted++
			toClose = append(toClose, c)
		}
	}
	for key, until := range p.backoff {
		if !now.Before(until) {
			delete(p.backoff, key)
		}
	}
	return toClose
}

func (p *CodexWSPool) removeLocked(c *codexWSConn) {
	if _, ok := p.conns[c]; !ok {
		return
	}
	delete(p.conns, c)
	group := p.groups[c.group]
	for i := range group {
		if group[i] == c {
			group = append(group[:i], group[i+1:]...)
			break
		}
	}
	if len(group) == 0 {
		delete(p.groups, c.group)
		if auths := p.authsBy[c.group.conversation]; auths != nil {
			delete(auths, c.group.authID)
			if len(auths) == 0 {
				delete(p.authsBy, c.group.conversation)
			}
		}
	} else {
		p.groups[c.group] = group
	}
	c.closed = true
}

func (p *CodexWSPool) authSocketCountLocked(authID string) int {
	count := p.pendingByAuth[authID]
	for key, group := range p.groups {
		if key.authID == authID {
			count += len(group)
		}
	}
	return count
}

// evictLRULocked removes the least recently used idle socket accepted by match.
func (p *CodexWSPool) evictLRULocked(match func(*codexWSConn) bool) *codexWSConn {
	var victim *codexWSConn
	for c := range p.conns {
		if c.busy || !match(c) {
			continue
		}
		if victim == nil || c.lastUsed.Before(victim.lastUsed) {
			victim = c
		}
	}
	if victim != nil {
		p.removeLocked(victim)
		p.stats.Evicted++
	}
	return victim
}

// codexWSLease is the result of acquire: either a socket reserved for this
// request, or a reserved slot to dial a new one (conn == nil).
type codexWSLease struct {
	conn        *codexWSConn
	incremental []string
	responseID  string

	// Invalidation generations at reservation time, checked by adopt.
	generation     uint64
	authGeneration uint64
}

type codexWSAcquireRequest struct {
	group     codexWSGroupKey
	compat    string
	authFP    string
	shape     codexWSRequestShape
	forceFull bool
}

// acquire selects a socket for one request. ok=false means the request should
// use plain HTTP (pool disabled, credential in dial backoff, or every eligible
// socket busy with no room for another).
func (p *CodexWSPool) acquire(req codexWSAcquireRequest) (lease codexWSLease, ok bool) {
	if p == nil {
		return codexWSLease{}, false
	}
	var toClose []*codexWSConn
	defer func() { closeCodexWSConns(toClose, "superseded") }()

	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.settings.Enabled {
		return codexWSLease{}, false
	}
	now := p.now()
	toClose = append(toClose, p.evictLocked(now)...)
	if until, inBackoff := p.backoff[req.group.authID+"\x00"+req.authFP]; inBackoff && now.Before(until) {
		p.stats.HTTPFallbacks++
		return codexWSLease{}, false
	}

	// A refreshed token makes every socket opened with the old one stale.
	for c := range p.conns {
		if c.group.authID != req.group.authID || c.authFP == req.authFP {
			continue
		}
		p.retireConnLocked(c, &toClose)
	}

	// The conductor routed this conversation to a credential; sockets other
	// credentials hold for the same conversation are no longer useful.
	for authID := range p.authsBy[req.group.conversation] {
		if authID == req.group.authID {
			continue
		}
		other := codexWSGroupKey{authID: authID, conversation: req.group.conversation}
		for _, c := range append([]*codexWSConn(nil), p.groups[other]...) {
			p.retireConnLocked(c, &toClose)
		}
	}

	var bestDelta, related *codexWSConn
	bestPrefix, bestShared := -1, 0
	var unused, lru *codexWSConn
	for _, c := range append([]*codexWSConn(nil), p.groups[req.group]...) {
		if c.busy {
			continue
		}
		if c.compat != req.compat || c.authFP != req.authFP || !c.drainIdleEvents() {
			// Stale headers or token, or a socket that saw traffic while idle.
			p.removeLocked(c)
			toClose = append(toClose, c)
			continue
		}
		if !req.forceFull {
			if prefix := codexWSMatchedPrefix(req.shape, c.baseline); prefix > bestPrefix {
				bestPrefix, bestDelta = prefix, c
			}
		}
		if shared := codexWSSharedPrefix(req.shape, c.baseline); shared > bestShared {
			bestShared, related = shared, c
		}
		if c.baseline == nil && unused == nil {
			unused = c
		}
		if lru == nil || c.lastUsed.Before(lru.lastUsed) {
			lru = c
		}
	}

	pick := func(c *codexWSConn, incremental bool) codexWSLease {
		c.busy = true
		c.lastUsed = now
		lease := codexWSLease{conn: c}
		if incremental {
			lease.incremental, _ = codexWSIncrementalItems(req.shape, c.baseline)
			lease.responseID = c.baseline.responseID
		}
		return lease
	}
	// Preference: a socket whose last response this request extends (send only
	// the new items); a socket already serving the same chain, or an unused
	// one (full request); a new socket, so another chain of the conversation
	// (for example a subagent) keeps its baseline; finally the least recently
	// used socket.
	switch {
	case bestDelta != nil:
		return pick(bestDelta, true), true
	case related != nil:
		return pick(related, false), true
	case unused != nil:
		return pick(unused, false), true
	}

	if p.reserveLocked(req.group, &toClose) {
		return codexWSLease{generation: p.generation, authGeneration: p.authGeneration[req.group.authID]}, true
	}
	if lru != nil {
		return pick(lru, false), true
	}
	p.stats.HTTPFallbacks++
	return codexWSLease{}, false
}

// reserveLocked claims room for one more socket in group, evicting idle
// sockets elsewhere when a global or per-credential cap is reached.
func (p *CodexWSPool) reserveLocked(group codexWSGroupKey, toClose *[]*codexWSConn) bool {
	if len(p.groups[group])+p.pending[group] >= p.settings.MaxSocketsPerConversation {
		return false
	}
	for p.authSocketCountLocked(group.authID) >= p.settings.MaxSocketsPerAuth {
		victim := p.evictLRULocked(func(c *codexWSConn) bool { return c.group.authID == group.authID && c.group != group })
		if victim == nil {
			return false
		}
		*toClose = append(*toClose, victim)
	}
	for len(p.conns)+p.pendingTotal >= p.settings.MaxSockets {
		victim := p.evictLRULocked(func(c *codexWSConn) bool { return c.group != group })
		if victim == nil {
			return false
		}
		*toClose = append(*toClose, victim)
	}
	p.pending[group]++
	p.pendingTotal++
	p.pendingByAuth[group.authID]++
	return true
}

func (p *CodexWSPool) unreserveLocked(group codexWSGroupKey) {
	if p.pending[group] > 0 {
		p.pending[group]--
		if p.pending[group] == 0 {
			delete(p.pending, group)
		}
		p.pendingTotal--
		p.pendingByAuth[group.authID]--
		if p.pendingByAuth[group.authID] <= 0 {
			delete(p.pendingByAuth, group.authID)
		}
	}
}

// dialFailed releases a reserved slot and starts the credential's dial backoff.
func (p *CodexWSPool) dialFailed(group codexWSGroupKey, authFP string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.unreserveLocked(group)
	p.stats.DialFailures++
	p.stats.HTTPFallbacks++
	if p.settings.DialBackoff > 0 {
		p.backoff[group.authID+"\x00"+authFP] = p.now().Add(p.settings.DialBackoff)
	}
}

// adopt registers a freshly dialed socket in its reserved slot, leased to the
// caller, and starts its reader. When the pool was disabled or closed, or the
// credential removed, while the handshake was in flight, the socket still
// serves the request that dialed it but is retired, so it closes on release
// instead of joining the pool.
func (p *CodexWSPool) adopt(lease codexWSLease, group codexWSGroupKey, ws *websocket.Conn, compat, authFP string, handshake http.Header) *codexWSConn {
	c := &codexWSConn{
		pool:      p,
		ws:        ws,
		group:     group,
		compat:    compat,
		authFP:    authFP,
		handshake: handshake,
		events:    make(chan codexWSEvent, codexWSEventBuffer),
		closing:   make(chan struct{}),
	}
	p.mu.Lock()
	p.unreserveLocked(group)
	p.nextID++
	c.id = p.nextID
	now := p.now()
	c.created = now
	c.lastUsed = now
	c.busy = true
	c.retired = !p.settings.Enabled || lease.generation != p.generation || lease.authGeneration != p.authGeneration[group.authID]
	p.conns[c] = struct{}{}
	p.groups[group] = append(p.groups[group], c)
	auths := p.authsBy[group.conversation]
	if auths == nil {
		auths = make(map[string]struct{})
		p.authsBy[group.conversation] = auths
	}
	auths[group.authID] = struct{}{}
	p.stats.Dials++
	startJanitor := !p.janitorRunning
	p.janitorRunning = true
	p.mu.Unlock()

	go c.readLoop()
	if startJanitor {
		go p.janitor()
	}
	return c
}

// watchLease ties a leased socket to its request until release: when ctx ends
// first, the socket is detached and closed at once. Closing is what unblocks a
// write or read stuck on an upstream that stopped reading or responding, so an
// abandoned request never pins a goroutine or a socket. No deadline is set on
// the connection; only cancellation closes it.
func (p *CodexWSPool) watchLease(ctx context.Context, c *codexWSConn) {
	if p == nil || c == nil || ctx == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	c.leaseCtx = ctx
	c.leaseStop = context.AfterFunc(ctx, func() { p.cancelLease(c) })
}

// cancelLease closes a socket whose request was cancelled mid-lease.
func (p *CodexWSPool) cancelLease(c *codexWSConn) {
	p.mu.Lock()
	c.cancelled = true
	p.removeLocked(c)
	p.mu.Unlock()
	c.close("context_canceled")
}

// release returns a leased socket. A healthy socket goes back to the pool with
// the baseline of its completed response (nil when the next request must be
// full) unless that puts a scope over its cap; an unhealthy, retired or
// cancelled socket is closed.
func (p *CodexWSPool) release(c *codexWSConn, baseline *codexWSBaseline, healthy bool, reason string) {
	if p == nil || c == nil {
		return
	}
	p.mu.Lock()
	// stopLeaseLocked reports false when the request was cancelled and the
	// watcher is closing the socket: it must never go back to the pool.
	watched := c.stopLeaseLocked()
	closeIt := !healthy || !watched || c.retired || c.closed || c.cancelled || !p.settings.Enabled
	c.busy = false
	c.baseline = baseline
	c.lastUsed = p.now()
	var overCap []*codexWSConn
	if closeIt {
		p.removeLocked(c)
	} else {
		overCap = p.enforceCapsLocked()
	}
	p.mu.Unlock()
	if closeIt {
		c.close(reason)
	}
	closeCodexWSConns(overCap, "over_cap")
}

// readerExited drops a socket whose upstream connection ended.
func (p *CodexWSPool) readerExited(c *codexWSConn) {
	p.mu.Lock()
	idle := !c.busy
	if idle {
		p.removeLocked(c)
	} else {
		c.closed = true
	}
	p.mu.Unlock()
	if idle {
		c.close("upstream_closed")
	}
}

func (p *CodexWSPool) countDecision(incremental bool) {
	p.mu.Lock()
	if incremental {
		p.stats.Incremental++
	} else {
		p.stats.Full++
	}
	p.mu.Unlock()
}

func (p *CodexWSPool) countRecovery(previousMissing bool) {
	p.mu.Lock()
	p.stats.Recoveries++
	if previousMissing {
		p.stats.PreviousMissing++
	}
	p.mu.Unlock()
}

func (p *CodexWSPool) countFallback() {
	p.mu.Lock()
	p.stats.HTTPFallbacks++
	p.mu.Unlock()
}

// janitor periodically closes idle sockets. Eviction decisions use the pool
// clock; the ticker only decides when to look. It is pool housekeeping and
// never touches a socket that is serving a response.
func (p *CodexWSPool) janitor() {
	ticker := time.NewTicker(codexWSPoolJanitorInterval)
	defer ticker.Stop()
	for range ticker.C {
		p.mu.Lock()
		toClose := p.evictLocked(p.now())
		stop := len(p.conns) == 0 && p.pendingTotal == 0
		if stop {
			p.janitorRunning = false
		}
		p.mu.Unlock()
		closeCodexWSConns(toClose, "idle_evicted")
		if stop {
			return
		}
	}
}

func closeCodexWSConns(conns []*codexWSConn, reason string) {
	for _, c := range conns {
		c.close(reason)
	}
}

// codexWSEvent is one message read from an upstream socket.
type codexWSEvent struct {
	payload []byte
	err     error
}

var errCodexWSConnClosed = errors.New("codex websocket pool: connection closed")

// codexWSConn is one pooled upstream socket.
type codexWSConn struct {
	pool      *CodexWSPool
	id        uint64
	ws        *websocket.Conn
	group     codexWSGroupKey
	compat    string
	authFP    string
	handshake http.Header

	// Guarded by pool.mu.
	created   time.Time
	lastUsed  time.Time
	busy      bool
	retired   bool
	closed    bool
	cancelled bool
	baseline  *codexWSBaseline
	leaseCtx  context.Context
	leaseStop func() bool

	events    chan codexWSEvent
	closing   chan struct{}
	closeOnce sync.Once
}

func (c *codexWSConn) readLoop() {
	for {
		msgType, payload, errRead := c.ws.ReadMessage()
		if errRead != nil {
			c.deliver(codexWSEvent{err: errRead})
			c.pool.readerExited(c)
			return
		}
		if msgType == websocket.BinaryMessage {
			c.deliver(codexWSEvent{err: errors.New("codex websocket pool: unexpected binary message")})
			c.pool.readerExited(c)
			c.close("unexpected_binary")
			return
		}
		if msgType != websocket.TextMessage {
			continue
		}
		if !c.deliver(codexWSEvent{payload: payload}) {
			return
		}
	}
}

func (c *codexWSConn) deliver(event codexWSEvent) bool {
	select {
	case c.events <- event:
		return true
	case <-c.closing:
		return false
	}
}

// stopLeaseLocked ends the cancellation watch of the current lease. It reports
// false when the request was cancelled first (the watcher has run or is
// running). Called with pool.mu held.
func (c *codexWSConn) stopLeaseLocked() bool {
	stop := c.leaseStop
	c.leaseStop = nil
	c.leaseCtx = nil
	if stop == nil {
		return true
	}
	return stop()
}

// leaseCancelledLocked reports whether the request holding this socket has
// been cancelled. Called with pool.mu held.
func (c *codexWSConn) leaseCancelledLocked() bool {
	return c.cancelled || (c.leaseCtx != nil && c.leaseCtx.Err() != nil)
}

// drainIdleEvents discards frames that arrived while the socket was idle. It
// reports false when the socket is unusable: the connection ended, or the
// upstream sent a response or error frame no request was waiting for.
// Telemetry that trails a completed response (for example timing events) is
// harmless and dropped. Called with pool.mu held.
func (c *codexWSConn) drainIdleEvents() bool {
	if c.closed {
		return false
	}
	for {
		select {
		case event := <-c.events:
			if event.err != nil {
				return false
			}
			eventType := gjson.GetBytes(event.payload, "type").String()
			if eventType == "error" || strings.HasPrefix(eventType, "response.") {
				return false
			}
		default:
			return true
		}
	}
}

// next waits for the next frame of the in-flight response.
func (c *codexWSConn) next(done <-chan struct{}, doneErr func() error) (codexWSEvent, error) {
	select {
	case event := <-c.events:
		return event, nil
	case <-c.closing:
		select {
		case event := <-c.events:
			return event, nil
		default:
			return codexWSEvent{}, errCodexWSConnClosed
		}
	case <-done:
		return codexWSEvent{}, doneErr()
	}
}

func (c *codexWSConn) write(payload []byte) error {
	return c.ws.WriteMessage(websocket.TextMessage, payload)
}

func (c *codexWSConn) close(reason string) {
	if c == nil {
		return
	}
	c.closeOnce.Do(func() {
		close(c.closing)
		if errClose := c.ws.Close(); errClose != nil {
			log.Debugf("codex websocket pool: close socket=%d error: %v", c.id, errClose)
		}
		log.Debugf("codex websocket pool: closed socket=%d auth=%s reason=%s", c.id, c.group.authID, reason)
	})
}

// sortedHeaderFingerprint renders headers deterministically for fingerprinting.
func sortedHeaderFingerprint(headers http.Header, skip func(string) bool) string {
	keys := make([]string, 0, len(headers))
	for key := range headers {
		if skip != nil && skip(key) {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, key := range keys {
		b.WriteString(strings.ToLower(key))
		b.WriteByte(':')
		b.WriteString(strings.Join(headers[key], ","))
		b.WriteByte('\n')
	}
	return b.String()
}
