package management

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// GetQuotaWindows returns compiled schedules and the live quota-window ledger.
func (h *Handler) GetQuotaWindows(c *gin.Context) {
	h.mu.Lock()
	gate := h.quotaWindows
	authManager := h.authManager
	h.mu.Unlock()
	if gate == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "provider quota windows unavailable"})
		return
	}
	var auths []*coreauth.Auth
	if authManager != nil {
		auths = authManager.List()
	}
	c.JSON(http.StatusOK, gate.ManagementSnapshot(time.Now(), auths))
}

// ResetQuotaWindows clears matching live-window consumption without changing cooldown state.
func (h *Handler) ResetQuotaWindows(c *gin.Context) {
	var body struct {
		Provider   string `json:"provider"`
		Model      string `json:"model"`
		Credential string `json:"credential"`
	}
	if errBind := c.ShouldBindJSON(&body); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	body.Provider = strings.TrimSpace(body.Provider)
	if body.Provider == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "provider is required"})
		return
	}
	h.mu.Lock()
	gate := h.quotaWindows
	h.mu.Unlock()
	if gate == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "provider quota windows unavailable"})
		return
	}
	reset := gate.Reset(body.Provider, body.Model, body.Credential)
	c.JSON(http.StatusOK, gin.H{"status": "ok", "reset": reset})
}
