package cliproxy

import (
	"context"
	"crypto/sha256"
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const restrictedModelRefreshInterval = 15 * time.Minute

type restrictedModelAccessEntry struct {
	identity string
	listed   map[string]struct{}
	upstream []coreauth.UpstreamModel // Objects from the last successful fetch; nil until one succeeds.
	applied  map[string]struct{}      // Grants from the last reconciliation registration.
	fetching bool
	done     chan struct{}
	nextAt   time.Time
}

type restrictedModelAccessCache struct {
	mu            sync.RWMutex
	entries       map[string]*restrictedModelAccessEntry
	ctx           context.Context
	started       bool
	now           func() time.Time                 // Injectable clock for refresh tests.
	restrictedIDs func(string) map[string]struct{} // Injectable catalog view for tests.
	// persisted seeds model discovery across restarts, keyed by hashed identity.
	persisted   map[string][]coreauth.UpstreamModel
	persistPath string // Empty disables the best-effort copy.
	persistMu   sync.Mutex
}

func (c *restrictedModelAccessCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func restrictedModelIdentity(auth *coreauth.Auth) string {
	if auth != nil && strings.EqualFold(strings.TrimSpace(auth.Provider), "meta") {
		if auth.AuthKind() != coreauth.AuthKindOAuth {
			return ""
		}
		if email, _ := auth.Metadata["email"].(string); strings.TrimSpace(email) != "" {
			return "meta:" + strings.ToLower(strings.TrimSpace(email))
		}
		// Minted API keys rotate; the DCA token identifies the OAuth account.
		token, _ := auth.Metadata["dca_token"].(string)
		token = strings.TrimSpace(token)
		if token == "" {
			accessToken, _ := auth.Metadata["access_token"].(string)
			if accessToken = strings.TrimSpace(accessToken); strings.HasPrefix(accessToken, "dca:") {
				token = accessToken
			}
		}
		if token == "" {
			return ""
		}
		return fmt.Sprintf("meta:%x", sha256.Sum256([]byte(token)))
	}
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

// Catalog IDs remain authoritative even if a plugin omits RestrictedAccess.
// MetadataModelID is the upstream name for configured aliases and prefixed routes.
func restrictedModelID(model *ModelInfo, restricted map[string]struct{}) string {
	if _, ok := restricted[model.MetadataModelID]; ok {
		return model.MetadataModelID
	}
	if _, ok := restricted[model.ID]; ok {
		return model.ID
	}
	return ""
}

func (s *Service) filterRestrictedModels(auth *coreauth.Auth, models []*ModelInfo) []*ModelInfo {
	s.restrictedAccess.mu.RLock()
	defer s.restrictedAccess.mu.RUnlock()
	return s.filterRestrictedModelsLocked(auth, models)
}

// Registration callers hold restrictedAccess.mu.RLock through the registry commit:
// old grants cannot commit after a new list publishes.
func (s *Service) filterRestrictedModelsLocked(auth *coreauth.Auth, models []*ModelInfo) []*ModelInfo {
	if len(models) == 0 {
		return models
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	restricted := s.restrictedCatalogIDsLocked(provider)
	explicit := s.explicitRestrictedModelIDs(auth)
	var listed map[string]struct{}
	if identity := restrictedModelIdentity(auth); identity != "" {
		if entry := s.restrictedAccess.entries[auth.ID]; entry != nil && entry.identity == identity {
			listed = entry.listed
		}
	}
	filtered := make([]*ModelInfo, 0, len(models))
	for _, model := range models {
		if model == nil {
			continue
		}
		id := restrictedModelID(model, restricted)
		if !restrictedCatalogProvider(provider) && model.RestrictedAccess && !model.UserDefined {
			id = model.ID
		}
		if id == "" {
			filtered = append(filtered, model)
			continue
		}
		// API-key exceptions require the configured upstream name, not merely
		// a coincidentally matching alias or plugin UserDefined metadata.
		upstream := model.MetadataModelID
		if upstream == "" {
			upstream = model.ID
		}
		if _, ok := explicit[upstream]; ok {
			filtered = append(filtered, model)
		} else if _, ok := listed[id]; ok {
			filtered = append(filtered, model)
		}
	}
	return filtered
}

// restrictedCatalogIDsLocked returns the provider's restricted catalog IDs.
// Callers hold restrictedAccess.mu.
func (s *Service) restrictedCatalogIDsLocked(provider string) map[string]struct{} {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if !restrictedCatalogProvider(provider) {
		return nil
	}
	restrictedIDs := s.restrictedAccess.restrictedIDs
	if restrictedIDs == nil {
		restrictedIDs = restrictedCatalogIDs
	}
	return restrictedIDs(provider)
}

func (s *Service) explicitRestrictedModelIDs(auth *coreauth.Auth) map[string]struct{} {
	if auth.AuthKind() != coreauth.AuthKindAPIKey {
		return definedModelIDs(auth)
	}
	var names []string
	switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
	case "claude":
		if entry := s.resolveConfigClaudeKey(auth); entry != nil {
			for _, model := range entry.Models {
				names = append(names, model.Name)
			}
		}
	case "codex":
		if entry := s.resolveConfigCodexKey(auth); entry != nil {
			for _, model := range entry.Models {
				names = append(names, model.Name)
			}
		}
	case "meta":
		if entry := s.resolveConfigMetaKey(auth); entry != nil {
			for _, model := range entry.Models {
				names = append(names, model.Name)
			}
		}
	}
	ids := make(map[string]struct{}, len(names))
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			ids[name] = struct{}{}
		}
	}
	return ids
}

func (s *Service) dropRestrictedModelAccess(id string) {
	s.restrictedAccess.mu.Lock()
	delete(s.restrictedAccess.entries, id)
	s.restrictedAccess.mu.Unlock()
}

// A credential has one in-flight fetch. Registration and other credentials never
// wait on the network; a failed fetch retains the last known model list.
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
	if entry.fetching || cache.clock().Before(entry.nextAt) {
		cache.mu.Unlock()
		return
	}
	entry.fetching = true
	entry.done = make(chan struct{})
	entry.nextAt = cache.clock().Add(restrictedModelRefreshInterval)
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

func restrictedCatalogProvider(provider string) bool {
	switch provider {
	case "claude", "codex", "meta":
		return true
	default:
		return false
	}
}

func restrictedCatalogIDs(provider string) map[string]struct{} {
	var models []*registry.ModelInfo
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude":
		models = registry.GetClaudeModels()
	case "meta":
		models = registry.GetMetaModels()
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

// Read actual registration rather than a cached registration epoch. Routes of
// auth-file upstream_id entries are registered without a grant, so they do not
// count as registering their upstream model.
func registeredRestrictedIDs(auth *coreauth.Auth, restricted map[string]struct{}) map[string]struct{} {
	registered := make(map[string]struct{})
	routes := definedRouteIDs(auth)
	prefix := strings.TrimSpace(auth.Prefix)
	for _, model := range GlobalModelRegistry().GetModelsForClient(auth.ID) {
		local := model.ID
		if prefix != "" {
			local = strings.TrimPrefix(local, prefix+"/")
		}
		if _, ok := routes[strings.ToLower(local)]; ok {
			continue
		}
		if id := restrictedModelID(model, restricted); id != "" {
			registered[id] = struct{}{}
		}
		if prefix != "" {
			if id, ok := strings.CutPrefix(model.ID, prefix+"/"); ok {
				if _, restrictedID := restricted[id]; restrictedID {
					registered[id] = struct{}{}
				}
			}
		}
	}
	return registered
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
	// Listers may refresh credentials in place (Meta mints its API key and learns
	// the account email), which would change the identity checked below.
	models, errFetch := lister.ListUpstreamModels(ctx, auth.Clone())
	listed := make(map[string]struct{}, len(models))
	upstream := make([]coreauth.UpstreamModel, 0, len(models))
	for _, model := range models {
		if model.ID = strings.TrimSpace(model.ID); model.ID != "" {
			listed[model.ID] = struct{}{}
			upstream = append(upstream, model)
		}
	}
	cache := &s.restrictedAccess
	cache.mu.Lock()
	if cache.entries[auth.ID] != entry || !s.currentModelRegistrationAuth(auth) {
		cache.mu.Unlock()
		return
	}
	if errFetch != nil {
		cache.mu.Unlock()
		if ctx.Err() == nil {
			log.WithField("auth_id", auth.ID).Warn("restricted model list fetch failed")
		}
		return
	}
	previous := s.upstreamModelsLocked(auth)
	entry.listed = listed
	entry.upstream = upstream
	cache.mu.Unlock()
	discoveryChanged := s.reconcileDiscoveredModels(auth, previous, upstream)
	if s.modelDiscoveryEnabled(auth) && !accessProgramVariantsEqual(previous, upstream) {
		discoveryChanged = true
	}
	if s.modelDiscoveryEnabled(auth) && !upstreamModelsEqual(previous, upstream) {
		s.persistUpstreamModels(restrictedModelIdentity(auth), upstream)
	}

	restrictedIDs := cache.restrictedIDs
	if restrictedIDs == nil {
		restrictedIDs = restrictedCatalogIDs
	}
	restricted := restrictedIDs(auth.Provider)
	// Defined models are registered without a grant, so they never need a refresh.
	if defined := s.explicitRestrictedModelIDs(auth); len(defined) > 0 {
		restricted = maps.Clone(restricted)
		for id := range defined {
			delete(restricted, id)
		}
	}
	granted := make(map[string]struct{})
	for id := range restricted {
		if _, ok := listed[id]; ok {
			granted[id] = struct{}{}
		}
	}
	cache.mu.RLock()
	applied := entry.applied
	cache.mu.RUnlock()
	registered := registeredRestrictedIDs(auth, restricted)
	// Denied registrations are always repaired; missing grants only require a
	// refresh when the upstream grant set differs from the last applied one.
	denied := false
	for id := range registered {
		if _, ok := granted[id]; !ok {
			denied = true
			break
		}
	}
	// A matching registry is already reconciled, even when no registration ran.
	registryMatches := maps.Equal(registered, granted)
	// A changed discovered set re-registers even when grants are reconciled.
	grantRefresh := !registryMatches && (denied || !maps.Equal(applied, granted))
	refreshed := (grantRefresh || discoveryChanged) && s.refreshModelRegistrationForAuth(auth)
	if registryMatches || (grantRefresh && refreshed) {
		cache.mu.Lock()
		if cache.entries[auth.ID] == entry && maps.Equal(entry.listed, listed) {
			entry.applied = granted
		}
		cache.mu.Unlock()
	}
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
	seed := cache.persistPath == ""
	cache.mu.Unlock()
	if seed {
		s.seedUpstreamModels(defaultUpstreamModelsPath())
	}
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
		if !auth.Disabled {
			s.queueRestrictedModelFetch(ctx, auth)
		}
	}
}
