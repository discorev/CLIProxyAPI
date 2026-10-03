package helps

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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

func TestUsageResponseBodyLimit(t *testing.T) {
	for _, endpoint := range []struct{ provider, path, rawKey, label string }{
		{"claude", "/api/oauth/usage", "usage", "claude usage"},
		{"claude", "/api/oauth/profile", "profile", "claude profile"},
		{"codex", "/backend-api/wham/usage", "usage", "codex usage"},
		{"codex", "/backend-api/subscriptions", "subscription", "codex subscription"},
		{"codex", "/backend-api/wham/rate-limit-reset-credits", "reset_credits", "codex reset credits"},
	} {
		for _, oversized := range []bool{false, true} {
			name := endpoint.label + "/at-limit"
			if oversized {
				name = endpoint.label + "/over-limit"
			}
			t.Run(name, func(t *testing.T) {
				size := usageResponseMaxBytes
				if oversized {
					size++
				}
				payload := "{}" + strings.Repeat(" ", size-2)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == endpoint.path {
						// Exercise an unknown-length (chunked) response too.
						w.(http.Flusher).Flush()
						_, _ = io.WriteString(w, payload)
						return
					}
					_, _ = io.WriteString(w, `{}`)
				}))
				defer server.Close()
				request := func(ctx context.Context, _ *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
					if _, deadline := ctx.Deadline(); deadline {
						t.Fatal("fetch added a network deadline")
					}
					req.URL.Scheme, req.URL.Host = "http", server.Listener.Addr().String()
					return server.Client().Do(req)
				}
				auth := &cliproxyauth.Auth{Provider: endpoint.provider, Metadata: map[string]interface{}{"access_token": "fake-token"}}
				fetch := FetchClaudeUsage
				if endpoint.provider == "codex" {
					fetch = FetchCodexUsage
				}
				result, err := fetch(context.Background(), auth, request)
				if !oversized {
					if err != nil || result.LastError != "" || len(result.Raw[endpoint.rawKey]) != size {
						t.Fatalf("exactly 4 MiB rejected: err=%v ancillary=%q length=%d", err, result.LastError, len(result.Raw[endpoint.rawKey]))
					}
					return
				}
				want := endpoint.label + ": usage response exceeds 4 MiB"
				if endpoint.rawKey == "usage" {
					if err == nil || err.Error() != want {
						t.Fatalf("oversized usage error=%v want=%q", err, want)
					}
				} else if err != nil || result.LastError != want {
					t.Fatalf("oversized ancillary error=%v last_error=%q want=%q", err, result.LastError, want)
				}
				if _, stored := result.Raw[endpoint.rawKey]; stored {
					t.Fatal("oversized body was stored")
				}
			})
		}
	}
}
