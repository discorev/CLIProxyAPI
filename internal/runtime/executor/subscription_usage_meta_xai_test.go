package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestMetaSubscriptionUsageTransportAndSanitization(t *testing.T) {
	for _, usage := range []string{
		`{"tier":"Power","window":{"used_percent":"12.5","resets_at":1800000000,"window_duration_mins":300,"api_key":"discard-nested"},"weekly":{"used_percent":42,"resets_at":1800500000},"email":"discard-nested"}`,
		`null`,
	} {
		t.Run(usage, func(t *testing.T) {
			auth := &cliproxyauth.Auth{Provider: "meta", Attributes: map[string]string{"auth_kind": "oauth", "api_key": "fake-generation-key"}, Metadata: map[string]any{"dca_token": "fake-dca", "access_token": "fake-generation-key", "email": "stored@example.test"}}
			before := auth.Clone()
			calls := 0
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", usageRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.Method != http.MethodPost || req.URL.String() != "https://api.meta.ai/muse-code/key" {
					t.Errorf("wrong Meta usage method or endpoint")
				}
				for key, want := range map[string]string{"Authorization": "Bearer fake-dca", "x-api-version": "1.0.0", "Content-Type": "application/json", "Accept": "application/json"} {
					if req.Header.Get(key) != want {
						t.Errorf("wrong %s header", key)
					}
				}
				if _, deadline := req.Context().Deadline(); deadline {
					t.Fatal("usage request added a deadline")
				}
				body, err := io.ReadAll(req.Body)
				if err != nil || string(body) != `{"onboard":false}` {
					t.Fatal("wrong Meta usage body")
				}
				response := `{"api_key":"discard-key","user_email":"discard-email","user_full_name":"discard-name","user_avatar_url":"discard-avatar","base_url":"discard-base","action_url":"discard-action","payment_method":"discard-payment","subs_tier_name":"Power","subs_tier_id":"power","is_subs_active":true,"subs_usage":` + usage + `}`
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(response))}, nil
			}))
			var fetcher cliproxyauth.UsageFetcher = NewMetaExecutor(nil)
			result, err := fetcher.FetchUsage(ctx, auth)
			if err != nil || calls != 1 || result.Resets != nil || !reflect.DeepEqual(before, auth) {
				t.Fatalf("Meta usage failed or modified credential: err=%v calls=%d", err, calls)
			}
			raw, errMarshal := json.Marshal(result.Raw)
			if errMarshal != nil || strings.Contains(string(raw), "discard-") || strings.Contains(string(raw), "api_key") || strings.Contains(string(raw), "email") || strings.Contains(string(raw), "user_full_name") {
				t.Fatal("Meta usage exposed a credential or identity field")
			}
			if !strings.Contains(string(raw), `"subs_tier_name":"Power"`) {
				t.Fatal("Meta plan was lost")
			}
			if usage == `null` {
				if result.Windows == nil || len(result.Windows) != 0 || !strings.Contains(string(raw), `"subs_usage":null`) {
					t.Fatal("unknown Meta usage must stay unknown with plan data")
				}
				return
			}
			want := []cliproxyauth.UsageWindow{
				{Kind: "5h", Length: 18000, UsedPercent: 12.5, ResetsAt: time.Unix(1800000000, 0).UTC()},
				{Kind: "7d", Length: 604800, UsedPercent: 42, ResetsAt: time.Unix(1800500000, 0).UTC()},
			}
			if !reflect.DeepEqual(result.Windows, want) {
				t.Fatalf("windows=%+v", result.Windows)
			}
		})
	}
}

func TestMetaSubscriptionUsageRequiresValidDCA(t *testing.T) {
	for _, metadata := range []map[string]any{
		{"auth_kind": "oauth", "access_token": "fake-key"},
		{"auth_kind": "oauth", "dca_token": "fake-dca", "dca_expired": "2000-01-01T00:00:00Z"},
		{"auth_kind": "oauth", "dca_token": "fake-dca", "dca_expires_at": float64(946684800)},
		{"auth_kind": "oauth", "dca_token": "fake-dca", "dca_expires_at": int64(946684800)},
		{"auth_kind": "oauth", "dca_token": "fake-dca", "dca_expires_at": "946684800"},
	} {
		ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", usageRoundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("missing/expired DCA called upstream")
			return nil, nil
		}))
		if _, err := NewMetaExecutor(nil).FetchUsage(ctx, &cliproxyauth.Auth{Provider: "meta", Metadata: metadata}); err == nil {
			t.Fatal("missing/expired DCA accepted")
		}
	}
}

