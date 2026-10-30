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
	for _, provider := range []string{"claude", "codex", "meta"} {
		t.Run(provider, func(t *testing.T) {
			calls := 0
			token := "fake-token"
			if provider == "meta" {
				token = "fake-api-key"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				calls++
				if req.Method != http.MethodGet || req.Header.Get("Authorization") != "Bearer "+token {
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
				} else if provider == "meta" {
					if req.URL.Path != "/muse-code/models" || req.Header.Get("Accept") != "application/json" || req.Header.Get("User-Agent") != metaUserAgent || req.Header.Get("X-Client-Id") != "tbh:tui" {
						t.Errorf("wrong Meta model request: %s %v", req.URL, req.Header)
					}
					_, _ = fmt.Fprint(w, `{"object":"list","data":[{"id":" muse-spark-1.3 "},{"id":"muse-wonder-full-1.0","metadata":{"muse-code":{"is_hidden":true}}},{"id":" "},{}]}`)
				} else {
					if req.URL.Path != "/backend-api/codex/models" || req.URL.Query().Get("client_version") != "99.0.0" || req.Header.Get("Chatgpt-Account-Id") != "account-1" || req.Header.Get("Originator") != "codex_cli_rs" || !strings.Contains(req.Header.Get("User-Agent"), "99.0.0") {
						t.Errorf("wrong Codex model request: %s %v", req.URL, req.Header)
					}
					_, _ = fmt.Fprint(w, `{"models":[{"slug":"gpt-5.5","visibility":"list"},{"slug":"gpt-daybreak-blue-latest","visibility":"hide"}]}`)
				}
			}))
			defer server.Close()
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", usageRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				if provider == "claude" && req.URL.Host != "api.anthropic.com" || provider == "codex" && req.URL.Host != "chatgpt.com" || provider == "meta" && req.URL.Host != "api.meta.ai" {
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
			} else if provider == "meta" {
				auth.Attributes = map[string]string{"auth_kind": "oauth", "api_key": token}
				lister = NewMetaExecutor(nil)
				want = []string{"muse-spark-1.3", "muse-wonder-full-1.0"}
				wantCalls = 1
			}
			models, err := lister.ListUpstreamModels(ctx, auth)
			if err != nil || !reflect.DeepEqual(upstreamModelIDs(models), want) || calls != wantCalls {
				t.Fatalf("models = %v, err = %v, calls = %d; want %v, %d", models, err, calls, want, wantCalls)
			}
			for _, model := range models {
				if !strings.Contains(string(model.Raw), model.ID) {
					t.Fatalf("model %q kept raw object %s", model.ID, model.Raw)
				}
			}
		})
	}
}

func upstreamModelIDs(models []cliproxyauth.UpstreamModel) []string {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	return ids
}

func TestMetaModelListDoesNotMintAPIKey(t *testing.T) {
	mintServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		t.Error("model listing called the mint endpoint")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer mintServer.Close()
	t.Setenv("META_MINT_URL", mintServer.URL)

	calls := 0
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", usageRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return nil, fmt.Errorf("unexpected model list request")
	}))
	auth := &cliproxyauth.Auth{
		Provider: "meta",
		Metadata: map[string]any{"dca_token": "dca:fake", "auth_kind": "oauth"},
	}
	before := auth.Clone()
	ids, err := NewMetaExecutor(nil).ListUpstreamModels(ctx, auth)
	if err == nil || err.Error() != "meta model list: no minted API key" || len(ids) != 0 {
		t.Fatalf("DCA-only listing: models=%v err=%v", ids, err)
	}
	if calls != 0 {
		t.Fatalf("model listing made %d requests without a minted API key", calls)
	}
	if !reflect.DeepEqual(auth, before) {
		t.Fatal("model listing mutated the DCA-only auth")
	}
}

func TestCodexModelListRequiresAccountID(t *testing.T) {
	if ids, err := NewCodexExecutor(nil).ListUpstreamModels(context.Background(), &cliproxyauth.Auth{Metadata: map[string]any{"access_token": "fake-token"}}); err == nil || len(ids) != 0 {
		t.Fatalf("missing account ID accepted: models=%v err=%v", ids, err)
	}
}

func TestOAuthModelListRejectsInvalidOrFailedResponse(t *testing.T) {
	for _, provider := range []string{"claude", "codex", "meta"} {
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
				auth := &cliproxyauth.Auth{Metadata: map[string]any{"access_token": "fake-token", "account_id": "account-1"}}
				if provider == "meta" {
					lister = NewMetaExecutor(nil)
					auth.Attributes = map[string]string{"auth_kind": "oauth", "api_key": "fake-api-key"}
				}
				ids, err := lister.ListUpstreamModels(ctx, auth)
				if err == nil || len(ids) != 0 {
					t.Fatalf("invalid upstream list accepted: ids=%v err=%v", ids, err)
				}
				if provider == "meta" {
					if !strings.HasPrefix(err.Error(), "meta model list: ") {
						t.Fatalf("missing Meta error context: %v", err)
					}
					if response.status == http.StatusOK && err.Error() != "meta model list: invalid data" {
						t.Fatalf("invalid Meta data error = %v", err)
					}
				}
			})
		}
	}
}
