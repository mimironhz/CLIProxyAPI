package config

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

var quotaDayIndex = map[string]int{
	"mon": 0,
	"tue": 1,
	"wed": 2,
	"thu": 3,
	"fri": 4,
	"sat": 5,
	"sun": 6,
}

var builtInQuotaProviders = map[string]struct{}{
	"aistudio":     {},
	"antigravity":  {},
	"claude":       {},
	"codex":        {},
	"gemini":       {},
	"interactions": {},
	"kimi":         {},
	"vertex":       {},
	"xai":          {},
}

// ValidateProviderQuota validates every top-level and inline quota-window schedule.
func (cfg *Config) ValidateProviderQuota() error {
	if cfg == nil {
		return nil
	}
	if cfg.Home.Enabled && hasProviderQuotaWindows(cfg) {
		return fmt.Errorf("provider quota windows are not supported with home.enabled because Home does not provide an authoritative credential inventory")
	}
	known := make(map[string]struct{}, len(builtInQuotaProviders)+len(cfg.OpenAICompatibility))
	quotaProviderNames := make(map[string]struct{}, len(cfg.ProviderQuota))
	for provider := range cfg.ProviderQuota {
		quotaProviderNames[strings.ToLower(strings.TrimSpace(provider))] = struct{}{}
	}
	for provider := range builtInQuotaProviders {
		known[provider] = struct{}{}
	}
	compatNames := make(map[string]bool, len(cfg.OpenAICompatibility))
	for i := range cfg.OpenAICompatibility {
		entry := &cfg.OpenAICompatibility[i]
		name := strings.ToLower(strings.TrimSpace(entry.Name))
		_, topLevelQuota := quotaProviderNames[name]
		entryQuota := openAICompatibilityHasProviderQuota(entry)
		if previousQuota, duplicate := compatNames[name]; duplicate && (topLevelQuota || entryQuota || previousQuota) {
			return fmt.Errorf("openai-compatibility.%s: duplicate provider name after normalization is ambiguous for provider quota", entry.Name)
		}
		compatNames[name] = compatNames[name] || entryQuota
		if _, reserved := builtInQuotaProviders[name]; reserved && (topLevelQuota || entryQuota) {
			return fmt.Errorf("openai-compatibility.%s: provider name collides with built-in provider quota identity", entry.Name)
		}
		if name != "" {
			known[name] = struct{}{}
		}
		if entry.Quota != nil {
			if err := validateProviderQuota("openai-compatibility."+entry.Name+".quota", *entry.Quota, "provider"); err != nil {
				return err
			}
		}
		modelKeys := make(map[string]string, len(entry.Models))
		for j := range entry.Models {
			if entry.Models[j].Quota == nil {
				continue
			}
			label := strings.TrimSpace(entry.Models[j].Alias)
			if label == "" {
				label = strings.TrimSpace(entry.Models[j].Name)
			}
			modelKey := strings.ToLower(label)
			if previous, exists := modelKeys[modelKey]; exists {
				return fmt.Errorf("openai-compatibility.%s.models.%s.quota: duplicates model %q after normalization", entry.Name, label, previous)
			}
			modelKeys[modelKey] = label
			if err := validateQuotaWindows("openai-compatibility."+entry.Name+".models."+label+".quota", *entry.Models[j].Quota); err != nil {
				return err
			}
		}
	}

	providerKeys := make(map[string]string, len(cfg.ProviderQuota))
	for rawProvider, quota := range cfg.ProviderQuota {
		provider := strings.ToLower(strings.TrimSpace(rawProvider))
		if previous, exists := providerKeys[provider]; exists {
			return fmt.Errorf("provider-quota.%s: duplicates provider %q after normalization", rawProvider, previous)
		}
		providerKeys[provider] = rawProvider
		if _, ok := known[provider]; !ok {
			return fmt.Errorf("provider-quota.%s: unknown provider", rawProvider)
		}
		defaultScope := "credential"
		if _, builtIn := builtInQuotaProviders[provider]; !builtIn {
			defaultScope = "provider"
		}
		if err := validateProviderQuota("provider-quota."+rawProvider, quota, defaultScope); err != nil {
			return err
		}
	}
	return validateSharedUpstreamQuotaSchedules(cfg)
}

