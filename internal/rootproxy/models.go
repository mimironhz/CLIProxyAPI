package rootproxy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	openaihandler "github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers/openai"
	log "github.com/sirupsen/logrus"
)

const modelsClientVersionQuery = "client_version"

// maxOfficialCatalogBytes bounds the ChatGPT catalog response.
const maxOfficialCatalogBytes = 8 << 20

// modelsCatalogRefreshAfter bounds how old a cached merged catalog may be before
// the next request triggers a background refresh. Desktop polls roughly every
// 60s, so this converges within one poll.
const modelsCatalogRefreshAfter = 30 * time.Second

type modelsHandler struct {
	// body and etag are the precomputed static catalog. They are empty in auto
	// mode, where the catalog is assembled from both upstreams and cached.
	body             []byte
	etag             string
	allowedOrigins   map[string]struct{}
	mode             discoveryMode
	preferWebsockets bool
	resolver         *routeResolver
	discovery        *relayDiscovery
	officialBaseURL  string
	officialClient   *http.Client
	// stockPinOrder preserves configuration order for the cold-start catalog.
	stockPinOrder []string
	// multiAgentV2Models names stock models advertised as multi-agent v2.
	// multiAgentV2RelayAll advertises the whole discovered Relay half, and
	// multiAgentV2RelayModels selects it by provider-qualified identifier.
	multiAgentV2Models      map[string]struct{}
	multiAgentV2RelayAll    bool
	multiAgentV2RelayModels map[string]struct{}
	modelContext            map[string]ModelContextConfig

	cacheMu    sync.RWMutex
	cachedBody []byte
	cachedETag string
	cachedAt   time.Time
	refreshing atomic.Bool
}

func (h *modelsHandler) cached() ([]byte, string, time.Time) {
	h.cacheMu.RLock()
	defer h.cacheMu.RUnlock()
	return h.cachedBody, h.cachedETag, h.cachedAt
}

func (h *modelsHandler) storeCache(body []byte, etag string) {
	h.cacheMu.Lock()
	defer h.cacheMu.Unlock()
	h.cachedBody = body
	h.cachedETag = etag
	h.cachedAt = time.Now()
}

type rootModelsPayload struct {
	Models []map[string]any `json:"models"`
}

func newModelsHandler(config *Config) (*modelsHandler, error) {
	return newModelsHandlerWithDiscovery(config, nil, nil, "", nil)
}

func newModelsHandlerWithDiscovery(config *Config, resolver *routeResolver, discovery *relayDiscovery, officialBaseURL string, officialClient *http.Client) (*modelsHandler, error) {
	if config == nil {
		return nil, errors.New("root proxy config is nil")
	}
	mode, errMode := config.discoveryMode()
	if errMode != nil {
		return nil, errMode
	}
	if errOrigins := validateOrigins(config.Websocket.AllowedOrigins); errOrigins != nil {
		return nil, errOrigins
	}
	preferWebsockets := false
	switch config.Websocket.Mode {
	case websocketModeHTTPFallback:
	case websocketModeFirstMessage:
		preferWebsockets = true
	default:
		return nil, fmt.Errorf("websocket mode %q is unsupported", config.Websocket.Mode)
	}
	allowedOrigins := make(map[string]struct{}, len(config.Websocket.AllowedOrigins))
	for _, origin := range config.Websocket.AllowedOrigins {
		allowedOrigins[origin] = struct{}{}
	}

	multiAgentV2Models := make(map[string]struct{}, len(config.Routing.MultiAgentV2Models))
	for _, model := range config.Routing.MultiAgentV2Models {
		multiAgentV2Models[model] = struct{}{}
	}
	multiAgentV2RelayModels, errMultiAgentV2Relay := buildMultiAgentV2RelaySet(mode, config.Routing.MultiAgentV2Relay, config.Routing.RelayModels)
	if errMultiAgentV2Relay != nil {
		return nil, errMultiAgentV2Relay
	}

	handler := &modelsHandler{
		allowedOrigins:          allowedOrigins,
		mode:                    mode,
		preferWebsockets:        preferWebsockets,
		resolver:                resolver,
		discovery:               discovery,
		officialBaseURL:         strings.TrimSuffix(strings.TrimSpace(officialBaseURL), "/"),
		officialClient:          officialClient,
		stockPinOrder:           append([]string(nil), config.Routing.StockModels...),
		multiAgentV2Models:      multiAgentV2Models,
		multiAgentV2RelayAll:    config.Routing.MultiAgentV2Relay.All,
		multiAgentV2RelayModels: multiAgentV2RelayModels,
		modelContext:            cloneModelContext(config.Routing.ModelContext),
	}
	if mode == discoveryAuto {
		if resolver == nil || discovery == nil || officialClient == nil || handler.officialBaseURL == "" {
			return nil, errors.New("auto model discovery requires a resolver, relay discovery and an official HTTP client")
		}
		return handler, nil
	}

	routes, errRoutes := buildRouteTable(
		config.Routing.StockModels,
		config.Routing.RelayModels,
		config.Routing.RelayModelProviders,
	)
	if errRoutes != nil {
		return nil, errRoutes
	}
	configured := make([]string, 0, len(config.Routing.StockModels)+len(config.Routing.RelayModels))
	configured = append(configured, config.Routing.StockModels...)
	configured = append(configured, config.Routing.RelayModels...)

	models, errBuild := synthesizeCodexModels(configured, func(slug string) (route, relayProvider, error) {
		selected, errRoute := routes.classify(slug)
		if errRoute != nil {
			return 0, "", errRoute
		}
		return selected, routes.relayProvider(slug), nil
	}, preferWebsockets, handler.multiAgentV2Policy(), handler.modelContextPolicy(routes))
	if errBuild != nil {
		return nil, errBuild
	}

	body, errEncode := encodeModelsPayload(models)
	if errEncode != nil {
		return nil, errEncode
	}
	handler.body = body
	handler.etag = strongETag(body)
	return handler, nil
}