func TestXAISubscriptionUsageTransportAndForwarder(t *testing.T) {
	for _, auto := range []bool{false, true} {
		for _, monthlyFails := range []bool{false, true} {
			auth := &cliproxyauth.Auth{Provider: "xai", Metadata: map[string]any{"access_token": "fake-xai"}}
			seen := make(map[string]int)
			weekly := `{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-10-08T00:00:00Z","end":"2026-10-15T00:00:00Z"},"creditUsagePercent":"63.5","productUsage":[{"product":"cli","usagePercent":99}]}}`
			monthly := `{"config":{"monthlyLimit":2000,"used":2000}}`
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", usageRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.Method != http.MethodGet || req.URL.Host != "cli-chat-proxy.grok.com" {
					t.Error("wrong xAI usage method or endpoint")
				}
				for key, want := range map[string]string{"Authorization": "Bearer fake-xai", "x-xai-token-auth": "xai-grok-cli", "x-grok-client-version": helps.GetXAIClientVersion(), "User-Agent": "xai-grok-workspace/" + helps.GetXAIClientVersion(), "Accept": "application/json"} {
					if req.Header.Get(key) != want {
						t.Errorf("wrong %s header", key)
					}
				}
				seen[req.URL.RequestURI()]++
				status, body := 200, ""
				switch req.URL.RequestURI() {
				case "/v1/billing?format=credits":
					body = weekly
				case "/v1/billing":
					body = monthly
					if monthlyFails {
						status, body = 503, `{"error":"discard-private-error"}`
					}
				case "/v1/user?include=subscription":
					body = `{"email":"discard-email","id":"discard-id","subscription":{"subscription_tier":"super","subscriptionTierDisplay":"Super"}}`
				case "/v1/settings":
					body = `{"subscriptionTierDisplay":"Super Grok","name":"discard-name"}`
				default:
					t.Fatal("unexpected xAI request")
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
			}))
			var fetcher cliproxyauth.UsageFetcher = NewXAIExecutor(nil)
			if auto {
				fetcher = NewXAIAutoExecutor(nil)
			}
			result, err := fetcher.FetchUsage(ctx, auth)
			if err != nil || len(seen) != 4 || result.Resets != nil || string(result.Raw["usage"]) != weekly {
				t.Fatalf("xAI fetch: err=%v requests=%d", err, len(seen))
			}
			if (result.LastError != "") != monthlyFails || (!monthlyFails && string(result.Raw["billing"]) != monthly) {
				t.Fatal("monthly billing failure did not stay ancillary")
			}
			if got := string(result.Raw["subscription"]); got != `{"subscriptionTier":"super","subscriptionTierDisplay":"Super Grok"}` {
				t.Fatal("wrong sanitized xAI subscription")
			}
			if len(result.Windows) != 1 || result.Windows[0].UsedPercent != 63.5 || result.Windows[0].Kind != "7d" || result.Windows[0].Length != 604800 || result.Windows[0].Scope != "" {
				t.Fatalf("wrong xAI weekly windows: %+v", result.Windows)
			}
			if strings.Contains(result.LastError, "discard-") {
				t.Fatal("ancillary failure exposed upstream body")
			}
		}
	}
	var empty *XAIAutoExecutor
	if _, err := empty.FetchUsage(context.Background(), nil); err == nil {
		t.Fatal("nil auto executor accepted")
	}
}

func TestNewUsageExecutorsDoNotSupportResets(t *testing.T) {
	for _, executor := range []cliproxyauth.ProviderExecutor{NewMetaExecutor(nil), NewXAIExecutor(nil), NewXAIAutoExecutor(nil)} {
		if _, ok := executor.(cliproxyauth.ResetApplier); ok {
			t.Fatalf("%s unexpectedly supports resets", executor.Identifier())
		}
	}
}

func TestXAISubscriptionUsageAncillaryCacheTTL(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := cliproxyauth.NewManager(nil, nil, nil)
		manager.RegisterExecutor(NewXAIAutoExecutor(nil))
		if _, err := manager.Register(context.Background(), &cliproxyauth.Auth{ID: "xai", Provider: "xai", Metadata: map[string]any{"access_token": "fake"}}); err != nil {
			t.Fatal(err)
		}
		seen := make(map[string]int)
		ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", usageRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			seen[req.URL.RequestURI()]++
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		}))
		for i, advance := range []time.Duration{0, cliproxyauth.UsageMinFetchInterval, cliproxyauth.UsageAncillaryTTL - cliproxyauth.UsageMinFetchInterval} {
			// synctest advances virtual time; this never sleeps on the wall clock.
			time.Sleep(advance)
			entry, err := manager.RefreshUsage(ctx, "xai")
			if err != nil || string(entry.Raw["subscription"]) != `{}` {
				t.Fatalf("refresh=%v", err)
			}
			want := []int{1, 1, 2}[i]
			if seen["/v1/user?include=subscription"] != want || seen["/v1/settings"] != want || seen["/v1/billing?format=credits"] != i+1 || seen["/v1/billing"] != i+1 {
				t.Fatalf("request counts=%v", seen)
			}
		}
	})
}
