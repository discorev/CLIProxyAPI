package cliproxy

import (
	"context"
	"crypto/sha256"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	restrictedModelRefreshInterval = 6 * time.Hour
	// Unknown access hides restricted models, so failed fetches retry sooner.
	restrictedModelRetryInterval = 15 * time.Minute
)

type restrictedModelAccessEntry struct {
	identity     string
	listed       map[string]struct{}
	fetched      bool
	fetching     bool
	pending      atomic.Bool   // A changed grant or drift still needs a successful registration.
	appliedEpoch atomic.Uint64 // Distinguishes registry drift from a successfully filtered-out grant.
	done         chan struct{}
	nextAt       time.Time
}

type restrictedModelAccessCache struct {
	mu              sync.RWMutex
	registrationMu  sync.Mutex
	registrationSeq map[string]uint64
	nextSeq         uint64
	beforeCommit    func() // Test hook: pause after filtering but before the registry commit.
	beforePublish   func() // Test hook: observe a fetch before publishing its result.
	entries         map[string]*restrictedModelAccessEntry
	ctx             context.Context
	started         bool
	now             func() time.Time                 // Injectable for deterministic refresh tests.
	restrictedIDs   func(string) map[string]struct{} // Injectable catalog view for tests.
}

func (c *restrictedModelAccessCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
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

func (s *Service) currentModelRegistrationAuth(auth *coreauth.Auth) bool {
	if s.coreManager == nil {
		return true
	}
	current, ok := s.coreManager.GetByID(auth.ID)
	return ok && current != nil && !current.Disabled && strings.EqualFold(current.Provider, auth.Provider) &&
		current.AuthKind() == auth.AuthKind() && restrictedModelIdentity(current) == restrictedModelIdentity(auth)
}

func (s *Service) beginModelRegistration(auth *coreauth.Auth) uint64 {
	cache := &s.restrictedAccess
	cache.registrationMu.Lock()
	defer cache.registrationMu.Unlock()
	if !s.currentModelRegistrationAuth(auth) {
		return 0
	}
	cache.nextSeq++
	if cache.registrationSeq == nil {
		cache.registrationSeq = make(map[string]uint64)
	}
	cache.registrationSeq[auth.ID] = cache.nextSeq
	return cache.nextSeq
}

func (s *Service) commitModelRegistration(auth *coreauth.Auth, seq uint64, commit func()) {
	cache := &s.restrictedAccess
	cache.registrationMu.Lock()
	defer cache.registrationMu.Unlock()
	// The last-started current registration wins; checking and committing under
	// one lock prevents an older filtered snapshot from overwriting it.
	if cache.registrationSeq[auth.ID] != seq || !s.currentModelRegistrationAuth(auth) {
		return
	}
	commit()
}

// filterRestrictedModels uses catalog IDs as the authority for Claude and Codex,
// including plugin models that do not carry the catalog's RestrictedAccess flag.
func (s *Service) filterRestrictedModels(auth *coreauth.Auth, models []*ModelInfo) []*ModelInfo {
	if len(models) == 0 {
		return models
	}
	s.restrictedAccess.mu.RLock()
	defer s.restrictedAccess.mu.RUnlock()
	return s.filterRestrictedModelsLocked(auth, models)
}

// filterRestrictedModelsLocked must be called under the access cache's RLock.
// Registration holds that lock through RegisterClient: a revoked grant cannot
// be published between filtering and the registry commit. This path must never
// acquire the cache lock again; RegisterClient's model hook runs asynchronously.
func (s *Service) filterRestrictedModelsLocked(auth *coreauth.Auth, models []*ModelInfo) []*ModelInfo {
	if len(models) == 0 {
		return models
	}
	identity := restrictedModelIdentity(auth)
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	var restricted map[string]struct{}
	var explicit map[string]struct{}
	if provider == "claude" || provider == "codex" {
		restrictedIDs := s.restrictedAccess.restrictedIDs
		if restrictedIDs == nil {
			restrictedIDs = restrictedCatalogIDs
		}
		restricted = restrictedIDs(provider)
		explicit = s.explicitRestrictedModelIDs(auth)
	}
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
		_, catalogRestricted := restricted[model.ID]
		if (!catalogRestricted && !model.RestrictedAccess) || (provider != "claude" && provider != "codex" && model.UserDefined) {
			filtered = append(filtered, model)
			continue
		}
		if _, configured := explicit[model.ID]; configured {
			filtered = append(filtered, model)
			continue
		}
		if _, granted := listed[model.ID]; granted {
			filtered = append(filtered, model)
		}
	}
	return filtered
}

