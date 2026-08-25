package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

// QuotaWindowBlock describes an exhausted budget key backing a requested model.
type QuotaWindowBlock struct {
	Provider    string
	Window      string
	Exhausted   []string
	AvailableAt time.Time
}

// QuotaWindowGate reports and reserves configured quota-window budget.
type QuotaWindowGate interface {
	BlockedForModel(auths []*Auth, model string, now time.Time) (QuotaWindowBlock, bool)
	Admit(auth *Auth, model string, now time.Time) (string, bool)
}

type quotaWindowGateAvailability interface {
	AvailableAuths(auths []*Auth, model string, now time.Time) []*Auth
}

type quotaWindowGateEvaluation interface {
	EvaluateAuths(auths []*Auth, model string, now time.Time) ([]*Auth, QuotaWindowBlock, bool)
}

func quotaWindowAvailableAuths(gate QuotaWindowGate, auths []*Auth, model string, now time.Time) []*Auth {
	if availability, ok := gate.(quotaWindowGateAvailability); ok && availability != nil {
		return availability.AvailableAuths(auths, model, now)
	}
	return auths
}

func evaluateQuotaWindowAuths(gate QuotaWindowGate, auths []*Auth, model string, now time.Time) ([]*Auth, QuotaWindowBlock, bool) {
	if evaluation, ok := gate.(quotaWindowGateEvaluation); ok && evaluation != nil {
		return evaluation.EvaluateAuths(auths, model, now)
	}
	if block, exhausted := gate.BlockedForModel(auths, model, now); exhausted {
		return nil, block, true
	}
	originalAuths := auths
	auths = quotaWindowAvailableAuths(gate, originalAuths, model, now)
	if len(auths) != len(originalAuths) {
		if block, exhausted := gate.BlockedForModel(originalAuths, model, now); exhausted {
			return nil, block, true
		}
	}
	return auths, QuotaWindowBlock{}, false
}

// QuotaWindowCountTokensMetering lets executors identify local-only token estimators.
type QuotaWindowCountTokensMetering interface {
	QuotaWindowCountTokensUsesUpstream(auth *Auth) bool
}

// QuotaWindowTarget is the credential-aware billing identity used by quota gates.
type QuotaWindowTarget struct {
	Provider           string
	ClientModel        string
	SharedClientModels []string
	UpstreamModel      string
	Credential         string
	AuthID             string
	RoutingConflict    bool
	RouteKey           string
}

type quotaWindowAuthRoutes struct {
	byClient            map[string]string
	byUpstream          map[string][]string
	routeKeysByUpstream map[string]string
}

type quotaWindowRouteTable map[string]quotaWindowAuthRoutes

type quotaWindowReservationContextKey struct{}
type quotaWindowRouteModelContextKey struct{}
type quotaWindowAttemptAdmitterContextKey struct{}
type quotaWindowAttemptSequenceContextKey struct{}
type quotaWindowReservationSettlerContextKey struct{}
type quotaWindowSelectorGateBypassContextKey struct{}

const quotaWindowBillingModelMetadataKey = "cliproxy.quota_window_billing_model"

type quotaWindowAttemptSequence struct {
	mu          sync.Mutex
	initialUsed bool
}

type quotaWindowAttemptAdmitter func(context.Context) (context.Context, error)
type quotaWindowReservationSettler func(string, coreusage.Detail)

type quotaWindowGateReservationSettler interface {
	SettleQuotaWindowReservation(string, coreusage.Detail)
}

// WithQuotaWindowModel identifies the client-visible model for a direct provider request.
func WithQuotaWindowModel(ctx context.Context, model string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, quotaWindowRouteModelContextKey{}, strings.TrimSpace(model))
}

func quotaWindowModelFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	model, _ := ctx.Value(quotaWindowRouteModelContextKey{}).(string)
	return strings.TrimSpace(model)
}

// WithQuotaWindowReservation stores the reservation for final usage settlement.
func WithQuotaWindowReservation(ctx context.Context, reservation string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, quotaWindowReservationContextKey{}, strings.TrimSpace(reservation))
}

