package helps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const codexModelListClientVersion = "99.0.0"

// upstreamModels keeps each listed object with its trimmed ID, dropping entries without one.
func upstreamModels(objects []json.RawMessage, idKey string) []cliproxyauth.UpstreamModel {
	models := make([]cliproxyauth.UpstreamModel, 0, len(objects))
	for _, raw := range objects {
		var fields map[string]any
		if json.Unmarshal(raw, &fields) != nil {
			continue
		}
		if id, _ := fields[idKey].(string); strings.TrimSpace(id) != "" {
			models = append(models, cliproxyauth.UpstreamModel{ID: strings.TrimSpace(id), Raw: raw})
		}
	}
	return models
}

// FetchMetaModelList lists every model visible to this OAuth account, including hidden models.
func FetchMetaModelList(ctx context.Context, auth *cliproxyauth.Auth, request UsageHTTPRequest) ([]cliproxyauth.UpstreamModel, error) {
	headers := make(http.Header)
	headers.Set("Accept", "application/json")
	body, errFetch := fetchUsageJSON(ctx, auth, request, "https://api.meta.ai/muse-code/models", headers)
	if errFetch != nil {
		return nil, fmt.Errorf("meta model list: %w", errFetch)
	}
	var result struct {
		Data []json.RawMessage `json:"data"`
	}
	if errDecode := json.Unmarshal(body, &result); errDecode != nil || result.Data == nil {
		return nil, errors.New("meta model list: invalid data")
	}
	return upstreamModels(result.Data, "id"), nil
}

// FetchClaudeModelList lists every model visible to this OAuth credential.
func FetchClaudeModelList(ctx context.Context, auth *cliproxyauth.Auth, request UsageHTTPRequest) ([]cliproxyauth.UpstreamModel, error) {
	headers := claudeUsageHeaders()
	headers.Set("Anthropic-Version", "2023-06-01")
	models := make([]cliproxyauth.UpstreamModel, 0)
	after := ""
	for {
		endpoint := "https://api.anthropic.com/v1/models?limit=1000"
		if after != "" {
			endpoint += "&after_id=" + url.QueryEscape(after)
		}
		body, errFetch := fetchUsageJSON(ctx, auth, request, endpoint, headers)
		if errFetch != nil {
			return nil, fmt.Errorf("claude model list: %w", errFetch)
		}
		var page struct {
			Data    []json.RawMessage `json:"data"`
			HasMore bool              `json:"has_more"`
		}
		if errDecode := json.Unmarshal(body, &page); errDecode != nil || page.Data == nil {
			return nil, errors.New("claude model list: invalid data")
		}
		pageModels := upstreamModels(page.Data, "id")
		models = append(models, pageModels...)
		if !page.HasMore {
			return models, nil
		}
		if len(pageModels) == 0 || pageModels[len(pageModels)-1].ID == after {
			return nil, errors.New("claude model list: missing pagination cursor")
		}
		after = pageModels[len(pageModels)-1].ID
	}
}

// FetchCodexModelList lists every model visible to this OAuth account, including hidden models.
func FetchCodexModelList(ctx context.Context, auth *cliproxyauth.Auth, request UsageHTTPRequest) ([]cliproxyauth.UpstreamModel, error) {
	accountID, _ := auth.Metadata["account_id"].(string)
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil, errors.New("codex model list: missing account ID")
	}
	headers := codexUsageHeaders(accountID)
	headers.Set("Accept", "application/json")
	headers.Set("Originator", "codex_cli_rs")
	headers.Set("User-Agent", "codex_cli_rs/"+codexModelListClientVersion)
	endpoint := "https://chatgpt.com/backend-api/codex/models?client_version=" + codexModelListClientVersion
	body, errFetch := fetchUsageJSON(ctx, auth, request, endpoint, headers)
	if errFetch != nil {
		return nil, fmt.Errorf("codex model list: %w", errFetch)
	}
	var result struct {
		Models []json.RawMessage `json:"models"`
	}
	if errDecode := json.Unmarshal(body, &result); errDecode != nil || result.Models == nil {
		return nil, errors.New("codex model list: invalid models")
	}
	return upstreamModels(result.Models, "slug"), nil
}