// multiAgentVersionKey is the Codex catalog field naming a model's multi-agent
// backend; multiAgentVersionV2 is the value that makes it a valid spawn_agent
// target for a multi-agent v2 parent.
const (
	multiAgentVersionKey = "multi_agent_version"
	multiAgentVersionV2  = "v2"
)

// multiAgentV2Policy decides which catalog entries advertise multi-agent v2.
// The Relay half is discovered at runtime, so it is switched as a whole rather
// than enumerated.
type multiAgentV2Policy struct {
	stockModels map[string]struct{}
	relayAll    bool
	// relayModels is keyed by relayModelKey, so a configured entry only matches
	// a model the Relay catalog attributes to that same provider.
	relayModels map[string]struct{}
}

func (p multiAgentV2Policy) advertises(slug string, selected route, provider relayProvider) bool {
	if selected != routeRelay {
		_, advertised := p.stockModels[slug]
		return advertised
	}
	if p.relayAll {
		return true
	}
	if provider == "" {
		// An unattributed Relay model cannot be named in a provider-qualified
		// list, so a selective configuration never advertises it.
		return false
	}
	_, advertised := p.relayModels[relayModelKey(provider, slug)]
	return advertised
}

// advertiseMultiAgentV2 marks an entry as a valid spawn_agent target.
//
// A multi-agent v2 parent filters spawn targets on the catalog's own
// multi_agent_version: anything that is not v2 is refused with "Unknown model
// ... Available models: gpt-5.6-sol, gpt-5.6-terra". Neither a v1 stock model
// such as gpt-5.6-luna nor a Relay entry — whose field sanitizeRelayModelMetadata
// removes — can be delegated to without this.
//
// The advertisement is not free: a v2 child is a full participant that receives
// the collaboration tools and can spawn its own subagents, rather than a leaf
// worker. It is therefore opt-in per model surface.
func advertiseMultiAgentV2(model map[string]any, slug string, selected route, provider relayProvider, policy multiAgentV2Policy) {
	if policy.advertises(slug, selected, provider) {
		model[multiAgentVersionKey] = multiAgentVersionV2
	}
}

func (h *modelsHandler) multiAgentV2Policy() multiAgentV2Policy {
	return multiAgentV2Policy{
		stockModels: h.multiAgentV2Models,
		relayAll:    h.multiAgentV2RelayAll,
		relayModels: h.multiAgentV2RelayModels,
	}
}

type modelContextPolicy struct {
	overrides    map[string]ModelContextConfig
	relayWindows map[string]int
}