func openAICompatibilityHasProviderQuota(entry *OpenAICompatibility) bool {
	if entry == nil {
		return false
	}
	if entry.Quota != nil {
		if len(entry.Quota.Windows) > 0 || len(entry.Quota.Models) > 0 {
			return true
		}
	}
	for i := range entry.Models {
		if entry.Models[i].Quota != nil && len(entry.Models[i].Quota.Windows) > 0 {
			return true
		}
	}
	return false
}

type configuredQuotaAliasRoutes map[string]map[string]map[string]struct{}

func validateSharedUpstreamQuotaSchedules(cfg *Config) error {
	providerModels := effectiveProviderQuotaModels(cfg)
	aliases := configuredProviderQuotaAliases(cfg)
	for provider, models := range providerModels {
		type policy struct {
			model string
			key   string
		}
		byUpstream := make(map[string]policy)
		for model, windows := range models {
			policyKey := quotaWindowsPolicyKey(windows)
			for upstream := range configuredQuotaAliasUpstreams(aliases[provider], model) {
				previous, exists := byUpstream[upstream]
				if exists && previous.key != policyKey {
					return fmt.Errorf("provider-quota.%s.models: %q and %q resolve to shared upstream %q with conflicting schedules", provider, previous.model, model, upstream)
				}
				byUpstream[upstream] = policy{model: model, key: policyKey}
			}
		}
	}
	return nil
}

func effectiveProviderQuotaModels(cfg *Config) map[string]map[string]QuotaWindows {
	out := make(map[string]map[string]QuotaWindows)
	for rawProvider, quota := range cfg.ProviderQuota {
		provider := strings.ToLower(strings.TrimSpace(rawProvider))
		out[provider] = inheritedQuotaModelWindows(quota)
	}
	for i := range cfg.OpenAICompatibility {
		entry := &cfg.OpenAICompatibility[i]
		provider := strings.ToLower(strings.TrimSpace(entry.Name))
		quota, hasQuota := cfg.ProviderQuota[entry.Name]
		if !hasQuota {
			for key, candidate := range cfg.ProviderQuota {
				if strings.EqualFold(strings.TrimSpace(key), provider) {
					quota, hasQuota = candidate, true
					break
				}
			}
		}
		if entry.Quota != nil {
			quota = *entry.Quota
			hasQuota = true
		}
		models := make(map[string]QuotaWindows)
		if hasQuota {
			models = inheritedQuotaModelWindows(quota)
		}
		for j := range entry.Models {
			model := &entry.Models[j]
			if model.Quota == nil {
				continue
			}
			clientModel := strings.TrimSpace(model.Alias)
			if clientModel == "" {
				clientModel = strings.TrimSpace(model.Name)
			}
			clientModel = prefixedProviderQuotaModel(entry.Prefix, clientModel)
			models[strings.ToLower(clientModel)] = inheritProviderQuotaWindows(*model.Quota, quota.QuotaWindows)
		}
		if len(models) > 0 {
			out[provider] = models
		}
	}
	return out
}

func inheritedQuotaModelWindows(quota ProviderQuota) map[string]QuotaWindows {
	models := make(map[string]QuotaWindows, len(quota.Models))
	for model, windows := range quota.Models {
		models[strings.ToLower(strings.TrimSpace(model))] = inheritProviderQuotaWindows(windows, quota.QuotaWindows)
	}
	return models
}

func inheritProviderQuotaWindows(child, parent QuotaWindows) QuotaWindows {
	if strings.TrimSpace(child.Timezone) == "" {
		child.Timezone = parent.Timezone
	}
	if child.Persist == nil {
		child.Persist = parent.Persist
	}
	return child
}

