package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type blockingQuotaWindowGate struct {
	block QuotaWindowBlock
}

type countingQuotaWindowGate struct {
	admits       int
	lastBlocked  string
	lastAdmitted string
}

func (g *countingQuotaWindowGate) BlockedForModel(_ []*Auth, model string, _ time.Time) (QuotaWindowBlock, bool) {
	g.lastBlocked = model
	return QuotaWindowBlock{}, false
}

func (g *countingQuotaWindowGate) Admit(_ *Auth, model string, _ time.Time) (string, bool) {
	g.admits++
	g.lastAdmitted = model
	return strconv.Itoa(g.admits), true
}

type localCountQuotaExecutor struct{ schedulerTestExecutor }

func (localCountQuotaExecutor) QuotaWindowCountTokensUsesUpstream(*Auth) bool { return false }

type exhaustionAfterAdmitGate struct {
	exhausted bool
}

type weightedExhaustionGate struct {
	exhausted bool
}

type oneDirectAdmissionGate struct {
	exhausted bool
}

func (g *oneDirectAdmissionGate) BlockedForModel(_ []*Auth, _ string, now time.Time) (QuotaWindowBlock, bool) {
	if !g.exhausted {
		return QuotaWindowBlock{}, false
	}
	return QuotaWindowBlock{Provider: "codex", Window: "workday", Exhausted: []string{"requests"}, AvailableAt: now.Add(time.Hour)}, true
}

func (g *oneDirectAdmissionGate) Admit(*Auth, string, time.Time) (string, bool) {
	if g.exhausted {
		return "", false
	}
	g.exhausted = true
	return "direct-reservation", true
}

func (g *exhaustionAfterAdmitGate) BlockedForModel(auths []*Auth, _ string, now time.Time) (QuotaWindowBlock, bool) {
	if !g.exhausted {
		return QuotaWindowBlock{}, false
	}
	for _, auth := range auths {
		if auth != nil && auth.Provider == "provider-a" {
			return QuotaWindowBlock{Provider: "provider-a", Window: "peak", Exhausted: []string{"requests"}, AvailableAt: now.Add(time.Hour)}, true
		}
	}
	return QuotaWindowBlock{}, false
}

func (g *exhaustionAfterAdmitGate) Admit(auth *Auth, _ string, _ time.Time) (string, bool) {
	if auth != nil && auth.Provider == "provider-a" {
		g.exhausted = true
	}
	return "reservation", true
}

func (g *weightedExhaustionGate) BlockedForModel(auths []*Auth, _ string, now time.Time) (QuotaWindowBlock, bool) {
	if !g.exhausted {
		return QuotaWindowBlock{}, false
	}
	hasProviderA := false
	for _, auth := range auths {
		if auth == nil || auth.Provider != "provider-a" {
			continue
		}
		hasProviderA = true
		if auth.ID == "quota-weight-zero" {
			return QuotaWindowBlock{}, false
		}
	}
	if hasProviderA {
		return QuotaWindowBlock{Provider: "provider-a", Window: "peak", Exhausted: []string{"requests"}, AvailableAt: now.Add(time.Hour)}, true
	}
	return QuotaWindowBlock{}, false
}

func (g *weightedExhaustionGate) Admit(auth *Auth, _ string, _ time.Time) (string, bool) {
	if auth != nil && auth.Provider == "provider-a" {
		g.exhausted = true
	}
	return "reservation", true
}

type quotaRetryTestExecutor struct {
	provider string
	calls    *int
	fail     bool
}

type quotaHTTPTestExecutor struct {
	quotaRetryTestExecutor
	calls int
}

func (e *quotaHTTPTestExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	e.calls++
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
}

func (e quotaRetryTestExecutor) Identifier() string { return e.provider }

func (e quotaRetryTestExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	*e.calls++
	if e.fail {
		return cliproxyexecutor.Response{}, errors.New("upstream failed")
	}
	return cliproxyexecutor.Response{}, nil
}