func (h *modelsHandler) modelContextPolicy(table routeTable) modelContextPolicy {
	return modelContextPolicy{
		overrides:    h.modelContext,
		relayWindows: table.relayWindows,
	}
}

func applyModelContext(model map[string]any, slug string, selected route, policy modelContextPolicy) {
	if model == nil {
		return
	}
	if selected == routeRelay {
		if window := policy.relayWindows[slug]; window > 0 {
			model["context_window"] = window
			model["max_context_window"] = window
		}
	}
	override, configured := policy.overrides[slug]
	if !configured {
		return
	}
	if override.ContextWindow > 0 {
		model["context_window"] = override.ContextWindow
		model["max_context_window"] = override.ContextWindow
	}
	if override.AutoCompactTokenLimit > 0 {
		model["auto_compact_token_limit"] = override.AutoCompactTokenLimit
	}
}

func cloneModelContext(source map[string]ModelContextConfig) map[string]ModelContextConfig {
	if len(source) == 0 {
		return nil
	}
	cloned := make(map[string]ModelContextConfig, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

// synthesizeCodexModels builds Codex catalog entries for models Root describes
// itself, using the repository's validated metadata templates.
func synthesizeCodexModels(slugs []string, classify func(string) (route, relayProvider, error), preferWebsockets bool, multiAgentV2 multiAgentV2Policy, contextPolicy modelContextPolicy) ([]map[string]any, error) {
	inputs := make([]map[string]any, 0, len(slugs))
	for _, model := range slugs {
		inputs = append(inputs, map[string]any{"id": model})
	}
	generated := openaihandler.CodexClientModelsResponse(inputs)
	generatedModels, ok := generated["models"].([]map[string]any)
	if !ok || len(generatedModels) != len(slugs) {
		return nil, errors.New("build complete Codex model catalog")
	}
	bySlug := make(map[string]map[string]any, len(generatedModels))
	for _, model := range generatedModels {
		slug, _ := model["slug"].(string)
		slug = strings.TrimSpace(slug)
		if slug == "" {
			return nil, errors.New("generated Codex model has no slug")
		}
		if _, duplicate := bySlug[slug]; duplicate {
			return nil, fmt.Errorf("generated Codex catalog contains duplicate model %q", slug)
		}
		bySlug[slug] = model
	}

	models := make([]map[string]any, 0, len(slugs))
	for index, slug := range slugs {
		model, exists := bySlug[slug]
		if !exists {
			return nil, fmt.Errorf("generated Codex catalog is missing configured model %q", slug)
		}
		selected, provider, errRoute := classify(slug)
		if errRoute != nil {
			return nil, errRoute
		}
		// Configuration order is authoritative. In particular, the first stock
		// model remains the default instead of allowing a Relay model's template
		// priority to become the Desktop default.
		model["priority"] = index + 1
		// Cutover-safe mode deliberately keeps Desktop on HTTP/SSE. Explicit
		// first-message mode advertises the experimental turn-aware controller.
		model["prefer_websockets"] = preferWebsockets
		if selected == routeRelay {
			sanitizeRelayModelMetadata(model, provider)
		}
		advertiseMultiAgentV2(model, slug, selected, provider, multiAgentV2)
		applyModelContext(model, slug, selected, contextPolicy)
		models = append(models, model)
	}
	return models, nil
}

func encodeModelsPayload(models []map[string]any) ([]byte, error) {
	body, errMarshal := json.Marshal(rootModelsPayload{Models: models})
	if errMarshal != nil {
		return nil, fmt.Errorf("encode Root model catalog: %w", errMarshal)
	}
	return append(body, '\n'), nil
}

func strongETag(body []byte) string {
	digest := sha256.Sum256(body)
	return fmt.Sprintf("\"%x\"", digest)
}

func sanitizeRelayModelMetadata(model map[string]any, provider relayProvider) {
	// comp_hash expresses server-side prompt compatibility between OpenAI
	// models. A synthesized third-party entry must not inherit the default GPT
	// template's hash: Codex treats a changed hash as a mandatory old-model
	// compaction before the first new-model turn, producing opaque state that the
	// third-party provider cannot consume. An absent hash means compatibility is
	// unknown and lets Codex replay the full portable history instead.
	delete(model, "comp_hash")
	model["additional_speed_tiers"] = []any{}
	model["service_tiers"] = []any{}
	delete(model, "default_service_tier")
	delete(model, "upgrade")
	delete(model, "availability_nux")
	delete(model, "apply_patch_tool_type")
	delete(model, "auto_review_model_override")
	delete(model, "tool_mode")
	// Removed unconditionally so an unadvertised Relay entry can never inherit
	// the template's multi-agent metadata; advertiseMultiAgentV2 re-adds it for
	// the surfaces that opt in.
	delete(model, multiAgentVersionKey)
	// Every relay provider reaches an upstream whose executor rewrites Codex's
	// hosted tool_search into a plain function and restores the call on the way
	// back, so deferred tool loading works for all of them. An unattributed
	// relay model has no such guarantee and keeps the tool switched off.
	switch provider {
	case relayProviderXAI, relayProviderKimi, relayProviderDeepSeek:
		model["supports_search_tool"] = true
	default:
		model["supports_search_tool"] = false
	}
}

func (h *modelsHandler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if h == nil {
		writeHTTPError(response, http.StatusServiceUnavailable, "upstream_error", "model catalog is unavailable", "")
		return
	}
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		response.Header().Set("Allow", "GET, HEAD")
		writeHTTPError(response, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed", "")
		return
	}
	if len(headerValuesCaseInsensitive(request.Header, rootHopHeader)) != 0 {
		writeHTTPError(response, http.StatusLoopDetected, "invalid_request_error", "root proxy loop detected", "")
		return
	}
	if !modelCatalogOriginAllowed(request.Header, h.allowedOrigins) {
		writeHTTPError(response, http.StatusForbidden, "invalid_request_error", "origin is not allowed", "")
		return
	}
	if _, errAuthorization := singleBearerAuthorization(request.Header); errAuthorization != nil {
		response.Header().Set("WWW-Authenticate", "Bearer")
		writeHTTPError(response, http.StatusUnauthorized, "authentication_error", "Desktop bearer authorization is required", "")
		return
	}
	if errQuery := validateModelsQuery(request.URL); errQuery != nil {
		writeHTTPError(response, http.StatusBadRequest, "invalid_request_error", errQuery.Error(), modelsClientVersionQuery)
		return
	}

	body := h.body
	etag := h.etag
	if h.mode == discoveryAuto {
		var cachedAt time.Time
		body, etag, cachedAt = h.cached()
		if body == nil || time.Since(cachedAt) > modelsCatalogRefreshAfter {
			h.refreshAsync(request)
		}
		if body == nil {
			// Nothing merged yet. Desktop abandons this request after ~100ms, far
			// less than a ChatGPT round trip, so blocking here would leave it with
			// no catalog at all. Serve the locally synthesized set instead; the
			// background refresh upgrades it before the next poll.
			var errFallback error
			body, etag, errFallback = h.coldStartCatalog()
			if errFallback != nil {
				log.WithError(errFallback).Warn("root proxy failed to build the cold-start model catalog")
				writeHTTPError(response, http.StatusBadGateway, "upstream_error", "model catalog is unavailable", "")
				return
			}
		}
	}

	response.Header().Set("Cache-Control", "private, no-cache")
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("ETag", etag)
	response.Header().Set("X-Content-Type-Options", "nosniff")
	if etagMatches(request.Header, etag) {
		response.WriteHeader(http.StatusNotModified)
		return
	}
	response.WriteHeader(http.StatusOK)
	if request.Method == http.MethodHead {
		return
	}
	if _, errWrite := response.Write(body); errWrite != nil {
		log.WithError(errWrite).Debug("root proxy failed to write model catalog")
	}
}

// refreshAsync assembles the merged catalog outside the inbound request's
// lifetime. Desktop treats Root as a local endpoint and cancels well before a
// WAN round trip finishes, so the fetch is deliberately detached from that
// cancellation while still borrowing the request's bearer. The bearer is used
// only for the duration of this refresh and is never stored.
func (h *modelsHandler) refreshAsync(request *http.Request) {
	authorization, errAuthorization := singleBearerAuthorization(request.Header)
	if errAuthorization != nil {
		return
	}
	// Single-flight: concurrent polls must not fan out to the upstream.
	if !h.refreshing.CompareAndSwap(false, true) {
		return
	}
	clientVersion := request.URL.Query().Get(modelsClientVersionQuery)
	accountID := request.Header.Get("ChatGPT-Account-ID")
	ctx := context.WithoutCancel(request.Context())
	go func() {
		defer h.refreshing.Store(false)
		body, etag, errAssemble := h.assemble(ctx, authorization, clientVersion, accountID)
		if errAssemble != nil {
			log.WithError(errAssemble).Warn("root proxy failed to assemble model catalog")
			return
		}
		h.storeCache(body, etag)
		log.Debug("root proxy refreshed the merged model catalog")
	}()
}

// coldStartCatalog synthesizes a catalog from the configured stock pin and the
// discovered Relay set, for use before the first successful merge.
func (h *modelsHandler) coldStartCatalog() ([]byte, string, error) {
	table := h.resolver.table()
	contextPolicy := h.modelContextPolicy(table)
	relaySlugs := make([]string, 0, len(table.relay))
	for slug := range table.relay {
		relaySlugs = append(relaySlugs, slug)
	}
	sort.Strings(relaySlugs)

	slugs := make([]string, 0, len(h.stockPinOrder)+len(relaySlugs))
	slugs = append(slugs, h.stockPinOrder...)
	slugs = append(slugs, relaySlugs...)
	if len(slugs) == 0 {
		return nil, "", errors.New("no stock pin and no discovered relay models")
	}
	stockPin := make(map[string]struct{}, len(h.stockPinOrder))
	for _, slug := range h.stockPinOrder {
		stockPin[slug] = struct{}{}
	}
	models, errBuild := synthesizeCodexModels(slugs, func(slug string) (route, relayProvider, error) {
		if _, stock := stockPin[slug]; stock {
			return routeOfficial, "", nil
		}
		return routeRelay, table.relayProvider(slug), nil
	}, h.preferWebsockets, h.multiAgentV2Policy(), contextPolicy)
	if errBuild != nil {
		return nil, "", errBuild
	}
	body, errEncode := encodeModelsPayload(models)
	if errEncode != nil {
		return nil, "", errEncode
	}
	return body, strongETag(body), nil
}

// assemble builds the merged catalog: the live ChatGPT listing followed by the
// discovered Relay models. The Desktop bearer is required for the official half,
// which is why this cannot run at startup.
func (h *modelsHandler) assemble(ctx context.Context, authorization, clientVersion, accountID string) ([]byte, string, error) {
	stockModels, errStock := h.fetchOfficialCatalog(ctx, authorization, clientVersion, accountID)
	if errStock != nil {
		return nil, "", errStock
	}

	// Refresh the Relay half in the same pass so the advertised catalog and the
	// routing table are derived from one observation.
	if _, errRelay := h.discovery.refresh(ctx); errRelay != nil {
		log.WithError(errRelay).Warn("root proxy could not refresh the relay catalog for discovery; using the last known set")
	}
	table := h.resolver.table()
	contextPolicy := h.modelContextPolicy(table)

	stockSlugs := make(map[string]struct{}, len(stockModels))
	for _, model := range stockModels {
		if slug, _ := model["slug"].(string); slug != "" {
			stockSlugs[slug] = struct{}{}
		}
	}
	relaySlugs := make([]string, 0, len(table.relay))
	for slug := range table.relay {
		// The official catalog wins an identifier collision outright, so a Relay
		// entry can never shadow a real ChatGPT model in the advertised list.
		if _, collides := stockSlugs[slug]; collides {
			log.Warnf("root proxy: relay model %q collides with an official model and is omitted from the catalog", slug)
			continue
		}
		relaySlugs = append(relaySlugs, slug)
	}
	sort.Strings(relaySlugs)

	relayModels, errRelayModels := synthesizeCodexModels(relaySlugs, func(slug string) (route, relayProvider, error) {
		return routeRelay, table.relayProvider(slug), nil
	}, h.preferWebsockets, h.multiAgentV2Policy(), contextPolicy)
	if errRelayModels != nil {
		return nil, "", errRelayModels
	}

	// Stock entries arrive verbatim from ChatGPT, so the advertisement is the one
	// deliberate exception to fetchOfficialCatalog's passthrough: the upstream
	// catalog reports gpt-5.6-luna as v1, which a v2 parent refuses to spawn.
	multiAgentV2 := h.multiAgentV2Policy()
	for _, model := range stockModels {
		slug, _ := model["slug"].(string)
		advertiseMultiAgentV2(model, slug, routeOfficial, "", multiAgentV2)
		applyModelContext(model, slug, routeOfficial, contextPolicy)
	}

	merged := make([]map[string]any, 0, len(stockModels)+len(relayModels))
	merged = append(merged, stockModels...)
	merged = append(merged, relayModels...)
	// Stock entries keep their upstream order and precede every Relay entry, so
	// the first official model remains Desktop's default.
	for index, model := range merged {
		model["priority"] = index + 1
		model["prefer_websockets"] = h.preferWebsockets
	}

	body, errEncode := encodeModelsPayload(merged)
	if errEncode != nil {
		return nil, "", errEncode
	}
	return body, strongETag(body), nil
}

// fetchOfficialCatalog reads the live ChatGPT Codex catalog using the caller's
// Desktop bearer. Stock metadata is passed through untouched so Desktop keeps
// the upstream's real compatibility hashes and speed tiers.
func (h *modelsHandler) fetchOfficialCatalog(ctx context.Context, authorization, clientVersion, accountID string) ([]map[string]any, error) {
	target := h.officialBaseURL + "/models"
	if clientVersion != "" {
		target += "?" + url.Values{modelsClientVersionQuery: []string{clientVersion}}.Encode()
	}
	// Root installs no deadline; the caller's context is the only bound.
	upstream, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if errRequest != nil {
		return nil, fmt.Errorf("build official catalog request: %w", errRequest)
	}
	upstream.Header.Set("Authorization", authorization)
	upstream.Header.Set("Accept", "application/json")
	if strings.TrimSpace(accountID) != "" {
		upstream.Header.Set("ChatGPT-Account-ID", accountID)
	}
	// If-None-Match is deliberately not forwarded: a 304 would leave no upstream
	// body to merge the Relay half into.

	response, errDo := h.officialClient.Do(upstream)
	if errDo != nil {
		return nil, fmt.Errorf("fetch official catalog: %w", errDo)
	}
	defer func() {
		if errClose := response.Body.Close(); errClose != nil {
			log.Debugf("root proxy: close official catalog body error: %v", errClose)
		}
	}()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("official catalog returned HTTP %d", response.StatusCode)
	}
	body, errRead := io.ReadAll(io.LimitReader(response.Body, maxOfficialCatalogBytes))
	if errRead != nil {
		return nil, fmt.Errorf("read official catalog: %w", errRead)
	}
	var payload rootModelsPayload
	if errDecode := json.Unmarshal(body, &payload); errDecode != nil {
		return nil, fmt.Errorf("decode official catalog: %w", errDecode)
	}
	if len(payload.Models) == 0 {
		return nil, errors.New("official catalog contains no models")
	}
	return payload.Models, nil
}

func validateModelsQuery(requestURL *url.URL) error {
	if requestURL == nil || (requestURL.RawQuery == "" && !requestURL.ForceQuery) {
		return nil
	}
	query, errParse := url.ParseQuery(requestURL.RawQuery)
	if errParse != nil {
		return errors.New("model query parameters are malformed")
	}
	for name, values := range query {
		if name != modelsClientVersionQuery {
			return fmt.Errorf("model query parameter %q is unsupported", name)
		}
		if len(values) != 1 {
			return errors.New("client_version must appear at most once")
		}
		if strings.TrimSpace(values[0]) != values[0] {
			return errors.New("client_version has surrounding whitespace")
		}
	}
	return nil
}

func modelCatalogOriginAllowed(headers http.Header, allowed map[string]struct{}) bool {
	origins := headerValuesCaseInsensitive(headers, "Origin")
	if len(origins) == 0 {
		return true
	}
	if len(origins) != 1 {
		return false
	}
	_, ok := allowed[origins[0]]
	return ok
}

func etagMatches(headers http.Header, etag string) bool {
	values := headerValuesCaseInsensitive(headers, "If-None-Match")
	if len(values) != 1 {
		return false
	}
	for _, candidate := range strings.Split(values[0], ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == etag || strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}
