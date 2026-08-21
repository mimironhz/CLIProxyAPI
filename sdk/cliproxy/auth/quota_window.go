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

func quotaWindowAvailableAuths(gate QuotaWindowGate, auths []*Auth, model string, now time.Time) []*Auth {
	if availability, ok := gate.(quotaWindowGateAvailability); ok && availability != nil {
		return availability.AvailableAuths(auths, model, now)
	}
	return auths
}

// QuotaWindowCountTokensMetering lets executors identify local-only token estimators.
type QuotaWindowCountTokensMetering interface {
	QuotaWindowCountTokensUsesUpstream(auth *Auth) bool
}

// QuotaWindowTarget is the credential-aware billing identity used by quota gates.
type QuotaWindowTarget struct {
	Provider      string
	ClientModel   string
	UpstreamModel string
	Credential    string
	AuthID        string
}

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

	aliasResult := m.resolveExecutionAliasResultForRequestedWithRouting(routing, auth, requestedModel)
	poolModel := executionAliasPoolModel(auth, requestedModel, aliasResult)
	candidates := []string(nil)
	if auth.Attributes != nil {
		if homeModel := strings.TrimSpace(auth.Attributes[homeUpstreamModelAttributeKey]); homeModel != "" {
			candidates = []string{homeModel}
		}
	}
	if len(candidates) == 0 {
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
	seen := make(map[string]struct{}, len(models))
	canonical := make([]string, 0, len(models))
	for _, model := range models {
		model = strings.TrimSpace(thinking.ParseSuffix(model).ModelName)
		if model == "" {
			continue
		}
		key := strings.ToLower(model)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		canonical = append(canonical, key)
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

func (e *quotaWindowError) Error() string {
	message := fmt.Sprintf("Model %s exhausted its %s quota for provider %s", e.model, e.block.Window, e.block.Provider)
	resetIn := e.block.AvailableAt.Sub(e.now)
	if resetIn < 0 {
		resetIn = 0
	}
	resetSeconds := int(math.Ceil(resetIn.Seconds()))
	displayDuration := resetIn.Round(time.Second)
	if resetIn > 0 && resetIn < time.Second {
		displayDuration = time.Second
	}
	errorBody := map[string]any{
		"code":          "quota_window_exhausted",
		"message":       message,
		"model":         e.model,
		"provider":      e.block.Provider,
		"window":        e.block.Window,
		"exhausted":     append([]string(nil), e.block.Exhausted...),
		"available_at":  nil,
		"reset_time":    displayDuration.String(),
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
	resetIn := e.block.AvailableAt.Sub(e.now)
	if resetIn < 0 {
		resetIn = 0
	}
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	headers.Set("Retry-After", strconv.Itoa(int(math.Ceil(resetIn.Seconds()))))
	return headers
}

func isQuotaWindowError(err error) bool {
	var quotaErr *quotaWindowError
	return errors.As(err, &quotaErr) && quotaErr != nil
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
	if block, exhausted := gate.BlockedForModel(candidates, model, time.Now()); exhausted {
		return nil, newQuotaWindowError(model, block, time.Now())
	}
	return nil, errQuotaWindowCredentialExhausted
}

// AdmitQuotaWindowAttempt reserves a direct upstream request before its dial.
func (m *Manager) AdmitQuotaWindowAttempt(ctx context.Context, auth *Auth, model string) (context.Context, error) {
	return m.quotaWindowAttemptContext(WithQuotaWindowModel(ctx, model), auth, model)
}

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
		candidates = append(candidates, candidate.Clone())
	}
	m.mu.RUnlock()
	return selectorAvailabilityCandidates(selector, candidates)
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
