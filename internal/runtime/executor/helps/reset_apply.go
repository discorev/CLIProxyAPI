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

func ApplyCodexReset(ctx context.Context, auth *cliproxyauth.Auth, baseURL string, request UsageHTTPRequest) (cliproxyauth.ResetResult, error) {
	if !cliproxyauth.UsageFetchable(auth) || !strings.EqualFold(auth.Provider, "codex") {
		return resetRefusal(cliproxyauth.ErrUsageNotFetchable)
	}
	if baseURL == "" {
		baseURL = "https://chatgpt.com"
	}
	requestID, errID := uuid.NewRandom()
	if errID != nil {
		return resetRefusal(errors.New("could not create reset request ID"))
	}
	body, _ := json.Marshal(map[string]string{"redeem_request_id": requestID.String()})
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
	requestID, errID := uuid.NewRandom()
	if errID != nil {
		return resetRefusal(errors.New("could not create reset request ID"))
	}
	body, _ := json.Marshal(map[string]string{"program": "cedar_ember", "grant_id": claim.GrantID, "request_id": requestID.String()})
	return sendReset(ctx, auth, strings.TrimRight(baseURL, "/")+"/api/organizations/"+orgID+"/reset_rate_limits", body, claudeUsageHeaders(), request, true)
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
	if response.StatusCode == http.StatusTooManyRequests {
		return cliproxyauth.ResetResult{Result: "rate_limited"}, nil
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return cliproxyauth.ResetResult{Result: "auth_error"}, nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return unknown, errors.New("reset response unsuccessful; outcome unknown")
	}
	if !claude {
		return cliproxyauth.ResetResult{Result: "reset"}, nil
	}
	body, errRead := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	if errRead != nil {
		return unknown, errors.New("reset response unreadable; outcome unknown")
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
