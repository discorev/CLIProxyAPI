package cliproxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// Model discovery registers models that an OAuth credential's upstream model
// list includes but the curated catalog section lacks, so a new model works
// before the catalog is updated. Once the catalog lists a model, the curated
// entry is used and the discovered one disappears.

// Defaults for discovered models when no catalog entry can be inherited.
const (
	discoveredClaudeContextLength       = 1000000
	discoveredClaudeMaxCompletionTokens = 128000
	discoveredClaudeThinkingMin         = 1024
	discoveredClaudeThinkingMax         = 128000
	discoveredCodexMaxCompletionTokens  = 128000
)

// claudeEffortLevels orders Claude effort levels from lowest to highest.
var claudeEffortLevels = []string{"low", "medium", "high", "xhigh", "max"}

const upstreamModelsFileName = "cliproxyapi-upstream-models.json"

func defaultUpstreamModelsPath() string {
	return filepath.Join(os.TempDir(), upstreamModelsFileName)
}

func modelDiscoveryProvider(provider string) bool {
	provider = strings.ToLower(strings.TrimSpace(provider))
	return provider == "claude" || provider == "codex"
}

// modelDiscoveryEnabled limits discovery to Claude and Codex OAuth credentials
// outside Home mode.
func (s *Service) modelDiscoveryEnabled(auth *coreauth.Auth) bool {
	return auth != nil && modelDiscoveryProvider(auth.Provider) && auth.AuthKind() == coreauth.AuthKindOAuth &&
		(s.cfg == nil || !s.cfg.Home.Enabled)
}

// codexCatalogModelsForPlan returns the Codex catalog section an OAuth plan registers from.
func codexCatalogModelsForPlan(planType string) []*ModelInfo {
	switch strings.ToLower(strings.TrimSpace(planType)) {
	case "plus":
		return registry.GetCodexPlusModels()
	case "team", "business", "go":
		return registry.GetCodexTeamModels()
	case "free":
		return registry.GetCodexFreeModels()
	default:
		return registry.GetCodexProModels()
	}
}

// discoveryCatalogSection returns the catalog section a discovery-enabled credential registers from.
func discoveryCatalogSection(auth *coreauth.Auth) []*ModelInfo {
	switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
	case "claude":
		return registry.GetClaudeModels()
	case "codex":
		return codexCatalogModelsForPlan(auth.Attributes["plan_type"])
	}
	return nil
}

func upstreamModelsKey(identity string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(identity)))
}

// upstreamModelsLocked returns the credential's last fetched upstream objects,
// falling back to the persisted copy until the first fetch succeeds.
// Callers hold restrictedAccess.mu.
func (s *Service) upstreamModelsLocked(auth *coreauth.Auth) []coreauth.UpstreamModel {
	identity := restrictedModelIdentity(auth)
	if identity == "" {
		return nil
	}
	if entry := s.restrictedAccess.entries[auth.ID]; entry != nil && entry.identity == identity && entry.upstream != nil {
		return entry.upstream
	}
	return s.restrictedAccess.persisted[upstreamModelsKey(identity)]
}

// discoveredModelsLocked synthesizes the credential's discovered models.
// Callers hold restrictedAccess.mu.
func (s *Service) discoveredModelsLocked(auth *coreauth.Auth, catalog []*ModelInfo) []*ModelInfo {
	if !s.modelDiscoveryEnabled(auth) {
		return nil
	}
	upstream := s.upstreamModelsLocked(auth)
	discovered := discoverableUpstreamModels(auth.Provider, catalog, upstream)
	if len(discovered) == 0 {
		return nil
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	if provider == "codex" {
		raws := make([]json.RawMessage, 0, len(discovered))
		for _, model := range discovered {
			raws = append(raws, model.Raw)
		}
		registry.RegisterDiscoveredCodexClientTemplates(raws)
	}
	return synthesizeDiscoveredModels(provider, catalog, upstream, discovered)
}

// discoverableUpstreamModels returns upstream objects whose IDs the catalog
// section lacks. Hidden Codex models and entries without an object are skipped.
func discoverableUpstreamModels(provider string, catalog []*ModelInfo, upstream []coreauth.UpstreamModel) []coreauth.UpstreamModel {
	if len(upstream) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(catalog)+len(upstream))
	for _, model := range catalog {
		if model != nil {
			seen[model.ID] = struct{}{}
		}
	}
	codex := strings.EqualFold(strings.TrimSpace(provider), "codex")
	var discovered []coreauth.UpstreamModel
	for _, model := range upstream {
		if _, ok := seen[model.ID]; ok || model.ID == "" || len(model.Raw) == 0 {
			continue
		}
		if codex {
			var object struct {
				Visibility string `json:"visibility"`
			}
			if json.Unmarshal(model.Raw, &object) != nil || strings.EqualFold(object.Visibility, "hide") {
				continue
			}
		}
		seen[model.ID] = struct{}{}
		discovered = append(discovered, model)
	}
	return discovered
}

