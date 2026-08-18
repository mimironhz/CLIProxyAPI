package rootproxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseRelayCatalogMapsOwnedByToProvider(t *testing.T) {
	models, errParse := parseRelayCatalog([]byte(`{"data":[
		{"id":"grok-4.5","owned_by":"xai","max_context_length":1000000},
		{"id":"deepseek-v4-flash","owned_by":"deepseek"},
		{"id":"kimi-k3","owned_by":"kimi"},
		{"id":"mystery-1","owned_by":"someone-else"},
		{"id":"no-owner"}
	]}`))
	if errParse != nil {
		t.Fatalf("parseRelayCatalog() error = %v", errParse)
	}
	want := []discoveredRelayModel{
		{id: "deepseek-v4-flash", provider: relayProviderDeepSeek},
		{id: "grok-4.5", provider: relayProviderXAI, contextWindow: 1000000},
		{id: "kimi-k3", provider: relayProviderKimi},
		// An unmapped or absent owned_by stays routable but unattributed, which
		// keeps compaction disabled for it.
		{id: "mystery-1"},
		{id: "no-owner"},
	}
	if !reflect.DeepEqual(models, want) {
		t.Fatalf("models = %#v, want %#v", models, want)
	}
}

func TestParseRelayCatalogRejectsEmptyCatalog(t *testing.T) {
	if _, errParse := parseRelayCatalog([]byte(`{"data":[]}`)); errParse == nil {
		t.Fatal("expected an error for a catalog with no usable models")
	}
}

func TestAutoRouteResolverAcceptsDiscoveredRelayModels(t *testing.T) {
	resolver := newTestResolver(t, nil, nil, nil)
	accepted, collisions := resolver.applyRelayCatalog([]discoveredRelayModel{
		{id: "grok-4.5", provider: relayProviderXAI},
		{id: "deepseek-v4-flash", provider: relayProviderDeepSeek},
	})
	if len(collisions) != 0 {
		t.Fatalf("collisions = %#v, want none", collisions)
	}
	if len(accepted) != 2 {
		t.Fatalf("accepted = %#v, want 2 models", accepted)
	}
	assertRoute(t, resolver, "grok-4.5", routeRelay)
	assertRoute(t, resolver, "deepseek-v4-flash", routeRelay)
	if provider := resolver.relayProvider("grok-4.5"); provider != relayProviderXAI {
		t.Fatalf("grok-4.5 provider = %q, want xai", provider)
	}
	// Anything Relay does not serve belongs to the official arm.
	assertRoute(t, resolver, "gpt-5.6-sol", routeOfficial)
	assertRoute(t, resolver, "never-heard-of-it", routeOfficial)
}

func TestAutoRouteResolverStockPinBeatsRelayCollision(t *testing.T) {
	resolver := newTestResolver(t, []string{"gpt-5.6-sol"}, nil, nil)
	accepted, collisions := resolver.applyRelayCatalog([]discoveredRelayModel{
		{id: "gpt-5.6-sol", provider: relayProviderXAI},
		{id: "grok-4.5", provider: relayProviderXAI},
	})
	if !reflect.DeepEqual(collisions, []string{"gpt-5.6-sol"}) {
		t.Fatalf("collisions = %#v, want [gpt-5.6-sol]", collisions)
	}
	if !reflect.DeepEqual(accepted, []string{"grok-4.5"}) {
		t.Fatalf("accepted = %#v, want [grok-4.5]", accepted)
	}
	// The pinned stock model must never be diverted to a third party.
	assertRoute(t, resolver, "gpt-5.6-sol", routeOfficial)
	assertRoute(t, resolver, "grok-4.5", routeRelay)
}

func TestAutoRouteResolverRelayPinNarrowsCatalog(t *testing.T) {
	resolver := newTestResolver(t, nil, []string{"grok-4.5"}, nil)
	accepted, _ := resolver.applyRelayCatalog([]discoveredRelayModel{
		{id: "grok-4.5", provider: relayProviderXAI},
		{id: "deepseek-v4-flash", provider: relayProviderDeepSeek},
	})
	if !reflect.DeepEqual(accepted, []string{"grok-4.5"}) {
		t.Fatalf("accepted = %#v, want [grok-4.5]", accepted)
	}
	assertRoute(t, resolver, "grok-4.5", routeRelay)
	// An excluded Relay model falls through to the official arm rather than
	// silently reaching Relay anyway.
	assertRoute(t, resolver, "deepseek-v4-flash", routeOfficial)
}