// QuotaWindowReservationFromContext returns the quota reservation attached to ctx.
func QuotaWindowReservationFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	reservation, _ := ctx.Value(quotaWindowReservationContextKey{}).(string)
	return strings.TrimSpace(reservation)
}

func withQuotaWindowBillingModel(opts cliproxyexecutor.Options, model string) cliproxyexecutor.Options {
	model = strings.TrimSpace(model)
	if model == "" {
		return opts
	}
	metadata := make(map[string]any, len(opts.Metadata)+1)
	for key, value := range opts.Metadata {
		metadata[key] = value
	}
	metadata[quotaWindowBillingModelMetadataKey] = model
	opts.Metadata = metadata
	return opts
}

func quotaWindowBillingModel(opts cliproxyexecutor.Options, fallback string) string {
	if raw, ok := opts.Metadata[quotaWindowBillingModelMetadataKey]; ok {
		if model, okString := raw.(string); okString && strings.TrimSpace(model) != "" {
			return strings.TrimSpace(model)
		}
	}
	return strings.TrimSpace(fallback)
}

func withQuotaWindowSelectorGateBypass(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, quotaWindowSelectorGateBypassContextKey{}, true)
}

func selectorQuotaWindowGate(ctx context.Context, binding *quotaGateBinding) QuotaWindowGate {
	if ctx != nil {
		if bypass, _ := ctx.Value(quotaWindowSelectorGateBypassContextKey{}).(bool); bypass {
			return nil
		}
	}
	return binding.get()
}

// QuotaWindowContextForUpstreamAttempt assigns a fresh reservation to executor-internal
// follow-up dials while preserving the manager's initial reservation for the first dial.
func QuotaWindowContextForUpstreamAttempt(ctx context.Context) (context.Context, error) {
	if ctx == nil {
		return context.Background(), nil
	}
	sequence, _ := ctx.Value(quotaWindowAttemptSequenceContextKey{}).(*quotaWindowAttemptSequence)
	if sequence == nil {
		return ctx, nil
	}
	sequence.mu.Lock()
	if !sequence.initialUsed {
		sequence.initialUsed = true
		sequence.mu.Unlock()
		return ctx, nil
	}
	sequence.mu.Unlock()
	admit, _ := ctx.Value(quotaWindowAttemptAdmitterContextKey{}).(quotaWindowAttemptAdmitter)
	if admit == nil {
		return ctx, nil
	}
	return admit(ctx)
}

// FinishQuotaWindowUpstreamAttempt releases the token reservation for an upstream
// attempt that completed without a usage record. The request admission remains counted.
func FinishQuotaWindowUpstreamAttempt(ctx context.Context) {
	SettleQuotaWindowUpstreamAttempt(ctx, coreusage.Detail{})
}

// SettleQuotaWindowUpstreamAttempt applies token usage to the reservation on ctx.
func SettleQuotaWindowUpstreamAttempt(ctx context.Context, detail coreusage.Detail) {
	if ctx == nil {
		return
	}
	reservation := QuotaWindowReservationFromContext(ctx)
	if reservation == "" {
		return
	}
	settle, _ := ctx.Value(quotaWindowReservationSettlerContextKey{}).(quotaWindowReservationSettler)
	if settle != nil {
		settle(reservation, detail)
	}
}

type quotaGateBinding struct {
	mu   sync.RWMutex
	gate QuotaWindowGate
}

func (b *quotaGateBinding) set(gate QuotaWindowGate) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.gate = gate
	b.mu.Unlock()
}

func (b *quotaGateBinding) get() QuotaWindowGate {
	if b == nil {
		return nil
	}
	b.mu.RLock()
	gate := b.gate
	b.mu.RUnlock()
	return gate
}

type quotaWindowGateAware interface {
	setQuotaWindowGate(QuotaWindowGate)
}