func synthesizeDiscoveredModels(provider string, catalog []*ModelInfo, upstream, discovered []coreauth.UpstreamModel) []*ModelInfo {
	models := make([]*ModelInfo, 0, len(discovered))
	for _, model := range discovered {
		var info *ModelInfo
		switch provider {
		case "claude":
			info = synthesizeClaudeModel(model, catalog, upstream)
		case "codex":
			info = synthesizeCodexModel(model, catalog)
		}
		if info != nil {
			models = append(models, info)
		}
	}
	return models
}

type upstreamSupport struct {
	Supported bool `json:"supported"`
}

type claudeUpstreamModel struct {
	ID           string `json:"id"`
	DisplayName  string `json:"display_name"`
	CreatedAt    string `json:"created_at"`
	Line         string `json:"line"`
	Capabilities struct {
		Effort     map[string]json.RawMessage `json:"effort"`
		ImageInput upstreamSupport            `json:"image_input"`
		Thinking   *struct {
			Supported bool                       `json:"supported"`
			Types     map[string]upstreamSupport `json:"types"`
		} `json:"thinking"`
	} `json:"capabilities"`
}

func (m claudeUpstreamModel) created() int64 {
	created, errParse := time.Parse(time.RFC3339, m.CreatedAt)
	if errParse != nil {
		return 0
	}
	return created.Unix()
}

// effortLevels lists the supported effort levels from lowest to highest.
func (m claudeUpstreamModel) effortLevels() []string {
	var levels []string
	for _, level := range claudeEffortLevels {
		var support upstreamSupport
		if raw, ok := m.Capabilities.Effort[level]; ok && json.Unmarshal(raw, &support) == nil && support.Supported {
			levels = append(levels, level)
		}
	}
	return levels
}

// synthesizeClaudeModel clones the newest catalog model of the same line
// (by upstream created_at), or builds one from provider defaults.
func synthesizeClaudeModel(model coreauth.UpstreamModel, catalog []*ModelInfo, upstream []coreauth.UpstreamModel) *ModelInfo {
	var object claudeUpstreamModel
	if json.Unmarshal(model.Raw, &object) != nil {
		return nil
	}
	byID := make(map[string]*ModelInfo, len(catalog))
	for _, info := range catalog {
		if info != nil {
			byID[info.ID] = info
		}
	}
	var predecessor *ModelInfo
	var predecessorCreated time.Time
	for _, candidate := range upstream {
		info := byID[candidate.ID]
		if info == nil {
			continue
		}
		var other claudeUpstreamModel
		if json.Unmarshal(candidate.Raw, &other) != nil || other.Line == "" || other.Line != object.Line {
			continue
		}
		created, errParse := time.Parse(time.RFC3339, other.CreatedAt)
		if errParse != nil {
			continue
		}
		if predecessor == nil || created.After(predecessorCreated) {
			predecessor, predecessorCreated = info, created
		}
	}

	var info ModelInfo
	if predecessor != nil {
		info = cloneTemplateModel(predecessor)
		info.Description = ""
		info.Version = ""
	} else {
		info = claudeDefaultModel()
		if object.Capabilities.ImageInput.Supported {
			info.SupportedInputModalities = append(info.SupportedInputModalities, "image")
		}
		if object.Capabilities.Thinking != nil {
			info.Thinking.ZeroAllowed = object.Capabilities.Thinking.Types["disabled"].Supported
		}
	}
	info.ID = object.ID
	info.DisplayName = object.DisplayName
	info.Created = object.created()
	info.RestrictedAccess = false
	if levels := object.effortLevels(); len(levels) > 0 && info.Thinking != nil {
		info.Thinking.Levels = levels
	}
	if object.Capabilities.Thinking != nil && !object.Capabilities.Thinking.Supported {
		info.Thinking = nil
	}
	return &info
}