func TestAutoRouteResolverProviderOverrideBeatsOwnedBy(t *testing.T) {
	resolver := newTestResolver(t, nil, nil, map[string]string{"mystery-1": "kimi"})
	if _, collisions := resolver.applyRelayCatalog([]discoveredRelayModel{
		{id: "mystery-1"},
		{id: "grok-4.5", provider: relayProviderXAI},
	}); len(collisions) != 0 {
		t.Fatalf("collisions = %#v, want none", collisions)
	}
	if provider := resolver.relayProvider("mystery-1"); provider != relayProviderKimi {
		t.Fatalf("mystery-1 provider = %q, want kimi from the configured override", provider)
	}
}

func TestAutoRouteResolverBeforeDiscoveryKeepsRelayModelsOffRelay(t *testing.T) {
	resolver := newTestResolver(t, nil, nil, nil)
	// Before any successful discovery the Relay half is empty. This is exactly
	// why the bridges call discovery.ensure before classifying.
	assertRoute(t, resolver, "grok-4.5", routeOfficial)
	if provider := resolver.relayProvider("grok-4.5"); provider != "" {
		t.Fatalf("provider = %q, want empty before discovery", provider)
	}
}

func TestStaticModeStillRejectsUnknownModels(t *testing.T) {
	resolver, errResolver := newRouteResolver(discoveryStatic, []string{"gpt-5.6-sol"}, []string{"grok-4.5"}, nil)
	if errResolver != nil {
		t.Fatalf("newRouteResolver() error = %v", errResolver)
	}
	assertRoute(t, resolver, "gpt-5.6-sol", routeOfficial)
	assertRoute(t, resolver, "grok-4.5", routeRelay)
	if _, errClassify := resolver.classify("unknown-model"); errClassify == nil {
		t.Fatal("static mode must still reject an unconfigured model")
	}
	// Discovery must not be able to mutate a static routing surface.
	if accepted, _ := resolver.applyRelayCatalog([]discoveredRelayModel{{id: "sneaky"}}); accepted != nil {
		t.Fatalf("accepted = %#v, want nil in static mode", accepted)
	}
	if _, errClassify := resolver.classify("sneaky"); errClassify == nil {
		t.Fatal("static mode accepted a discovered model")
	}
}

func TestRelayDiscoveryRefreshUpdatesResolver(t *testing.T) {
	var gotAuthorization, gotHop string
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		gotHop = r.Header.Get(rootHopHeader)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"grok-4.5","owned_by":"xai"}]}`))
	}))
	defer relay.Close()

	resolver := newTestResolver(t, nil, nil, nil)
	discovery, errDiscovery := newRelayDiscovery(relay.URL+"/v1", "relay-secret", relay.Client(), resolver)
	if errDiscovery != nil {
		t.Fatalf("newRelayDiscovery() error = %v", errDiscovery)
	}
	if _, errRefresh := discovery.refresh(context.Background()); errRefresh != nil {
		t.Fatalf("refresh() error = %v", errRefresh)
	}
	if gotAuthorization != "Bearer relay-secret" {
		t.Fatalf("Authorization = %q, want the Relay CPA key", gotAuthorization)
	}
	if gotHop != "1" {
		t.Fatalf("%s = %q, want 1", rootHopHeader, gotHop)
	}
	assertRoute(t, resolver, "grok-4.5", routeRelay)
}

func TestRelayDiscoveryKeepsLastGoodCatalogOnFailure(t *testing.T) {
	healthy := true
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !healthy {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"grok-4.5","owned_by":"xai"}]}`))
	}))
	defer relay.Close()

	resolver := newTestResolver(t, nil, nil, nil)
	discovery, errDiscovery := newRelayDiscovery(relay.URL+"/v1", "relay-secret", relay.Client(), resolver)
	if errDiscovery != nil {
		t.Fatalf("newRelayDiscovery() error = %v", errDiscovery)
	}
	if _, errRefresh := discovery.refresh(context.Background()); errRefresh != nil {
		t.Fatalf("refresh() error = %v", errRefresh)
	}
	healthy = false
	if _, errRefresh := discovery.refresh(context.Background()); errRefresh == nil {
		t.Fatal("expected the failing refresh to report an error")
	}
	// A failed refresh must not empty the Relay half and silently reroute
	// grok-4.5 to ChatGPT.
	assertRoute(t, resolver, "grok-4.5", routeRelay)
}

