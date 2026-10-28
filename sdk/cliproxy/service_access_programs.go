package cliproxy

import (
	"encoding/json"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// Daybreak variants expose the non-standard access programs Codex /models lists
// for a model (available_access_programs.cyber) as separate models:
// gpt-6-sol-daybreak-blue routes to gpt-6-sol and the executor sends
// access_programs.cyber = "daybreak_blue". Requests then select only
// credentials whose upstream model list includes the program.

const standardAccessProgram = "standard"

type codexAccessProgramsObject struct {
	Visibility              string `json:"visibility"`
	AvailableAccessPrograms struct {
		Cyber []string `json:"cyber"`
	} `json:"available_access_programs"`
}

// accessProgramVariant is one variant an upstream model object grants.
type accessProgramVariant struct {
	base    string
	program string
}

func (v accessProgramVariant) id() string {
	return v.base + "-" + strings.ReplaceAll(v.program, "_", "-")
}

// accessProgramVariants lists the non-standard programs of visible upstream
// Codex models, in upstream order. Hidden models get no variant.
func accessProgramVariants(upstream []coreauth.UpstreamModel) []accessProgramVariant {
	var variants []accessProgramVariant
	for _, model := range upstream {
		if model.ID == "" || len(model.Raw) == 0 {
			continue
		}
		var object codexAccessProgramsObject
		if json.Unmarshal(model.Raw, &object) != nil || strings.EqualFold(object.Visibility, "hide") {
			continue
		}
		for _, program := range object.AvailableAccessPrograms.Cyber {
			program = strings.ToLower(strings.TrimSpace(program))
			if program != "" && program != standardAccessProgram {
				variants = append(variants, accessProgramVariant{base: model.ID, program: program})
			}
		}
	}
	return variants
}

// accessProgramVariantsEqual reports whether two upstream lists grant the same variants.
func accessProgramVariantsEqual(a, b []coreauth.UpstreamModel) bool {
	left, right := accessProgramVariants(a), accessProgramVariants(b)
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

// appendAccessProgramVariantsLocked adds a Daybreak variant for each program
// the credential's upstream list grants a resolved model. The variant copies
// the base's final entry and routes to its upstream model. Bases the
// credential does not resolve, excluded variant IDs and IDs already resolved
// are skipped. Callers hold restrictedAccess.mu.
func (s *Service) appendAccessProgramVariantsLocked(auth *coreauth.Auth, excluded []string, models []*ModelInfo) []*ModelInfo {
	if !s.modelDiscoveryEnabled(auth) || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return models
	}
	variants := accessProgramVariants(s.upstreamModelsLocked(auth))
	if len(variants) == 0 {
		return models
	}
	out := models
	for _, variant := range variants {
		base := findModelByID(models, variant.base)
		id := variant.id()
		if base == nil || findModelByID(out, id) != nil || len(applyExcludedModels([]*ModelInfo{{ID: id}}, excluded)) == 0 {
			continue
		}
		info := cloneModelInfoForCatalogRoute(base)
		info.ID = id
		if info.MetadataModelID == "" {
			info.MetadataModelID = base.ID
		}
		name := base.DisplayName
		if name == "" {
			name = base.ID
		}
		info.DisplayName = name + " (" + accessProgramTitle(variant.program) + ")"
		info.AccessProgram = variant.program
		out = append(out, &info)
	}
	return out
}

// accessProgramTitle renders "daybreak_blue" as "Daybreak Blue".
func accessProgramTitle(program string) string {
	words := strings.Split(program, "_")
	for i, word := range words {
		if word != "" {
			words[i] = strings.ToUpper(word[:1]) + word[1:]
		}
	}
	return strings.Join(words, " ")
}
