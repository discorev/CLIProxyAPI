package handlers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// resolveAccessProgramModel routes a base model requested with a non-standard
// access_programs.cyber to its Daybreak variant (gpt-6-sol with daybreak_blue
// becomes gpt-6-sol-daybreak-blue), so only credentials whose upstream model
// list includes the program are selected. When no credential registers the
// variant, the request is rejected instead of being silently downgraded. A
// standard or absent program, and a model that already is a variant, pass
// through unchanged.
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
	if len(util.GetProviderName(variant)) == 0 {
		body := `{"error":{"message":"","type":"invalid_request_error","code":"model_not_found","param":"model"}}`
		body, errSet := sjson.Set(body, "error.message", "model "+base+" is not available with access program "+program+" on any credential")
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