func configuredProviderQuotaAliases(cfg *Config) configuredQuotaAliasRoutes {
	aliases := make(configuredQuotaAliasRoutes)
	for channel, entries := range cfg.OAuthModelAlias {
		for _, entry := range entries {
			addConfiguredProviderQuotaAlias(aliases, channel, "", entry.Alias, []string{entry.Name})
		}
	}
	for i := range cfg.ClaudeKey {
		addConfiguredProviderQuotaModels(aliases, "claude", cfg.ClaudeKey[i].Prefix, cfg.ClaudeKey[i].Models)
	}
	for i := range cfg.CodexKey {
		addConfiguredProviderQuotaModels(aliases, "codex", cfg.CodexKey[i].Prefix, cfg.CodexKey[i].Models)
	}
	for i := range cfg.XAIKey {
		addConfiguredProviderQuotaModels(aliases, "xai", cfg.XAIKey[i].Prefix, cfg.XAIKey[i].Models)
	}
	for i := range cfg.GeminiKey {
		addConfiguredProviderQuotaModels(aliases, "gemini", cfg.GeminiKey[i].Prefix, cfg.GeminiKey[i].Models)
	}
	for i := range cfg.InteractionsKey {
		addConfiguredProviderQuotaModels(aliases, "interactions", cfg.InteractionsKey[i].Prefix, cfg.InteractionsKey[i].Models)
	}
	for i := range cfg.VertexCompatAPIKey {
		addConfiguredProviderQuotaModels(aliases, "vertex", cfg.VertexCompatAPIKey[i].Prefix, cfg.VertexCompatAPIKey[i].Models)
	}
	for i := range cfg.OpenAICompatibility {
		entry := &cfg.OpenAICompatibility[i]
		addConfiguredProviderQuotaModels(aliases, entry.Name, entry.Prefix, entry.Models)
	}
	return aliases
}

func addConfiguredProviderQuotaModels[T interface {
	GetName() string
	GetAlias() string
}](aliases configuredQuotaAliasRoutes, provider, prefix string, models []T) {
	grouped := make(map[string][]string)
	for i := range models {
		clientModel := strings.TrimSpace(models[i].GetAlias())
		if clientModel == "" {
			clientModel = strings.TrimSpace(models[i].GetName())
		}
		clientModel = prefixedProviderQuotaModel(prefix, clientModel)
		if clientModel != "" {
			grouped[clientModel] = append(grouped[clientModel], models[i].GetName())
		}
	}
	for clientModel, upstreams := range grouped {
		addConfiguredProviderQuotaAlias(aliases, provider, "", clientModel, upstreams)
	}
}

func addConfiguredProviderQuotaAlias(aliases configuredQuotaAliasRoutes, provider, prefix, clientModel string, upstreams []string) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	clientModel = strings.ToLower(strings.TrimSpace(prefixedProviderQuotaModel(prefix, clientModel)))
	upstream := canonicalProviderQuotaModels(upstreams, clientModel)
	if provider == "" || clientModel == "" || upstream == "" {
		return
	}
	if aliases[provider] == nil {
		aliases[provider] = make(map[string]map[string]struct{})
	}
	if aliases[provider][clientModel] == nil {
		aliases[provider][clientModel] = make(map[string]struct{})
	}
	aliases[provider][clientModel][upstream] = struct{}{}
}

func configuredQuotaAliasUpstreams(aliases map[string]map[string]struct{}, model string) map[string]struct{} {
	model = strings.ToLower(strings.TrimSpace(model))
	resolved := make(map[string]struct{})
	for alias, upstreams := range aliases {
		if model != alias && !strings.HasSuffix(model, "/"+alias) {
			continue
		}
		for upstream := range upstreams {
			resolved[upstream] = struct{}{}
		}
	}
	if len(resolved) == 0 && model != "" {
		resolved[model] = struct{}{}
	}
	return resolved
}

