package helps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	metaauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/meta"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// FetchMetaUsage only observes the key endpoint: its key and account fields must
// never enter the usage cache or mutate the credential.
func FetchMetaUsage(ctx context.Context, auth *cliproxyauth.Auth, request UsageHTTPRequest) (cliproxyauth.UsageFetchResult, error) {
	result := cliproxyauth.UsageFetchResult{Raw: make(map[string]json.RawMessage)}
	if !cliproxyauth.UsageFetchable(auth) || !strings.EqualFold(strings.TrimSpace(auth.Provider), "meta") {
		return result, cliproxyauth.ErrUsageNotFetchable
	}
	now := cliproxyauth.UsageFetchOptionsFromContext(ctx).Now
	for _, key := range []string{"dca_expired", "dca_expires_at"} {
		if expiry := metaDCAExpiry(auth.Metadata[key]); !expiry.IsZero() && !expiry.After(now) {
			return result, errors.New("meta usage: DCA token expired")
		}
	}
	token, _ := auth.Metadata["dca_token"].(string)
	headers := http.Header{
		"Authorization": {"Bearer " + strings.TrimSpace(token)},
		"X-Api-Version": {"1.0.0"},
		"Content-Type":  {"application/json"},
		"Accept":        {"application/json"},
	}
	endpoint := strings.TrimSuffix(metaauth.DefaultAPIBaseURL, "/v1") + "/muse-code/key"
	body, err := fetchUsageJSON(ctx, auth, request, http.MethodPost, endpoint, headers, strings.NewReader(`{"onboard":false}`))
	if err != nil {
		return result, fmt.Errorf("meta usage: %w", err)
	}
	raw, windows, errParse := parseMetaUsage(body)
	if errParse != nil {
		return result, errParse
	}
	result.Raw["usage"], result.Windows = raw, windows
	return result, nil
}

func metaDCAExpiry(value any) time.Time {
	if text, ok := value.(string); ok {
		if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(text)); err == nil {
			return parsed
		}
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return time.Time{}
	}
	return usageUnixTime(raw)
}

func parseMetaUsage(body []byte) (json.RawMessage, []cliproxyauth.UsageWindow, error) {
	invalid := errors.New("parse meta usage: invalid response")
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil || payload == nil {
		return nil, nil, invalid
	}
	sanitized := usageFields(payload, "subs_tier_name", "subs_tier_id", "is_subs_active")
	windows := make([]cliproxyauth.UsageWindow, 0, 2)
	if raw, exists := payload["subs_usage"]; exists {
		var usage map[string]json.RawMessage
		if err := json.Unmarshal(raw, &usage); err != nil {
			return nil, nil, invalid
		}
		sanitized["subs_usage"] = json.RawMessage(`null`)
		if usage != nil {
			cleanUsage := usageFields(usage, "tier")
			for _, name := range []string{"window", "weekly"} {
				rawWindow, exists := usage[name]
				if !exists {
					continue
				}
				var window map[string]json.RawMessage
				if err := json.Unmarshal(rawWindow, &window); err != nil {
					return nil, nil, invalid
				}
				cleanUsage[name] = json.RawMessage(`null`)
				if window == nil {
					continue
				}
				cleanUsage[name], _ = json.Marshal(usageFields(window, "used_percent", "resets_at", "window_duration_mins"))
				percent, ok := usageNumber(window["used_percent"])
				if !ok {
					continue
				}
				kind, length := "7d", int64(604800)
				if name == "window" {
					length = 0
					if minutes, valid := usageNumber(window["window_duration_mins"]); valid && minutes > 0 && minutes < float64(math.MaxInt64)/60 {
						length = int64(minutes * 60)
					}
					kind = cliproxyauth.UsageWindowKind(length, "5h")
				}
				windows = append(windows, cliproxyauth.UsageWindow{
					Kind: kind, Length: length, UsedPercent: min(100, max(0, percent)),
					ResetsAt: usageUnixTime(window["resets_at"]),
				})
			}
			sanitized["subs_usage"], _ = json.Marshal(cleanUsage)
		}
	}
	raw, _ := json.Marshal(sanitized)
	return raw, windows, nil
}

func usageFields(source map[string]json.RawMessage, keys ...string) map[string]json.RawMessage {
	fields := make(map[string]json.RawMessage, len(keys))
	for _, key := range keys {
		if value, exists := source[key]; exists {
			fields[key] = value
		}
	}
	return fields
}

// usageNumber accepts the numeric strings returned by some quota endpoints,
// but never treats null, missing, non-finite or malformed values as zero.
func usageNumber(raw json.RawMessage) (float64, bool) {
	text := strings.TrimSpace(string(raw))
	if strings.HasPrefix(text, `"`) {
		if err := json.Unmarshal(raw, &text); err != nil {
			return 0, false
		}
	}
	number, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	return number, err == nil && !math.IsNaN(number) && !math.IsInf(number, 0)
}

func usageUnixTime(raw json.RawMessage) time.Time {
	if seconds, ok := usageNumber(raw); ok && seconds > 0 && seconds < float64(math.MaxInt64) {
		return time.Unix(int64(seconds), 0).UTC()
	}
	return time.Time{}
}