// cloneTemplateModel copies a catalog model so callers can change its thinking
// support without mutating the catalog.
func cloneTemplateModel(template *ModelInfo) ModelInfo {
	info := *template
	if template.Thinking != nil {
		thinking := *template.Thinking
		info.Thinking = &thinking
	}
	return info
}

// newestCatalogModel returns the matching catalog model created last (the
// first one on ties), or nil.
func newestCatalogModel(catalog []*ModelInfo, match func(*ModelInfo) bool) *ModelInfo {
	var newest *ModelInfo
	for _, info := range catalog {
		if info != nil && match(info) && (newest == nil || info.Created > newest.Created) {
			newest = info
		}
	}
	return newest
}

// claudeDefaultModel is the Claude entry used when no catalog model can be inherited.
func claudeDefaultModel() ModelInfo {
	return ModelInfo{
		Object:                    "model",
		OwnedBy:                   "anthropic",
		Type:                      "claude",
		ContextLength:             discoveredClaudeContextLength,
		MaxCompletionTokens:       discoveredClaudeMaxCompletionTokens,
		SupportedInputModalities:  []string{"text"},
		SupportedOutputModalities: []string{"text"},
		Thinking:                  &registry.ThinkingSupport{Min: discoveredClaudeThinkingMin, Max: discoveredClaudeThinkingMax},
	}
}

// codexTemplateModel clones the newest model of a Codex catalog section, or
// returns Codex defaults when the section is empty.
func codexTemplateModel(catalog []*ModelInfo) ModelInfo {
	template := newestCatalogModel(catalog, func(*ModelInfo) bool { return true })
	if template == nil {
		return ModelInfo{Object: "model", OwnedBy: "openai", Type: "openai", MaxCompletionTokens: discoveredCodexMaxCompletionTokens}
	}
	return cloneTemplateModel(template)
}

type codexUpstreamModel struct {
	Slug                     string   `json:"slug"`
	DisplayName              string   `json:"display_name"`
	Description              string   `json:"description"`
	ContextWindow            int      `json:"context_window"`
	InputModalities          []string `json:"input_modalities"`
	SupportedReasoningLevels []struct {
		Effort string `json:"effort"`
	} `json:"supported_reasoning_levels"`
}

// synthesizeCodexModel clones the newest catalog model of the section as
// defaults and overrides it with the upstream object's fields.
func synthesizeCodexModel(model coreauth.UpstreamModel, catalog []*ModelInfo) *ModelInfo {
	var object codexUpstreamModel
	if json.Unmarshal(model.Raw, &object) != nil {
		return nil
	}
	info := codexTemplateModel(catalog)
	info.ID = model.ID
	info.DisplayName = object.DisplayName
	info.Description = object.Description
	info.Version = model.ID
	info.Created = 0
	info.RestrictedAccess = false
	if object.ContextWindow > 0 {
		info.ContextLength = object.ContextWindow
	}
	var levels []string
	for _, level := range object.SupportedReasoningLevels {
		if effort := strings.TrimSpace(level.Effort); effort != "" {
			levels = append(levels, effort)
		}
	}
	if len(levels) > 0 {
		if info.Thinking == nil {
			info.Thinking = &registry.ThinkingSupport{}
		}
		info.Thinking.Levels = levels
	}
	if len(object.InputModalities) > 0 {
		info.SupportedInputModalities = slices.Clone(object.InputModalities)
	}
	return &info
}

func upstreamModelsEqual(a, b []coreauth.UpstreamModel) bool {
	return slices.EqualFunc(a, b, func(x, y coreauth.UpstreamModel) bool {
		return x.ID == y.ID && bytes.Equal(x.Raw, y.Raw)
	})
}

