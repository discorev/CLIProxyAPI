package auth

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestUsageFetchableProviderCredentials(t *testing.T) {
	for _, tt := range []struct {
		name, provider, kind string
		attributes           map[string]string
		metadata             map[string]any
		want                 bool
	}{
		{"meta login", "meta", AuthKindOAuth, map[string]string{"auth_kind": "oauth", "api_key": "fake-minted"}, map[string]any{"auth_kind": "oauth", "access_token": "fake-minted", "dca_token": "fake-dca"}, true},
		{"meta refreshed", "meta", AuthKindOAuth, map[string]string{"api_key": "fake-new-minted"}, map[string]any{"auth_kind": "oauth", "access_token": "fake-new-minted", "dca_token": "fake-dca"}, true},
		{"meta file reloaded", "meta", AuthKindOAuth, map[string]string{"auth_kind": "oauth", "source": "fixture.json"}, map[string]any{"api_key": "fake-minted", "access_token": "fake-minted", "dca_token": "fake-dca"}, true},
		{"meta dca only", " META ", AuthKindOAuth, nil, map[string]any{"auth_kind": "oauth", "dca_token": "fake-dca"}, true},
		{"meta config key", "meta", AuthKindAPIKey, map[string]string{"auth_kind": "apikey", "source": "config:meta", "api_key": "fake-key"}, nil, false},
		{"meta config with dca", "meta", AuthKindAPIKey, map[string]string{"auth_kind": "apikey", "api_key": "fake-key"}, map[string]any{"dca_token": "fake-dca"}, false},
		{"meta missing dca", "meta", AuthKindOAuth, nil, map[string]any{"auth_kind": "oauth", "access_token": "fake-key"}, false},
		{"meta blank dca", "meta", AuthKindOAuth, nil, map[string]any{"auth_kind": "oauth", "dca_token": " "}, false},
		{"xai oauth", " XAI ", AuthKindOAuth, nil, map[string]any{"auth_kind": "oauth", "access_token": "fake-oauth"}, true},
		{"xai config key", "xai", AuthKindAPIKey, map[string]string{"auth_kind": "apikey", "source": "config:xai", "api_key": "fake-key"}, nil, false},
		{"xai key plus oauth", "xai", AuthKindOAuth, map[string]string{"api_key": "fake-key"}, map[string]any{"auth_kind": "oauth", "access_token": "fake-oauth"}, false},
		{"xai empty token", "xai", AuthKindOAuth, nil, map[string]any{"auth_kind": "oauth", "access_token": " "}, false},
		{"claude oauth", "claude", AuthKindOAuth, nil, map[string]any{"access_token": "fake"}, true},
		{"codex oauth", "codex", AuthKindOAuth, nil, map[string]any{"access_token": "fake"}, true},
		{"claude key plus oauth", "claude", AuthKindAPIKey, map[string]string{"api_key": "fake"}, map[string]any{"access_token": "fake"}, false},
		{"codex key plus oauth", "codex", AuthKindAPIKey, map[string]string{"api_key": "fake"}, map[string]any{"access_token": "fake"}, false},
		{"unknown provider", "other", AuthKindOAuth, nil, map[string]any{"access_token": "fake"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			auth := &Auth{Provider: tt.provider, Attributes: tt.attributes, Metadata: tt.metadata}
			if got := auth.AuthKind(); got != tt.kind {
				t.Fatalf("kind=%q want=%q", got, tt.kind)
			}
			if got := UsageFetchable(auth); got != tt.want {
				t.Fatalf("fetchable=%v want=%v", got, tt.want)
			}
		})
	}
	if UsageFetchable(nil) {
		t.Fatal("nil auth fetchable")
	}
}

func TestResetSweepExcludesMetaAndXAI(t *testing.T) {
	for _, provider := range []string{"meta", "xai"} {
		for _, dryRun := range []bool{false, true} {
			t.Run(provider+map[bool]string{false: "/apply", true: "/dry-run"}[dryRun], func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					manager, executor, _ := setupResetManager(t, provider)
					manager.SetConfig(&config.Config{ResetCredits: config.ResetCreditsConfig{AutoApply: !dryRun, DryRun: dryRun}})
					auth := registerUsageAuth(t, manager, "a", provider)
					if !UsageFetchable(auth) {
						t.Fatal("test requires fetchable OAuth")
					}
					manager.StartResetLoop()
					synctest.Wait()
					if executor.fetches.Load() != 0 || executor.calls.Load() != 0 {
						t.Fatal("reset loop fetched or applied for unsupported provider")
					}
					manager.StopResetLoop()
					synctest.Wait()
				})
			})
		}
	}
}

func TestNewUsageProviderStableIdentity(t *testing.T) {
	for _, provider := range []string{"meta", "xai"} {
		before := &Auth{Provider: provider, Metadata: map[string]any{"email": "first@example.test", "sub": "first", "access_token": "old", "dca_token": "old"}}
		after := before.Clone()
		after.Metadata["access_token"], after.Metadata["dca_token"] = "new", "new"
		if usageAccountChanged(before, after) {
			t.Fatal("rotating credentials invalidated usage")
		}
		after.Metadata["email"] = "second@example.test"
		if !usageAccountChanged(before, after) {
			t.Fatal("account replacement did not invalidate usage")
		}
		if provider == "xai" {
			after = before.Clone()
			after.Metadata["sub"] = "second"
			if !usageAccountChanged(before, after) {
				t.Fatal("subject replacement did not invalidate usage")
			}
		}
	}
}

func TestIntelligentFillMetaAndXAIUnknownFallback(t *testing.T) {
	for _, provider := range []string{"meta", "xai"} {
		manager := NewManager(nil, nil, nil)
		now := time.Unix(1800000000, 0)
		manager.usage.now = func() time.Time { return now }
		manager.usage.entries = map[string]*usageEntry{
			"exhausted": {CredentialUsage: CredentialUsage{Windows: []UsageWindow{{Kind: "7d", Length: 604800, UsedPercent: 100, ResetsAt: now.Add(time.Hour)}}}},
			"unknown":   {CredentialUsage: CredentialUsage{Windows: []UsageWindow{}}},
		}
		selector := NewIntelligentFillSelector(manager)
		auths := []*Auth{{ID: "exhausted", Provider: provider}, {ID: "unknown", Provider: provider}}
		got, err := selector.Pick(context.Background(), provider, "model", cliproxyexecutor.Options{}, auths)
		if err != nil || got == nil || got.ID != "unknown" {
			t.Fatalf("unknown fallback not selected: %v", err)
		}
		manager.usage.entries["exhausted"].Windows[0].UsedPercent = 10
		got, err = selector.Pick(context.Background(), provider, "model", cliproxyexecutor.Options{}, auths)
		if err != nil || got == nil || got.ID != "exhausted" {
			t.Fatalf("known capacity not preferred: %v", err)
		}
	}
}
