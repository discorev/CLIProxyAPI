package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type usageRoundTripFunc func(*http.Request) (*http.Response, error)

func (f usageRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestSubscriptionUsageFetchersUseExecutorHTTPTransport(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			auth := &cliproxyauth.Auth{Provider: provider, Metadata: map[string]interface{}{"access_token": "fake-token", "account_id": "fake account"}}
			seen := make(map[string]bool)
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", usageRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.Method != http.MethodGet || req.Header.Get("Authorization") != "Bearer fake-token" || req.Header.Get("Content-Type") != "application/json" {
					t.Errorf("wrong request: %s %s headers=%v", req.Method, req.URL, req.Header)
				}
				seen[req.URL.Path] = true
				body := `{}`
				if provider == "claude" {
					if req.URL.Host != "api.anthropic.com" || req.Header.Get("User-Agent") != "claude-cli/2.1.280 (external, cli)" || req.Header.Get("anthropic-beta") != "oauth-2025-04-20" {
						t.Errorf("wrong claude endpoint/headers: %s %v", req.URL, req.Header)
					}
					switch req.URL.Path {
					case "/api/oauth/usage":
						if req.URL.Query().Get("cedar_ember") != "1" || req.URL.Query().Get("skip_spend") != "1" {
							t.Errorf("missing claude usage options: %s", req.URL)
						}
						body = `{"five_hour":{"utilization":6}}`
					case "/api/oauth/profile":
					default:
						t.Fatalf("unexpected URL: %s", req.URL)
					}
				} else {
					if req.URL.Host != "chatgpt.com" || req.Header.Get("Chatgpt-Account-Id") != "fake account" || !strings.HasPrefix(req.Header.Get("User-Agent"), "codex-tui/0.149.1 ") {
						t.Errorf("wrong codex endpoint/headers: %s %v", req.URL, req.Header)
					}
					switch req.URL.Path {
					case "/backend-api/wham/usage":
						body = `{"rate_limit":{"primary_window":{"used_percent":6}}}`
					case "/backend-api/subscriptions":
						if req.URL.Query().Get("account_id") != "fake account" {
							t.Errorf("wrong subscription account: %s", req.URL)
						}
					case "/backend-api/wham/rate-limit-reset-credits":
						if req.Header.Get("Accept") != "application/json" || req.Header.Get("OpenAI-Beta") != "codex-1" || req.Header.Get("Originator") != "Codex Desktop" {
							t.Errorf("missing credit headers: %v", req.Header)
						}
					default:
						t.Fatalf("unexpected URL: %s", req.URL)
					}
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
			}))
			var fetcher cliproxyauth.UsageFetcher = NewClaudeExecutor(nil)
			wantCalls := 2
			if provider == "codex" {
				// The service registers the auto executor, not the bare HTTP one.
				fetcher = NewCodexAutoExecutor(nil)
				wantCalls = 3
			}
			result, err := fetcher.FetchUsage(ctx, auth)
			if err != nil || len(result.Raw) != wantCalls || len(seen) != wantCalls || len(result.Windows) != 1 || result.Windows[0].UsedPercent != 6 || result.LastError != "" {
				t.Fatalf("fetch = %+v, %v; requests=%v", result, err, seen)
			}
		})
	}
}

