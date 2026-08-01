package rootproxy

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	openaihandler "github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers/openai"
	log "github.com/sirupsen/logrus"
)

const modelsClientVersionQuery = "client_version"

type modelsHandler struct {
	body           []byte
	etag           string
	allowedOrigins map[string]struct{}
}

type rootModelsPayload struct {
	Models []map[string]any `json:"models"`
}

func newModelsHandler(config *Config) (*modelsHandler, error) {
	if config == nil {
		return nil, errors.New("root proxy config is nil")
	}
	routes, errRoutes := buildRouteTable(
		config.Routing.StockModels,
		config.Routing.RelayModels,
		config.Routing.RelayModelProviders,
	)
	if errRoutes != nil {
		return nil, errRoutes
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

	configured := make([]string, 0, len(config.Routing.StockModels)+len(config.Routing.RelayModels))
	configured = append(configured, config.Routing.StockModels...)
	configured = append(configured, config.Routing.RelayModels...)
	inputs := make([]map[string]any, 0, len(configured))
	for _, model := range configured {
		inputs = append(inputs, map[string]any{"id": model})
	}

	generated := openaihandler.CodexClientModelsResponse(inputs)
	generatedModels, ok := generated["models"].([]map[string]any)
	if !ok || len(generatedModels) != len(configured) {
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

	models := make([]map[string]any, 0, len(configured))
	for index, slug := range configured {
		model, exists := bySlug[slug]
		if !exists {
			return nil, fmt.Errorf("generated Codex catalog is missing configured model %q", slug)
		}
		selected, errRoute := routes.classify(slug)
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
			sanitizeRelayModelMetadata(model, routes.relayProvider(slug))
		}
		models = append(models, model)
	}

	body, errMarshal := json.Marshal(rootModelsPayload{Models: models})
	if errMarshal != nil {
		return nil, fmt.Errorf("encode Root model catalog: %w", errMarshal)
	}
	body = append(body, '\n')
	digest := sha256.Sum256(body)
	allowedOrigins := make(map[string]struct{}, len(config.Websocket.AllowedOrigins))
	for _, origin := range config.Websocket.AllowedOrigins {
		allowedOrigins[origin] = struct{}{}
	}
	return &modelsHandler{
		body:           body,
		etag:           fmt.Sprintf("\"%x\"", digest),
		allowedOrigins: allowedOrigins,
	}, nil
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
	delete(model, "multi_agent_version")
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

	response.Header().Set("Cache-Control", "private, no-cache")
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("ETag", h.etag)
	response.Header().Set("X-Content-Type-Options", "nosniff")
	if etagMatches(request.Header, h.etag) {
		response.WriteHeader(http.StatusNotModified)
		return
	}
	response.WriteHeader(http.StatusOK)
	if request.Method == http.MethodHead {
		return
	}
	if _, errWrite := response.Write(h.body); errWrite != nil {
		log.WithError(errWrite).Debug("root proxy failed to write model catalog")
	}
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
