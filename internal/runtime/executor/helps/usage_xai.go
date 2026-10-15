package helps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	xaiauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/xai"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func FetchXAIUsage(ctx context.Context, auth *cliproxyauth.Auth, request UsageHTTPRequest) (cliproxyauth.UsageFetchResult, error) {
	result := cliproxyauth.UsageFetchResult{Raw: make(map[string]json.RawMessage)}
	if !cliproxyauth.UsageFetchable(auth) || !strings.EqualFold(strings.TrimSpace(auth.Provider), "xai") {
		return result, cliproxyauth.ErrUsageNotFetchable
	}
	token, _ := auth.Metadata["access_token"].(string)
	version := GetXAIClientVersion()
	headers := http.Header{
		"Authorization":         {"Bearer " + strings.TrimSpace(token)},
		"X-Xai-Token-Auth":      {"xai-grok-cli"},
		"X-Grok-Client-Version": {version},
		"User-Agent":            {"xai-grok-workspace/" + version},
		"Accept":                {"application/json"},
	}
	body, err := fetchUsageJSON(ctx, auth, request, xaiauth.CLIChatProxyBaseURL+"/billing?format=credits", headers)
	if err != nil {
		return result, fmt.Errorf("xai usage: %w", err)
	}
	result.Windows, err = ParseXAISubscriptionUsage(body)
	if err != nil {
		return result, err
	}
	result.Raw["usage"] = body
	billing, errBilling := fetchUsageJSON(ctx, auth, request, xaiauth.CLIChatProxyBaseURL+"/billing", headers)
	if errBilling != nil {
		setUsageAncillaryError(&result, fmt.Errorf("xai billing: %w", errBilling))
		if result.RateLimit != nil {
			return result, nil
		}
	} else {
		result.Raw["billing"] = billing
	}
	if !cliproxyauth.UsageFetchOptionsFromContext(ctx).SkipSubscription {
		subscription := make(map[string]string)
		complete := true
		for _, path := range []string{"/user?include=subscription", "/settings"} {
			profile, errProfile := fetchUsageJSON(ctx, auth, request, xaiauth.CLIChatProxyBaseURL+path, headers)
			if errProfile == nil {
				errProfile = mergeXAISubscription(subscription, profile)
			}
			if errProfile != nil {
				complete = false
				setUsageAncillaryError(&result, fmt.Errorf("xai subscription: %w", errProfile))
				if result.RateLimit != nil {
					break
				}
			}
		}
		// Cache successful enrichment for 24h. A partial/error result is retried
		// after the shared failure backoff rather than hidden for a whole day.
		if complete {
			result.Raw["subscription"], _ = json.Marshal(subscription)
		}
	}
	return result, nil
}

func ParseXAISubscriptionUsage(body []byte) ([]cliproxyauth.UsageWindow, error) {
	var payload struct {
		Config struct {
			CreditUsagePercent json.RawMessage `json:"creditUsagePercent"`
			CurrentPeriod      struct {
				Type  string    `json:"type"`
				Start time.Time `json:"start"`
				End   time.Time `json:"end"`
			} `json:"currentPeriod"`
		} `json:"config"`
	}
	invalid := errors.New("parse xai usage: invalid response")
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, invalid
	}
	windows := make([]cliproxyauth.UsageWindow, 0, 1)
	period := payload.Config.CurrentPeriod
	length := int64(0)
	if !period.Start.IsZero() && !period.End.IsZero() && period.End.After(period.Start) {
		length = int64(period.End.Sub(period.Start) / time.Second)
	}
	// A one-hour tolerance accepts weekly windows across daylight-saving shifts.
	if period.Type != "USAGE_PERIOD_TYPE_WEEKLY" && (length < 604800-3600 || length > 604800+3600) {
		return windows, nil
	}
	percent := float64(0)
	if raw := payload.Config.CreditUsagePercent; len(raw) != 0 {
		var ok bool
		percent, ok = usageNumber(raw)
		if !ok {
			return nil, invalid
		}
	}
	if length == 0 {
		length = 604800
	}
	return append(windows, cliproxyauth.UsageWindow{
		Kind: "7d", Length: length, UsedPercent: min(100, max(0, percent)), ResetsAt: period.End,
	}), nil
}

// Only plan labels may leave user/settings responses; never identity fields.
// Later (settings) values override user values when both supply a label.
func mergeXAISubscription(subscription map[string]string, body []byte) error {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil || payload == nil {
		return errors.New("invalid response")
	}
	var nested map[string]json.RawMessage
	if raw, exists := payload["subscription"]; exists {
		_ = json.Unmarshal(raw, &nested)
	}
	for _, object := range []map[string]json.RawMessage{payload, nested} {
		for _, keys := range [][2]string{{"subscriptionTier", "subscription_tier"}, {"subscriptionTierDisplay", "subscription_tier_display"}} {
			for _, key := range keys {
				var value string
				if json.Unmarshal(object[key], &value) == nil && strings.TrimSpace(value) != "" {
					subscription[keys[0]] = value
					break
				}
			}
		}
	}
	return nil
}
