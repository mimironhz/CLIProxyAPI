package quotawindow

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

type providerSchedule struct {
	name     string
	scope    string
	disabled bool
	base     *Schedule
	models   map[string]*Schedule
	raw      config.ProviderQuota
}

// Gate implements selection-time quota checks and owns the persistent ledger.
type Gate struct {
	mu              sync.RWMutex
	admissionMu     sync.Mutex
	resolver        *coreauth.Manager
	providers       map[string]*providerSchedule
	budgetPolicies  map[string]string
	budgetConflicts map[string]struct{}
	ledger          *Ledger
	storeMu         sync.Mutex
	store           *Store
	authDir         string
}

// New constructs a gate, restores persisted counters, and compiles cfg.
func New(cfg *config.Config, resolver *coreauth.Manager, authDir string) (*Gate, error) {
	ledger := NewLedger()
	gate := &Gate{
		resolver:        resolver,
		providers:       make(map[string]*providerSchedule),
		budgetPolicies:  make(map[string]string),
		budgetConflicts: make(map[string]struct{}),
		ledger:          ledger,
		authDir:         cleanAuthDir(authDir),
	}
	gate.store = NewStore(authDir, ledger)
	if gate.store != nil {
		records, errLoad := gate.store.Load()
		if errLoad != nil {
			return nil, errLoad
		}
		if errReplace := ledger.Replace(records); errReplace != nil {
			return nil, fmt.Errorf("restore quota-window snapshot: %w", errReplace)
		}
		ledger.SetOnChange(gate.store.Schedule)
	}
	if errUpdate := gate.Update(cfg); errUpdate != nil {
		_ = gate.Close()
		return nil, errUpdate
	}
	return gate, nil
}

// Update atomically swaps compiled schedules while retaining unchanged live instances.
func (g *Gate) Update(cfg *config.Config) error {
	providers, errCompile := compileProviders(cfg)
	if errCompile != nil {
		return errCompile
	}
	if cfg != nil && strings.TrimSpace(cfg.AuthDir) != "" {
		if errStore := g.updateStore(cfg.AuthDir); errStore != nil {
			return errStore
		}
	}
	g.mu.Lock()
	g.providers = providers
	g.budgetPolicies = make(map[string]string)
	g.budgetConflicts = make(map[string]struct{})
	g.mu.Unlock()
	active := make(map[string][]Instance)
	knownSchedules := make(map[string]struct{})
	now := time.Now()
	for _, provider := range providers {
		if provider.base != nil {
			knownSchedules[provider.base.id] = struct{}{}
			if instance, ok := provider.base.InstanceAt(now); ok {
				active[instance.ID] = append(active[instance.ID], instance)
			}
		}
		for _, schedule := range provider.models {
			knownSchedules[schedule.id] = struct{}{}
			if instance, ok := schedule.InstanceAt(now); ok {
				active[instance.ID] = append(active[instance.ID], instance)
			}
		}
	}
	g.ledger.Reconcile(active, knownSchedules)
	return nil
}

func cleanAuthDir(authDir string) string {
	authDir = strings.TrimSpace(authDir)
	if authDir == "" {
		return ""
	}
	return filepath.Clean(authDir)
}

func (g *Gate) updateStore(authDir string) error {
	if g == nil {
		return nil
	}
	authDir = cleanAuthDir(authDir)
	g.storeMu.Lock()
	defer g.storeMu.Unlock()
	if authDir == g.authDir {
		return nil
	}
	previous := g.store
	next := NewStore(authDir, g.ledger)
	if next != nil {
		if errFlush := next.Flush(); errFlush != nil {
			return fmt.Errorf("initialize quota-window store: %w", errFlush)
		}
	}
	g.store = next
	g.authDir = authDir
	if next == nil {
		g.ledger.SetOnChange(nil)
	} else {
		g.ledger.SetOnChange(next.Schedule)
	}
	if next != nil {
		if errFlush := next.Flush(); errFlush != nil {
			return fmt.Errorf("activate quota-window store: %w", errFlush)
		}
	}
	if previous != nil {
		if errClose := previous.Close(); errClose != nil {
			log.WithError(errClose).Warn("failed to close previous provider quota-window store")
		}
	}
	return nil
}

