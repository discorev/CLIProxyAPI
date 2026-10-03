package executor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestSubscriptionResetExecutorHTTP(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			var calls atomic.Int32
			org := "12345678-1234-1234-1234-123456789abc"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer fake-token" || r.Header.Get("Content-Type") != "application/json" {
					t.Errorf("wrong method/headers: %s %v", r.Method, r.Header)
				}
				var body map[string]string
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if provider == "codex" {
					if r.URL.Path != "/backend-api/wham/rate-limit-reset-credits/consume" || r.Header.Get("Chatgpt-Account-Id") != "fake-account" || !strings.HasPrefix(r.Header.Get("User-Agent"), "codex-tui/0.149.1 ") {
						t.Errorf("wrong codex request: %s %v", r.URL, r.Header)
					}
					id, err := uuid.Parse(body["redeem_request_id"])
					if err != nil || id.Version() != 4 || len(body) != 1 {
						t.Errorf("wrong body: %+v", body)
					}
				} else {
					if r.URL.Path != "/api/organizations/"+org+"/reset_rate_limits" || r.Header.Get("User-Agent") != "claude-cli/2.1.280 (external, cli)" || r.Header.Get("Anthropic-Beta") != "oauth-2025-04-20" {
						t.Errorf("wrong claude request: %s %v", r.URL, r.Header)
					}
					if body["program"] != "cedar_ember" || body["grant_id"] != "grant-1" || !regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`).MatchString(body["request_id"]) || len(body) != 3 {
						t.Errorf("wrong body: %+v", body)
					}
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"result":"reset"}`))
			}))
			defer server.Close()
			auth := &cliproxyauth.Auth{Provider: provider, Metadata: map[string]interface{}{"access_token": "fake-token", "account_id": "fake-account", "organization_uuid": org}}
			var applier cliproxyauth.ResetApplier
			if provider == "codex" {
				e := NewCodexAutoExecutor(nil)
				e.httpExec.resetBaseURL = server.URL
				applier = e
			} else {
				e := NewClaudeExecutor(nil)
				e.resetBaseURL = server.URL
				applier = e
			}
			result, err := applier.ApplyReset(context.Background(), auth, cliproxyauth.ResetRequest{GrantID: "grant-1"})
			if err != nil || result.Result != "reset" || result.NotSent || calls.Load() != 1 {
				t.Fatalf("apply=%+v,%v calls=%d", result, err, calls.Load())
			}
		})
	}
}

func TestSubscriptionResetClaudeOutcomesAndRefusals(t *testing.T) {
	for _, tt := range []struct {
		name       string
		status     int
		body, want string
		wantErr    bool
	}{
		{"reset", 200, `{"result":"reset"}`, "reset", false},
		{"already used", 200, `{"result":"already_used"}`, "already_used", false},
		{"not limited", 200, `{"result":"not_limited"}`, "not_limited", false},
		{"cooldown", 200, `{"result":"cooldown"}`, "cooldown", false},
		{"ineligible", 200, `{"result":"ineligible"}`, "ineligible", false},
		{"unavailable", 200, `{"result":"unavailable"}`, "unavailable", false},
		{"limited", 429, `{}`, "rate_limited", false},
		{"unauthorized", 401, `{}`, "auth_error", false},
		{"forbidden", 403, `{}`, "auth_error", false},
		{"server error", 500, `{}`, "unknown", true},
		{"malformed", 200, `bad`, "unknown", true},
		{"unexpected result", 200, `{"result":"future"}`, "unknown", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()
			e := NewClaudeExecutor(nil)
			e.resetBaseURL = server.URL
			auth := &cliproxyauth.Auth{Provider: "claude", Metadata: map[string]interface{}{"access_token": "fake-token"}}
			result, err := e.ApplyReset(context.Background(), auth, cliproxyauth.ResetRequest{GrantID: "grant", OrganizationID: "12345678-1234-1234-1234-123456789abc"})
			if result.Result != tt.want || result.NotSent || (err != nil) != tt.wantErr {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
	var calls atomic.Int32
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", usageRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("secret proxy error")
	}))
	e := NewClaudeExecutor(nil)
	e.resetBaseURL = "http://127.0.0.1:1"
	auth := &cliproxyauth.Auth{Provider: "claude", Metadata: map[string]interface{}{"access_token": "fake-token"}}
	valid := cliproxyauth.ResetRequest{GrantID: "grant", OrganizationID: "12345678-1234-1234-1234-123456789abc"}
	for _, claim := range []cliproxyauth.ResetRequest{{}, {GrantID: "../bad", OrganizationID: valid.OrganizationID}, {GrantID: "grant", OrganizationID: "bad"}} {
		result, err := e.ApplyReset(ctx, auth, claim)
		if !result.NotSent || err == nil || calls.Load() != 0 {
			t.Fatalf("unsafe pre-send refusal: %+v %v calls=%d", result, err, calls.Load())
		}
	}
	result, err := e.ApplyReset(ctx, auth, valid)
	if result.NotSent || result.Result != "unknown" || err == nil || strings.Contains(err.Error(), "secret") || calls.Load() != 1 {
		t.Fatalf("transport outcome=%+v %v", result, err)
	}
}