func canonicalProviderQuotaModels(models []string, fallback string) string {
	seen := make(map[string]struct{}, len(models))
	canonical := make([]string, 0, len(models))
	for _, model := range models {
		model = strings.ToLower(strings.TrimSpace(model))
		if model == "" {
			continue
		}
		if _, exists := seen[model]; exists {
			continue
		}
		seen[model] = struct{}{}
		canonical = append(canonical, model)
	}
	if len(canonical) == 0 {
		return strings.ToLower(strings.TrimSpace(fallback))
	}
	sort.Strings(canonical)
	if len(canonical) == 1 {
		return canonical[0]
	}
	return "pool:" + strings.Join(canonical, ",")
}

func prefixedProviderQuotaModel(prefix, model string) string {
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	model = strings.TrimLeft(strings.TrimSpace(model), "/")
	if prefix == "" || model == "" {
		return model
	}
	return prefix + "/" + model
}

func quotaWindowsPolicyKey(windows QuotaWindows) string {
	timezone := strings.TrimSpace(windows.Timezone)
	if timezone == "" {
		timezone = "UTC"
	}
	persist := true
	if windows.Persist != nil {
		persist = *windows.Persist
	}
	parts := make([]string, 0, len(windows.Windows))
	for _, window := range windows.Windows {
		start, _ := parseQuotaClock(window.Start)
		end, _ := parseQuotaClock(window.End)
		days, _ := quotaDays(window.Days)
		sort.Ints(days)
		parts = append(parts, fmt.Sprintf("%s|%d|%d|%v|%s", strings.ToLower(strings.TrimSpace(window.Name)), start, end, days, quotaBudgetPolicyKey(window.Budget)))
	}
	sort.Strings(parts)
	return timezone + "|" + fmt.Sprintf("%t", persist) + "|" + strings.Join(parts, ";")
}

func quotaBudgetPolicyKey(budget *QuotaBudget) string {
	if budget == nil {
		return "unmetered"
	}
	value := func(limit *int64) string {
		if limit == nil {
			return "*"
		}
		return fmt.Sprintf("%d", *limit)
	}
	return strings.Join([]string{value(budget.Requests), value(budget.InputTokens), value(budget.OutputTokens), value(budget.TotalTokens)}, ",")
}

func hasProviderQuotaWindows(cfg *Config) bool {
	if cfg == nil {
		return false
	}
	for _, quota := range cfg.ProviderQuota {
		if len(quota.Windows) > 0 {
			return true
		}
		for _, windows := range quota.Models {
			if len(windows.Windows) > 0 {
				return true
			}
		}
	}
	for i := range cfg.OpenAICompatibility {
		entry := &cfg.OpenAICompatibility[i]
		if entry.Quota != nil {
			if len(entry.Quota.Windows) > 0 {
				return true
			}
			for _, windows := range entry.Quota.Models {
				if len(windows.Windows) > 0 {
					return true
				}
			}
		}
		for j := range entry.Models {
			if entry.Models[j].Quota != nil && len(entry.Models[j].Quota.Windows) > 0 {
				return true
			}
		}
	}
	return false
}

func validateProviderQuota(path string, quota ProviderQuota, defaultScope string) error {
	scope := strings.ToLower(strings.TrimSpace(quota.Scope))
	if scope == "" {
		scope = defaultScope
	}
	if scope != "provider" && scope != "credential" {
		return fmt.Errorf("%s.scope: must be provider or credential", path)
	}
	if err := validateQuotaWindows(path, quota.QuotaWindows); err != nil {
		return err
	}
	modelKeys := make(map[string]string, len(quota.Models))
	for model, windows := range quota.Models {
		if strings.TrimSpace(model) == "" {
			return fmt.Errorf("%s.models: model name must not be empty", path)
		}
		modelKey := strings.ToLower(strings.TrimSpace(model))
		if previous, exists := modelKeys[modelKey]; exists {
			return fmt.Errorf("%s.models.%s: duplicates model %q after normalization", path, model, previous)
		}
		modelKeys[modelKey] = model
		if err := validateQuotaWindows(path+".models."+model, windows); err != nil {
			return err
		}
	}
	return nil
}

