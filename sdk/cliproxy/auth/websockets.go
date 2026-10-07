package auth

import (
	"strconv"
	"strings"
)

// AttributeWebsockets is the attribute and metadata key that toggles the
// upstream Responses websocket transport for a credential.
const AttributeWebsockets = "websockets"

// WebsocketsEnabled reports whether the upstream Responses websocket transport
// is enabled for auth. It is the single source of truth shared by the
// executor, the selector and the API handlers.
func WebsocketsEnabled(auth *Auth) bool {
	enabled, _ := WebsocketsSetting(auth)
	return enabled
}

// WebsocketsSetting returns the effective websocket setting for auth and
// whether it was set explicitly through the "websockets" attribute or
// metadata key.
//
// An explicit value always wins, so `websockets: false` opts a credential out.
// When the key is absent, Codex OAuth/file credentials default to enabled:
// the ChatGPT backend always supports the Responses websocket (the Codex CLI's
// built-in provider declares supports_websockets), and an opt-in default made
// websocket clients filter the candidate set down to the few credentials that
// had it switched on. Codex API-key entries, which synthesize the attribute
// from their config, and every other provider keep the opt-in default.
func WebsocketsSetting(auth *Auth) (enabled bool, explicit bool) {
	if auth == nil {
		return false, false
	}
	if len(auth.Attributes) > 0 {
		if raw := strings.TrimSpace(auth.Attributes[AttributeWebsockets]); raw != "" {
			if parsed, errParse := strconv.ParseBool(raw); errParse == nil {
				return parsed, true
			}
		}
	}
	if len(auth.Metadata) > 0 {
		if parsed, ok := parseWebsocketsMetadataValue(auth.Metadata[AttributeWebsockets]); ok {
			return parsed, true
		}
	}
	return websocketsDefault(auth), false
}

func parseWebsocketsMetadataValue(raw any) (bool, bool) {
	switch v := raw.(type) {
	case bool:
		return v, true
	case string:
		parsed, errParse := strconv.ParseBool(strings.TrimSpace(v))
		if errParse == nil {
			return parsed, true
		}
	}
	return false, false
}

// websocketsDefault is the value used when a credential does not set the key.
func websocketsDefault(auth *Auth) bool {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	if auth.AuthKind() == AuthKindAPIKey {
		return false
	}
	// Config-sourced Codex credentials are codex-api-key entries, including
	// base-url-only entries that carry no api_key attribute.
	return auth.AuthSourceKind() != AuthSourceConfig
}