func TestSubscriptionResetDoesNotFollowRedirect(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Location", "/second-redemption")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	e := NewCodexExecutor(nil)
	e.resetBaseURL = server.URL
	result, err := e.ApplyReset(context.Background(), &cliproxyauth.Auth{Provider: "codex", Metadata: map[string]interface{}{"access_token": "fake-token"}}, cliproxyauth.ResetRequest{})
	if result.Result != "unknown" || result.NotSent || err == nil || calls.Load() != 1 {
		t.Fatalf("redirect replay: result=%+v err=%v calls=%d", result, err, calls.Load())
	}
}

func TestSubscriptionResetCodexHTTPRefusals(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
			}))
			defer server.Close()
			executor := NewCodexExecutor(nil)
			executor.resetBaseURL = server.URL
			auth := &cliproxyauth.Auth{Provider: "codex", Metadata: map[string]interface{}{"access_token": "fake-token"}}
			result, err := executor.ApplyReset(context.Background(), auth, cliproxyauth.ResetRequest{})
			want := "auth_error"
			if status == http.StatusInternalServerError {
				want = "unknown"
			}
			if result.Result != want || result.NotSent || (err != nil) != (want == "unknown") {
				t.Fatalf("status=%d result=%+v err=%v", status, result, err)
			}
		})
	}
}

func TestSubscriptionResetSendsIdempotencyKey(t *testing.T) {
	const key = "5b3d7a6c-1f2e-5a4b-9c8d-0e1f2a3b4c5d"
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			var sent []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]string
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				sent = append(sent, body["redeem_request_id"]+body["request_id"])
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"result":"reset"}`))
			}))
			defer server.Close()
			auth := &cliproxyauth.Auth{Provider: provider, Metadata: map[string]interface{}{"access_token": "fake-token", "account_id": "fake-account", "organization_uuid": "12345678-1234-1234-1234-123456789abc"}}
			var applier cliproxyauth.ResetApplier
			if provider == "codex" {
				e := NewCodexExecutor(nil)
				e.resetBaseURL = server.URL
				applier = e
			} else {
				e := NewClaudeExecutor(nil)
				e.resetBaseURL = server.URL
				applier = e
			}
			for range 2 {
				if _, err := applier.ApplyReset(context.Background(), auth, cliproxyauth.ResetRequest{GrantID: "grant-1", IdempotencyKey: key}); err != nil {
					t.Fatal(err)
				}
			}
			// An empty key (manual reset) falls back to a fresh random ID.
			if _, err := applier.ApplyReset(context.Background(), auth, cliproxyauth.ResetRequest{GrantID: "grant-1"}); err != nil {
				t.Fatal(err)
			}
			if len(sent) != 3 || sent[0] != key || sent[1] != key || sent[2] == key {
				t.Fatalf("request IDs = %v", sent)
			}
			if _, errParse := uuid.Parse(sent[2]); errParse != nil {
				t.Fatalf("random fallback is not a UUID: %q", sent[2])
			}
		})
	}
}
