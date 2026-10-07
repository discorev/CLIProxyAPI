package cliproxy

import (
	"context"
	"crypto/sha256"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const restrictedModelRefreshInterval = 6 * time.Hour

type restrictedModelAccessEntry struct {
	identity string
	listed   map[string]struct{}
	fetched  bool
	fetching bool
	done     chan struct{}
	nextAt   time.Time
}

type restrictedModelAccessCache struct {
	mu            sync.RWMutex
	entries       map[string]*restrictedModelAccessEntry
	ctx           context.Context
	started       bool
	now           func() time.Time                 // Injectable for deterministic refresh tests.
	restrictedIDs func(string) map[string]struct{} // Injectable catalog view for tests.
}

func restrictedModelIdentity(auth *coreauth.Auth) string {
	if auth == nil || auth.AuthKind() != coreauth.AuthKindOAuth || !coreauth.UsageFetchable(auth) {
		return ""
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	if provider == "codex" {
		if accountID, _ := auth.Metadata["account_id"].(string); strings.TrimSpace(accountID) != "" {
			return provider + ":" + strings.TrimSpace(accountID)
		}
	} else if provider == "claude" {
		accountUUID, _ := auth.Metadata["account_uuid"].(string)
		organizationUUID, _ := auth.Metadata["organization_uuid"].(string)
		if strings.TrimSpace(accountUUID) != "" && strings.TrimSpace(organizationUUID) != "" {
			return fmt.Sprintf("%s:%s:%s", provider, strings.TrimSpace(accountUUID), strings.TrimSpace(organizationUUID))
		}
	}
	// Legacy OAuth credentials without stable account metadata must not inherit
	// grants from a different token later loaded under the same credential ID.
	token, _ := auth.Metadata["access_token"].(string)
	return fmt.Sprintf("%s:%x", provider, sha256.Sum256([]byte(token)))
}

// filterRestrictedModels applies only to catalog entries. Explicit API-key models
// are created independently and retain their existing configuration behavior.
func (s *Service) filterRestrictedModels(auth *coreauth.Auth, models []*ModelInfo) []*ModelInfo {
	if len(models) == 0 {
		return models
	}
	identity := restrictedModelIdentity(auth)
	s.restrictedAccess.mu.RLock()
	entry := s.restrictedAccess.entries[auth.ID]
	var listed map[string]struct{}
	if entry != nil && entry.fetched && entry.identity == identity && identity != "" {
		listed = entry.listed
	}
	filtered := make([]*ModelInfo, 0, len(models))
	for _, model := range models {
		if model == nil {
			continue
		}
		if !model.RestrictedAccess || model.UserDefined {
			filtered = append(filtered, model)
			continue
		}
		if _, ok := listed[model.ID]; ok {
			filtered = append(filtered, model)
		}
	}
	s.restrictedAccess.mu.RUnlock()
	return filtered
}

func (s *Service) dropRestrictedModelAccess(id string) {
	s.restrictedAccess.mu.Lock()
	delete(s.restrictedAccess.entries, id)
	s.restrictedAccess.mu.Unlock()
}

// queueRestrictedModelFetch never waits for the upstream API. Each auth has at
// most one in-flight fetch and its last successful list survives fetch errors.
func (s *Service) queueRestrictedModelFetch(ctx context.Context, auth *coreauth.Auth) {
	if s == nil || auth == nil || auth.ID == "" || auth.Disabled {
		return
	}
	identity := restrictedModelIdentity(auth)
	if identity == "" {
		s.dropRestrictedModelAccess(auth.ID)
		return
	}
	if s.coreManager == nil {
		return
	}
	executor, ok := s.coreManager.Executor(auth.Provider)
	if !ok {
		return
	}
	lister, ok := executor.(coreauth.UpstreamModelLister)
	if !ok {
		return
	}
	cache := &s.restrictedAccess
	cache.mu.Lock()
	if cache.entries == nil {
		cache.entries = make(map[string]*restrictedModelAccessEntry)
	}
	entry := cache.entries[auth.ID]
	if entry == nil || entry.identity != identity {
		entry = &restrictedModelAccessEntry{identity: identity}
		cache.entries[auth.ID] = entry
	}
	now := time.Now()
	if cache.now != nil {
		now = cache.now()
	}
	if entry.fetching || now.Before(entry.nextAt) {
		cache.mu.Unlock()
		return
	}
	entry.fetching = true
	entry.done = make(chan struct{})
	entry.nextAt = now.Add(restrictedModelRefreshInterval)
	fetchCtx := cache.ctx
	if fetchCtx == nil {
		if ctx == nil {
			ctx = context.Background()
		}
		fetchCtx = context.WithoutCancel(ctx)
	}
	cache.mu.Unlock()
	go s.fetchRestrictedModels(fetchCtx, auth.Clone(), lister, entry)
}

func restrictedCatalogIDs(provider string) map[string]struct{} {
	var models []*registry.ModelInfo
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude":
		models = registry.GetClaudeModels()
	case "codex":
		models = append(models, registry.GetCodexFreeModels()...)
		models = append(models, registry.GetCodexTeamModels()...)
		models = append(models, registry.GetCodexPlusModels()...)
		models = append(models, registry.GetCodexProModels()...)
	}
	ids := make(map[string]struct{})
	for _, model := range models {
		if model != nil && model.RestrictedAccess {
			ids[model.ID] = struct{}{}
		}
	}
	return ids
}

func grantedRestrictedIDs(listed, restricted map[string]struct{}) map[string]struct{} {
	granted := make(map[string]struct{})
	for id := range restricted {
		if _, ok := listed[id]; ok {
			granted[id] = struct{}{}
		}
	}
	return granted
}

func (s *Service) fetchRestrictedModels(ctx context.Context, auth *coreauth.Auth, lister coreauth.UpstreamModelLister, entry *restrictedModelAccessEntry) {
	done := entry.done
	defer func() {
		s.restrictedAccess.mu.Lock()
		if s.restrictedAccess.entries[auth.ID] == entry {
			entry.fetching = false
		}
		s.restrictedAccess.mu.Unlock()
		if done != nil {
			close(done)
		}
	}()
	ids, errFetch := lister.ListUpstreamModels(ctx, auth)
	listed := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			listed[id] = struct{}{}
		}
	}
	cache := &s.restrictedAccess
	cache.mu.Lock()
	if cache.entries[auth.ID] != entry {
		cache.mu.Unlock() // Removed or replaced credentials cannot publish stale grants.
		return
	}
	if errFetch != nil {
		cache.mu.Unlock()
		if ctx.Err() == nil {
			log.WithField("auth_id", auth.ID).Warn("restricted model list fetch failed")
		}
		return
	}
	restrictedIDs := cache.restrictedIDs
	if restrictedIDs == nil {
		restrictedIDs = restrictedCatalogIDs
	}
	restricted := restrictedIDs(auth.Provider)
	previous := grantedRestrictedIDs(entry.listed, restricted)
	granted := grantedRestrictedIDs(listed, restricted)
	changed := !maps.Equal(previous, granted)
	entry.listed = listed
	entry.fetched = true
	cache.mu.Unlock()
	if !changed {
		return
	}
	changedIDs := make(map[string]struct{})
	for id := range previous {
		if _, ok := granted[id]; !ok {
			changedIDs[id] = struct{}{}
		}
	}
	for id := range granted {
		if _, ok := previous[id]; !ok {
			changedIDs[id] = struct{}{}
		}
	}
	modelIDs := slices.Sorted(maps.Keys(changedIDs))
	log.WithFields(log.Fields{"auth_id": auth.ID, "model_ids": strings.Join(modelIDs, ",")}).Info("restricted model access changed")
	s.refreshModelRegistrationForAuth(auth)
}

func (s *Service) startRestrictedModelRefresh(ctx context.Context) {
	cache := &s.restrictedAccess
	cache.mu.Lock()
	if cache.started {
		cache.mu.Unlock()
		return
	}
	cache.started = true
	cache.ctx = ctx
	cache.mu.Unlock()
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.sweepRestrictedModelAccess(ctx)
			}
		}
	}()
}

func (s *Service) sweepRestrictedModelAccess(ctx context.Context) {
	if s.coreManager == nil {
		return
	}
	for _, auth := range s.coreManager.List() {
		s.queueRestrictedModelFetch(ctx, auth)
	}
}
