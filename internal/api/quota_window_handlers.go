package api

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/quotawindow"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func (s *Server) quotaWindowsHandler(compact bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		if s == nil || s.quotaWindows == nil || s.handlers == nil || s.handlers.AuthManager == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "provider quota windows unavailable"})
			return
		}
		now := time.Now()
		requestedModels := c.QueryArray("model")
		models := advertisedQuotaModels(nil)
		auths := s.handlers.AuthManager.QuotaWindowAuths()
		registryRef := registry.GetGlobalRegistry()
		statuses := s.quotaWindows.ModelSnapshots(models, auths, func(auth *coreauth.Auth, model string) bool {
			return auth != nil && registryRef.ClientSupportsModel(auth.ID, model)
		}, now, c.QueryArray("provider"))
		statuses = filterQuotaModelStatuses(statuses, requestedModels)
		if compact {
			data := make([]gin.H, 0, len(statuses))
			for _, status := range statuses {
				data = append(data, gin.H{
					"model":               status.Model,
					"available":           status.Available,
					"available_at":        status.AvailableAt,
					"retry_after_seconds": status.RetryAfterSeconds,
				})
			}
			c.JSON(http.StatusOK, gin.H{"now": now.UTC(), "data": data})
			return
		}
		c.JSON(http.StatusOK, gin.H{"object": "list", "now": now.UTC(), "data": statuses})
	}
}

func filterQuotaModelStatuses(statuses []quotawindow.ModelStatus, requested []string) []quotawindow.ModelStatus {
	if len(requested) == 0 {
		return statuses
	}
	requestedSet := make(map[string]struct{}, len(requested))
	for _, model := range requested {
		if key := strings.ToLower(strings.TrimSpace(model)); key != "" {
			requestedSet[key] = struct{}{}
		}
	}
	filtered := make([]quotawindow.ModelStatus, 0, len(requestedSet))
	for _, status := range statuses {
		if _, include := requestedSet[strings.ToLower(status.Model)]; include {
			filtered = append(filtered, status)
		}
	}
	return filtered
}

func advertisedQuotaModels(requested []string) []string {
	available := registry.GetGlobalRegistry().GetAvailableModels("openai")
	advertised := make(map[string]string, len(available))
	for _, model := range available {
		id, _ := model["id"].(string)
		id = strings.TrimSpace(id)
		if id != "" {
			advertised[strings.ToLower(id)] = id
		}
	}
	models := make([]string, 0, len(advertised))
	if len(requested) == 0 {
		for _, id := range advertised {
			models = append(models, id)
		}
	} else {
		seen := make(map[string]struct{}, len(requested))
		for _, raw := range requested {
			key := strings.ToLower(strings.TrimSpace(raw))
			id, ok := advertised[key]
			if !ok {
				continue
			}
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			models = append(models, id)
		}
	}
	sort.Strings(models)
	return models
}