// reconcileDiscoveredModels reports whether the credential's discovered models
// changed between two upstream lists and logs gained or lost models.
func (s *Service) reconcileDiscoveredModels(auth *coreauth.Auth, previous, current []coreauth.UpstreamModel) bool {
	if !s.modelDiscoveryEnabled(auth) {
		return false
	}
	catalog := discoveryCatalogSection(auth)
	before := discoverableUpstreamModels(auth.Provider, catalog, previous)
	after := discoverableUpstreamModels(auth.Provider, catalog, current)
	if upstreamModelsEqual(before, after) {
		return false
	}
	beforeIDs := make(map[string]struct{}, len(before))
	for _, model := range before {
		beforeIDs[model.ID] = struct{}{}
	}
	var added []string
	for _, model := range after {
		if _, ok := beforeIDs[model.ID]; ok {
			delete(beforeIDs, model.ID)
		} else {
			added = append(added, model.ID)
		}
	}
	removed := make([]string, 0, len(beforeIDs))
	for _, model := range before {
		if _, ok := beforeIDs[model.ID]; ok {
			removed = append(removed, model.ID)
		}
	}
	if len(added) > 0 || len(removed) > 0 {
		log.WithFields(log.Fields{
			"provider": strings.ToLower(strings.TrimSpace(auth.Provider)),
			"count":    len(after),
			"added":    strings.Join(added, ","),
			"removed":  strings.Join(removed, ","),
		}).Info("discovered models changed")
	}
	return true
}

// loadPersistedUpstreamModels reads the best-effort upstream model copy keyed
// by hashed credential identity.
func loadPersistedUpstreamModels(path string) (map[string][]coreauth.UpstreamModel, error) {
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		if os.IsNotExist(errRead) {
			return nil, nil
		}
		return nil, errRead
	}
	var persisted map[string][]coreauth.UpstreamModel
	if errDecode := json.Unmarshal(data, &persisted); errDecode != nil {
		return nil, errDecode
	}
	return persisted, nil
}

func writePersistedUpstreamModels(path string, persisted map[string][]coreauth.UpstreamModel) error {
	data, errMarshal := json.Marshal(persisted)
	if errMarshal != nil {
		return errMarshal
	}
	file, errCreate := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if errCreate != nil {
		return errCreate
	}
	tmpPath := file.Name()
	_, errWrite := file.Write(data)
	errClose := file.Close()
	if errWrite == nil {
		errWrite = errClose
	}
	if errWrite == nil {
		errWrite = os.Chmod(tmpPath, 0o600)
	}
	if errWrite == nil {
		errWrite = os.Rename(tmpPath, path)
	}
	if errWrite != nil {
		_ = os.Remove(tmpPath)
	}
	return errWrite
}

// seedUpstreamModels loads the persisted copy so discovered models register
// before the first fetch. Seeded data never delays a fetch.
func (s *Service) seedUpstreamModels(path string) {
	persisted, errLoad := loadPersistedUpstreamModels(path)
	if errLoad != nil {
		log.Debugf("upstream model copy not loaded: %v", errLoad)
	}
	cache := &s.restrictedAccess
	cache.mu.Lock()
	cache.persistPath = path
	if persisted != nil {
		cache.persisted = persisted
	}
	cache.mu.Unlock()
}

// persistUpstreamModels records one credential's upstream objects and rewrites
// the copy. Errors are logged and otherwise ignored.
func (s *Service) persistUpstreamModels(identity string, upstream []coreauth.UpstreamModel) {
	cache := &s.restrictedAccess
	cache.persistMu.Lock()
	defer cache.persistMu.Unlock()
	cache.mu.Lock()
	path := cache.persistPath
	if path == "" {
		cache.mu.Unlock()
		return
	}
	if cache.persisted == nil {
		cache.persisted = make(map[string][]coreauth.UpstreamModel)
	}
	cache.persisted[upstreamModelsKey(identity)] = upstream
	snapshot := make(map[string][]coreauth.UpstreamModel, len(cache.persisted))
	for key, models := range cache.persisted {
		snapshot[key] = models
	}
	cache.mu.Unlock()
	if errWrite := writePersistedUpstreamModels(path, snapshot); errWrite != nil {
		log.Debugf("upstream model copy not written: %v", errWrite)
	}
}
