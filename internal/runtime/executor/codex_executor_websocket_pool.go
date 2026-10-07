package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

// codexHTTPWebsocketPoolSettings maps the config onto pool settings.
func codexHTTPWebsocketPoolSettings(cfg *config.Config) helps.CodexWSPoolSettings {
	settings := helps.DefaultCodexWSPoolSettings()
	if cfg == nil {
		return settings
	}
	pool := cfg.Codex.HTTPWebsocketPool
	settings.Enabled = pool.IsEnabled()
	if idle := pool.IdleTimeoutDuration(); idle > 0 {
		settings.IdleTimeout = idle
	}
	if pool.MaxSockets > 0 {
		settings.MaxSockets = pool.MaxSockets
	}
	if pool.MaxSocketsPerAuth > 0 {
		settings.MaxSocketsPerAuth = pool.MaxSocketsPerAuth
	}
	return settings
}

// CloseCodexHTTPWebsocketPoolForAuthID closes pooled HTTP-serving websockets of
// one credential. Idle sockets close immediately; in-flight responses finish
// first.
func CloseCodexHTTPWebsocketPoolForAuthID(authID string, reason string) {
	helps.DefaultCodexWSPool().CloseAuth(authID, reason)
}

func (e *CodexExecutor) codexWSPool() *helps.CodexWSPool {
	if e != nil && e.wsPool != nil {
		return e.wsPool
	}
	return helps.DefaultCodexWSPool()
}

// codexHTTPWebsocketEligible reports whether a plain-HTTP downstream request
// may be served over the upstream websocket pool.
func (e *CodexExecutor) codexHTTPWebsocketEligible(ctx context.Context, auth *cliproxyauth.Auth) bool {
	if e == nil || auth == nil {
		return false
	}
	if e.cfg != nil && !e.cfg.Codex.HTTPWebsocketPool.IsEnabled() {
		return false
	}
	// Downstream websocket clients have their own upstream session handling.
	if cliproxyexecutor.DownstreamWebsocket(ctx) {
		return false
	}
	return codexWebsocketsEnabled(auth)
}

// withCodexHTTPWebsocket returns client unchanged when the request is not
// eligible, or a copy whose transport serves the Responses POST over a pooled
// upstream websocket and falls back to client's own transport otherwise.
func (e *CodexExecutor) withCodexHTTPWebsocket(ctx context.Context, auth *cliproxyauth.Auth, client *http.Client, httpURL string, requestHeaders http.Header, upstreamBody []byte) *http.Client {
	if client == nil || !e.codexHTTPWebsocketEligible(ctx, auth) {
		return client
	}
	conversation := codexHTTPWebsocketConversationKey(ctx, upstreamBody, requestHeaders)
	if conversation == "" {
		return client
	}
	wsURL, errURL := buildCodexResponsesWebsocketURL(httpURL)
	if errURL != nil {
		return client
	}
	authType, authValue := auth.AccountInfo()
	cfg := e.cfg
	wrapped := *client
	wrapped.Transport = helps.NewCodexWSRoundTripper(helps.CodexWSRoundTripConfig{
		Pool:           e.codexWSPool(),
		Base:           client.Transport,
		Config:         cfg,
		AuthID:         auth.ID,
		AuthLabel:      auth.Label,
		AuthType:       authType,
		AuthValue:      authValue,
		Conversation:   conversation,
		Target:         wsURL + "\x00" + executionProxyURL(ctx, cfg, auth),
		URL:            wsURL,
		Body:           upstreamBody,
		PrepareHeaders: codexHTTPWebsocketHandshakeHeaders,
		Dial: func(dialCtx context.Context, headers http.Header) (*websocket.Conn, *http.Response, error) {
			conn, _, resp, errDial := dialCodexResponsesWebsocket(dialCtx, cfg, auth, wsURL, headers)
			return conn, resp, errDial
		},
	})
	return &wrapped
}

// codexHTTPWebsocketHandshakeHeaders adapts the headers of the HTTP POST to a
// websocket handshake: hop-by-hop and body headers are dropped, per-turn
// headers move into client_metadata (see helps.buildCodexWSMessage), the
// session header uses the websocket spelling, and the Responses websocket beta
// is added.
func codexHTTPWebsocketHandshakeHeaders(headers http.Header) http.Header {
	if headers == nil {
		headers = http.Header{}
	}
	for _, name := range []string{
		"Content-Type", "Content-Length", "Accept", "Accept-Encoding", "Connection", "Upgrade", "Host",
		"Keep-Alive", "Transfer-Encoding", "Te", "Sec-Websocket-Key", "Sec-Websocket-Version",
		"Sec-Websocket-Extensions", "Sec-Websocket-Protocol",
		"X-Codex-Turn-State", "X-Codex-Turn-Metadata",
	} {
		deleteHeaderCaseInsensitive(headers, name)
	}
	ensureCodexWebsocketSessionHeader(headers, nil, "")
	beta := strings.TrimSpace(headers.Get("OpenAI-Beta"))
	switch {
	case beta == "":
		beta = codexResponsesWebsocketBetaHeaderValue
	case !strings.Contains(beta, "responses_websockets="):
		beta = beta + "," + codexResponsesWebsocketBetaHeaderValue
	}
	headers.Set("OpenAI-Beta", beta)
	return headers
}

// codexHTTPWebsocketConversationKey derives the conversation identity used to
// key pooled sockets. It reuses what the HTTP path already chose for prompt
// caching - the prompt_cache_key (explicit, Claude Code session, execution or
// derived session) or the upstream session header - but rejects the
// per-API-key fallback the Chat Completions path uses, which spans every
// conversation of a client. The key is scoped to the downstream API key so
// tenants never share sockets.
func codexHTTPWebsocketConversationKey(ctx context.Context, upstreamBody []byte, headers http.Header) string {
	apiKey := strings.TrimSpace(helps.APIKeyFromContext(ctx))
	perClientFallback := ""
	if apiKey != "" {
		perClientFallback = uuid.NewSHA1(uuid.NameSpaceOID, []byte("cli-proxy-api:codex:prompt-cache:"+apiKey)).String()
	}
	usable := func(value string) string {
		value = strings.TrimSpace(value)
		if value == "" || value == perClientFallback {
			return ""
		}
		return value
	}
	conversation := usable(gjson.GetBytes(upstreamBody, "prompt_cache_key").String())
	if conversation == "" {
		conversation = usable(codexSessionHeaderValue(headers))
	}
	if conversation == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("codex-ws-pool\x00" + apiKey + "\x00" + conversation))
	return hex.EncodeToString(sum[:])
}
