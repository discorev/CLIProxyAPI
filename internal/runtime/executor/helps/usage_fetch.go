package helps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const usageResponseMaxBytes = 4 * 1024 * 1024

// UsageHTTPRequest uses the executor's proxy-aware, fingerprinted HTTP path.
type UsageHTTPRequest func(context.Context, *cliproxyauth.Auth, *http.Request) (*http.Response, error)

func FetchClaudeUsage(ctx context.Context, auth *cliproxyauth.Auth, request UsageHTTPRequest) (cliproxyauth.UsageFetchResult, error) {
	headers := claudeUsageHeaders()
	result := cliproxyauth.UsageFetchResult{Raw: make(map[string]json.RawMessage)}
	body, err := fetchUsageJSON(ctx, auth, request, "https://api.anthropic.com/api/oauth/usage?cedar_ember=1&skip_spend=1", headers)
	if err != nil {
		return result, fmt.Errorf("claude usage: %w", err)
	}
	result.Raw["usage"] = body
	result.Resets = ParseClaudeResetGrants(body)
	result.Windows, err = ParseClaudeSubscriptionUsage(body)
	if err != nil {
		return result, err
	}
	if !cliproxyauth.UsageFetchOptionsFromContext(ctx).SkipProfile {
		profile, errProfile := fetchUsageJSON(ctx, auth, request, "https://api.anthropic.com/api/oauth/profile", headers)
		if errProfile != nil {
			setUsageAncillaryError(&result, fmt.Errorf("claude profile: %w", errProfile))
		} else {
			result.Raw["profile"] = profile
		}
	}
	return result, nil
}

func FetchCodexUsage(ctx context.Context, auth *cliproxyauth.Auth, request UsageHTTPRequest) (cliproxyauth.UsageFetchResult, error) {
	accountID, _ := auth.Metadata["account_id"].(string)
	accountID = strings.TrimSpace(accountID)
	headers := codexUsageHeaders(accountID)
	result := cliproxyauth.UsageFetchResult{Raw: make(map[string]json.RawMessage)}
	body, err := fetchUsageJSON(ctx, auth, request, "https://chatgpt.com/backend-api/wham/usage", headers)
	if err != nil {
		return result, fmt.Errorf("codex usage: %w", err)
	}
	result.Raw["usage"] = body
	options := cliproxyauth.UsageFetchOptionsFromContext(ctx)
	result.Windows, err = ParseCodexSubscriptionUsage(body, options.Now)
	if err != nil {
		return result, err
	}
	if !options.SkipSubscription {
		subscription, errSubscription := fetchUsageJSON(ctx, auth, request, "https://chatgpt.com/backend-api/subscriptions?"+url.Values{"account_id": {accountID}}.Encode(), headers)
		if errSubscription != nil {
			setUsageAncillaryError(&result, fmt.Errorf("codex subscription: %w", errSubscription))
			if result.RateLimit != nil {
				return result, nil
			}
		} else {
			result.Raw["subscription"] = subscription
		}
	}
	headers.Set("Accept", "application/json")
	headers.Set("OpenAI-Beta", "codex-1")
	headers.Set("Originator", "Codex Desktop")
	credits, errCredits := fetchUsageJSON(ctx, auth, request, "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits", headers)
	if errCredits != nil {
		setUsageAncillaryError(&result, fmt.Errorf("codex reset credits: %w", errCredits))
	} else {
		result.Raw["reset_credits"] = credits
		result.Resets = ParseCodexResetCredits(credits)
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
		return nil, &cliproxyauth.UsageHTTPError{StatusCode: response.StatusCode, RetryAfter: usageRetryAfter(response.Header.Get("Retry-After"), cliproxyauth.UsageFetchOptionsFromContext(ctx).Now)}
	}
	body, errRead := io.ReadAll(io.LimitReader(response.Body, usageResponseMaxBytes+1))
	if len(body) > usageResponseMaxBytes {
		return nil, errors.New("usage response exceeds 4 MiB")
	}
	if errRead != nil {
		return nil, errors.New("failed to read response")
	}
	if !json.Valid(body) {
		return nil, errors.New("invalid JSON response")
	}
	return body, nil
}

func setUsageAncillaryError(result *cliproxyauth.UsageFetchResult, err error) {
	if result.LastError != "" {
		result.LastError += "; "
	}
	result.LastError += err.Error()
	var httpError *cliproxyauth.UsageHTTPError
	if errors.As(err, &httpError) && httpError.StatusCode == http.StatusTooManyRequests {
		result.RateLimit = httpError
	}
}

func usageRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds > 0 {
			return time.Duration(min(seconds, int64(cliproxyauth.Usage429Maximum/time.Second))) * time.Second
		}
		return 0
	}
	if until, err := http.ParseTime(value); err == nil && until.After(now) {
		return min(until.Sub(now), cliproxyauth.Usage429Maximum)
	}
	return 0
}

func claudeUsageHeaders() http.Header {
	return http.Header{
		"User-Agent":     {"claude-cli/2.1.280 (external, cli)"},
		"Content-Type":   {"application/json"},
		"Anthropic-Beta": {"oauth-2025-04-20"},
	}
}

func codexUsageHeaders(accountID string) http.Header {
	return http.Header{
		"User-Agent":         {"codex-tui/0.149.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.149.1)"},
		"Content-Type":       {"application/json"},
		"Chatgpt-Account-Id": {accountID},
	}
}