func (e quotaRetryTestExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, errors.New("unused")
}

func (e quotaRetryTestExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e quotaRetryTestExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("unused")
}

func (e quotaRetryTestExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("unused")
}

func TestQuotaWindowGateCoversSchedulerFastPath(t *testing.T) {
	now := time.Now()
	gate := blockingQuotaWindowGate{block: QuotaWindowBlock{
		Provider: "codex", Window: "workday", Exhausted: []string{"requests"}, AvailableAt: now.Add(time.Hour),
	}}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(schedulerTestExecutor{provider: "codex"})
	manager.SetQuotaWindowGate(gate)
	auth := &Auth{ID: "quota-scheduler-a", Provider: "codex", Status: StatusActive}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	registryRef := registry.GetGlobalRegistry()
	registryRef.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5"}})
	defer registryRef.UnregisterClient(auth.ID)

	_, _, _, errPick := manager.pickNextMixed(context.Background(), []string{"codex"}, "gpt-5", cliproxyexecutor.Options{}, nil)
	assertQuotaWindowError(t, errPick)
	_, errExecute := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5"}, cliproxyexecutor.Options{})
	assertQuotaWindowError(t, errExecute)
}

func TestQuotaWindowGateBlocksDirectHTTPRequestBeforeDial(t *testing.T) {
	now := time.Now()
	gate := blockingQuotaWindowGate{block: QuotaWindowBlock{
		Provider: "codex", Window: "workday", Exhausted: []string{"requests"}, AvailableAt: now.Add(time.Hour),
	}}
	manager := NewManager(nil, nil, nil)
	executor := &quotaHTTPTestExecutor{quotaRetryTestExecutor: quotaRetryTestExecutor{provider: "codex"}}
	manager.RegisterExecutor(executor)
	manager.SetQuotaWindowGate(gate)
	auth := &Auth{ID: "codex-http", Provider: "codex"}
	request := httptest.NewRequest(http.MethodPost, "https://example.com/upstream", nil)

	_, errRequest := manager.HttpRequest(WithQuotaWindowModel(context.Background(), "gpt-5"), auth, request)
	assertQuotaWindowError(t, errRequest)
	if executor.calls != 0 {
		t.Fatalf("direct HTTP calls = %d, want 0", executor.calls)
	}
}

func TestQuotaWindowDirectHTTPRetryRequiresFreshAdmission(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	executor := &quotaHTTPTestExecutor{quotaRetryTestExecutor: quotaRetryTestExecutor{provider: "codex"}}
	manager.RegisterExecutor(executor)
	manager.SetQuotaWindowGate(&oneDirectAdmissionGate{})
	auth := &Auth{ID: "codex-http", Provider: "codex"}
	ctx := WithQuotaWindowModel(context.Background(), "gpt-5")

	if _, errRequest := manager.HttpRequest(ctx, auth, httptest.NewRequest(http.MethodPost, "https://example.com/first", nil)); errRequest != nil {
		t.Fatalf("first HttpRequest() error = %v", errRequest)
	}
	_, errRetry := manager.HttpRequest(ctx, auth, httptest.NewRequest(http.MethodPost, "https://example.com/retry", nil))
	assertQuotaWindowError(t, errRetry)
	if executor.calls != 1 {
		t.Fatalf("direct HTTP calls = %d, want 1", executor.calls)
	}
}

func (g blockingQuotaWindowGate) BlockedForModel([]*Auth, string, time.Time) (QuotaWindowBlock, bool) {
	return g.block, true
}

func (blockingQuotaWindowGate) Admit(*Auth, string, time.Time) (string, bool) { return "", false }

