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
	name      string
	scope     string
	disabled  bool
	base      *Schedule
	models    map[string]*Schedule
	upstreams map[string]*Schedule
	raw       config.ProviderQuota
}

// Gate implements selection-time quota checks and owns the persistent ledger.
type Gate struct {
	mu             sync.RWMutex
	admissionMu    sync.Mutex
	resolver       *coreauth.Manager
	providers      map[string]*providerSchedule
	budgetPolicies map[string]string
	// Budget conflicts are sticky until Update clears them.
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
			// Persisted spend cannot be reconstructed safely; fail closed instead of
			// booting with a refunded budget.
			return nil, errLoad
		}
		if errReplace := ledger.Replace(records); errReplace != nil {
			return nil, fmt.Errorf("restore quota-window snapshot: %w", errReplace)
		}
		ledger.SetOnChange(gate.store.Schedule)
	}
	if errUpdate := gate.update(cfg, false); errUpdate != nil {
		_ = gate.Close()
		return nil, errUpdate
	}
	return gate, nil
}

// Update atomically swaps compiled schedules while retaining unchanged live instances.
func (g *Gate) Update(cfg *config.Config) error {
	return g.update(cfg, true)
}

func (g *Gate) update(cfg *config.Config, clearEmptyAuthDir bool) error {
	providers, errCompile := compileProviders(cfg)
	if errCompile != nil {
		return errCompile
	}
	if cfg != nil && (clearEmptyAuthDir || strings.TrimSpace(cfg.AuthDir) != "") {
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

func validateSharedUpstreamSchedules(cfg *config.Config, providers map[string]*providerSchedule) error {
	routeGroups := config.ConfiguredQuotaRouteGroups(cfg)
	for providerName, provider := range providers {
		if provider == nil {
			continue
		}
		type policy struct {
			model string
			key   string
		}
		byUpstream := make(map[string]policy)
		add := func(upstream, model string, schedule *Schedule) error {
			if upstream == "" || schedule == nil {
				return nil
			}
			policyKey := schedulePolicyKey(schedule)
			previous, exists := byUpstream[upstream]
			if exists && previous.key != policyKey {
				return fmt.Errorf("provider-quota.%s.models: %q and %q resolve to shared upstream %q with conflicting schedules", providerName, previous.model, model, upstream)
			}
			byUpstream[upstream] = policy{model: model, key: policyKey}
			provider.upstreams[upstream] = schedule
			return nil
		}
		for _, routes := range routeGroups[providerName] {
			seenUpstreams := make(map[string]struct{}, len(routes))
			for _, upstream := range routes {
				if _, seen := seenUpstreams[upstream]; seen {
					continue
				}
				seenUpstreams[upstream] = struct{}{}
				matchedOverride := false
				for model, schedule := range provider.models {
					resolved := routes[strings.ToLower(strings.TrimSpace(model))]
					if resolved == "" {
						resolved = config.CanonicalQuotaModels(nil, model)
					}
					if resolved != upstream {
						continue
					}
					matchedOverride = true
					if errAdd := add(upstream, model, schedule); errAdd != nil {
						return errAdd
					}
				}
				if !matchedOverride {
					if errAdd := add(upstream, "<provider default>", provider.base); errAdd != nil {
						return errAdd
					}
				}
			}
		}
		for model, schedule := range provider.models {
			if errAdd := add(config.CanonicalQuotaModels(nil, model), model, schedule); errAdd != nil {
				return errAdd
			}
		}
	}
	return nil
}

func schedulePolicyKey(schedule *Schedule) string {
	if schedule == nil {
		return ""
	}
	return config.QuotaWindowsPolicyKey(schedule.raw)
}

func compileProvider(name string, raw config.ProviderQuota, defaultScope string, disabled bool) (*providerSchedule, error) {
	scope := strings.ToLower(strings.TrimSpace(raw.Scope))
	if scope == "" {
		scope = defaultScope
	}
	provider := &providerSchedule{name: name, scope: scope, disabled: disabled, models: make(map[string]*Schedule), upstreams: make(map[string]*Schedule), raw: raw}
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

// The gate is installed even when unconfigured so hot reload can enable quota
// windows later; these checks avoid resolution work until that happens.
func (g *Gate) hasProviders() bool {
	if g == nil {
		return false
	}
	g.mu.RLock()
	hasProviders := len(g.providers) > 0
	g.mu.RUnlock()
	return hasProviders
}

type budgetPolicyFunc func(budgetKey, policyKey string) bool

func (g *Gate) resolveWith(auth *coreauth.Auth, model string, policy budgetPolicyFunc) (resolvedTarget, bool) {
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
	conflict = policy(budgetKey, policyKey)
	return resolvedTarget{target: target, provider: provider, schedule: schedule, budgetKey: budgetKey, conflict: conflict}, true
}

// resolve records budget policies; use it only on admission paths.
func (g *Gate) resolve(auth *coreauth.Auth, model string) (resolvedTarget, bool) {
	return g.resolveWith(auth, model, g.recordBudgetPolicy)
}

// recordBudgetPolicy binds a budget key to its schedule policy and reports
// whether the key is conflicted.
func (g *Gate) recordBudgetPolicy(budgetKey, policyKey string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if previous, exists := g.budgetPolicies[budgetKey]; exists && previous != policyKey {
		g.budgetConflicts[budgetKey] = struct{}{}
	} else if !exists {
		g.budgetPolicies[budgetKey] = policyKey
	}
	_, conflict := g.budgetConflicts[budgetKey]
	return conflict
}

// observedResolver detects conflicts within one reporting request without
// mutating the Gate's sticky admission policy state.
func (g *Gate) observedResolver() targetResolver {
	policy := g.observedBudgetPolicy()
	type cacheKey struct {
		auth  *coreauth.Auth
		model string
	}
	type cacheEntry struct {
		resolved    resolvedTarget
		configured  bool
		policyKey   string
		policyBound bool
	}
	cache := make(map[cacheKey]cacheEntry)
	return func(auth *coreauth.Auth, model string) (resolvedTarget, bool) {
		key := cacheKey{auth: auth, model: model}
		if cached, exists := cache[key]; exists {
			resolved := cached.resolved
			if cached.policyBound {
				resolved.conflict = policy(resolved.budgetKey, cached.policyKey)
			}
			return resolved, cached.configured
		}
		resolved, configured := g.resolveWith(auth, model, policy)
		entry := cacheEntry{resolved: resolved, configured: configured}
		if configured && resolved.schedule != nil && resolved.budgetKey != "" {
			entry.policyKey = schedulePolicyKey(resolved.schedule)
			entry.policyBound = true
		}
		cache[key] = entry
		return resolved, configured
	}
}

func (g *Gate) observedBudgetPolicy() budgetPolicyFunc {
	policies := make(map[string]string)
	conflicts := make(map[string]struct{})
	return func(budgetKey, policyKey string) bool {
		g.mu.RLock()
		globalPolicy, globalPolicyExists := g.budgetPolicies[budgetKey]
		_, globalConflict := g.budgetConflicts[budgetKey]
		g.mu.RUnlock()
		if globalConflict {
			conflicts[budgetKey] = struct{}{}
		}
		previous, exists := policies[budgetKey]
		if !exists && globalPolicyExists {
			previous = globalPolicy
			exists = true
			policies[budgetKey] = globalPolicy
		}
		if exists && previous != policyKey {
			conflicts[budgetKey] = struct{}{}
		} else if !exists {
			policies[budgetKey] = policyKey
		}
		_, conflict := conflicts[budgetKey]
		return conflict
	}
}

func (g *Gate) sharedUpstreamSchedule(provider *providerSchedule, auth *coreauth.Auth, target coreauth.QuotaWindowTarget) (*Schedule, bool) {
	if g == nil || g.resolver == nil || provider == nil {
		return nil, false
	}
	if schedule := provider.upstreams[strings.ToLower(strings.TrimSpace(target.UpstreamModel))]; schedule != nil {
		return schedule, false
	}
	// Fall back to live routing for identities not predictable from configured routes.
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

type targetResolver func(*coreauth.Auth, string) (resolvedTarget, bool)

type resolvedCandidate struct {
	auth       *coreauth.Auth
	resolved   resolvedTarget
	configured bool
}

func (g *Gate) resolveCandidatesUsing(resolve targetResolver, auths []*coreauth.Auth, model string) []resolvedCandidate {
	candidates := make([]resolvedCandidate, 0, len(auths))
	for _, candidate := range auths {
		if candidate == nil || candidate.Disabled || candidate.Status == coreauth.StatusDisabled {
			continue
		}
		resolved, configured := resolve(candidate, model)
		candidates = append(candidates, resolvedCandidate{auth: candidate, resolved: resolved, configured: configured})
	}
	return candidates
}

func (g *Gate) resolveCandidates(auths []*coreauth.Auth, model string) []resolvedCandidate {
	return g.resolveCandidatesUsing(g.resolve, auths, model)
}

func enabledAuths(auths []*coreauth.Auth) []*coreauth.Auth {
	firstExcluded := -1
	for index, auth := range auths {
		if auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled {
			firstExcluded = index
			break
		}
	}
	if firstExcluded < 0 {
		return auths
	}
	filtered := make([]*coreauth.Auth, 0, len(auths)-1)
	filtered = append(filtered, auths[:firstExcluded]...)
	for _, auth := range auths[firstExcluded+1:] {
		if auth != nil && !auth.Disabled && auth.Status != coreauth.StatusDisabled {
			filtered = append(filtered, auth)
		}
	}
	return filtered
}

// EvaluateAuths atomically classifies request-level exhaustion and filters
// credential-level exhaustion from one resolved candidate snapshot.
func (g *Gate) EvaluateAuths(auths []*coreauth.Auth, model string, now time.Time) ([]*coreauth.Auth, coreauth.QuotaWindowBlock, bool) {
	if g == nil || !g.hasProviders() {
		return enabledAuths(auths), coreauth.QuotaWindowBlock{}, false
	}
	candidates := g.resolveCandidates(auths, model)
	g.admissionMu.Lock()
	defer g.admissionMu.Unlock()
	if block, exhausted := g.blockedForResolved(candidates, now); exhausted {
		return nil, block, true
	}
	return g.availableResolved(candidates, now), coreauth.QuotaWindowBlock{}, false
}

// BlockedForModel implements auth.QuotaWindowGate.
func (g *Gate) BlockedForModel(auths []*coreauth.Auth, model string, now time.Time) (coreauth.QuotaWindowBlock, bool) {
	if g == nil || !g.hasProviders() {
		return coreauth.QuotaWindowBlock{}, false
	}
	candidates := g.resolveCandidates(auths, model)
	g.admissionMu.Lock()
	defer g.admissionMu.Unlock()
	return g.blockedForResolved(candidates, now)
}

func (g *Gate) blockedForModelUsing(resolve targetResolver, auths []*coreauth.Auth, model string, now time.Time) (coreauth.QuotaWindowBlock, bool) {
	if g == nil || !g.hasProviders() {
		return coreauth.QuotaWindowBlock{}, false
	}
	return g.blockedForResolved(g.resolveCandidatesUsing(resolve, auths, model), now)
}

func (g *Gate) blockedForResolved(candidates []resolvedCandidate, now time.Time) (coreauth.QuotaWindowBlock, bool) {
	providers := make(map[string]map[string]*keyBlock)
	availableKeys := make(map[string]map[string]struct{})
	for _, candidate := range candidates {
		resolved := candidate.resolved
		if !candidate.configured {
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
	if !g.hasProviders() {
		return enabledAuths(auths)
	}
	candidates := g.resolveCandidates(auths, model)
	g.admissionMu.Lock()
	defer g.admissionMu.Unlock()
	return g.availableResolved(candidates, now)
}

func (g *Gate) availableAuthsUsing(resolve targetResolver, auths []*coreauth.Auth, model string, now time.Time) []*coreauth.Auth {
	if g == nil {
		return nil
	}
	if !g.hasProviders() {
		return enabledAuths(auths)
	}
	return g.availableResolved(g.resolveCandidatesUsing(resolve, auths, model), now)
}

func (g *Gate) availableResolved(candidates []resolvedCandidate, now time.Time) []*coreauth.Auth {
	available := make([]*coreauth.Auth, 0, len(candidates))
	for _, candidate := range candidates {
		resolved := candidate.resolved
		if !candidate.configured {
			available = append(available, candidate.auth)
			continue
		}
		if resolved.conflict {
			continue
		}
		instance, active := resolved.schedule.InstanceAt(now)
		if !active || instance.Budget == nil {
			available = append(available, candidate.auth)
			continue
		}
		_, exhausted := g.ledger.Snapshot(resolved.budgetKey, instance, instance.Budget)
		if len(exhausted) == 0 {
			available = append(available, candidate.auth)
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
	if !g.hasProviders() {
		return "", true
	}
	candidates := g.admissionCandidates(auth, model)
	// Resolve every sibling first so cross-auth policies are recorded before the
	// admission decision. Resolve the exact selected auth separately because the
	// manager candidates are fresh clones that may reflect a concurrent update.
	resolvedCandidates := g.resolveCandidates(candidates, model)
	resolved, configured := g.resolve(auth, model)
	var instance Instance
	active := false
	var clientModels []string
	if configured && !resolved.conflict {
		instance, active = resolved.schedule.InstanceAt(now)
		if active && instance.Budget != nil {
			clientModels = g.clientModelsSharingBudget(auth, resolved)
		}
	}
	g.admissionMu.Lock()
	defer g.admissionMu.Unlock()
	if _, blocked := g.blockedForResolved(resolvedCandidates, now); blocked {
		return "", false
	}
	if !configured {
		return "", true
	}
	if resolved.conflict {
		return "", false
	}
	if !active || instance.Budget == nil {
		return "", true
	}
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
		for _, candidate := range g.resolver.QuotaWindowAuthsForModel(model) {
			if candidate == nil {
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
	if g == nil || !g.hasProviders() {
		return 0
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	model = strings.ToLower(strings.TrimSpace(model))
	credential = strings.TrimSpace(credential)
	budgetKeys := make(map[string]struct{})
	if model != "" && g != nil && g.resolver != nil {
		resolve := g.observedResolver()
		for _, candidate := range g.resolver.List() {
			if candidate == nil || candidate.Disabled || candidate.Status == coreauth.StatusDisabled {
				continue
			}
			resolved, configured := resolve(candidate, model)
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
	if g == nil || !g.hasProviders() {
		return nil
	}
	resolve := g.observedResolver()
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
			resolved, configured := resolve(candidate, model.ID)
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
