package openai

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
)

func TestOpenAIModelsDoesNotExposeRestrictedAccess(t *testing.T) {
	const id = "restricted-model-response-test"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("restricted-response-auth", "claude", []*registry.ModelInfo{{ID: id, RestrictedAccess: true}})
	t.Cleanup(func() { reg.UnregisterClient("restricted-response-auth") })

	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	NewOpenAIAPIHandler(&handlers.BaseAPIHandler{}).OpenAIModels(ctx)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	var payload struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	for _, model := range payload.Data {
		if model["id"] == id {
			if _, exposed := model["restricted_access"]; exposed {
				t.Fatalf("restricted_access appeared in /v1/models: %v", model)
			}
			return
		}
	}
	t.Fatalf("registered model %q missing in /v1/models", id)
}
