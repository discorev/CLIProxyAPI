package executor

import (
	"context"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// CodexExecutor is a stateless executor for Codex (OpenAI Responses API entrypoint).
// If api_key is unavailable on auth, it falls back to legacy via ClientAdapter.
type CodexExecutor struct {
	cfg          *config.Config
	resetBaseURL string // Only overridden by offline executor tests.
}

func NewCodexExecutor(cfg *config.Config) *CodexExecutor { return &CodexExecutor{cfg: cfg} }

func (e *CodexExecutor) Identifier() string { return "codex" }

func (e *CodexExecutor) modelLevelCooling() bool {
	return e != nil && e.cfg != nil && e.cfg.Codex.ModelLevelCooling
}

func (e *CodexExecutor) FetchUsage(ctx context.Context, auth *cliproxyauth.Auth) (cliproxyauth.UsageFetchResult, error) {
	return helps.FetchCodexUsage(ctx, auth, e.HttpRequest)
}

func (e *CodexExecutor) ListUpstreamModels(ctx context.Context, auth *cliproxyauth.Auth) ([]string, error) {
	return helps.FetchCodexModelList(ctx, auth, e.HttpRequest)
}

func (e *CodexExecutor) ApplyReset(ctx context.Context, auth *cliproxyauth.Auth, request cliproxyauth.ResetRequest) (cliproxyauth.ResetResult, error) {
	return helps.ApplyCodexReset(ctx, auth, request, e.resetBaseURL, helps.ResetHTTPTransport(e.cfg, e.PrepareRequest))
}

// SupportsApplyPatch reports the actual executor contract, independent of its provider name.
func (e *CodexExecutor) SupportsApplyPatch() bool { return e != nil }
