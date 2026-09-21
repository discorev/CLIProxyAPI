package executor

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func (e *ClaudeExecutor) prepareClaudeNativePassthroughRequest(
	auth *cliproxyauth.Auth,
	apiKey string,
	upstreamURL string,
	upstreamModel string,
	originalPayload []byte,
	incomingHeaders http.Header,
	claudeSessionID string,
	countTokens bool,
	cchSigning bool,
) ([]byte, http.Header, error) {
	parsedUpstreamURL, errURL := url.Parse(upstreamURL)
	if errURL != nil {
		return nil, nil, fmt.Errorf("parse Claude passthrough upstream URL: %w", errURL)
	}

	body := originalPayload
	if currentModel := gjson.GetBytes(body, "model").String(); upstreamModel != "" && currentModel != upstreamModel {
		updated, errSet := sjson.SetBytes(body, "model", upstreamModel)
		if errSet != nil {
			return nil, nil, fmt.Errorf("set Claude passthrough model: %w", errSet)
		}
		body = updated
	}

	fingerprint := resolveClaudeFingerprintPolicy(e.cfg, auth, apiKey)
	if fingerprint.AuthIsOAuthToken && !countTokens && gjson.GetBytes(body, "metadata").Exists() {
		updated, _, errMetadata := helps.ApplyClaudeCredentialMetadata(body, auth, claudeSessionID)
		if errMetadata != nil {
			return nil, nil, fmt.Errorf("apply Claude passthrough credential metadata: %w", errMetadata)
		}
		body = updated
	}

	if cchSigning && !countTokens {
		updated, errCCH := finalizeAnthropicMessagesBodyCCH(body, "")
		if errCCH != nil {
			return nil, nil, fmt.Errorf("finalize Claude passthrough CCH: %w", errCCH)
		}
		body = updated
	}

	headers := cloneClaudePassthroughHeaders(incomingHeaders)
	applyClaudeUpstreamAuth(headers, parsedUpstreamURL, auth, apiKey)

	if fingerprint.AuthIsOAuthToken {
		incomingBetas := strings.Join(helps.HeaderValuesCaseInsensitive(headers, "Anthropic-Beta"), ",")
		includeExtendedCacheTTL := false
		if !countTokens {
			isSubagent := helps.IsClaudeSubagentRequest(incomingHeaders, body)
			includeExtendedCacheTTL = (!isSubagent || helps.ClaudeSubagentRequests1h(incomingHeaders, body)) && !helps.IsClaudeProbeOrHelperRequest(body)
		}
		deleteClaudeHeaderCaseInsensitive(headers, "Anthropic-Beta")
		headers.Set("Anthropic-Beta", appendClaudeOAuthCredentialBetas(incomingBetas, includeExtendedCacheTTL))
	}
	if helps.HeaderValueCaseInsensitive(headers, "Content-Type") == "" {
		headers.Set("Content-Type", "application/json")
	}
	if claudeSessionID != "" && helps.HeaderValueCaseInsensitive(headers, "X-Claude-Code-Session-Id") == "" {
		headers.Set("X-Claude-Code-Session-Id", claudeSessionID)
	}
	return body, headers, nil
}

func cloneClaudePassthroughHeaders(incoming http.Header) http.Header {
	headers := incoming.Clone()
	connectionScoped := make(map[string]bool)
	for _, value := range helps.HeaderValuesCaseInsensitive(headers, "Connection") {
		for _, name := range strings.Split(value, ",") {
			if name = strings.TrimSpace(name); name != "" {
				connectionScoped[strings.ToLower(name)] = true
			}
		}
	}
	for name := range headers {
		lowerName := strings.ToLower(strings.TrimSpace(name))
		if connectionScoped[lowerName] || claudePassthroughHeaderBlocked(lowerName) {
			delete(headers, name)
		}
	}
	return headers
}

func claudePassthroughHeaderBlocked(lowerName string) bool {
	if strings.HasPrefix(lowerName, "proxy-") {
		return true
	}
	switch lowerName {
	case "connection", "keep-alive", "te", "trailer", "transfer-encoding", "upgrade",
		"content-length", "host", "authorization", "x-api-key", "x-goog-api-key", "x-management-key":
		return true
	default:
		return false
	}
}

func deleteClaudeHeaderCaseInsensitive(headers http.Header, target string) {
	for name := range headers {
		if strings.EqualFold(name, target) {
			delete(headers, name)
		}
	}
}