func compileProviders(cfg *config.Config) (map[string]*providerSchedule, error) {
	providers := make(map[string]*providerSchedule)
	if cfg == nil {
		return providers, nil
	}
	quotaRelevantCompat := make(map[string]struct{}, len(cfg.ProviderQuota)+len(cfg.OpenAICompatibility))
	for provider := range cfg.ProviderQuota {
		quotaRelevantCompat[strings.ToLower(strings.TrimSpace(provider))] = struct{}{}
	}
	for i := range cfg.OpenAICompatibility {
		entry := &cfg.OpenAICompatibility[i]
		relevant := entry.Quota != nil && (len(entry.Quota.Windows) > 0 || len(entry.Quota.Models) > 0)
		for j := range entry.Models {
			relevant = relevant || (entry.Models[j].Quota != nil && len(entry.Models[j].Quota.Windows) > 0)
		}
		if relevant {
			quotaRelevantCompat[strings.ToLower(strings.TrimSpace(entry.Name))] = struct{}{}
		}
	}
	compatByName := make(map[string]*config.OpenAICompatibility, len(cfg.OpenAICompatibility))
	for i := range cfg.OpenAICompatibility {
		entry := &cfg.OpenAICompatibility[i]
		name := strings.ToLower(strings.TrimSpace(entry.Name))
		if name != "" {
			if _, exists := compatByName[name]; exists {
				if _, relevant := quotaRelevantCompat[name]; relevant {
					return nil, fmt.Errorf("openai-compatibility.%s: duplicate provider name after normalization", entry.Name)
				}
				continue
			}
			compatByName[name] = entry
		}
	}
	topLevelNames := make(map[string]string, len(cfg.ProviderQuota))
	for rawName, rawQuota := range cfg.ProviderQuota {
		name := strings.ToLower(strings.TrimSpace(rawName))
		if previous, exists := topLevelNames[name]; exists {
			return nil, fmt.Errorf("provider-quota.%s: duplicates provider %q after normalization", rawName, previous)
		}
		topLevelNames[name] = rawName
		defaultScope := "credential"
		if _, ok := compatByName[name]; ok {
			defaultScope = "provider"
		}
		compiled, errProvider := compileProvider(name, rawQuota, defaultScope, false)
		if errProvider != nil {
			return nil, fmt.Errorf("provider-quota.%s: %w", rawName, errProvider)
		}
		providers[name] = compiled
	}
	for i := range cfg.OpenAICompatibility {
		entry := &cfg.OpenAICompatibility[i]
		name := strings.ToLower(strings.TrimSpace(entry.Name))
		if name == "" {
			continue
		}
		rawQuota, hasQuota := cfg.ProviderQuota[entry.Name]
		if !hasQuota {
			for key, candidate := range cfg.ProviderQuota {
				if strings.EqualFold(strings.TrimSpace(key), name) {
					rawQuota, hasQuota = candidate, true
					break
				}
			}
		}
		if entry.Quota != nil {
			if hasQuota {
				log.Warnf("inline quota overrides provider-quota.%s", entry.Name)
			}
			rawQuota = *entry.Quota
			hasQuota = true
		}
		modelInline := false
		for j := range entry.Models {
			if entry.Models[j].Quota != nil {
				modelInline = true
				break
			}
		}
		if !hasQuota && !modelInline {
			continue
		}
		compiled, errProvider := compileProvider(name, rawQuota, "provider", entry.Disabled)
		if errProvider != nil {
			return nil, fmt.Errorf("openai-compatibility.%s.quota: %w", entry.Name, errProvider)
		}
		inlineModels := make(map[string]string, len(entry.Models))
		for j := range entry.Models {
			model := &entry.Models[j]
			if model.Quota == nil {
				continue
			}
			clientModel := strings.TrimSpace(model.Alias)
			if clientModel == "" {
				clientModel = strings.TrimSpace(model.Name)
			}
			if prefix := strings.Trim(strings.TrimSpace(entry.Prefix), "/"); prefix != "" {
				clientModel = prefix + "/" + strings.TrimLeft(clientModel, "/")
			}
			modelKey := strings.ToLower(clientModel)
			if previous, exists := inlineModels[modelKey]; exists {
				return nil, fmt.Errorf("model %s duplicates model %s after normalization", clientModel, previous)
			}
			inlineModels[modelKey] = clientModel
			windows := inheritQuotaWindows(*model.Quota, rawQuota.QuotaWindows)
			schedule, errSchedule := compileProviderSchedule(name, name+"|model:"+modelKey, windows)
			if errSchedule != nil {
				return nil, fmt.Errorf("model %s: %w", clientModel, errSchedule)
			}
			compiled.models[modelKey] = schedule
		}
		providers[name] = compiled
	}
	if errAliases := validateSharedUpstreamSchedules(cfg, providers); errAliases != nil {
		return nil, errAliases
	}
	return providers, nil
}

