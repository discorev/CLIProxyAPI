package cliproxy

import (
	"encoding/json"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/modelconfig"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// Model definitions let users define or override an OAuth credential's models:
// provider-wide through oauth.settings and per credential through the auth
// file's "models" key. Per field, the auth file wins over settings, settings
// over the catalog, and the catalog over discovery.

// applyModelDefinitionsForAuth adds the models that settings or the auth file
// define but the credential does not resolve, then applies settings and auth
// file overrides in that order. Excluded models are never defined.
func applyModelDefinitionsForAuth(cfg *config.Config, auth *coreauth.Auth, provider string, excluded []string, models []*ModelInfo) []*ModelInfo {
	if auth == nil {
		return models
	}
	channel := coreauth.OAuthModelAliasChannel(provider, auth.AuthKind())
	if channel == "" {
		return models
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	settings := oauthSettingsForAuth(cfg, channel)
	definitions := coreauth.ModelDefinitionsFromAttributes(auth.Attributes)
	if len(settings) == 0 && len(definitions) == 0 {
		return models
	}
	catalog := discoveryCatalogSection(auth)
	for _, setting := range settings {
		if setting.Alias == "" && setting.DefinesModel() {
			models = appendDefinedModel(provider, catalog, excluded, models, setting.Name, "")
		}
	}
	for _, definition := range definitions {
		models = appendDefinedModel(provider, catalog, excluded, models, definition.ID, definition.UpstreamID)
	}
	models = applyOAuthSettingEntries(settings, models)
	if len(definitions) == 0 {
		return models
	}
	out := make([]*ModelInfo, 0, len(models))
	for _, model := range models {
		if model == nil {
			continue
		}
		// Overrides of the upstream model apply first, so the model's own ID wins.
		for _, key := range []string{model.MetadataModelID, model.ID} {
			if key == "" || (key == model.MetadataModelID && strings.EqualFold(key, model.ID)) {
				continue
			}
			for _, definition := range definitions {
				if strings.EqualFold(definition.ID, key) {
					model = overlayModelDefinition(model, definition.Raw)
				}
			}
		}
		out = append(out, model)
	}
	return out
}

// definedModelIDs lists the upstream model IDs an OAuth credential's
// definitions opt in to, bypassing restricted-access filtering.
func definedModelIDs(cfg *config.Config, auth *coreauth.Auth) map[string]struct{} {
	channel := coreauth.OAuthModelAliasChannel(auth.Provider, auth.AuthKind())
	if channel == "" {
		return nil
	}
	ids := make(map[string]struct{})
	for _, setting := range oauthSettingsForAuth(cfg, channel) {
		if setting.Alias == "" && setting.DefinesModel() {
			ids[setting.Name] = struct{}{}
		}
	}
	for _, definition := range coreauth.ModelDefinitionsFromAttributes(auth.Attributes) {
		ids[definition.ID] = struct{}{}
		if definition.UpstreamID != "" {
			ids[definition.UpstreamID] = struct{}{}
		}
	}
	return ids
}

// appendDefinedModel adds a model the credential does not resolve yet, with
// fields inherited from the provider's catalog or defaults.
func appendDefinedModel(provider string, catalog []*ModelInfo, excluded []string, models []*ModelInfo, id, upstreamID string) []*ModelInfo {
	id = strings.TrimSpace(id)
	if id == "" {
		return models
	}
	for _, model := range models {
		if model != nil && (strings.EqualFold(model.ID, id) || strings.EqualFold(model.MetadataModelID, id)) {
			return models
		}
	}
	upstream := upstreamID
	if upstream == "" {
		upstream = id
	}
	if len(applyExcludedModels([]*ModelInfo{{ID: id}, {ID: upstream}}, excluded)) < 2 {
		return models
	}
	info := definedModelBase(provider, catalog, models, upstream)
	info.ID = id
	info.MetadataModelID = upstreamID
	return append(models, &info)
}

// definedModelBase returns the entry a newly defined model inherits missing
// fields from: for Claude the newest model of the same family (claude-<line>-)
// in the catalog section, for Codex the discovery template, and otherwise only
// the provider's object, owner and type.
func definedModelBase(provider string, catalog []*ModelInfo, models []*ModelInfo, upstreamID string) ModelInfo {
	switch provider {
	case "claude":
		if prefix := claudeFamilyPrefix(upstreamID); prefix != "" {
			predecessor := newestCatalogModel(catalog, func(info *ModelInfo) bool {
				return strings.HasPrefix(strings.ToLower(info.ID), prefix)
			})
			if predecessor != nil {
				info := cloneTemplateModel(predecessor)
				info.DisplayName, info.Description, info.Version, info.Created = "", "", "", 0
				info.RestrictedAccess = false
				return info
			}
		}
		return claudeDefaultModel()
	case "codex":
		info := codexTemplateModel(catalog)
		info.DisplayName, info.Description, info.Created = "", "", 0
		info.Version = upstreamID
		info.RestrictedAccess = false
		return info
	}
	info := ModelInfo{Object: "model", OwnedBy: provider, Type: provider}
	for _, model := range models {
		if model != nil && model.OwnedBy != "" && model.Type != "" {
			info.OwnedBy, info.Type = model.OwnedBy, model.Type
			break
		}
	}
	return info
}

// claudeFamilyPrefix returns "claude-<line>-" for a Claude model ID.
func claudeFamilyPrefix(id string) string {
	rest, ok := strings.CutPrefix(strings.ToLower(strings.TrimSpace(id)), "claude-")
	if !ok {
		return ""
	}
	line, _, ok := strings.Cut(rest, "-")
	if !ok || line == "" {
		return ""
	}
	return "claude-" + line + "-"
}

// modelDefinitionFields decodes catalog-shaped JSON without ModelInfo's custom
// UnmarshalJSON, so only the keys present in an entry overwrite fields.
type modelDefinitionFields ModelInfo

// overlayModelDefinition returns a copy of model with the fields present in a
// definition entry. Present keys replace whole values: "thinking": null clears
// thinking. restricted_access is ignored.
func overlayModelDefinition(model *ModelInfo, raw json.RawMessage) *ModelInfo {
	var keys map[string]json.RawMessage
	if errDecode := json.Unmarshal(raw, &keys); errDecode != nil {
		return model
	}
	info := cloneModelInfoForCatalogRoute(model)
	// Reset reference fields first so decoding never writes into shared catalog data.
	for key := range keys {
		switch key {
		case "thinking":
			info.Thinking = nil
		case "config":
			info.Config = nil
		case "supportedGenerationMethods":
			info.SupportedGenerationMethods = nil
		case "supported_parameters":
			info.SupportedParameters = nil
		case "supportedInputModalities":
			info.SupportedInputModalities = nil
		case "supportedOutputModalities":
			info.SupportedOutputModalities = nil
		}
	}
	id := info.ID
	if errDecode := json.Unmarshal(raw, (*modelDefinitionFields)(&info)); errDecode != nil {
		log.Warnf("model definition %s ignored: %v", id, errDecode)
		return model
	}
	info.ID = id
	if value, ok := keys["native_capabilities"]; ok {
		var capabilities *registry.NativeCapabilities
		if json.Unmarshal(value, &capabilities) == nil {
			info.NativeCapabilities = capabilities
		}
	}
	if value, ok := keys["support_configuration_update"]; ok {
		_ = json.Unmarshal(value, &info.SupportConfigurationUpdate)
	}
	if _, ok := keys["context_length"]; ok {
		// Codex clients read the context window from MaxContextLength.
		info.MaxContextLength = info.ContextLength
	}
	if _, ok := keys["thinking"]; ok {
		info.Thinking = modelconfig.NormalizeThinkingSupport(info.Thinking)
		info.ExplicitThinking = true
	}
	if _, ok := keys["supportedInputModalities"]; ok {
		info.ExplicitInputModalities = true
	}
	return &info
}
