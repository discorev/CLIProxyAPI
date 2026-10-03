package management

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// ResetCredentialUsage is an explicit spending action, never an automatic rule.
// Only the v8 management API registers it; the deprecated API is unchanged.
func (h *Handler) ResetCredentialUsage(c *gin.Context) {
	var request struct {
		AuthIndex string `json:"auth_index"`
		GrantID   string `json:"grant_id"`
	}
	if err := c.ShouldBindJSON(&request); err != nil || strings.TrimSpace(request.AuthIndex) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index is required"})
		return
	}
	auths, ok := h.usageCredentials(c, request.AuthIndex)
	if !ok {
		return
	}
	auth := auths[0]
	result, entry, err := h.authManager.ApplyCredentialReset(c.Request.Context(), auth.ID, request.GrantID)
	if err != nil {
		status := http.StatusBadRequest
		switch {
		case errors.Is(err, coreauth.ErrUsageAuthNotFound):
			status = http.StatusNotFound
		case errors.Is(err, coreauth.ErrResetInFlight), errors.Is(err, coreauth.ErrResetPendingRefresh):
			status = http.StatusConflict
		}
		response := gin.H{"error": err.Error()}
		if result.RefreshPending {
			response["refresh_pending"] = true
			response["next_fetch_at"] = entry.NextFetchAt
		}
		c.JSON(status, response)
		return
	}
	response := gin.H{
		"result": result.Result,
		"entry": credentialUsageResponse{
			AuthIndex: auth.Index, AuthID: auth.ID, Provider: auth.Provider, CredentialUsage: entry,
		},
	}
	if result.RefreshPending {
		response["refresh_pending"] = true
		response["next_fetch_at"] = entry.NextFetchAt
	}
	c.JSON(http.StatusOK, response)
}