func setSelectorQuotaWindowGate(selector Selector, gate QuotaWindowGate) {
	if aware, ok := selector.(quotaWindowGateAware); ok && aware != nil {
		aware.setQuotaWindowGate(gate)
	}
}

func (s *RoundRobinSelector) setQuotaWindowGate(gate QuotaWindowGate) {
	if s != nil {
		s.quota.set(gate)
	}
}

func (s *WeightedRoundRobinSelector) setQuotaWindowGate(gate QuotaWindowGate) {
	if s != nil {
		s.quota.set(gate)
	}
}

func (s *FillFirstSelector) setQuotaWindowGate(gate QuotaWindowGate) {
	if s != nil {
		s.quota.set(gate)
	}
}

func (s *SessionAffinitySelector) setQuotaWindowGate(gate QuotaWindowGate) {
	if s == nil {
		return
	}
	s.quota.set(gate)
	setSelectorQuotaWindowGate(s.fallback, gate)
}

// SetQuotaWindowGate installs the pre-selection quota gate. A nil gate disables it.
func (m *Manager) SetQuotaWindowGate(gate QuotaWindowGate) {
	if m == nil {
		return
	}
	m.quotaWindowGate.set(gate)
	m.mu.RLock()
	selector := m.selector
	m.mu.RUnlock()
	setSelectorQuotaWindowGate(selector, gate)
}

func (m *Manager) quotaWindowGateSnapshot() QuotaWindowGate {
	if m == nil {
		return nil
	}
	return m.quotaWindowGate.get()
}

// ResolveQuotaWindowTarget returns the stable provider, upstream-model, and credential identity.
func (m *Manager) ResolveQuotaWindowTarget(auth *Auth, routeModel string) QuotaWindowTarget {
	target := QuotaWindowTarget{AuthID: ""}
	if auth == nil {
		return target
	}
	target.AuthID = auth.ID
	routing := m.loadAPIKeyModelRouting()
	clientModel := strings.TrimSpace(thinking.ParseSuffix(strings.TrimSpace(routeModel)).ModelName)
	if clientModel == "" {
		clientModel = strings.TrimSpace(routeModel)
	}
	target.ClientModel = clientModel
	requestedModel := rewriteModelForAuth(clientModel, auth)

	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	providerKey := ""
	compatName := ""
	if auth.Attributes != nil {
		providerKey = strings.TrimSpace(auth.Attributes["provider_key"])
		compatName = strings.TrimSpace(auth.Attributes["compat_name"])
	}
	if entry := resolveOpenAICompatConfigForAuth(routing.config, auth, providerKey, compatName); entry != nil && (compatName != "" || strings.EqualFold(provider, "openai-compatibility") || strings.HasPrefix(provider, "openai-compatible-")) {
		provider = strings.ToLower(strings.TrimSpace(entry.Name))
	}
	switch provider {
	case "gemini-cli":
		provider = "gemini"
	case "gemini-interactions":
		provider = "interactions"
	}
	target.Provider = provider

	compiledUpstream := ""
	routeFingerprint := routing.quotaRouteFingerprints[auth.ID]
	routeFingerprintMatches := routeFingerprint != "" && routeFingerprint == quotaWindowRoutingFingerprint(auth)
	if routeFingerprint != "" && !routeFingerprintMatches {
		target.RoutingConflict = true
	}
	if routeFingerprintMatches {
		routes := routing.quotaRoutes[auth.ID]
		compiledUpstream = routes.byClient[strings.ToLower(strings.TrimSpace(target.ClientModel))]
	}
	aliasResult := homeForceMappingAliasResult(auth, requestedModel)
	if !target.RoutingConflict && !aliasResult.ForceMapping && compiledUpstream == "" {
		aliasResult = m.resolveExecutionAliasResultForRequestedWithRouting(routing, auth, requestedModel)
	}
	poolModel := executionAliasPoolModel(auth, requestedModel, aliasResult)
	candidates := []string(nil)
	if auth.Attributes != nil {
		if homeModel := strings.TrimSpace(auth.Attributes[homeUpstreamModelAttributeKey]); homeModel != "" {
			candidates = []string{homeModel}
		}
	}
	if len(candidates) == 0 && compiledUpstream != "" {
		candidates = []string{compiledUpstream}
	}
	if len(candidates) == 0 && !target.RoutingConflict {
		if pool := resolveOpenAICompatUpstreamModelPool(routing.config, auth, poolModel); len(pool) > 0 {
			candidates = append(candidates, pool...)
		} else {
			resolved := m.applyAPIKeyModelAliasWithRouting(routing, auth, poolModel)
			if strings.TrimSpace(resolved) == "" {
				resolved = poolModel
			}
			candidates = []string{resolved}
		}
	}
	target.UpstreamModel = canonicalQuotaModels(candidates, requestedModel)
	target.SharedClientModels, target.RouteKey = m.quotaWindowSharedClientModels(routing, auth, routeModel, target.ClientModel, target.UpstreamModel)

	credentialSource := strings.TrimSpace(cooldownAuthFile(auth))
	if credentialSource == "" {
		credentialSource = strings.TrimSpace(auth.ID)
	}
	if credentialSource != "" {
		sum := sha256.Sum256([]byte(credentialSource))
		target.Credential = hex.EncodeToString(sum[:])
	}
	return target
}