func TestModelsHandlerMergesOfficialAndRelayCatalogs(t *testing.T) {
	official := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer desktop-token" {
			t.Errorf("official Authorization = %q, want the Desktop bearer", got)
		}
		if got := r.URL.Query().Get(modelsClientVersionQuery); got != "0.146.0" {
			t.Errorf("client_version = %q, want it forwarded", got)
		}
		_, _ = w.Write([]byte(`{"models":[
			{"slug":"gpt-5.6-sol","comp_hash":"abc","display_name":"Sol"},
			{"slug":"gpt-5.6-terra","comp_hash":"def","display_name":"Terra"}
		]}`))
	}))
	defer official.Close()
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[
			{"id":"grok-4.5","owned_by":"xai"},
			{"id":"deepseek-v4-flash","owned_by":"deepseek"}
		]}`))
	}))
	defer relay.Close()

	handler := newTestAutoModelsHandler(t, official, relay, []string{"gpt-5.6-sol"}, nil, nil)
	// The first poll triggers the background merge and is answered from the
	// cold-start catalog; the merged result lands shortly after.
	serveModels(t, handler, http.MethodGet, "/v1/models?client_version=0.146.0", nil)
	waitForMergedCatalog(t, handler)
	response := serveModels(t, handler, http.MethodGet, "/v1/models?client_version=0.146.0", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}

	var payload rootModelsPayload
	if errDecode := json.Unmarshal(response.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode merged catalog: %v", errDecode)
	}
	gotSlugs := make([]string, 0, len(payload.Models))
	for index, model := range payload.Models {
		slug, _ := model["slug"].(string)
		gotSlugs = append(gotSlugs, slug)
		if priority, ok := model["priority"].(float64); !ok || int(priority) != index+1 {
			t.Errorf("%s priority = %#v, want %d", slug, model["priority"], index+1)
		}
	}
	want := []string{"gpt-5.6-sol", "gpt-5.6-terra", "deepseek-v4-flash", "grok-4.5"}
	if !reflect.DeepEqual(gotSlugs, want) {
		t.Fatalf("slugs = %#v, want %#v", gotSlugs, want)
	}
	// Stock metadata is passed through untouched, so the real comp_hash survives.
	if payload.Models[0]["comp_hash"] != "abc" {
		t.Errorf("stock comp_hash = %#v, want the upstream value", payload.Models[0]["comp_hash"])
	}
	// Relay entries stay synthesized and must not inherit a GPT comp_hash.
	for _, model := range payload.Models[2:] {
		if _, exists := model["comp_hash"]; exists {
			t.Errorf("%v inherited a comp_hash", model["slug"])
		}
	}
	if response.Body.String() != "" && containsSecret(response.Body.String()) {
		t.Fatal("merged catalog leaked the Relay key")
	}
}

func TestModelsHandlerOmitsRelayModelCollidingWithOfficial(t *testing.T) {
	official := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-5.6-sol","display_name":"Sol"}]}`))
	}))
	defer official.Close()
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Relay claims an official model name.
		_, _ = w.Write([]byte(`{"data":[
			{"id":"gpt-5.6-sol","owned_by":"xai"},
			{"id":"grok-4.5","owned_by":"xai"}
		]}`))
	}))
	defer relay.Close()

	handler := newTestAutoModelsHandler(t, official, relay, nil, nil, nil)
	serveModels(t, handler, http.MethodGet, "/v1/models", nil)
	waitForMergedCatalog(t, handler)
	response := serveModels(t, handler, http.MethodGet, "/v1/models", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	var payload rootModelsPayload
	if errDecode := json.Unmarshal(response.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode merged catalog: %v", errDecode)
	}
	gotSlugs := make([]string, 0, len(payload.Models))
	for _, model := range payload.Models {
		slug, _ := model["slug"].(string)
		gotSlugs = append(gotSlugs, slug)
	}
	// The identifier appears exactly once, sourced from the official catalog.
	if !reflect.DeepEqual(gotSlugs, []string{"gpt-5.6-sol", "grok-4.5"}) {
		t.Fatalf("slugs = %#v, want [gpt-5.6-sol grok-4.5]", gotSlugs)
	}
}

