package helps

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestParseMetaSubscriptionUsage(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		count      int
		percent    float64
	}{
		{"absent", `{"subs_tier_name":"Power"}`, 0, 0},
		{"null", `{"subs_usage":null}`, 0, 0},
		{"missing percent", `{"subs_usage":{"window":{"resets_at":1800000000}}}`, 0, 0},
		{"null percent", `{"subs_usage":{"weekly":{"used_percent":null}}}`, 0, 0},
		{"invalid percent", `{"subs_usage":{"weekly":{"used_percent":"NaN"}}}`, 0, 0},
		{"zero", `{"subs_usage":{"weekly":{"used_percent":0}}}`, 1, 0},
		{"clamp high", `{"subs_usage":{"weekly":{"used_percent":"123"}}}`, 1, 100},
		{"clamp low", `{"subs_usage":{"weekly":{"used_percent":-3}}}`, 1, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw, windows, err := parseMetaUsage([]byte(tt.body))
			if err != nil || windows == nil || len(windows) != tt.count || !json.Valid(raw) {
				t.Fatalf("parse: windows=%+v err=%v", windows, err)
			}
			if len(windows) > 0 && windows[0].UsedPercent != tt.percent {
				t.Fatalf("percent=%v", windows[0].UsedPercent)
			}
		})
	}
}

func TestParseXAISubscriptionUsage(t *testing.T) {
	period := `"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-10-08T00:00:00Z","end":"2026-10-15T00:00:00Z"}`
	for _, tt := range []struct {
		name, config string
		count        int
		percent      float64
	}{
		{"omitted zero", period, 1, 0},
		{"numeric", period + `,"creditUsagePercent":36.25`, 1, 36.25},
		{"numeric string", period + `,"creditUsagePercent":"36.25"`, 1, 36.25},
		{"clamp high", period + `,"creditUsagePercent":150`, 1, 100},
		{"clamp low", period + `,"creditUsagePercent":-10`, 1, 0},
		{"weekly by duration", `"currentPeriod":{"start":"2026-10-08T00:00:00Z","end":"2026-10-15T00:00:00Z"}`, 1, 0},
		{"monthly not quota", `"currentPeriod":{"type":"USAGE_PERIOD_TYPE_MONTHLY","start":"2026-10-01T00:00:00Z","end":"2026-11-01T00:00:00Z"},"creditUsagePercent":100`, 0, 0},
		{"missing period", `"creditUsagePercent":100`, 0, 0},
		{"empty", ``, 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			windows, err := ParseXAISubscriptionUsage([]byte(`{"config":{` + tt.config + `}}`))
			if err != nil || windows == nil || len(windows) != tt.count {
				t.Fatalf("windows=%+v err=%v", windows, err)
			}
			if len(windows) > 0 {
				w := windows[0]
				if w.Kind != "7d" || w.Scope != "" || w.Length != 604800 || w.UsedPercent != tt.percent || w.ResetsAt.Format(time.RFC3339) != "2026-10-15T00:00:00Z" {
					t.Fatalf("window=%+v", w)
				}
			}
		})
	}
	for _, body := range []string{
		`{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY"},"creditUsagePercent":"private-invalid-number"}}`,
		`{"config":{"currentPeriod":{"end":"private-invalid-date"}}}`,
	} {
		if _, err := ParseXAISubscriptionUsage([]byte(body)); err == nil || strings.Contains(err.Error(), "private") {
			t.Fatal("invalid xAI payload must fail without exposing values")
		}
	}
}

func TestXAISubscriptionEnrichmentFailuresAreAncillary(t *testing.T) {
	for _, failure := range []struct {
		path   string
		status int
	}{
		{"/v1/billing", 429},
		{"/v1/user", 503},
		{"/v1/user", 429},
		{"/v1/settings", 503},
	} {
		calls := 0
		request := func(_ context.Context, _ *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
			calls++
			status, body := 200, `{}`
			if req.URL.RawQuery == "format=credits" {
				body = `{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY"}}}`
			} else if req.URL.Path == failure.path {
				status, body = failure.status, "private-error"
			}
			return &http.Response{StatusCode: status, Header: http.Header{"Retry-After": {"1800"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
		}
		result, err := FetchXAIUsage(context.Background(), &cliproxyauth.Auth{Provider: "xai", Metadata: map[string]any{"access_token": "fake"}}, request)
		if err != nil || len(result.Windows) != 1 || len(result.Raw["usage"]) == 0 || result.LastError == "" || strings.Contains(result.LastError, "private") {
			t.Fatalf("failed to retain usage: err=%v", err)
		}
		if failure.status == 429 {
			if result.RateLimit == nil || result.RateLimit.RetryAfter != 30*time.Minute || calls > 3 {
				t.Fatal("ancillary 429 must stop calls and carry cooldown")
			}
		} else if calls != 4 || result.RateLimit != nil {
			t.Fatal("non-rate-limit failure should allow remaining enrichment")
		}
		if len(result.Raw["subscription"]) != 0 {
			t.Fatal("failed enrichment must not receive a fresh 24h TTL")
		}
	}
}

func TestNewUsageHTTPFailuresAreSanitized(t *testing.T) {
	for _, provider := range []string{"meta", "xai"} {
		for _, transportFailure := range []bool{false, true} {
			request := func(context.Context, *cliproxyauth.Auth, *http.Request) (*http.Response, error) {
				if transportFailure {
					return nil, errors.New("private proxy URL")
				}
				return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": {"1200"}}, Body: io.NopCloser(strings.NewReader("private response"))}, nil
			}
			auth := &cliproxyauth.Auth{Provider: provider, Metadata: map[string]any{"auth_kind": "oauth", "access_token": "fake", "dca_token": "fake"}}
			fetch := FetchMetaUsage
			if provider == "xai" {
				fetch = FetchXAIUsage
			}
			_, err := fetch(context.Background(), auth, request)
			if err == nil || strings.Contains(err.Error(), "private") {
				t.Fatal("usage failure leaked upstream details")
			}
			if !transportFailure {
				var limit *cliproxyauth.UsageHTTPError
				if !errors.As(err, &limit) || limit.StatusCode != 429 || limit.RetryAfter != 20*time.Minute {
					t.Fatal("usage failure lost retry metadata")
				}
			}
		}
	}
}