func canonicalQuotaModels(models []string, fallback string) string {
	return internalconfig.CanonicalQuotaModels(models, fallback)
}

func quotaWindowRoutingFingerprint(auth *Auth) string {
	if auth == nil {
		return ""
	}
	hash := sha256.New()
	write := func(value string) {
		_, _ = hash.Write([]byte(strconv.Itoa(len(value))))
		_, _ = hash.Write([]byte{':'})
		_, _ = hash.Write([]byte(value))
	}
	write(auth.ID)
	write(auth.Provider)
	write(auth.Prefix)
	write(auth.FileName)
	write(auth.ProxyURL)
	write(auth.AuthKind())
	write(auth.AuthSourceKind())
	keys := make([]string, 0, len(auth.Attributes))
	for key := range auth.Attributes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		write(key)
		write(auth.Attributes[key])
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func compileQuotaWindowRoutesForAuth(cfg *internalconfig.Config, auth *Auth) quotaWindowAuthRoutes {
	if cfg == nil || auth == nil || !isConfiguredModelRoutingAuth(auth) {
		return quotaWindowAuthRoutes{}
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	switch provider {
	case "gemini":
		if entry := resolveGeminiAPIKeyConfig(cfg, auth); entry != nil {
			return compileQuotaWindowRoutes(auth.Prefix, entry.Models)
		}
	case "gemini-interactions":
		if entry := resolveInteractionsAPIKeyConfig(cfg, auth); entry != nil {
			return compileQuotaWindowRoutes(auth.Prefix, entry.Models)
		}
	case "claude":
		if entry := resolveClaudeAPIKeyConfig(cfg, auth); entry != nil {
			return compileQuotaWindowRoutes(auth.Prefix, entry.Models)
		}
	case "codex":
		if entry := resolveCodexAPIKeyConfig(cfg, auth); entry != nil {
			return compileQuotaWindowRoutes(auth.Prefix, entry.Models)
		}
	case "xai":
		if entry := resolveXAIAPIKeyConfig(cfg, auth); entry != nil {
			return compileQuotaWindowRoutes(auth.Prefix, entry.Models)
		}
	case "vertex":
		if entry := resolveVertexAPIKeyConfig(cfg, auth); entry != nil {
			return compileQuotaWindowRoutes(auth.Prefix, entry.Models)
		}
	default:
		providerKey := ""
		compatName := ""
		if auth.Attributes != nil {
			providerKey = strings.TrimSpace(auth.Attributes["provider_key"])
			compatName = strings.TrimSpace(auth.Attributes["compat_name"])
		}
		if entry := resolveOpenAICompatConfigForAuth(cfg, auth, providerKey, compatName); entry != nil {
			return compileQuotaWindowRoutes(auth.Prefix, entry.Models)
		}
	}
	return quotaWindowAuthRoutes{}
}

func compileQuotaWindowRoutes[T interface {
	GetName() string
	GetAlias() string
}](prefix string, models []T) quotaWindowAuthRoutes {
	grouped := make(map[string][]string)
	clientOrder := make([]string, 0, len(models))
	for i := range models {
		clientModel := strings.TrimSpace(models[i].GetAlias())
		if clientModel == "" {
			clientModel = strings.TrimSpace(models[i].GetName())
		}
		clientModel = prefixedQuotaWindowClientModel(prefix, clientModel)
		if clientModel != "" {
			if _, exists := grouped[clientModel]; !exists {
				clientOrder = append(clientOrder, clientModel)
			}
			grouped[clientModel] = append(grouped[clientModel], models[i].GetName())
		}
	}
	routes := quotaWindowAuthRoutes{byClient: make(map[string]string), byUpstream: make(map[string][]string), routeKeysByUpstream: make(map[string]string)}
	for _, clientModel := range clientOrder {
		upstreams := grouped[clientModel]
		upstream := internalconfig.CanonicalQuotaModels(upstreams, clientModel)
		if upstream != "" {
			routes.byClient[clientModel] = upstream
			canonicalClient := internalconfig.CanonicalQuotaModels(nil, clientModel)
			if _, exists := routes.byClient[canonicalClient]; !exists {
				routes.byClient[canonicalClient] = upstream
			}
			routes.byUpstream[upstream] = append(routes.byUpstream[upstream], clientModel)
		}
	}
	for upstream := range routes.byUpstream {
		sort.Strings(routes.byUpstream[upstream])
		routes.routeKeysByUpstream[upstream] = internalconfig.QuotaRouteKey(upstream, routes.byUpstream[upstream])
	}
	return routes
}

func prefixedQuotaWindowClientModel(prefix, model string) string {
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	model = strings.TrimLeft(strings.TrimSpace(model), "/")
	if prefix != "" && model != "" {
		model = prefix + "/" + model
	}
	return strings.ToLower(model)
}

func (m *Manager) quotaWindowSharedClientModels(routing *apiKeyModelRoutingSnapshot, auth *Auth, routeModel, clientModel, upstreamModel string) ([]string, string) {
	upstreamModel = strings.ToLower(strings.TrimSpace(upstreamModel))
	if isConfiguredModelRoutingAuth(auth) && routing != nil && auth != nil {
		routes := routing.quotaRoutes[auth.ID]
		if key := routes.routeKeysByUpstream[upstreamModel]; key != "" {
			return nil, key
		}
	}
	seen := make(map[string]struct{})
	models := make([]string, 0, 4)
	add := func(model string) {
		model = strings.ToLower(strings.TrimSpace(model))
		if model == "" {
			return
		}
		if _, exists := seen[model]; exists {
			return
		}
		seen[model] = struct{}{}
		models = append(models, model)
	}
	add(routeModel)
	add(clientModel)
	if isConfiguredModelRoutingAuth(auth) {
		if routing != nil && auth != nil {
			for _, model := range routing.quotaRoutes[auth.ID].byUpstream[upstreamModel] {
				add(model)
			}
		}
	} else {
		oauthModels, key, complete := m.oauthQuotaWindowClientModels(auth, upstreamModel)
		if complete {
			return nil, key
		}
		for _, model := range oauthModels {
			add(model)
		}
	}
	sort.Strings(models)
	return models, ""
}

func (m *Manager) oauthQuotaWindowClientModels(auth *Auth, upstreamModel string) ([]string, string, bool) {
	if m == nil || auth == nil || upstreamModel == "" {
		return nil, "", false
	}
	channel := modelAliasChannel(auth)
	if channel == "" {
		return nil, "", false
	}
	perAuth := make(map[string]string)
	for _, entry := range OAuthModelAliasesFromAttributes(authAttributes(auth)) {
		alias := strings.ToLower(strings.TrimSpace(entry.Alias))
		if alias == "" {
			continue
		}
		if _, exists := perAuth[alias]; !exists {
			perAuth[alias] = internalconfig.CanonicalQuotaModels(nil, entry.Name)
		}
	}
	table, _ := m.oauthModelAlias.Load().(*oauthModelAliasTable)
	if len(perAuth) == 0 && table != nil {
		models := table.quotaRoutes[channel][upstreamModel]
		if key := table.quotaRouteKeys[channel][upstreamModel]; key != "" {
			return models, key, true
		}
	}
	models := make([]string, 0, len(perAuth)+4)
	seen := make(map[string]struct{})
	add := func(model string) {
		if _, exists := seen[model]; exists {
			return
		}
		seen[model] = struct{}{}
		models = append(models, model)
	}
	if table != nil {
		for _, alias := range table.quotaRoutes[channel][upstreamModel] {
			if overridden, exists := perAuth[alias]; exists && overridden != upstreamModel {
				continue
			}
			add(alias)
		}
	}
	for alias, upstream := range perAuth {
		if upstream == upstreamModel {
			add(alias)
		}
	}
	sort.Strings(models)
	return models, "", false
}

// QuotaWindowCooldown reports the ordinary cooldown state for a candidate set.
func (m *Manager) QuotaWindowCooldown(auths []*Auth, model string, now time.Time) (time.Time, bool) {
	if m == nil || len(auths) == 0 {
		return time.Time{}, false
	}
	cooldownCount := 0
	var earliest time.Time
	for _, candidate := range auths {
		checkModel := m.selectionModelForAuth(candidate, model)
		blocked, reason, next := isAuthBlockedForModel(candidate, checkModel, now)
		if !blocked {
			return time.Time{}, false
		}
		if reason == blockReasonCooldown && !next.IsZero() {
			cooldownCount++
			if earliest.IsZero() || next.Before(earliest) {
				earliest = next
			}
		}
	}
	return earliest, cooldownCount == len(auths) && !earliest.IsZero()
}

type quotaWindowError struct {
	model string
	block QuotaWindowBlock
	now   time.Time
}

func newQuotaWindowError(model string, block QuotaWindowBlock, now time.Time) *quotaWindowError {
	if now.IsZero() {
		now = time.Now()
	}
	return &quotaWindowError{model: model, block: block, now: now}
}

// retryDelay reports the recovery delay and whether a recovery time is known.
// A zero AvailableAt is a permanent or configuration block, not an immediate retry.
func (e *quotaWindowError) retryDelay() (time.Duration, bool) {
	if e.block.AvailableAt.IsZero() {
		return 0, false
	}
	resetIn := e.block.AvailableAt.Sub(e.now)
	if resetIn < 0 {
		resetIn = 0
	}
	return resetIn, true
}

func (e *quotaWindowError) Error() string {
	message := fmt.Sprintf("Model %s exhausted its %s quota for provider %s", e.model, e.block.Window, e.block.Provider)
	delay, known := e.retryDelay()
	var resetTime any
	var resetSeconds any
	if known {
		resetSeconds = int(math.Ceil(delay.Seconds()))
		displayDuration := delay.Round(time.Second)
		if delay > 0 && delay < time.Second {
			displayDuration = time.Second
		}
		resetTime = displayDuration.String()
	}
	errorBody := map[string]any{
		"code":          "quota_window_exhausted",
		"message":       message,
		"model":         e.model,
		"provider":      e.block.Provider,
		"window":        e.block.Window,
		"exhausted":     append([]string(nil), e.block.Exhausted...),
		"available_at":  nil,
		"reset_time":    resetTime,
		"reset_seconds": resetSeconds,
	}
	if !e.block.AvailableAt.IsZero() {
		errorBody["available_at"] = e.block.AvailableAt.UTC().Format(time.RFC3339)
	}
	payload, err := json.Marshal(map[string]any{"error": errorBody})
	if err != nil {
		return fmt.Sprintf(`{"error":{"code":"quota_window_exhausted","message":%q}}`, message)
	}
	return string(payload)
}

func (e *quotaWindowError) StatusCode() int { return http.StatusTooManyRequests }

func (e *quotaWindowError) Headers() http.Header {
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	delay, known := e.retryDelay()
	if !known {
		return headers
	}
	seconds, positive := safeRetryAfterSeconds(delay)
	if !positive {
		// A known recovery instant that has just passed means the client may retry now.
		seconds = 0
	}
	headers.Set("Retry-After", strconv.FormatInt(seconds, 10))
	return headers
}

func isQuotaWindowError(err error) bool {
	var quotaErr *quotaWindowError
	return errors.As(err, &quotaErr) && quotaErr != nil
}

// IsQuotaWindowError reports whether err is a local provider quota-window denial.
func IsQuotaWindowError(err error) bool {
	return isQuotaWindowError(err)
}

var errQuotaWindowCredentialExhausted = errors.New("quota-window credential exhausted during admission")

func (m *Manager) quotaWindowAttemptContext(ctx context.Context, auth *Auth, model string) (context.Context, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	gate := m.quotaWindowGateSnapshot()
	if gate == nil {
		return ctx, nil
	}
	if _, exists := ctx.Value(quotaWindowReservationSettlerContextKey{}).(quotaWindowReservationSettler); !exists {
		if settler, ok := gate.(quotaWindowGateReservationSettler); ok && settler != nil {
			settle := quotaWindowReservationSettler(settler.SettleQuotaWindowReservation)
			ctx = context.WithValue(ctx, quotaWindowReservationSettlerContextKey{}, settle)
		}
	}
	if _, exists := ctx.Value(quotaWindowAttemptAdmitterContextKey{}).(quotaWindowAttemptAdmitter); !exists {
		admit := quotaWindowAttemptAdmitter(func(next context.Context) (context.Context, error) {
			return m.quotaWindowAttemptContext(next, auth, model)
		})
		ctx = context.WithValue(ctx, quotaWindowAttemptAdmitterContextKey{}, admit)
		ctx = context.WithValue(ctx, quotaWindowAttemptSequenceContextKey{}, &quotaWindowAttemptSequence{})
	}
	reservation, admitted := gate.Admit(auth, model, time.Now())
	if admitted {
		return WithQuotaWindowReservation(ctx, reservation), nil
	}
	candidates := m.quotaWindowCandidates(model)
	blockedAt := time.Now()
	if block, exhausted := gate.BlockedForModel(candidates, model, blockedAt); exhausted {
		return nil, newQuotaWindowError(model, block, blockedAt)
	}
	return nil, errQuotaWindowCredentialExhausted
}

// AdmitQuotaWindowAttempt reserves a direct upstream request before its dial.
func (m *Manager) AdmitQuotaWindowAttempt(ctx context.Context, auth *Auth, model string) (context.Context, error) {
	return m.quotaWindowAttemptContext(WithQuotaWindowModel(ctx, model), auth, model)
}

// quotaWindowCandidates builds the route-aware set used to classify a denied
// selected credential before deciding whether another credential can rotate in.
func (m *Manager) quotaWindowCandidates(model string) []*Auth {
	if m == nil {
		return nil
	}
	registryRef := registry.GetGlobalRegistry()
	m.mu.RLock()
	selector := m.selector
	candidates := make([]*Auth, 0, len(m.auths))
	for _, candidate := range m.auths {
		if candidate == nil || candidate.Disabled || candidate.Status == StatusDisabled {
			continue
		}
		if strings.TrimSpace(model) != "" && !m.authSupportsRouteModel(registryRef, candidate, model) {
			continue
		}
		candidates = append(candidates, candidate.cloneForQuotaWindow())
	}
	m.mu.RUnlock()
	return selectorAvailabilityCandidates(selector, candidates)
}

// cloneForQuotaWindow copies only the reference-typed fields read by admission:
// identity, Prefix/FileName, Attributes, Metadata, and Runtime. ModelStates is
// deliberately omitted to avoid cloning every per-model cooldown on each upstream
// attempt. Keep this in sync with Auth.Clone if Auth gains new reference fields.
func (a *Auth) cloneForQuotaWindow() *Auth {
	if a == nil {
		return nil
	}
	copyAuth := *a
	if len(a.Attributes) > 0 {
		copyAuth.Attributes = make(map[string]string, len(a.Attributes))
		for key, value := range a.Attributes {
			copyAuth.Attributes[key] = value
		}
	}
	if len(a.Metadata) > 0 {
		copyAuth.Metadata = make(map[string]any, len(a.Metadata))
		for key, value := range a.Metadata {
			copyAuth.Metadata[key] = value
		}
	}
	copyAuth.ModelStates = nil
	copyAuth.Runtime = a.Runtime
	return &copyAuth
}

// QuotaWindowAuthsForModel returns lightweight admission candidates that can
// serve model. Reporting callers must use QuotaWindowAuths to retain ModelStates.
func (m *Manager) QuotaWindowAuthsForModel(model string) []*Auth {
	if m == nil {
		return nil
	}
	registryRef := registry.GetGlobalRegistry()
	m.mu.RLock()
	selector := m.selector
	auths := make([]*Auth, 0, len(m.auths))
	for _, auth := range m.auths {
		if auth == nil || auth.Disabled || auth.Status == StatusDisabled {
			continue
		}
		if strings.TrimSpace(model) != "" && !registryRef.ClientSupportsModel(auth.ID, model) {
			continue
		}
		auths = append(auths, auth.cloneForQuotaWindow())
	}
	m.mu.RUnlock()
	return selectorAvailabilityCandidates(selector, auths)
}

// QuotaWindowAuths returns the credentials that the configured selector can use.
func (m *Manager) QuotaWindowAuths() []*Auth {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	selector := m.selector
	auths := make([]*Auth, 0, len(m.auths))
	for _, auth := range m.auths {
		if auth == nil || auth.Disabled || auth.Status == StatusDisabled {
			continue
		}
		auths = append(auths, auth.Clone())
	}
	m.mu.RUnlock()
	return selectorAvailabilityCandidates(selector, auths)
}

func (m *Manager) executeQuotaAttempt(ctx context.Context, executor ProviderExecutor, auth *Auth, routeModel string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	attemptCtx, errAdmit := m.quotaWindowAttemptContext(ctx, auth, routeModel)
	if errAdmit != nil {
		return cliproxyexecutor.Response{}, errAdmit
	}
	response, errExecute := executor.Execute(attemptCtx, auth, req, opts)
	if errExecute != nil {
		FinishQuotaWindowUpstreamAttempt(attemptCtx)
	}
	return response, errExecute
}

func (m *Manager) countQuotaAttempt(ctx context.Context, executor ProviderExecutor, auth *Auth, routeModel string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if metering, ok := executor.(QuotaWindowCountTokensMetering); ok && !metering.QuotaWindowCountTokensUsesUpstream(auth) {
		return executor.CountTokens(ctx, auth, req, opts)
	}
	attemptCtx, errAdmit := m.quotaWindowAttemptContext(ctx, auth, routeModel)
	if errAdmit != nil {
		return cliproxyexecutor.Response{}, errAdmit
	}
	response, errCount := executor.CountTokens(attemptCtx, auth, req, opts)
	FinishQuotaWindowUpstreamAttempt(attemptCtx)
	return response, errCount
}

func (m *Manager) streamQuotaAttempt(ctx context.Context, executor ProviderExecutor, auth *Auth, routeModel string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	attemptCtx, errAdmit := m.quotaWindowAttemptContext(ctx, auth, routeModel)
	if errAdmit != nil {
		return nil, errAdmit
	}
	result, errStream := executor.ExecuteStream(attemptCtx, auth, req, opts)
	if errStream != nil {
		FinishQuotaWindowUpstreamAttempt(attemptCtx)
	}
	return result, errStream
}