// The installed Desktop client abandons the catalog request after ~100ms, well
// short of a ChatGPT round trip. The refresh must therefore outlive the inbound
// request, and the client must still get a usable catalog immediately.
func TestModelsHandlerServesColdStartCatalogAndRefreshesAfterCancellation(t *testing.T) {
	released := make(chan struct{})
	official := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-released
		_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-5.6-sol","comp_hash":"abc"}]}`))
	}))
	defer official.Close()
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"grok-4.5","owned_by":"xai"}]}`))
	}))
	defer relay.Close()

	handler := newTestAutoModelsHandler(t, official, relay, []string{"gpt-5.6-sol"}, nil, nil)
	if _, errRefresh := handler.discovery.refresh(context.Background()); errRefresh != nil {
		t.Fatalf("relay refresh error = %v", errRefresh)
	}

	// Serve a request whose context is already canceled, mimicking Desktop
	// giving up on the poll.
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil).WithContext(canceledCtx)
	request.Header.Set("Authorization", "Bearer desktop-token")
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the cold-start catalog; body=%s", recorder.Code, recorder.Body.String())
	}
	var cold rootModelsPayload
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &cold); errDecode != nil {
		t.Fatalf("decode cold-start catalog: %v", errDecode)
	}
	coldSlugs := make([]string, 0, len(cold.Models))
	for _, model := range cold.Models {
		slug, _ := model["slug"].(string)
		coldSlugs = append(coldSlugs, slug)
	}
	if !reflect.DeepEqual(coldSlugs, []string{"gpt-5.6-sol", "grok-4.5"}) {
		t.Fatalf("cold-start slugs = %#v, want [gpt-5.6-sol grok-4.5]", coldSlugs)
	}

	// The detached refresh must complete despite the canceled inbound request.
	close(released)
	waitForMergedCatalog(t, handler)
	body, _, _ := handler.cached()
	if !strings.Contains(string(body), `"comp_hash":"abc"`) {
		t.Fatal("background refresh did not survive the canceled inbound request")
	}
}

func TestModelsHandlerFailsWhenNothingCanBeAdvertised(t *testing.T) {
	official := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer official.Close()
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer relay.Close()

	// No stock pin, no reachable Relay and no cached merge leaves nothing to say.
	handler := newTestAutoModelsHandler(t, official, relay, nil, nil, nil)
	response := serveModels(t, handler, http.MethodGet, "/v1/models", nil)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", response.Code, response.Body.String())
	}
}

func waitForMergedCatalog(t *testing.T, handler *modelsHandler) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if body, _, _ := handler.cached(); body != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the merged catalog to be cached")
}

func TestModelsHandlerRequiresBearerBeforeContactingUpstreams(t *testing.T) {
	contacted := false
	official := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contacted = true
		_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-5.6-sol"}]}`))
	}))
	defer official.Close()
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contacted = true
		_, _ = w.Write([]byte(`{"data":[{"id":"grok-4.5","owned_by":"xai"}]}`))
	}))
	defer relay.Close()

	handler := newTestAutoModelsHandler(t, official, relay, nil, nil, nil)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
	if contacted {
		t.Fatal("Root contacted an upstream before validating the Desktop bearer")
	}
}

func newTestResolver(t *testing.T, stockPin, relayPin []string, providers map[string]string) *routeResolver {
	t.Helper()
	resolver, errResolver := newRouteResolver(discoveryAuto, stockPin, relayPin, providers)
	if errResolver != nil {
		t.Fatalf("newRouteResolver() error = %v", errResolver)
	}
	return resolver
}

func newTestAutoModelsHandler(
	t *testing.T,
	official, relay *httptest.Server,
	stockPin, relayPin []string,
	providers map[string]string,
) *modelsHandler {
	t.Helper()
	config := defaultConfig()
	config.Routing.Discovery = "auto"
	config.Routing.StockModels = append([]string(nil), stockPin...)
	config.Routing.RelayModels = append([]string(nil), relayPin...)
	config.Routing.RelayModelProviders = cloneStringMap(providers)

	resolver := newTestResolver(t, stockPin, relayPin, providers)
	discovery, errDiscovery := newRelayDiscovery(relay.URL+"/v1", "relay-secret", relay.Client(), resolver)
	if errDiscovery != nil {
		t.Fatalf("newRelayDiscovery() error = %v", errDiscovery)
	}
	handler, errHandler := newModelsHandlerWithDiscovery(&config, resolver, discovery, official.URL, official.Client())
	if errHandler != nil {
		t.Fatalf("newModelsHandlerWithDiscovery() error = %v", errHandler)
	}
	return handler
}

func assertRoute(t *testing.T, resolver *routeResolver, model string, want route) {
	t.Helper()
	selected, errClassify := resolver.classify(model)
	if errClassify != nil {
		t.Fatalf("classify(%q) error = %v", model, errClassify)
	}
	if selected != want {
		t.Fatalf("classify(%q) = %s, want %s", model, selected, want)
	}
}

func containsSecret(body string) bool {
	return strings.Contains(body, "relay-secret")
}
