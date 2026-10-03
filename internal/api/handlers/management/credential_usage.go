package management

import (
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type credentialUsageResponse struct {
	AuthIndex string `json:"auth_index"`
	AuthID    string `json:"auth_id"`
	Provider  string `json:"provider"`
	coreauth.CredentialUsage
}

// GetCredentialUsage reads cached subscription usage without upstream calls.
func (h *Handler) GetCredentialUsage(c *gin.Context) {
	auths, ok := h.usageCredentials(c, c.Query("auth_index"))
	if !ok {
		return
	}
	c.JSON(http.StatusOK, h.credentialUsageResponses(auths))
}

// RefreshCredentialUsage synchronously refreshes selected OAuth credentials.
func (h *Handler) RefreshCredentialUsage(c *gin.Context) {
	var request struct {
		AuthIndex string `json:"auth_index"`
	}
	if err := c.ShouldBindJSON(&request); err != nil && !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	auths, ok := h.usageCredentials(c, request.AuthIndex)
	if !ok {
		return
	}
	var wg sync.WaitGroup
	for _, auth := range auths {
		wg.Go(func() {
			// Upstream errors live in last_error alongside the previous data.
			_, _ = h.authManager.RefreshUsage(c.Request.Context(), auth.ID)
		})
	}
	wg.Wait()
	c.JSON(http.StatusOK, h.credentialUsageResponses(auths))
}

func (h *Handler) usageCredentials(c *gin.Context, authIndex string) ([]*coreauth.Auth, bool) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth manager unavailable"})
		return nil, false
	}
	if authIndex = strings.TrimSpace(authIndex); authIndex != "" {
		auth := h.authByIndex(authIndex)
		if auth == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "credential not found"})
			return nil, false
		}
		if !coreauth.UsageFetchable(auth) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "credential does not support subscription usage"})
			return nil, false
		}
		return []*coreauth.Auth{auth}, true
	}
	auths := make([]*coreauth.Auth, 0)
	for _, auth := range h.authManager.List() {
		if coreauth.UsageFetchable(auth) {
			auth.EnsureIndex()
			auths = append(auths, auth)
		}
	}
	sort.Slice(auths, func(i, j int) bool { return auths[i].ID < auths[j].ID })
	return auths, true
}

func (h *Handler) credentialUsageResponses(auths []*coreauth.Auth) []credentialUsageResponse {
	responses := make([]credentialUsageResponse, 0, len(auths))
	for _, auth := range auths {
		responses = append(responses, credentialUsageResponse{
			AuthIndex: auth.Index, AuthID: auth.ID, Provider: auth.Provider,
			CredentialUsage: h.authManager.UsageSnapshot(auth.ID),
		})
	}
	return responses
}