type configuredQuotaAliases map[string]map[string]map[string]struct{}

func validateSharedUpstreamSchedules(cfg *config.Config, providers map[string]*providerSchedule) error {
	aliases := make(configuredQuotaAliases)
	if cfg != nil {
		for channel, entries := range cfg.OAuthModelAlias {
			provider := strings.ToLower(strings.TrimSpace(channel))
			for _, entry := range entries {
				addConfiguredQuotaAlias(aliases, provider, "", entry.Alias, []string{entry.Name})
			}
		}
		for i := range cfg.ClaudeKey {
			addConfiguredQuotaModels(aliases, "claude", cfg.ClaudeKey[i].Prefix, cfg.ClaudeKey[i].Models)
		}
		for i := range cfg.CodexKey {
			addConfiguredQuotaModels(aliases, "codex", cfg.CodexKey[i].Prefix, cfg.CodexKey[i].Models)
		}
		for i := range cfg.XAIKey {
			addConfiguredQuotaModels(aliases, "xai", cfg.XAIKey[i].Prefix, cfg.XAIKey[i].Models)
		}
		for i := range cfg.GeminiKey {
			addConfiguredQuotaModels(aliases, "gemini", cfg.GeminiKey[i].Prefix, cfg.GeminiKey[i].Models)
		}
		for i := range cfg.InteractionsKey {
			addConfiguredQuotaModels(aliases, "interactions", cfg.InteractionsKey[i].Prefix, cfg.InteractionsKey[i].Models)
		}
		for i := range cfg.VertexCompatAPIKey {
			addConfiguredQuotaModels(aliases, "vertex", cfg.VertexCompatAPIKey[i].Prefix, cfg.VertexCompatAPIKey[i].Models)
		}
		for i := range cfg.OpenAICompatibility {
			entry := &cfg.OpenAICompatibility[i]
			addConfiguredQuotaModels(aliases, strings.ToLower(strings.TrimSpace(entry.Name)), entry.Prefix, entry.Models)
		}
	}

	for providerName, provider := range providers {
		if provider == nil || len(provider.models) < 2 {
			continue
		}
		type policy struct {
			model string
			key   string
		}
		byUpstream := make(map[string]policy)
		for model, schedule := range provider.models {
			upstreams := configuredQuotaUpstreams(aliases[providerName], model)
			policyKey := schedulePolicyKey(schedule)
			for upstream := range upstreams {
				previous, exists := byUpstream[upstream]
				if exists && previous.key != policyKey {
					return fmt.Errorf("provider-quota.%s.models: %q and %q resolve to shared upstream %q with conflicting schedules", providerName, previous.model, model, upstream)
				}
				byUpstream[upstream] = policy{model: model, key: policyKey}
			}
		}
	}
	return nil
}