func validateQuotaWindows(path string, windows QuotaWindows) error {
	timezone := strings.TrimSpace(windows.Timezone)
	if timezone == "" {
		timezone = "UTC"
	}
	if _, err := time.LoadLocation(timezone); err != nil {
		return fmt.Errorf("%s.timezone: %w", path, err)
	}

	occupied := make([]string, 7*24*60)
	names := make(map[string]struct{}, len(windows.Windows))
	for i := range windows.Windows {
		window := windows.Windows[i]
		name := strings.TrimSpace(window.Name)
		if name == "" {
			return fmt.Errorf("%s.windows[%d].name: must not be empty", path, i)
		}
		nameKey := strings.ToLower(name)
		if _, exists := names[nameKey]; exists {
			return fmt.Errorf("%s.windows[%d].name: duplicate window %q", path, i, name)
		}
		names[nameKey] = struct{}{}

		start, errStart := parseQuotaClock(window.Start)
		if errStart != nil {
			return fmt.Errorf("%s.windows[%d].start: %w", path, i, errStart)
		}
		end, errEnd := parseQuotaClock(window.End)
		if errEnd != nil {
			return fmt.Errorf("%s.windows[%d].end: %w", path, i, errEnd)
		}
		if start == end {
			return fmt.Errorf("%s.windows[%d]: start and end must differ", path, i)
		}
		duration := end - start
		if duration <= 0 {
			duration += 24 * 60
		}

		days, errDays := quotaDays(window.Days)
		if errDays != nil {
			return fmt.Errorf("%s.windows[%d].days: %w", path, i, errDays)
		}
		for _, day := range days {
			startMinute := day*24*60 + start
			for offset := 0; offset < duration; offset++ {
				slot := (startMinute + offset) % len(occupied)
				if occupied[slot] != "" {
					return fmt.Errorf("%s.windows[%d]: overlaps window %q", path, i, occupied[slot])
				}
				occupied[slot] = name
			}
		}
		if errBudget := validateQuotaBudget(path, i, window.Budget); errBudget != nil {
			return errBudget
		}
	}
	return nil
}

func validateQuotaBudget(path string, index int, budget *QuotaBudget) error {
	if budget == nil {
		return nil
	}
	values := map[string]*int64{
		"requests":      budget.Requests,
		"input-tokens":  budget.InputTokens,
		"output-tokens": budget.OutputTokens,
		"total-tokens":  budget.TotalTokens,
	}
	for name, value := range values {
		if value != nil && *value < 0 {
			return fmt.Errorf("%s.windows[%d].budget.%s: must be >= 0", path, index, name)
		}
	}
	return nil
}

func parseQuotaClock(value string) (int, error) {
	parsed, err := time.Parse("15:04", strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("must use HH:MM: %w", err)
	}
	return parsed.Hour()*60 + parsed.Minute(), nil
}

func quotaDays(values []string) ([]int, error) {
	if len(values) == 0 {
		return []int{0, 1, 2, 3, 4, 5, 6}, nil
	}
	seen := make(map[int]struct{}, len(values))
	out := make([]int, 0, len(values))
	for _, raw := range values {
		day, ok := quotaDayIndex[strings.ToLower(strings.TrimSpace(raw))]
		if !ok {
			return nil, fmt.Errorf("unknown day %q", raw)
		}
		if _, exists := seen[day]; exists {
			continue
		}
		seen[day] = struct{}{}
		out = append(out, day)
	}
	return out, nil
}