// Only a model explicitly named in an API key's own config.models list may
// bypass an unknown or denied OAuth grant. Plugin UserDefined metadata cannot.
func (s *Service) explicitRestrictedModelIDs(auth *coreauth.Auth) map[string]struct{} {
	if auth.AuthKind() != coreauth.AuthKindAPIKey {
		return nil
	}
	var models []*ModelInfo
	switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
	case "claude":
		if entry := s.resolveConfigClaudeKey(auth); entry != nil && len(entry.Models) > 0 {
			models = buildClaudeConfigModels(entry)
		}
	case "codex":
		if entry := s.resolveConfigCodexKey(auth); entry != nil && len(entry.Models) > 0 {
			models = buildCodexConfigModels(entry)
		}
	}
	ids := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model != nil {
			ids[model.ID] = struct{}{}
		}
	}
	return ids
}

func (s *Service) dropRestrictedModelAccess(id string) {
	s.restrictedAccess.mu.Lock()
	delete(s.restrictedAccess.entries, id)
	s.restrictedAccess.mu.Unlock()
}

func (s *Service) dropModelRegistrationSequence(id string) {
	s.restrictedAccess.registrationMu.Lock()
	delete(s.restrictedAccess.registrationSeq, id)
	s.restrictedAccess.registrationMu.Unlock()
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
	now := cache.clock()
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

// Registered aliases and prefixed routes retain their source in MetadataModelID.
func registeredRestrictedIDs(auth *coreauth.Auth, restricted map[string]struct{}) map[string]struct{} {
	registered := make(map[string]struct{})
	for _, model := range GlobalModelRegistry().GetModelsForClient(auth.ID) {
		if _, ok := restricted[model.ID]; ok {
			registered[model.ID] = struct{}{}
		}
		if _, ok := restricted[model.MetadataModelID]; ok {
			registered[model.MetadataModelID] = struct{}{}
		}
		if prefix := strings.TrimSpace(auth.Prefix); prefix != "" {
			if id, ok := strings.CutPrefix(model.ID, prefix+"/"); ok {
				if _, restrictedID := restricted[id]; restrictedID {
					registered[id] = struct{}{}
				}
			}
		}
	}
	return registered
}

// Call under the access cache's RLock, held through any fail-closed unregister.
func (s *Service) hasDeniedRegisteredRestrictedModelsLocked(auth *coreauth.Auth) bool {
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	if (provider != "claude" && provider != "codex") || restrictedModelIdentity(auth) == "" {
		return false
	}
	restrictedIDs := s.restrictedAccess.restrictedIDs
	if restrictedIDs == nil {
		restrictedIDs = restrictedCatalogIDs
	}
	restricted := restrictedIDs(provider)
	entry := s.restrictedAccess.entries[auth.ID]
	var granted map[string]struct{}
	if entry != nil && entry.fetched && entry.identity == restrictedModelIdentity(auth) {
		granted = grantedRestrictedIDs(entry.listed, restricted)
	}
	for id := range registeredRestrictedIDs(auth, restricted) {
		if _, ok := granted[id]; !ok {
			return true
		}
	}
	return false
}

// Call under the access cache's RLock, after a successful registry commit.
func (s *Service) markRestrictedModelRegistrationAppliedLocked(auth *coreauth.Auth) {
	if entry := s.restrictedAccess.entries[auth.ID]; entry != nil && entry.identity == restrictedModelIdentity(auth) {
		entry.appliedEpoch.Store(GlobalModelRegistry().ClientRegistrationEpoch(auth.ID))
		entry.pending.Store(false)
	}
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
	if cache.beforePublish != nil {
		cache.beforePublish()
	}
	cache.mu.Lock()
	if cache.entries[auth.ID] != entry {
		cache.mu.Unlock() // Removed or replaced credentials cannot publish stale grants.
		return
	}
	if errFetch != nil {
		entry.nextAt = cache.clock().Add(restrictedModelRetryInterval)
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
	// The first fetch, a grant change, or drift in the actual registration
	// requires reconciliation. A failed plugin discovery leaves pending set so
	// the next successful fetch retries even after fail-closing the registration.
	changed := !entry.fetched || !maps.Equal(previous, granted)
	registered := registeredRestrictedIDs(auth, restricted)
	drifted := false
	for id := range registered {
		if _, allowed := granted[id]; !allowed {
			drifted = true // Never preserve a denied registration.
			break
		}
	}
	if !drifted && !maps.Equal(registered, granted) {
		// A successfully applied registration can omit a granted model due to
		// plan or config exclusions. Retry only if that registration changed.
		applied := entry.appliedEpoch.Load()
		drifted = applied == 0 || applied != GlobalModelRegistry().ClientRegistrationEpoch(auth.ID)
	}
	needsRefresh := changed || drifted || entry.pending.Load()
	entry.listed = listed
	entry.fetched = true
	if needsRefresh {
		entry.pending.Store(true)
	}
	cache.mu.Unlock()
	if !needsRefresh {
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
	if len(changedIDs) > 0 {
		modelIDs := slices.Sorted(maps.Keys(changedIDs))
		log.WithFields(log.Fields{"auth_id": auth.ID, "model_ids": strings.Join(modelIDs, ",")}).Info("restricted model access changed")
	}
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