func TestCodexChatGPTBackendRequestsUseOuterToken(t *testing.T) {
	for _, siwcToken := range []string{"api-openai-direct-token", ""} {
		t.Run("siwc="+siwcToken, func(t *testing.T) {
			auth := &cliproxyauth.Auth{
				Provider:   "codex",
				Attributes: map[string]string{"header:X-Test-Custom": "preserved"},
				Metadata: map[string]any{
					"use_siwc":     true,
					"access_token": "outer-chatgpt-token",
					"account_id":   "account-1",
					"siwc":         map[string]any{"access_token": siwcToken},
				},
			}
			executor := NewCodexExecutor(nil)
			if siwcToken != "" {
				req, err := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/responses", nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := executor.PrepareRequest(req, auth); err != nil {
					t.Fatal(err)
				}
				if got := req.Header.Get("Authorization"); got != "Bearer api-openai-direct-token" {
					t.Fatalf("generation authorization = %q", got)
				}
			}
			for _, operation := range []string{"usage", "models", "reset"} {
				t.Run(operation, func(t *testing.T) {
					seen := make(map[string]bool)
					ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", usageRoundTripFunc(func(req *http.Request) (*http.Response, error) {
						if req.URL.Host != "chatgpt.com" || req.Header.Get("Authorization") != "Bearer outer-chatgpt-token" || req.Header.Get("X-Test-Custom") != "preserved" {
							t.Errorf("wrong backend endpoint/headers: %s %v", req.URL, req.Header)
						}
						seen[req.URL.Path] = true
						body := `{}`
						switch req.URL.Path {
						case "/backend-api/wham/usage":
							body = `{"rate_limit":{"primary_window":{"used_percent":6}}}`
						case "/backend-api/subscriptions", "/backend-api/wham/rate-limit-reset-credits":
						case "/backend-api/codex/models":
							body = `{"models":[{"slug":"gpt-5.5"}]}`
						case "/backend-api/wham/rate-limit-reset-credits/consume":
							if req.Method != http.MethodPost {
								t.Errorf("reset method = %s", req.Method)
							}
							body = `{"code":"reset","windows_reset":1}`
						default:
							t.Fatalf("unexpected backend URL: %s", req.URL)
						}
						return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
					}))
					wantCalls := 1
					switch operation {
					case "usage":
						wantCalls = 3
						result, err := executor.FetchUsage(ctx, auth)
						if err != nil || len(result.Windows) != 1 || result.Windows[0].UsedPercent != 6 {
							t.Fatalf("usage = %+v, err = %v", result, err)
						}
					case "models":
						models, err := executor.ListUpstreamModels(ctx, auth)
						if err != nil || len(models) != 1 || models[0] != "gpt-5.5" {
							t.Fatalf("models = %v, err = %v", models, err)
						}
					case "reset":
						result, err := executor.ApplyReset(ctx, auth, cliproxyauth.ResetRequest{CreditID: "credit-1"})
						if err != nil || result.Result != "reset" || result.NotSent {
							t.Fatalf("reset = %+v, err = %v", result, err)
						}
					}
					if len(seen) != wantCalls {
						t.Fatalf("requests = %v, want %d", seen, wantCalls)
					}
				})
			}
		})
	}
}

func TestSubscriptionUsageFetchFailures(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		for _, failUsage := range []bool{false, true} {
			t.Run(provider+map[bool]string{false: "/ancillary", true: "/usage"}[failUsage], func(t *testing.T) {
				calls := 0
				ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", usageRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					calls++
					status, body := http.StatusTooManyRequests, `{}`
					if strings.HasSuffix(req.URL.Path, "/usage") && !failUsage {
						status = http.StatusOK
						if provider == "claude" {
							body = `{"seven_day":{"utilization":47}}`
						} else {
							body = `{"rate_limit":{"primary_window":{"used_percent":47,"limit_window_seconds":604800}}}`
						}
					}
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
				}))
				var fetcher cliproxyauth.UsageFetcher = NewClaudeExecutor(nil)
				if provider == "codex" {
					fetcher = NewCodexExecutor(nil)
				}
				result, err := fetcher.FetchUsage(ctx, &cliproxyauth.Auth{Provider: provider, Metadata: map[string]interface{}{"access_token": "fake-token"}})
				if failUsage {
					if err == nil || calls != 1 || !strings.Contains(err.Error(), "429") {
						t.Fatalf("usage failure = %+v, %v, calls=%d", result, err, calls)
					}
				} else if err != nil || len(result.Raw) != 1 || len(result.Windows) != 1 || result.Windows[0].UsedPercent != 47 || !strings.Contains(result.LastError, "429") {
					t.Fatalf("ancillary failure discarded usage: %+v, %v", result, err)
				}
			})
		}
	}
}