func addConfiguredQuotaModels[T interface {
	GetName() string
	GetAlias() string
}](aliases configuredQuotaAliases, provider, prefix string, models []T) {
	grouped := make(map[string][]string)
	for i := range models {
		clientModel := strings.TrimSpace(models[i].GetAlias())
		if clientModel == "" {
			clientModel = strings.TrimSpace(models[i].GetName())
		}
		clientModel = prefixedQuotaModel(prefix, clientModel)
		if clientModel == "" {
			continue
		}
		grouped[clientModel] = append(grouped[clientModel], models[i].GetName())
	}
	for clientModel, upstreams := range grouped {
		addConfiguredQuotaAlias(aliases, provider, "", clientModel, upstreams)
	}
}

func addConfiguredQuotaAlias(aliases configuredQuotaAliases, provider, prefix, clientModel string, upstreams []string) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	clientModel = strings.ToLower(strings.TrimSpace(prefixedQuotaModel(prefix, clientModel)))
	upstream := canonicalConfiguredQuotaModels(upstreams, clientModel)
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

func configuredQuotaUpstreams(aliases map[string]map[string]struct{}, model string) map[string]struct{} {
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

func canonicalConfiguredQuotaModels(models []string, fallback string) string {
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

func prefixedQuotaModel(prefix, model string) string {
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	model = strings.TrimLeft(strings.TrimSpace(model), "/")
	if prefix == "" || model == "" {
		return model
	}
	return prefix + "/" + model
}

func schedulePolicyKey(schedule *Schedule) string {
	if schedule == nil {
		return ""
	}
	windowKeys := make([]string, 0, len(schedule.windows))
	for _, window := range schedule.windows {
		windowKeys = append(windowKeys, fmt.Sprintf("%s|%d|%d|%v|%s", strings.ToLower(window.name), window.startMinute, window.endMinute, window.days, quotaBudgetPolicyKey(window.budget)))
	}
	sort.Strings(windowKeys)
	return schedule.location.String() + "|" + fmt.Sprintf("%t", schedule.persist) + "|" + strings.Join(windowKeys, ";")
}

func quotaBudgetPolicyKey(budget *config.QuotaBudget) string {
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

func compileProvider(name string, raw config.ProviderQuota, defaultScope string, disabled bool) (*providerSchedule, error) {
	scope := strings.ToLower(strings.TrimSpace(raw.Scope))
	if scope == "" {
		scope = defaultScope
	}
	provider := &providerSchedule{name: name, scope: scope, disabled: disabled, models: make(map[string]*Schedule), raw: raw}
	if len(raw.Windows) > 0 {
		base, errBase := compileProviderSchedule(name, name+"|provider", raw.QuotaWindows)
		if errBase != nil {
			return nil, errBase
		}
		provider.base = base
	}
	modelNames := make(map[string]string, len(raw.Models))
	for model, modelWindows := range raw.Models {
		modelKey := strings.ToLower(strings.TrimSpace(model))
		if previous, exists := modelNames[modelKey]; exists {
			return nil, fmt.Errorf("model %s duplicates model %s after normalization", model, previous)
		}
		modelNames[modelKey] = model
		modelWindows = inheritQuotaWindows(modelWindows, raw.QuotaWindows)
		schedule, errSchedule := compileProviderSchedule(name, name+"|model:"+modelKey, modelWindows)
		if errSchedule != nil {
			return nil, fmt.Errorf("model %s: %w", model, errSchedule)
		}
		provider.models[modelKey] = schedule
	}
	return provider, nil
}

func inheritQuotaWindows(child, parent config.QuotaWindows) config.QuotaWindows {
	if strings.TrimSpace(child.Timezone) == "" {
		child.Timezone = parent.Timezone
	}
	if child.Persist == nil {
		child.Persist = parent.Persist
	}
	return child
}

type resolvedTarget struct {
	target    coreauth.QuotaWindowTarget
	provider  *providerSchedule
	schedule  *Schedule
	budgetKey string
	conflict  bool
}

func (g *Gate) resolve(auth *coreauth.Auth, model string) (resolvedTarget, bool) {
	if g == nil || g.resolver == nil || auth == nil {
		return resolvedTarget{}, false
	}
	target := g.resolver.ResolveQuotaWindowTarget(auth, model)
	providerName := strings.ToLower(strings.TrimSpace(target.Provider))
	g.mu.RLock()
	provider := g.providers[providerName]
	g.mu.RUnlock()
	if provider == nil {
		return resolvedTarget{target: target}, false
	}
	schedule, conflict := g.sharedUpstreamSchedule(provider, auth, target)
	if conflict {
		return resolvedTarget{target: target, provider: provider, conflict: true}, true
	}
	if schedule == nil {
		schedule = provider.base
	}
	if schedule == nil {
		return resolvedTarget{target: target, provider: provider}, false
	}
	unit := providerName
	if provider.scope == "credential" {
		unit = target.Credential
		if unit == "" {
			unit = target.AuthID
		}
	}
	budgetKey := provider.scope + "|" + providerName + "|" + unit + "|" + strings.ToLower(strings.TrimSpace(target.UpstreamModel))
	policyKey := schedulePolicyKey(schedule)
	g.mu.Lock()
	if previous, exists := g.budgetPolicies[budgetKey]; exists && previous != policyKey {
		g.budgetConflicts[budgetKey] = struct{}{}
	} else if !exists {
		g.budgetPolicies[budgetKey] = policyKey
	}
	_, conflict = g.budgetConflicts[budgetKey]
	g.mu.Unlock()
	return resolvedTarget{target: target, provider: provider, schedule: schedule, budgetKey: budgetKey, conflict: conflict}, true
}

func (g *Gate) sharedUpstreamSchedule(provider *providerSchedule, auth *coreauth.Auth, target coreauth.QuotaWindowTarget) (*Schedule, bool) {
	if g == nil || g.resolver == nil || provider == nil {
		return nil, false
	}
	models := make([]string, 0, len(provider.models))
	for model := range provider.models {
		models = append(models, model)
	}
	sort.Strings(models)
	var selected *Schedule
	selectedPolicy := ""
	for _, model := range models {
		schedule := provider.models[model]
		candidate := g.resolver.ResolveQuotaWindowTarget(auth, model)
		if !strings.EqualFold(candidate.Provider, target.Provider) || !strings.EqualFold(candidate.UpstreamModel, target.UpstreamModel) {
			continue
		}
		policy := schedulePolicyKey(schedule)
		if selected == nil {
			selected = schedule
			selectedPolicy = policy
			continue
		}
		if selectedPolicy != policy {
			return nil, true
		}
	}
	return selected, false
}

type keyBlock struct {
	block coreauth.QuotaWindowBlock
	key   string
}

// BlockedForModel implements auth.QuotaWindowGate.
func (g *Gate) BlockedForModel(auths []*coreauth.Auth, model string, now time.Time) (coreauth.QuotaWindowBlock, bool) {
	if g == nil {
		return coreauth.QuotaWindowBlock{}, false
	}
	g.admissionMu.Lock()
	defer g.admissionMu.Unlock()
	return g.blockedForModel(auths, model, now)
}

func (g *Gate) blockedForModel(auths []*coreauth.Auth, model string, now time.Time) (coreauth.QuotaWindowBlock, bool) {
	providers := make(map[string]map[string]*keyBlock)
	availableKeys := make(map[string]map[string]struct{})
	for _, candidate := range auths {
		if candidate == nil || candidate.Disabled || candidate.Status == coreauth.StatusDisabled {
			continue
		}
		resolved, configured := g.resolve(candidate, model)
		if !configured {
			continue
		}
		if resolved.conflict {
			return coreauth.QuotaWindowBlock{Provider: resolved.target.Provider, Window: "configuration-conflict", Exhausted: []string{"configuration"}}, true
		}
		providerName := strings.ToLower(resolved.target.Provider)
		if providers[providerName] == nil {
			providers[providerName] = make(map[string]*keyBlock)
			availableKeys[providerName] = make(map[string]struct{})
		}
		if _, seen := providers[providerName][resolved.budgetKey]; seen {
			continue
		}
		instance, active := resolved.schedule.InstanceAt(now)
		if !active || instance.Budget == nil {
			availableKeys[providerName][resolved.budgetKey] = struct{}{}
			providers[providerName][resolved.budgetKey] = nil
			continue
		}
		_, exhausted := g.ledger.Snapshot(resolved.budgetKey, instance, instance.Budget)
		if len(exhausted) == 0 {
			availableKeys[providerName][resolved.budgetKey] = struct{}{}
			providers[providerName][resolved.budgetKey] = nil
			continue
		}
		providers[providerName][resolved.budgetKey] = &keyBlock{
			key: resolved.budgetKey,
			block: coreauth.QuotaWindowBlock{
				Provider:    resolved.target.Provider,
				Window:      instance.Name,
				Exhausted:   exhausted,
				AvailableAt: resolved.schedule.NextOpen(now, exhausted),
			},
		}
	}

	var modelBlock coreauth.QuotaWindowBlock
	blocked := false
	for providerName, keys := range providers {
		if len(keys) == 0 || len(availableKeys[providerName]) > 0 {
			continue
		}
		var providerBlock coreauth.QuotaWindowBlock
		for _, key := range keys {
			if key == nil {
				continue
			}
			if providerBlock.Provider == "" || recoveryEarlier(key.block.AvailableAt, providerBlock.AvailableAt) {
				providerBlock = key.block
			}
		}
		if providerBlock.Provider == "" {
			continue
		}
		if !blocked || recoveryLater(providerBlock.AvailableAt, modelBlock.AvailableAt) {
			modelBlock = providerBlock
		}
		blocked = true
	}
	return modelBlock, blocked
}

// AvailableAuths removes credential keys that cannot admit in the active window.
// BlockedForModel must run first so cross-provider exhaustion remains a request-level error.
func (g *Gate) AvailableAuths(auths []*coreauth.Auth, model string, now time.Time) []*coreauth.Auth {
	if g == nil {
		return nil
	}
	g.admissionMu.Lock()
	defer g.admissionMu.Unlock()
	available := make([]*coreauth.Auth, 0, len(auths))
	for _, candidate := range auths {
		if candidate == nil || candidate.Disabled || candidate.Status == coreauth.StatusDisabled {
			continue
		}
		resolved, configured := g.resolve(candidate, model)
		if !configured {
			available = append(available, candidate)
			continue
		}
		if resolved.conflict {
			continue
		}
		instance, active := resolved.schedule.InstanceAt(now)
		if !active || instance.Budget == nil {
			available = append(available, candidate)
			continue
		}
		_, exhausted := g.ledger.Snapshot(resolved.budgetKey, instance, instance.Budget)
		if len(exhausted) == 0 {
			available = append(available, candidate)
		}
	}
	return available
}

func recoveryEarlier(left, right time.Time) bool {
	if left.IsZero() {
		return false
	}
	return right.IsZero() || left.Before(right)
}

func recoveryLater(left, right time.Time) bool {
	if left.IsZero() {
		return true
	}
	return !right.IsZero() && left.After(right)
}

// Admit implements auth.QuotaWindowGate.
func (g *Gate) Admit(auth *coreauth.Auth, model string, now time.Time) (string, bool) {
	if g == nil {
		return "", true
	}
	candidates := g.admissionCandidates(auth, model)
	g.admissionMu.Lock()
	defer g.admissionMu.Unlock()
	if _, blocked := g.blockedForModel(candidates, model, now); blocked {
		return "", false
	}
	resolved, configured := g.resolve(auth, model)
	if !configured {
		return "", true
	}
	if resolved.conflict {
		return "", false
	}
	instance, active := resolved.schedule.InstanceAt(now)
	if !active || instance.Budget == nil {
		return "", true
	}
	clientModels := g.clientModelsSharingBudget(auth, resolved)
	return g.ledger.Admit(CounterRecord{
		BudgetKey:     resolved.budgetKey,
		Provider:      resolved.target.Provider,
		Scope:         resolved.provider.scope,
		ClientModel:   resolved.target.ClientModel,
		ClientModels:  clientModels,
		UpstreamModel: resolved.target.UpstreamModel,
		Credential:    resolved.target.Credential,
		AuthID:        resolved.target.AuthID,
		Instance:      instance,
		Budget:        instance.Budget,
		Persist:       instance.Persist,
	}, now)
}

func (g *Gate) admissionCandidates(selected *coreauth.Auth, model string) []*coreauth.Auth {
	candidates := make([]*coreauth.Auth, 0)
	seen := make(map[string]struct{})
	if g != nil && g.resolver != nil {
		registryRef := registry.GetGlobalRegistry()
		for _, candidate := range g.resolver.QuotaWindowAuths() {
			if candidate == nil || (strings.TrimSpace(model) != "" && !registryRef.ClientSupportsModel(candidate.ID, model)) {
				continue
			}
			seen[candidate.ID] = struct{}{}
			candidates = append(candidates, candidate)
		}
	}
	if selected != nil {
		if _, exists := seen[selected.ID]; !exists {
			candidates = append(candidates, selected)
		}
	}
	return candidates
}

func (g *Gate) clientModelsSharingBudget(auth *coreauth.Auth, resolved resolvedTarget) []string {
	models := map[string]struct{}{resolved.target.ClientModel: {}}
	if resolved.provider != nil {
		for model := range resolved.provider.models {
			target := g.resolver.ResolveQuotaWindowTarget(auth, model)
			if strings.EqualFold(target.Provider, resolved.target.Provider) && strings.EqualFold(target.UpstreamModel, resolved.target.UpstreamModel) {
				models[target.ClientModel] = struct{}{}
			}
		}
	}
	if auth != nil {
		for _, model := range registry.GetGlobalRegistry().GetModelsForClient(auth.ID) {
			if model == nil || strings.TrimSpace(model.ID) == "" {
				continue
			}
			target := g.resolver.ResolveQuotaWindowTarget(auth, model.ID)
			if strings.EqualFold(target.Provider, resolved.target.Provider) && strings.EqualFold(target.UpstreamModel, resolved.target.UpstreamModel) {
				models[target.ClientModel] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(models))
	for model := range models {
		if strings.TrimSpace(model) != "" {
			out = append(out, model)
		}
	}
	sort.Strings(out)
	return out
}

// Close flushes pending persistent consumption.
func (g *Gate) Close() error {
	if g == nil {
		return nil
	}
	g.storeMu.Lock()
	defer g.storeMu.Unlock()
	if g.store == nil {
		return nil
	}
	return g.store.Close()
}

// Reset clears usage for live counters matching the supplied filters.
func (g *Gate) Reset(provider, model, credential string) int {
	if g == nil {
		return 0
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	model = strings.ToLower(strings.TrimSpace(model))
	credential = strings.TrimSpace(credential)
	budgetKeys := make(map[string]struct{})
	if model != "" && g != nil && g.resolver != nil {
		for _, candidate := range g.resolver.List() {
			resolved, configured := g.resolve(candidate, model)
			if configured && !resolved.conflict && strings.EqualFold(resolved.target.Provider, provider) {
				budgetKeys[resolved.budgetKey] = struct{}{}
			}
		}
	}
	g.admissionMu.Lock()
	defer g.admissionMu.Unlock()
	return g.ledger.Reset(func(record CounterRecord) bool {
		if !strings.EqualFold(record.Provider, provider) {
			return false
		}
		if model != "" && !counterMatchesModel(record, model) {
			if _, resolvedMatch := budgetKeys[record.BudgetKey]; !resolvedMatch {
				return false
			}
		}
		if credential != "" && credential != record.Credential && credential != record.AuthID {
			return false
		}
		return true
	})
}

func counterMatchesModel(record CounterRecord, model string) bool {
	if strings.EqualFold(record.ClientModel, model) || strings.EqualFold(record.UpstreamModel, model) {
		return true
	}
	for _, clientModel := range record.ClientModels {
		if strings.EqualFold(clientModel, model) {
			return true
		}
	}
	return false
}

// ManagementSnapshot returns compiled schedules and the full live ledger.
func (g *Gate) ManagementSnapshot(now time.Time, auths []*coreauth.Auth) map[string]any {
	g.mu.RLock()
	names := make([]string, 0, len(g.providers))
	for name := range g.providers {
		names = append(names, name)
	}
	sort.Strings(names)
	providers := make([]map[string]any, 0, len(names))
	for _, name := range names {
		provider := g.providers[name]
		modelSchedules := make(map[string]any, len(provider.models))
		for model, schedule := range provider.models {
			modelSchedules[model] = map[string]any{
				"timezone": schedule.location.String(),
				"persist":  schedule.persist,
				"windows":  schedule.raw.Windows,
			}
		}
		entry := map[string]any{
			"provider":        name,
			"scope":           provider.scope,
			"disabled":        provider.disabled,
			"models":          sortedScheduleModelKeys(provider.models),
			"model_schedules": modelSchedules,
		}
		if provider.base != nil {
			entry["timezone"] = provider.base.location.String()
			entry["persist"] = provider.base.persist
			entry["windows"] = provider.raw.Windows
		}
		providers = append(providers, entry)
	}
	g.mu.RUnlock()
	resolvedKeys := g.managementResolvedKeys(now, auths)
	return map[string]any{
		"now":           now.UTC(),
		"providers":     providers,
		"resolved_keys": resolvedKeys,
		"ledger":        g.ledger.Records(false),
	}
}

func (g *Gate) managementResolvedKeys(now time.Time, auths []*coreauth.Auth) []map[string]any {
	registryRef := registry.GetGlobalRegistry()
	seen := make(map[string]struct{})
	keys := make([]map[string]any, 0)
	for _, candidate := range auths {
		if candidate == nil {
			continue
		}
		for _, model := range registryRef.GetModelsForClient(candidate.ID) {
			if model == nil || strings.TrimSpace(model.ID) == "" {
				continue
			}
			resolved, configured := g.resolve(candidate, model.ID)
			if !configured {
				continue
			}
			if resolved.conflict {
				keys = append(keys, map[string]any{
					"provider":       resolved.target.Provider,
					"model":          model.ID,
					"upstream_model": resolved.target.UpstreamModel,
					"error":          "conflicting schedules for shared upstream budget",
				})
				continue
			}
			identity := resolved.budgetKey + "|" + strings.ToLower(model.ID)
			if _, exists := seen[identity]; exists {
				continue
			}
			seen[identity] = struct{}{}
			entry := map[string]any{
				"provider":       resolved.target.Provider,
				"scope":          resolved.provider.scope,
				"model":          model.ID,
				"upstream_model": resolved.target.UpstreamModel,
				"budget_key":     resolved.budgetKey,
			}
			if resolved.provider.scope == "credential" {
				entry["credential"] = resolved.target.Credential
			}
			if instance, active := resolved.schedule.InstanceAt(now); active {
				entry["instance"] = instance
			}
			keys = append(keys, entry)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		left := fmt.Sprint(keys[i]["provider"], "|", keys[i]["model"], "|", keys[i]["budget_key"])
		right := fmt.Sprint(keys[j]["provider"], "|", keys[j]["model"], "|", keys[j]["budget_key"])
		return left < right
	})
	return keys
}

func sortedScheduleModelKeys(models map[string]*Schedule) []string {
	keys := make([]string, 0, len(models))
	for key := range models {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
