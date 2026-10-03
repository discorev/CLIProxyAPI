package helps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// UsageHTTPRequest uses the executor's proxy-aware, fingerprinted HTTP path.
type UsageHTTPRequest func(context.Context, *cliproxyauth.Auth, *http.Request) (*http.Response, error)

func FetchClaudeUsage(ctx context.Context, auth *cliproxyauth.Auth, request UsageHTTPRequest) (cliproxyauth.UsageFetchResult, error) {
	headers := http.Header{
		"User-Agent":     {"claude-cli/2.1.280 (external, cli)"},
		"Content-Type":   {"application/json"},
		"Anthropic-Beta": {"oauth-2025-04-20"},
	}
	result := cliproxyauth.UsageFetchResult{Raw: make(map[string]json.RawMessage)}
	body, err := fetchUsageJSON(ctx, auth, request, "https://api.anthropic.com/api/oauth/usage?cedar_ember=1&skip_spend=1", headers)
	if err != nil {
		return result, fmt.Errorf("claude usage: %w", err)
	}
	result.Raw["usage"] = body
	result.Windows, err = ParseClaudeSubscriptionUsage(body)
	if err != nil {
		return result, err
	}
	profile, errProfile := fetchUsageJSON(ctx, auth, request, "https://api.anthropic.com/api/oauth/profile", headers)
	if errProfile != nil {
		result.LastError = fmt.Sprintf("claude profile: %v", errProfile)
	} else {
		result.Raw["profile"] = profile
	}
	return result, nil
}

func FetchCodexUsage(ctx context.Context, auth *cliproxyauth.Auth, request UsageHTTPRequest) (cliproxyauth.UsageFetchResult, error) {
	accountID, _ := auth.Metadata["account_id"].(string)
	accountID = strings.TrimSpace(accountID)
	headers := http.Header{
		"User-Agent":         {"codex-tui/0.149.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.149.1)"},
		"Content-Type":       {"application/json"},
		"Chatgpt-Account-Id": {accountID},
	}
	result := cliproxyauth.UsageFetchResult{Raw: make(map[string]json.RawMessage)}
	body, err := fetchUsageJSON(ctx, auth, request, "https://chatgpt.com/backend-api/wham/usage", headers)
	if err != nil {
		return result, fmt.Errorf("codex usage: %w", err)
	}
	result.Raw["usage"] = body
	result.Windows, err = ParseCodexSubscriptionUsage(body, time.Now())
	if err != nil {
		return result, err
	}
	var ancillaryErrors []error
	subscription, errSubscription := fetchUsageJSON(ctx, auth, request, "https://chatgpt.com/backend-api/subscriptions?"+url.Values{"account_id": {accountID}}.Encode(), headers)
	if errSubscription != nil {
		ancillaryErrors = append(ancillaryErrors, fmt.Errorf("codex subscription: %w", errSubscription))
	} else {
		result.Raw["subscription"] = subscription
	}
	headers.Set("Accept", "application/json")
	headers.Set("OpenAI-Beta", "codex-1")
	headers.Set("Originator", "Codex Desktop")
	credits, errCredits := fetchUsageJSON(ctx, auth, request, "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits", headers)
	if errCredits != nil {
		ancillaryErrors = append(ancillaryErrors, fmt.Errorf("codex reset credits: %w", errCredits))
	} else {
		result.Raw["reset_credits"] = credits
	}
	if errAncillary := errors.Join(ancillaryErrors...); errAncillary != nil {
		result.LastError = errAncillary.Error()
	}
	return result, nil
}

func fetchUsageJSON(ctx context.Context, auth *cliproxyauth.Auth, request UsageHTTPRequest, endpoint string, headers http.Header) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header = headers.Clone()
	response, errRequest := request(ctx, auth, req)
	if errRequest != nil {
		// Transport errors may include arbitrary proxy URLs. Do not expose them
		// through the cache or logs, which could leak proxy credentials.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("request failed")
	}
	defer func() {
		if errClose := response.Body.Close(); errClose != nil {
			log.Debug("failed to close usage response body")
		}
	}()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("upstream status %d", response.StatusCode)
	}
	body, errRead := io.ReadAll(response.Body)
	if errRead != nil {
		return nil, errors.New("failed to read response")
	}
	if !json.Valid(body) {
		return nil, errors.New("invalid JSON response")
	}
	return body, nil
}
