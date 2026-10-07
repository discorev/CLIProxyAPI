package helps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const codexModelListClientVersion = "99.0.0"

// FetchClaudeModelList lists every model visible to this OAuth credential.
func FetchClaudeModelList(ctx context.Context, auth *cliproxyauth.Auth, request UsageHTTPRequest) ([]string, error) {
	headers := claudeUsageHeaders()
	headers.Set("Anthropic-Version", "2023-06-01")
	models := make([]string, 0)
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
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
			HasMore bool `json:"has_more"`
		}
		if errDecode := json.Unmarshal(body, &page); errDecode != nil || page.Data == nil {
			return nil, errors.New("claude model list: invalid data")
		}
		for _, model := range page.Data {
			if id := strings.TrimSpace(model.ID); id != "" {
				models = append(models, id)
			}
		}
		if !page.HasMore {
			return models, nil
		}
		if len(page.Data) == 0 || strings.TrimSpace(page.Data[len(page.Data)-1].ID) == after {
			return nil, errors.New("claude model list: missing pagination cursor")
		}
		after = strings.TrimSpace(page.Data[len(page.Data)-1].ID)
	}
}

// FetchCodexModelList lists every model visible to this OAuth account, including hidden models.
func FetchCodexModelList(ctx context.Context, auth *cliproxyauth.Auth, request UsageHTTPRequest) ([]string, error) {
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
		Models []struct {
			Slug string `json:"slug"`
		} `json:"models"`
	}
	if errDecode := json.Unmarshal(body, &result); errDecode != nil || result.Models == nil {
		return nil, errors.New("codex model list: invalid models")
	}
	models := make([]string, 0, len(result.Models))
	for _, model := range result.Models {
		if slug := strings.TrimSpace(model.Slug); slug != "" {
			models = append(models, slug)
		}
	}
	return models, nil
}
