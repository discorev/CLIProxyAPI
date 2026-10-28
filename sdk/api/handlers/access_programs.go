package handlers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// resolveAccessProgramRoute applies resolveAccessProgramModel to the model the
// request routes by: the requested model, or the target of a built-in Codex
// provider route, which keeps its provider and routes by the variant. Other
// provider routes are left unchanged.
func (h *BaseAPIHandler) resolveAccessProgramRoute(modelName, originalRequestedModel string, decision *modelRouteDecision, rawJSON []byte) (string, *interfaces.ErrorMessage) {
	switch decision.Provider {
	case "":
		return h.resolveAccessProgramModel(modelName, rawJSON)
	case "codex":
		target := decision.Model
		if target == "" {
			target = originalRequestedModel
		}
		resolved, errMsg := h.resolveAccessProgramModel(target, rawJSON)
		if errMsg != nil {
			return "", errMsg
		}
		decision.Model = resolved
	}
	return modelName, nil
}

// resolveAccessProgramModel routes a base model requested with a non-standard
// access_programs.cyber to its Daybreak variant (gpt-6-sol with daybreak_blue
// becomes gpt-6-sol-daybreak-blue), so only credentials whose upstream model
// list includes the program are selected. The request is rejected instead of
// being silently downgraded when no credential registers a genuine variant,
// or when an ordinary model (such as a user-defined alias) shares the
// variant's ID and could be selected for it. A standard or absent program,
// and a model that already is a variant, pass through unchanged.
func (h *BaseAPIHandler) resolveAccessProgramModel(modelName string, rawJSON []byte) (string, *interfaces.ErrorMessage) {
	program := strings.ToLower(strings.TrimSpace(gjson.GetBytes(rawJSON, "access_programs.cyber").String()))
	if program == "" || program == "standard" || (h != nil && h.AuthManager != nil && h.AuthManager.HomeEnabled()) {
		return modelName, nil
	}
	parsed := thinking.ParseSuffix(modelName)
	base := strings.TrimSpace(parsed.ModelName)
	if info := registry.GetGlobalRegistry().GetModelInfo(base, ""); info != nil && info.AccessProgram != "" {
		return modelName, nil
	}
	variant := base + "-" + strings.ReplaceAll(program, "_", "-")
	genuine, ordinary := registry.GetGlobalRegistry().AccessProgramVariantClients(variant, program)
	if genuine == 0 || ordinary > 0 {
		message := "model " + base + " is not available with access program " + program + " on any credential"
		if genuine > 0 {
			message = "model " + variant + " is also defined as an ordinary model, so access program " + program + " cannot be routed for " + base
		}
		body := `{"error":{"message":"","type":"invalid_request_error","code":"model_not_found","param":"model"}}`
		body, errSet := sjson.Set(body, "error.message", message)
		if errSet != nil {
			body = `{"error":{"message":"model is not available with the requested access program","type":"invalid_request_error","code":"model_not_found","param":"model"}}`
		}
		return "", &interfaces.ErrorMessage{StatusCode: http.StatusBadRequest, Error: errors.New(body)}
	}
	if parsed.HasSuffix {
		variant += "(" + parsed.RawSuffix + ")"
	}
	return variant, nil
}
