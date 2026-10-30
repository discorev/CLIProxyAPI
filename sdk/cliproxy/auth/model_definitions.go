package auth

import (
	"encoding/json"
	"strings"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

const modelDefinitionsAttributeKey = "models"

// ModelDefinition is one entry of an auth file's "models" key: a model in the
// catalog models.json entry shape that this credential defines or overrides.
// Raw keeps the entry so callers can tell present fields from absent ones.
type ModelDefinition struct {
	ID string
	// UpstreamID, when set, is the model name sent upstream for ID.
	UpstreamID string
	Raw        json.RawMessage
}

// ParseModelDefinitions decodes an auth file's "models" value. Entries without
// an id are dropped; an invalid value yields no definitions.
func ParseModelDefinitions(value any) []ModelDefinition {
	if value == nil {
		return nil
	}
	data, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil
	}
	var entries []json.RawMessage
	if errUnmarshal := json.Unmarshal(data, &entries); errUnmarshal != nil {
		return nil
	}
	definitions := make([]ModelDefinition, 0, len(entries))
	for _, raw := range entries {
		var head struct {
			ID         string `json:"id"`
			UpstreamID string `json:"upstream_id"`
		}
		if errUnmarshal := json.Unmarshal(raw, &head); errUnmarshal != nil {
			continue
		}
		definition := ModelDefinition{ID: strings.TrimSpace(head.ID), UpstreamID: strings.TrimSpace(head.UpstreamID), Raw: raw}
		if definition.ID == "" {
			continue
		}
		if strings.EqualFold(definition.UpstreamID, definition.ID) {
			definition.UpstreamID = ""
		}
		definitions = append(definitions, definition)
	}
	if len(definitions) == 0 {
		return nil
	}
	return definitions
}

// ModelDefinitionAliases returns the per-auth aliases that route each
// definition with an upstream_id to its upstream model. They fork, so the
// upstream model itself stays listed when the credential has it.
func ModelDefinitionAliases(definitions []ModelDefinition) []internalconfig.OAuthModelAlias {
	var aliases []internalconfig.OAuthModelAlias
	for _, definition := range definitions {
		if definition.UpstreamID != "" {
			aliases = append(aliases, internalconfig.OAuthModelAlias{Name: definition.UpstreamID, Alias: definition.ID, Fork: true})
		}
	}
	return aliases
}

// SetModelDefinitionsAttribute stores per-auth model definitions on an auth entry.
func SetModelDefinitionsAttribute(auth *Auth, definitions []ModelDefinition) {
	if auth == nil || len(definitions) == 0 {
		return
	}
	entries := make([]json.RawMessage, 0, len(definitions))
	for _, definition := range definitions {
		entries = append(entries, definition.Raw)
	}
	data, errMarshal := json.Marshal(entries)
	if errMarshal != nil {
		return
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes[modelDefinitionsAttributeKey] = string(data)
}

// ModelDefinitionsFromAttributes returns per-auth model definitions from auth attributes.
func ModelDefinitionsFromAttributes(attributes map[string]string) []ModelDefinition {
	raw := strings.TrimSpace(attributes[modelDefinitionsAttributeKey])
	if raw == "" {
		return nil
	}
	return ParseModelDefinitions(json.RawMessage(raw))
}
