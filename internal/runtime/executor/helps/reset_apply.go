package helps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

var resetOrganizationPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ResetHTTPTransport uses the same proxy-aware uTLS path as usage fetches, but
// never follows redirects (a 307 could otherwise replay a spending POST).
func ResetHTTPTransport(cfg *config.Config, prepare func(*http.Request, *cliproxyauth.Auth) error) UsageHTTPRequest {
	return func(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
		if err := prepare(req, auth); err != nil {
			return nil, err
		}
		client := NewUtlsHTTPClient(ctx, cfg, auth, 0)
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		return client.Do(req)
	}
}

func ApplyCodexReset(ctx context.Context, auth *cliproxyauth.Auth, claim cliproxyauth.ResetRequest, baseURL string, request UsageHTTPRequest) (cliproxyauth.ResetResult, error) {
	if !cliproxyauth.UsageFetchable(auth) || !strings.EqualFold(auth.Provider, "codex") {
		return resetRefusal(cliproxyauth.ErrUsageNotFetchable)
	}
	if baseURL == "" {
		baseURL = "https://chatgpt.com"
	}
	requestID, errID := resetRequestID(claim.IdempotencyKey)
	if errID != nil {
		return resetRefusal(errID)
	}
	claimBody := map[string]string{"redeem_request_id": requestID}
	if creditID := strings.TrimSpace(claim.CreditID); creditID != "" {
		claimBody["credit_id"] = creditID
	}
	body, _ := json.Marshal(claimBody)
	accountID, _ := auth.Metadata["account_id"].(string)
	return sendReset(ctx, auth, strings.TrimRight(baseURL, "/")+"/backend-api/wham/rate-limit-reset-credits/consume", body, codexUsageHeaders(strings.TrimSpace(accountID)), request, false)
}

func ApplyClaudeReset(ctx context.Context, auth *cliproxyauth.Auth, claim cliproxyauth.ResetRequest, baseURL string, request UsageHTTPRequest) (cliproxyauth.ResetResult, error) {
	if !cliproxyauth.UsageFetchable(auth) || !strings.EqualFold(auth.Provider, "claude") {
		return resetRefusal(cliproxyauth.ErrUsageNotFetchable)
	}
	if !resetGrantIDPattern.MatchString(claim.GrantID) {
		return resetRefusal(errors.New("invalid reset grant ID"))
	}
	orgID := claim.OrganizationID
	if orgID == "" {
		orgID, _ = auth.Metadata["organization_uuid"].(string)
	}
	if !resetOrganizationPattern.MatchString(orgID) {
		return resetRefusal(errors.New("missing or invalid organization UUID"))
	}
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	requestID, errID := resetRequestID(claim.IdempotencyKey)
	if errID != nil {
		return resetRefusal(errID)
	}
	body, _ := json.Marshal(map[string]string{"program": "cedar_ember", "grant_id": claim.GrantID, "request_id": requestID})
	return sendReset(ctx, auth, strings.TrimRight(baseURL, "/")+"/api/organizations/"+orgID+"/reset_rate_limits", body, claudeUsageHeaders(), request, true)
}

// resetRequestID uses the caller's idempotency key when set (automatic resets),
// otherwise a random ID (manual resets are explicit, one-off actions).
func resetRequestID(key string) (string, error) {
	if key = strings.TrimSpace(key); key != "" {
		if _, errParse := uuid.Parse(key); errParse != nil {
			return "", errors.New("invalid reset idempotency key")
		}
		return key, nil
	}
	requestID, errID := uuid.NewRandom()
	if errID != nil {
		return "", errors.New("could not create reset request ID")
	}
	return requestID.String(), nil
}

func resetRefusal(err error) (cliproxyauth.ResetResult, error) {
	return cliproxyauth.ResetResult{Result: "unavailable", NotSent: true}, err
}

func sendReset(ctx context.Context, auth *cliproxyauth.Auth, endpoint string, body []byte, headers http.Header, request UsageHTTPRequest, claude bool) (cliproxyauth.ResetResult, error) {
	if err := ctx.Err(); err != nil {
		return resetRefusal(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return resetRefusal(errors.New("invalid reset endpoint"))
	}
	req.Header = headers
	// A transport error is ambiguous: the POST may already have been accepted.
	unknown := cliproxyauth.ResetResult{Result: "unknown"}
	response, errRequest := request(ctx, auth, req)
	if errRequest != nil {
		return unknown, errors.New("reset request failed; outcome unknown")
	}
	defer func() {
		if errClose := response.Body.Close(); errClose != nil {
			log.Debug("failed to close reset response body")
		}
	}()
	responseBody, errRead := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	return resetResponseResult(response.StatusCode, responseBody, errRead, claude)
}

// codexResetOutcomes maps the Codex consume code (openai/codex
// rate_limit_resets.rs) to a reset outcome.
var codexResetOutcomes = map[string]string{
	"reset": "reset", "already_redeemed": "already_used", "nothing_to_reset": "not_limited", "no_credit": "unavailable",
}

// resetResponseResult maps a consume response to an outcome. Unrecognised
// bodies are "unknown", which keeps the spend lock until a later refresh.
func resetResponseResult(status int, body []byte, errRead error, claude bool) (cliproxyauth.ResetResult, error) {
	unknown := cliproxyauth.ResetResult{Result: "unknown"}
	if status == http.StatusTooManyRequests {
		return cliproxyauth.ResetResult{Result: "rate_limited"}, nil
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return cliproxyauth.ResetResult{Result: "auth_error"}, nil
	}
	if status < 200 || status >= 300 {
		return unknown, errors.New("reset response unsuccessful; outcome unknown")
	}
	if errRead != nil {
		return unknown, errors.New("reset response unreadable; outcome unknown")
	}
	if !claude {
		var payload struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(body, &payload) == nil {
			if outcome, ok := codexResetOutcomes[payload.Code]; ok {
				return cliproxyauth.ResetResult{Result: outcome}, nil
			}
		}
		return unknown, errors.New("unrecognized reset response; outcome unknown")
	}
	var payload struct {
		Result string `json:"result"`
	}
	if json.Unmarshal(body, &payload) == nil {
		switch payload.Result {
		case "reset", "already_used", "not_limited", "cooldown", "ineligible", "unavailable":
			return cliproxyauth.ResetResult{Result: payload.Result}, nil
		}
	}
	return unknown, errors.New("unrecognized reset response; outcome unknown")
}
