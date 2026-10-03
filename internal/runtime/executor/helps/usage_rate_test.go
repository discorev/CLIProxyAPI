package helps

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestUsageRetryAfterParsing(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		value string
		want  time.Duration
	}{
		{"", 0}, {"0", 0}, {"-1", 0}, {"bad", 0}, {"180", 3 * time.Minute},
		{now.Add(45 * time.Minute).Format(http.TimeFormat), 45 * time.Minute},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0}, {"999999999999", time.Hour},
	} {
		if got := usageRetryAfter(tt.value, now); got != tt.want {
			t.Fatalf("%q: got=%v want=%v", tt.value, got, tt.want)
		}
	}
}

func TestUsageAncillary429StopsFurtherCalls(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			calls := 0
			request := func(_ context.Context, _ *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
				calls++
				if calls > 2 {
					t.Fatal("continued fetching after ancillary 429")
				}
				status, body := 200, `{}`
				if !strings.HasSuffix(req.URL.Path, "/usage") {
					status = 429
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Retry-After": []string{"1800"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			}
			auth := &cliproxyauth.Auth{Provider: provider, Metadata: map[string]interface{}{"access_token": "fake-token"}}
			var result cliproxyauth.UsageFetchResult
			var err error
			if provider == "claude" {
				result, err = FetchClaudeUsage(context.Background(), auth, request)
			} else {
				result, err = FetchCodexUsage(context.Background(), auth, request)
			}
			if err != nil || calls != 2 || result.RateLimit == nil || result.RateLimit.RetryAfter != 30*time.Minute || result.LastError == "" {
				t.Fatalf("result=%+v err=%v calls=%d", result, err, calls)
			}
		})
	}
}

func TestUsageMain429CarriesSafeRetryMetadata(t *testing.T) {
	request := func(_ context.Context, _ *cliproxyauth.Auth, _ *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"0"}}, Body: io.NopCloser(strings.NewReader(`secret body`))}, nil
	}
	_, err := FetchClaudeUsage(context.Background(), &cliproxyauth.Auth{}, request)
	var limit *cliproxyauth.UsageHTTPError
	if !errors.As(err, &limit) || limit.StatusCode != 429 || limit.RetryAfter != 0 || strings.Contains(err.Error(), "secret") {
		t.Fatalf("error=%v limit=%+v", err, limit)
	}
}
