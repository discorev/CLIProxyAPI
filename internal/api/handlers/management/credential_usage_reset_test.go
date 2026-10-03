package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type credentialResetExecutor struct {
	coreauth.ProviderExecutor
	provider  string
	calls     atomic.Int32
	fetches   atomic.Int32
	lastError string
	grantID   string
	empty     bool
	block     chan struct{}
}

func (e *credentialResetExecutor) Identifier() string { return e.provider }
func (e *credentialResetExecutor) FetchUsage(context.Context, *coreauth.Auth) (coreauth.UsageFetchResult, error) {
	e.fetches.Add(1)
	result := coreauth.UsageFetchResult{Resets: &coreauth.CredentialResets{}, LastError: e.lastError, Windows: []coreauth.UsageWindow{{Kind: "7d", UsedPercent: 0, ResetsAt: time.Now().Add(time.Hour)}}}
	if e.empty || e.calls.Load() > 0 {
		return result, nil
	}
	if e.provider == "codex" {
		result.Resets = &coreauth.CredentialResets{Credits: []coreauth.ResetCredit{{ID: "credit", ExpiresAt: time.Now().Add(24 * time.Hour)}}}
	} else {
		next := "next-grant"
		result.Resets = &coreauth.CredentialResets{ClaudeResetStatus: &coreauth.ClaudeResetStatus{Eligible: true, NextGrantID: &next, Grants: []coreauth.ResetGrant{
			{ID: "first-grant", ResetsTotal: 1, ResetsLeft: 1, UsableNow: true, UseRequiresLimit: false},
			{ID: "next-grant", ResetsTotal: 1, ResetsLeft: 1, UsableNow: true, UseRequiresLimit: false},
		}}}
	}
	return result, nil
}
func (e *credentialResetExecutor) ApplyReset(_ context.Context, _ *coreauth.Auth, claim coreauth.ResetRequest) (coreauth.ResetResult, error) {
	e.calls.Add(1)
	e.grantID = claim.GrantID
	if e.block != nil {
		<-e.block
	}
	return coreauth.ResetResult{Result: "reset"}, nil
}

func TestCredentialUsageResetAPI(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, provider := range []string{"claude", "codex"} {
		for _, tt := range []struct {
			name, index, grant string
			empty              bool
			status             int
		}{
			{"manual bypasses auto rules", "oauth", "", false, 200},
			{"explicit grant", "oauth", "first-grant", false, map[string]int{"claude": 200, "codex": 400}[provider]},
			{"unknown grant", "oauth", "missing", false, 400},
			{"no resets", "oauth", "", true, 400},
			{"unknown credential", "missing", "", false, 404},
			{"API key", "key", "", false, 400},
			{"unsupported provider", "gemini", "", false, 400},
			{"index required", "", "", false, 400},
		} {
			t.Run(provider+"/"+tt.name, func(t *testing.T) {
				manager := coreauth.NewManager(nil, nil, nil)
				executor := &credentialResetExecutor{provider: provider, empty: tt.empty}
				manager.RegisterExecutor(executor)
				indices := map[string]string{"missing": "missing", "": ""}
				for _, auth := range []*coreauth.Auth{
					{ID: "oauth", Provider: provider, Metadata: map[string]interface{}{"access_token": "fake-token"}},
					{ID: "key", Provider: provider, Attributes: map[string]string{"api_key": "fake-key"}},
					{ID: "gemini", Provider: "gemini", Metadata: map[string]interface{}{"access_token": "fake-token"}},
				} {
					saved, err := manager.Register(context.Background(), auth)
					if err != nil {
						t.Fatal(err)
					}
					indices[auth.ID] = saved.Index
				}
				h := &Handler{authManager: manager}
				router := gin.New()
				router.POST("/v8/management/credentials/usage/reset", h.ResetCredentialUsage)
				body, _ := json.Marshal(map[string]string{"auth_index": indices[tt.index], "grant_id": tt.grant})
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v8/management/credentials/usage/reset", strings.NewReader(string(body))))
				if rec.Code != tt.status {
					t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
				}
				if tt.status == 200 {
					var response struct {
						Result         string                  `json:"result"`
						Entry          credentialUsageResponse `json:"entry"`
						RefreshPending bool                    `json:"refresh_pending"`
						NextFetchAt    time.Time               `json:"next_fetch_at"`
					}
					if json.Unmarshal(rec.Body.Bytes(), &response) != nil || response.Result != "reset" || response.Entry.AuthID != "oauth" || response.Entry.FetchedAt.IsZero() || response.Entry.Windows[0].UsedPercent != 0 || executor.calls.Load() != 1 || !response.RefreshPending || !response.NextFetchAt.Equal(response.Entry.NextFetchAt) || executor.fetches.Load() != 1 {
						t.Fatalf("bad reset response: %s", rec.Body.String())
					}
					if provider == "claude" {
						want := tt.grant
						if want == "" {
							want = "next-grant"
						}
						if executor.grantID != want {
							t.Fatalf("grant=%s want=%s", executor.grantID, want)
						}
					}
				} else if executor.calls.Load() != 0 {
					t.Fatal("invalid request spent reset")
				}
				if manager.ResetLoopRunning() || strings.Contains(rec.Body.String(), "fake-token") {
					t.Fatal("manual reset enabled auto loop or leaked token")
				}
			})
		}
	}
}

func TestCredentialUsageResetAPIInFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := coreauth.NewManager(nil, nil, nil)
		executor := &credentialResetExecutor{provider: "codex", block: make(chan struct{})}
		manager.RegisterExecutor(executor)
		auth, err := manager.Register(context.Background(), &coreauth.Auth{ID: "oauth", Provider: "codex", Metadata: map[string]interface{}{"access_token": "fake-token"}})
		if err != nil {
			t.Fatal(err)
		}
		h := &Handler{authManager: manager}
		router := gin.New()
		router.POST("/reset", h.ResetCredentialUsage)
		request := func() *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/reset", strings.NewReader(`{"auth_index":"`+auth.Index+`"}`)))
			return rec
		}
		first := make(chan *httptest.ResponseRecorder, 1)
		go func() { first <- request() }()
		synctest.Wait()
		if rec := request(); rec.Code != http.StatusConflict {
			t.Fatalf("concurrent reset=%d %s", rec.Code, rec.Body.String())
		}
		close(executor.block)
		synctest.Wait()
		if rec := <-first; rec.Code != http.StatusOK || executor.calls.Load() != 1 {
			t.Fatalf("first=%d calls=%d", rec.Code, executor.calls.Load())
		}
	})
}

func TestCredentialUsageResetAPIPendingRefreshWithAncillaryError(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				manager := coreauth.NewManager(nil, nil, nil)
				executor := &credentialResetExecutor{provider: provider, lastError: "ancillary endpoint failed"}
				manager.RegisterExecutor(executor)
				auth, err := manager.Register(context.Background(), &coreauth.Auth{ID: "oauth", Provider: provider, Metadata: map[string]interface{}{"access_token": "fake-token"}})
				if err != nil {
					t.Fatal(err)
				}
				h := &Handler{authManager: manager}
				router := gin.New()
				router.POST("/reset", h.ResetCredentialUsage)
				request := func() *httptest.ResponseRecorder {
					rec := httptest.NewRecorder()
					router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/reset", strings.NewReader(`{"auth_index":"`+auth.Index+`"}`)))
					return rec
				}
				var first struct {
					RefreshPending bool      `json:"refresh_pending"`
					NextFetchAt    time.Time `json:"next_fetch_at"`
				}
				rec := request()
				if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &first) != nil || !first.RefreshPending || !first.NextFetchAt.After(time.Now()) {
					t.Fatalf("first reset=%d %s", rec.Code, rec.Body.String())
				}
				var second struct {
					Error          string    `json:"error"`
					RefreshPending bool      `json:"refresh_pending"`
					NextFetchAt    time.Time `json:"next_fetch_at"`
				}
				rec = request()
				if rec.Code != http.StatusConflict || json.Unmarshal(rec.Body.Bytes(), &second) != nil || second.Error != "reset pending refresh" || !second.RefreshPending || !second.NextFetchAt.Equal(first.NextFetchAt) {
					t.Fatalf("pending reset=%d %s", rec.Code, rec.Body.String())
				}
				if executor.calls.Load() != 1 || executor.fetches.Load() != 1 {
					t.Fatal("manual reset bypassed fetch floor or spent twice")
				}
			})
		})
	}
}
