package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestOAuthModelListRequests(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				calls++
				if req.Method != http.MethodGet || req.Header.Get("Authorization") != "Bearer fake-token" {
					t.Errorf("wrong method or OAuth token: %s %v", req.Method, req.Header)
				}
				w.Header().Set("Content-Type", "application/json")
				if provider == "claude" {
					if req.URL.Path != "/v1/models" || req.URL.Query().Get("limit") != "1000" || req.Header.Get("Anthropic-Version") != "2023-06-01" || req.Header.Get("Anthropic-Beta") != "oauth-2025-04-20" {
						t.Errorf("wrong Claude model request: %s %v", req.URL, req.Header)
					}
					if calls == 1 {
						if req.URL.Query().Get("after_id") != "" {
							t.Errorf("unexpected first page cursor: %s", req.URL)
						}
						_, _ = fmt.Fprint(w, `{"data":[{"id":"claude-sonnet-5-5"}],"has_more":true}`)
					} else {
						if req.URL.Query().Get("after_id") != "claude-sonnet-5-5" {
							t.Errorf("wrong next-page cursor: %s", req.URL)
						}
						_, _ = fmt.Fprint(w, `{"data":[{"id":"claude-mythos-5-1"}],"has_more":false}`)
					}
				} else {
					if req.URL.Path != "/backend-api/codex/models" || req.URL.Query().Get("client_version") != "99.0.0" || req.Header.Get("Chatgpt-Account-Id") != "account-1" || req.Header.Get("Originator") != "codex_cli_rs" || !strings.Contains(req.Header.Get("User-Agent"), "99.0.0") {
						t.Errorf("wrong Codex model request: %s %v", req.URL, req.Header)
					}
					_, _ = fmt.Fprint(w, `{"models":[{"slug":"gpt-5.5","visibility":"list"},{"slug":"gpt-daybreak-blue-latest","visibility":"hide"}]}`)
				}
			}))
			defer server.Close()
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", usageRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				if provider == "claude" && req.URL.Host != "api.anthropic.com" || provider == "codex" && req.URL.Host != "chatgpt.com" {
					t.Errorf("unexpected upstream host: %s", req.URL)
				}
				redirected := req.Clone(req.Context())
				endpoint, _ := url.Parse(server.URL)
				redirected.URL.Scheme, redirected.URL.Host = endpoint.Scheme, endpoint.Host
				redirected.Host = endpoint.Host
				return http.DefaultTransport.RoundTrip(redirected)
			}))
			auth := &cliproxyauth.Auth{Provider: provider, Metadata: map[string]any{"access_token": "fake-token", "account_id": "account-1"}}
			var lister cliproxyauth.UpstreamModelLister = NewClaudeExecutor(nil)
			want := []string{"claude-sonnet-5-5", "claude-mythos-5-1"}
			wantCalls := 2
			if provider == "codex" {
				lister = NewCodexAutoExecutor(nil)
				want = []string{"gpt-5.5", "gpt-daybreak-blue-latest"}
				wantCalls = 1
			}
			models, err := lister.ListUpstreamModels(ctx, auth)
			if err != nil || !reflect.DeepEqual(models, want) || calls != wantCalls {
				t.Fatalf("models = %v, err = %v, calls = %d; want %v, %d", models, err, calls, want, wantCalls)
			}
		})
	}
}

func TestCodexModelListRequiresAccountID(t *testing.T) {
	if ids, err := NewCodexExecutor(nil).ListUpstreamModels(context.Background(), &cliproxyauth.Auth{Metadata: map[string]any{"access_token": "fake-token"}}); err == nil || len(ids) != 0 {
		t.Fatalf("missing account ID accepted: models=%v err=%v", ids, err)
	}
}

func TestOAuthModelListRejectsInvalidOrFailedResponse(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		for _, response := range []struct {
			status int
			body   string
		}{
			{http.StatusOK, `{}`},
			{http.StatusOK, `{"models":null,"data":null}`},
			{http.StatusServiceUnavailable, `service unavailable`},
		} {
			t.Run(fmt.Sprintf("%s/%d/%s", provider, response.status, response.body), func(t *testing.T) {
				ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", usageRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: response.status, Body: io.NopCloser(strings.NewReader(response.body)), Request: req}, nil
				}))
				var lister cliproxyauth.UpstreamModelLister = NewClaudeExecutor(nil)
				if provider == "codex" {
					lister = NewCodexExecutor(nil)
				}
				if ids, err := lister.ListUpstreamModels(ctx, &cliproxyauth.Auth{Metadata: map[string]any{"access_token": "fake-token", "account_id": "account-1"}}); err == nil || len(ids) != 0 {
					t.Fatalf("invalid upstream list accepted: ids=%v err=%v", ids, err)
				}
			})
		}
	}
}