func TestQuotaWindowGateCoversSelectorAndManagerAvailability(t *testing.T) {
	now := time.Now()
	gate := blockingQuotaWindowGate{block: QuotaWindowBlock{
		Provider: "deepseek", Window: "peak", Exhausted: []string{"requests"}, AvailableAt: now.Add(2 * time.Hour),
	}}
	auths := []*Auth{{ID: "a", Provider: "openai-compatible-deepseek"}}

	selector := &RoundRobinSelector{}
	selector.setQuotaWindowGate(gate)
	_, errPick := selector.Pick(nil, "mixed", "deepseek-v4", executorOptionsForQuotaTest(), auths)
	assertQuotaWindowError(t, errPick)

	manager := NewManager(nil, nil, nil)
	manager.SetQuotaWindowGate(gate)
	_, errAvailable := manager.availableAuthsForRouteModelWithPriorityMode(auths, "mixed", "deepseek-v4", now, false)
	assertQuotaWindowError(t, errAvailable)
}

func executorOptionsForQuotaTest() cliproxyexecutor.Options { return cliproxyexecutor.Options{} }

func assertQuotaWindowError(t *testing.T, err error) {
	t.Helper()
	quotaErr, ok := err.(*quotaWindowError)
	if !ok {
		t.Fatalf("error = %T %v, want *quotaWindowError", err, err)
	}
	if quotaErr.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("StatusCode() = %d", quotaErr.StatusCode())
	}
	if quotaErr.Headers().Get("Retry-After") == "" {
		t.Fatal("Retry-After header is empty")
	}
	if SafeResponseHeaders(quotaErr).Get("Retry-After") == "" {
		t.Fatal("safe Retry-After header is empty")
	}
	var payload map[string]any
	if errJSON := json.Unmarshal([]byte(quotaErr.Error()), &payload); errJSON != nil {
		t.Fatalf("error JSON = %v", errJSON)
	}
	errorBody, _ := payload["error"].(map[string]any)
	if errorBody["code"] != "quota_window_exhausted" {
		t.Fatalf("error code = %#v", errorBody["code"])
	}
}

func TestResolveQuotaWindowTargetMapsGeminiCLIToGemini(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{})
	target := manager.ResolveQuotaWindowTarget(&Auth{ID: "gemini", Provider: "gemini-cli", FileName: "gemini.json"}, "gemini-2.5-pro")
	if target.Provider != "gemini" {
		t.Fatalf("Provider = %q, want gemini", target.Provider)
	}
}

func TestQuotaWindowInternalUpstreamAttemptsGetDistinctReservations(t *testing.T) {
	gate := &countingQuotaWindowGate{}
	manager := NewManager(nil, nil, nil)
	manager.SetQuotaWindowGate(gate)
	auth := &Auth{ID: "xai", Provider: "xai"}
	ctx, errAdmit := manager.quotaWindowAttemptContext(context.Background(), auth, "grok")
	if errAdmit != nil {
		t.Fatalf("quotaWindowAttemptContext() error = %v", errAdmit)
	}
	if got := QuotaWindowReservationFromContext(ctx); got != "1" {
		t.Fatalf("initial reservation = %q, want 1", got)
	}
	first, errFirst := QuotaWindowContextForUpstreamAttempt(ctx)
	if errFirst != nil {
		t.Fatalf("first upstream attempt error = %v", errFirst)
	}
	if got := QuotaWindowReservationFromContext(first); got != "1" {
		t.Fatalf("first upstream reservation = %q, want 1", got)
	}
	second, errSecond := QuotaWindowContextForUpstreamAttempt(ctx)
	if errSecond != nil {
		t.Fatalf("second upstream attempt error = %v", errSecond)
	}
	if got := QuotaWindowReservationFromContext(second); got != "2" {
		t.Fatalf("second upstream reservation = %q, want 2", got)
	}
}

func TestWithQuotaWindowReservationClearsInheritedReservation(t *testing.T) {
	ctx := WithQuotaWindowReservation(context.Background(), "old")
	ctx = WithQuotaWindowReservation(ctx, "")
	if got := QuotaWindowReservationFromContext(ctx); got != "" {
		t.Fatalf("reservation = %q, want cleared", got)
	}
}

