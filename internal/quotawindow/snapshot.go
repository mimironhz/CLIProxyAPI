package quotawindow

import (
	"math"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// DimensionStatus reports one configured budget dimension.
type DimensionStatus struct {
	Limit     int64 `json:"limit"`
	Used      int64 `json:"used"`
	Remaining int64 `json:"remaining"`
}

// WindowStatus reports one concrete active window.
type WindowStatus struct {
	Name     string    `json:"name"`
	StartsAt time.Time `json:"starts_at"`
	EndsAt   time.Time `json:"ends_at"`
}

// ProviderStatus reports the quota state of one provider budget key.
type ProviderStatus struct {
	Provider      string                     `json:"provider"`
	Scope         string                     `json:"scope,omitempty"`
	Credential    string                     `json:"credential,omitempty"`
	UpstreamModel string                     `json:"upstream_model"`
	Available     bool                       `json:"available"`
	Window        *WindowStatus              `json:"window,omitempty"`
	Budget        map[string]DimensionStatus `json:"budget"`
	Exhausted     []string                   `json:"exhausted,omitempty"`
	AvailableAt   *time.Time                 `json:"available_at"`
}

// NextWindowStatus reports the next configured window for a model.
type NextWindowStatus struct {
	Provider string    `json:"provider"`
	Name     string    `json:"name"`
	StartsAt time.Time `json:"starts_at"`
	EndsAt   time.Time `json:"ends_at"`
}

// ModelStatus mirrors whether an inference request can run now.
type ModelStatus struct {
	Model             string            `json:"model"`
	Available         bool              `json:"available"`
	Reason            *string           `json:"reason"`
	AvailableAt       *time.Time        `json:"available_at"`
	RetryAfterSeconds int               `json:"retry_after_seconds"`
	SharesBudgetWith  []string          `json:"shares_budget_with"`
	Providers         []ProviderStatus  `json:"providers"`
	NextWindow        *NextWindowStatus `json:"next_window,omitempty"`
}

// ModelSnapshots returns read-only status for advertised client-visible models.
func (g *Gate) ModelSnapshots(models []string, auths []*coreauth.Auth, supports func(*coreauth.Auth, string) bool, now time.Time, providerFilters []string) []ModelStatus {
	filters := make(map[string]struct{}, len(providerFilters))
	for _, provider := range providerFilters {
		if normalized := strings.ToLower(strings.TrimSpace(provider)); normalized != "" {
			filters[normalized] = struct{}{}
		}
	}
	sort.Strings(models)
	candidatesByModel := make(map[string][]*coreauth.Auth, len(models))
	budgetModels := make(map[string]map[string]struct{})
	for _, model := range models {
		for _, candidate := range auths {
			if candidate == nil || candidate.Disabled || candidate.Status == coreauth.StatusDisabled {
				continue
			}
			if supports != nil && !supports(candidate, model) {
				continue
			}
			target := g.resolver.ResolveQuotaWindowTarget(candidate, model)
			if len(filters) > 0 {
				if _, ok := filters[strings.ToLower(target.Provider)]; !ok {
					continue
				}
			}
			candidatesByModel[model] = append(candidatesByModel[model], candidate)
			if resolved, configured := g.resolve(candidate, model); configured && !resolved.conflict {
				if budgetModels[resolved.budgetKey] == nil {
					budgetModels[resolved.budgetKey] = make(map[string]struct{})
				}
				budgetModels[resolved.budgetKey][model] = struct{}{}
			}
		}
	}

	statuses := make([]ModelStatus, 0, len(models))
	for _, model := range models {
		candidates := candidatesByModel[model]
		if len(filters) > 0 && len(candidates) == 0 {
			continue
		}
		status := ModelStatus{
			Model:            model,
			Available:        true,
			SharesBudgetWith: []string{},
			Providers:        g.providerStatuses(candidates, model, now),
		}
		shares := make(map[string]struct{})
		for _, candidate := range candidates {
			resolved, configured := g.resolve(candidate, model)
			if !configured || resolved.conflict {
				continue
			}
			for sibling := range budgetModels[resolved.budgetKey] {
				if sibling != model {
					shares[sibling] = struct{}{}
				}
			}
			if next, ok := resolved.schedule.NextFutureInstance(now); ok {
				if status.NextWindow == nil || next.StartsAt.Before(status.NextWindow.StartsAt) {
					status.NextWindow = &NextWindowStatus{
						Provider: resolved.target.Provider,
						Name:     next.Name,
						StartsAt: next.StartsAt.UTC(),
						EndsAt:   next.EndsAt.UTC(),
					}
				}
			}
		}
		for sibling := range shares {
			status.SharesBudgetWith = append(status.SharesBudgetWith, sibling)
		}
		sort.Strings(status.SharesBudgetWith)

		if block, exhausted := g.BlockedForModel(candidates, model, now); exhausted {
			reason := "quota_window_exhausted"
			status.Available = false
			status.Reason = &reason
			if !block.AvailableAt.IsZero() {
				availableAt := block.AvailableAt.UTC()
				status.AvailableAt = &availableAt
				status.RetryAfterSeconds = retrySeconds(now, block.AvailableAt)
			}
		} else if g.resolver != nil {
			quotaAvailable := g.AvailableAuths(candidates, model, now)
			if availableAt, cooling := g.resolver.QuotaWindowCooldown(quotaAvailable, model, now); cooling {
				reason := "model_cooldown"
				status.Available = false
				status.Reason = &reason
				availableUTC := availableAt.UTC()
				status.AvailableAt = &availableUTC
				status.RetryAfterSeconds = retrySeconds(now, availableAt)
			}
		}
		statuses = append(statuses, status)
	}
	return statuses
}

func (g *Gate) providerStatuses(auths []*coreauth.Auth, model string, now time.Time) []ProviderStatus {
	statuses := make([]ProviderStatus, 0)
	seen := make(map[string]struct{})
	for _, candidate := range auths {
		if candidate == nil {
			continue
		}
		target := g.resolver.ResolveQuotaWindowTarget(candidate, model)
		resolved, configured := g.resolve(candidate, model)
		key := "unconfigured|" + strings.ToLower(target.Provider) + "|" + strings.ToLower(target.UpstreamModel)
		if configured {
			key = resolved.budgetKey
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		status := ProviderStatus{
			Provider:      target.Provider,
			UpstreamModel: target.UpstreamModel,
			Available:     true,
			Budget:        map[string]DimensionStatus{},
		}
		if !configured {
			statuses = append(statuses, status)
			continue
		}
		status.Scope = resolved.provider.scope
		if resolved.provider.scope == "credential" {
			status.Credential = target.Credential
		}
		if resolved.conflict {
			status.Available = false
			status.Exhausted = []string{"configuration"}
			statuses = append(statuses, status)
			continue
		}
		instance, active := resolved.schedule.InstanceAt(now)
		if !active || instance.Budget == nil {
			statuses = append(statuses, status)
			continue
		}
		used, exhausted := g.ledger.Snapshot(resolved.budgetKey, instance, instance.Budget)
		status.Window = &WindowStatus{Name: instance.Name, StartsAt: instance.StartsAt.UTC(), EndsAt: instance.EndsAt.UTC()}
		status.Budget = dimensionStatuses(instance.Budget, used)
		status.Exhausted = exhausted
		if len(exhausted) > 0 {
			status.Available = false
			availableAt := resolved.schedule.NextOpen(now, exhausted)
			if !availableAt.IsZero() {
				availableUTC := availableAt.UTC()
				status.AvailableAt = &availableUTC
			}
		}
		statuses = append(statuses, status)
	}
	sort.Slice(statuses, func(i, j int) bool {
		if statuses[i].Provider != statuses[j].Provider {
			return statuses[i].Provider < statuses[j].Provider
		}
		return statuses[i].Credential < statuses[j].Credential
	})
	return statuses
}

func dimensionStatuses(budget *config.QuotaBudget, used Usage) map[string]DimensionStatus {
	statuses := make(map[string]DimensionStatus)
	if budget == nil {
		return statuses
	}
	add := func(name string, limit *int64, consumed int64) {
		if limit == nil {
			return
		}
		remaining := *limit - consumed
		if remaining < 0 {
			remaining = 0
		}
		statuses[name] = DimensionStatus{Limit: *limit, Used: consumed, Remaining: remaining}
	}
	add("requests", budget.Requests, used.Requests)
	add("input-tokens", budget.InputTokens, used.InputTokens)
	add("output-tokens", budget.OutputTokens, used.OutputTokens)
	add("total-tokens", budget.TotalTokens, used.TotalTokens)
	return statuses
}

func retrySeconds(now, availableAt time.Time) int {
	seconds := int(math.Ceil(availableAt.Sub(now).Seconds()))
	if seconds < 0 {
		return 0
	}
	return seconds
}
