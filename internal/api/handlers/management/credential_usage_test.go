package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type credentialUsageExecutor struct {
	coreauth.ProviderExecutor
	calls atomic.Int32
	fail  atomic.Bool
}

func (e *credentialUsageExecutor) Identifier() string { return "claude" }
func (e *credentialUsageExecutor) FetchUsage(context.Context, *coreauth.Auth) (coreauth.UsageFetchResult, error) {
	e.calls.Add(1)
	if e.fail.Load() {
		return coreauth.UsageFetchResult{}, errors.New("upstream status 429")
	}
	return coreauth.UsageFetchResult{
		Raw:     map[string]json.RawMessage{"usage": json.RawMessage(`{"five_hour":{"utilization":6}}`)},
		Windows: []coreauth.UsageWindow{{Kind: "5h", UsedPercent: 6, Length: 18000}},
	}, nil
}

func TestCredentialUsageAPI(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	executor := &credentialUsageExecutor{}
	manager.RegisterExecutor(executor)
	oauth, err := manager.Register(context.Background(), &coreauth.Auth{ID: "oauth", Provider: "claude", Metadata: map[string]interface{}{"access_token": "fake-token"}})
	if err != nil {
		t.Fatal(err)
	}
	key, err := manager.Register(context.Background(), &coreauth.Auth{ID: "key", Provider: "claude", Attributes: map[string]string{"api_key": "fake-key"}})
	if err != nil {
		t.Fatal(err)
	}
	handler := &Handler{authManager: manager}
	router := gin.New()
	router.GET("/v8/management/credentials/usage", handler.GetCredentialUsage)
	router.POST("/v8/management/credentials/usage/refresh", handler.RefreshCredentialUsage)
	request := func(method, query, body string) *httptest.ResponseRecorder {
		t.Helper()
		path := "/v8/management/credentials/usage"
		if method == http.MethodPost {
			path += "/refresh"
		}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path+query, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(rec, req)
		return rec
	}
	assertShape := func(rec *httptest.ResponseRecorder, refreshed bool) {
		t.Helper()
		var rows []map[string]json.RawMessage
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &rows) != nil || len(rows) != 1 {
			t.Fatalf("response: %d %s", rec.Code, rec.Body.String())
		}
		for _, field := range []string{"auth_index", "auth_id", "provider", "raw", "resets", "fetched_at", "windows", "observed_at", "refreshing", "last_error", "next_fetch_at", "cooldown_until"} {
			if _, ok := rows[0][field]; !ok {
				t.Errorf("missing field %s in %s", field, rec.Body.String())
			}
		}
		if string(rows[0]["auth_id"]) != `"oauth"` || string(rows[0]["auth_index"]) != `"`+oauth.Index+`"` {
			t.Errorf("wrong credential: %s", rec.Body.String())
		}
		if !refreshed && (string(rows[0]["raw"]) != `{}` || string(rows[0]["windows"]) != `[]`) {
			t.Errorf("empty cache shape: %s", rec.Body.String())
		}
		if refreshed && !strings.Contains(string(rows[0]["raw"]), `"utilization":6`) {
			t.Errorf("missing fetched raw data: %s", rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "fake-token") {
			t.Fatal("credential token exposed in response")
		}
	}
	assertShape(request(http.MethodGet, "", ""), false)
	if executor.calls.Load() != 0 || manager.UsageSweepRunning() {
		t.Fatal("GET triggered refresh or sweep")
	}
	assertShape(request(http.MethodPost, "", `{"auth_index":"`+oauth.Index+`"}`), true)
	assertShape(request(http.MethodGet, "?auth_index="+oauth.Index, ""), true)
	assertShape(request(http.MethodPost, "", `{}`), true)
	if executor.calls.Load() != 1 {
		t.Fatalf("refresh calls = %d", executor.calls.Load())
	}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		for _, test := range []struct {
			index string
			code  int
		}{{"missing", http.StatusNotFound}, {key.Index, http.StatusBadRequest}} {
			query, body := "?auth_index="+test.index, `{"auth_index":"`+test.index+`"}`
			if method == http.MethodPost {
				query = ""
			}
			if got := request(method, query, body); got.Code != test.code {
				t.Errorf("%s %s: %d %s", method, test.index, got.Code, got.Body.String())
			}
		}
	}
	if got := request(http.MethodPost, "", `{"auth_index": 1}`); got.Code != http.StatusBadRequest {
		t.Fatalf("malformed body status %d", got.Code)
	}
	executor.fail.Store(true)
	failed := request(http.MethodPost, "", `{"auth_index":"`+oauth.Index+`"}`)
	assertShape(failed, true)
	if executor.calls.Load() != 1 || strings.Contains(failed.Body.String(), "upstream status 429") {
		t.Fatalf("manual refresh bypassed fetch floor: %s", failed.Body.String())
	}
}

func TestCredentialUsageAPIEmptyArray(t *testing.T) {
	handler := &Handler{authManager: coreauth.NewManager(nil, nil, nil)}
	router := gin.New()
	router.GET("/usage", handler.GetCredentialUsage)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/usage", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "[]" {
		t.Fatalf("empty response: %d %s", rec.Code, rec.Body.String())
	}
}