func TestLocalCountTokensDoesNotAdmitQuota(t *testing.T) {
	gate := &countingQuotaWindowGate{}
	manager := NewManager(nil, nil, nil)
	manager.SetQuotaWindowGate(gate)
	executor := localCountQuotaExecutor{schedulerTestExecutor{provider: "codex"}}
	if _, errCount := manager.countQuotaAttempt(context.Background(), executor, &Auth{ID: "codex", Provider: "codex"}, "gpt-5", cliproxyexecutor.Request{Model: "gpt-5"}, cliproxyexecutor.Options{}); errCount != nil {
		t.Fatalf("countQuotaAttempt() error = %v", errCount)
	}
	if gate.admits != 0 {
		t.Fatalf("quota admits = %d, want 0 for local count", gate.admits)
	}
}

func TestQuotaWindowUsesBillingModelInsteadOfSelectionModel(t *testing.T) {
	gate := &countingQuotaWindowGate{}
	manager := NewManager(nil, nil, nil)
	manager.SetQuotaWindowGate(gate)
	auth := &Auth{ID: "codex", Provider: "codex"}
	if _, errAvailable := manager.availableAuthsForRouteModelWithQuotaModel([]*Auth{auth}, "codex", "selection-model", "billed-model", time.Now(), false); errAvailable != nil {
		t.Fatalf("availability error = %v", errAvailable)
	}
	if gate.lastBlocked != "billed-model" {
		t.Fatalf("blocked model = %q, want billed-model", gate.lastBlocked)
	}
	if _, errExecute := manager.executeQuotaAttempt(context.Background(), schedulerTestExecutor{provider: "codex"}, auth, "billed-model", cliproxyexecutor.Request{Model: "billed-model"}, cliproxyexecutor.Options{}); errExecute != nil {
		t.Fatalf("executeQuotaAttempt() error = %v", errExecute)
	}
	if gate.lastAdmitted != "billed-model" {
		t.Fatalf("admitted model = %q, want billed-model", gate.lastAdmitted)
	}
}

func TestQuotaWindowRetryDoesNotFallThroughPastTriedProvider(t *testing.T) {
	gate := &exhaustionAfterAdmitGate{}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetQuotaWindowGate(gate)
	callsA, callsB := 0, 0
	manager.RegisterExecutor(quotaRetryTestExecutor{provider: "provider-a", calls: &callsA, fail: true})
	manager.RegisterExecutor(quotaRetryTestExecutor{provider: "provider-b", calls: &callsB})
	authA := &Auth{ID: "quota-retry-a", Provider: "provider-a", Status: StatusActive, Attributes: map[string]string{"priority": "10"}}
	authB := &Auth{ID: "quota-retry-b", Provider: "provider-b", Status: StatusActive}
	if _, errRegister := manager.Register(context.Background(), authA); errRegister != nil {
		t.Fatalf("Register(authA) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), authB); errRegister != nil {
		t.Fatalf("Register(authB) error = %v", errRegister)
	}
	registryRef := registry.GetGlobalRegistry()
	registryRef.RegisterClient(authA.ID, authA.Provider, []*registry.ModelInfo{{ID: "shared-model"}})
	registryRef.RegisterClient(authB.ID, authB.Provider, []*registry.ModelInfo{{ID: "shared-model"}})
	defer registryRef.UnregisterClient(authA.ID)
	defer registryRef.UnregisterClient(authB.ID)

	_, errExecute := manager.Execute(context.Background(), []string{authA.Provider, authB.Provider}, cliproxyexecutor.Request{Model: "shared-model"}, cliproxyexecutor.Options{})
	assertQuotaWindowError(t, errExecute)
	if callsA != 1 || callsB != 0 {
		t.Fatalf("executor calls = A:%d B:%d, want A:1 B:0", callsA, callsB)
	}
}

func TestQuotaWindowWeightedZeroCredentialDoesNotPermitCrossProviderFallthrough(t *testing.T) {
	gate := &weightedExhaustionGate{}
	manager := NewManager(nil, &WeightedRoundRobinSelector{}, nil)
	manager.SetQuotaWindowGate(gate)
	callsA, callsB := 0, 0
	manager.RegisterExecutor(quotaRetryTestExecutor{provider: "provider-a", calls: &callsA, fail: true})
	manager.RegisterExecutor(quotaRetryTestExecutor{provider: "provider-b", calls: &callsB})
	weightOne := 1
	weightZero := 0
	authA := &Auth{ID: "quota-weight-one", Provider: "provider-a", Status: StatusActive, Attributes: map[string]string{"priority": "10"}, Metadata: map[string]any{AttributeWeight: weightOne}}
	authAZero := &Auth{ID: "quota-weight-zero", Provider: "provider-a", Status: StatusActive, Attributes: map[string]string{"priority": "10"}, Metadata: map[string]any{AttributeWeight: weightZero}}
	authB := &Auth{ID: "quota-weight-b", Provider: "provider-b", Status: StatusActive}
	for _, auth := range []*Auth{authA, authAZero, authB} {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, errRegister)
		}
	}
	registryRef := registry.GetGlobalRegistry()
	for _, auth := range []*Auth{authA, authAZero, authB} {
		registryRef.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "shared-model"}})
		defer registryRef.UnregisterClient(auth.ID)
	}

	_, errExecute := manager.Execute(context.Background(), []string{authA.Provider, authB.Provider}, cliproxyexecutor.Request{Model: "shared-model"}, cliproxyexecutor.Options{})
	assertQuotaWindowError(t, errExecute)
	if callsA != 1 || callsB != 0 {
		t.Fatalf("executor calls = A:%d B:%d, want A:1 B:0", callsA, callsB)
	}
}

