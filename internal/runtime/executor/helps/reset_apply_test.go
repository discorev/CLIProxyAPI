package helps

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"testing/synctest"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// blockingBody never yields data; Read blocks until Close.
type blockingBody struct {
	reads   atomic.Int32
	release chan struct{}
	closed  atomic.Bool
}

func (b *blockingBody) Read([]byte) (int, error) {
	b.reads.Add(1)
	<-b.release
	return 0, http.ErrBodyReadAfterClose
}

func (b *blockingBody) Close() error {
	if b.closed.CompareAndSwap(false, true) {
		close(b.release)
	}
	return nil
}

func TestResetErrorStatusIgnoresBody(t *testing.T) {
	for _, tt := range []struct {
		provider string
		status   int
		want     string
	}{
		{"codex", http.StatusTooManyRequests, "rate_limited"},
		{"codex", http.StatusUnauthorized, "auth_error"},
		{"codex", http.StatusForbidden, "auth_error"},
		{"codex", http.StatusInternalServerError, "unknown"},
		{"claude", http.StatusTooManyRequests, "rate_limited"},
		{"claude", http.StatusUnauthorized, "auth_error"},
	} {
		t.Run(tt.provider+"/"+http.StatusText(tt.status), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				body := &blockingBody{release: make(chan struct{})}
				request := func(context.Context, *cliproxyauth.Auth, *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: tt.status, Body: body}, nil
				}
				auth := &cliproxyauth.Auth{Provider: tt.provider, Metadata: map[string]any{
					"access_token": "fake-token", "organization_uuid": "12345678-1234-1234-1234-123456789abc",
				}}
				claim := cliproxyauth.ResetRequest{GrantID: "grant-1", CreditID: "credit-1"}
				done := make(chan cliproxyauth.ResetResult, 1)
				go func() {
					var result cliproxyauth.ResetResult
					if tt.provider == "codex" {
						result, _ = ApplyCodexReset(context.Background(), auth, claim, "https://example.test", request)
					} else {
						result, _ = ApplyClaudeReset(context.Background(), auth, claim, "https://example.test", request)
					}
					done <- result
				}()
				// Wait returns once every goroutine is blocked: a Read would park
				// sendReset on the body and leave done empty.
				synctest.Wait()
				select {
				case result := <-done:
					if result.Result != tt.want || result.NotSent {
						t.Fatalf("result=%+v, want %s", result, tt.want)
					}
				default:
					body.Close()
					<-done
					t.Fatal("sendReset blocked reading an error body")
				}
				if body.reads.Load() != 0 || !body.closed.Load() {
					t.Fatalf("reads=%d closed=%t", body.reads.Load(), body.closed.Load())
				}
			})
		})
	}
}