func TestQuotaWindowAuthsExcludeNonPositiveWeights(t *testing.T) {
	manager := NewManager(nil, &WeightedRoundRobinSelector{}, nil)
	positive := &Auth{ID: "quota-visible-positive", Provider: "codex", Status: StatusActive, Metadata: map[string]any{AttributeWeight: 1}}
	zero := &Auth{ID: "quota-visible-zero", Provider: "codex", Status: StatusActive, Metadata: map[string]any{AttributeWeight: 0}}
	for _, auth := range []*Auth{positive, zero} {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, errRegister)
		}
	}
	auths := manager.QuotaWindowAuths()
	if len(auths) != 1 || auths[0].ID != positive.ID {
		t.Fatalf("QuotaWindowAuths() = %#v, want only %s", auths, positive.ID)
	}
}

func TestQuotaWindowBlocksHomeBeforeProviderDispatch(t *testing.T) {
	gate := &exhaustionAfterAdmitGate{exhausted: true}
	manager := NewManager(nil, nil, nil)
	manager.SetQuotaWindowGate(gate)
	authA := &Auth{ID: "quota-home-a", Provider: "provider-a", Status: StatusActive}
	authB := &Auth{ID: "quota-home-b", Provider: "provider-b", Status: StatusActive}
	if _, errRegister := manager.Register(context.Background(), authA); errRegister != nil {
		t.Fatalf("Register(authA) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), authB); errRegister != nil {
		t.Fatalf("Register(authB) error = %v", errRegister)
	}
	registryRef := registry.GetGlobalRegistry()
	registryRef.RegisterClient(authA.ID, authA.Provider, []*registry.ModelInfo{{ID: "shared-model"}})
	registryRef.RegisterClient(authB.ID, authB.Provider, []*registry.ModelInfo{{ID: "shared-model"}})
	defer registryRef.UnregisterClient(authA.ID)
	defer registryRef.UnregisterClient(authB.ID)

	_, errSelect := manager.pickHomeDispatchSelection(context.Background(), "shared-model", cliproxyexecutor.Options{})
	assertQuotaWindowError(t, errSelect)
}
