package rootproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/sjson"
)

const officialHTTPBaseURL = "https://chatgpt.com/backend-api/codex"

type httpEndpoint uint8

const (
	httpEndpointResponses httpEndpoint = iota + 1
	httpEndpointCompact
)

type httpBridgeOptions struct {
	officialBaseURL string
	relayBaseURL    string
	relayAPIKey     string
	stockModels     []string
	relayModels     []string
	relayProviders  map[string]string
	fastModels      map[string]struct{}
	relayAgents     bool
	resolver        *routeResolver
	discovery       *relayDiscovery
	maxRequestBody  int64
	allowedOrigins  []string
	officialClient  *http.Client
	relayClient     *http.Client
	officialCookies http.CookieJar
	logging         *rootLogManager
}

type httpBridge struct {
	routes          *routeResolver
	discovery       *relayDiscovery
	officialBaseURL string
	relayBaseURL    string
	relayAPIKey     string
	fastModels      map[string]struct{}
	relayAgents     bool
	maxRequestBody  int64
	allowedOrigins  map[string]struct{}
	officialClient  *http.Client
	relayClient     *http.Client
	logging         *rootLogManager
}

type optionalBool struct {
	present bool
	isNull  bool
	value   bool
}

func newHTTPBridge(options httpBridgeOptions) (*httpBridge, error) {
	officialBaseURL := options.officialBaseURL
	if strings.TrimSpace(officialBaseURL) == "" {
		officialBaseURL = officialHTTPBaseURL
	}
	validatedOfficial, errOfficial := validateHTTPBaseURL(officialBaseURL, "official")
	if errOfficial != nil {
		return nil, errOfficial
	}
	validatedRelay, errRelay := validateHTTPBaseURL(options.relayBaseURL, "relay")
	if errRelay != nil {
		return nil, errRelay
	}
	relayAPIKey := strings.TrimSpace(options.relayAPIKey)
	if relayAPIKey == "" {
		return nil, errors.New("relay API key is empty")
	}
	if options.maxRequestBody <= 0 {
		return nil, errors.New("maximum HTTP request body size must be positive")
	}
	// A shared resolver is injected when discovery is active; otherwise the
	// configured lists remain the complete static routing surface.
	routes := options.resolver
	if routes == nil {
		var errRoutes error
		routes, errRoutes = newRouteResolver(discoveryStatic, options.stockModels, options.relayModels, options.relayProviders)
		if errRoutes != nil {
			return nil, errRoutes
		}
	}
	if errOrigins := validateOrigins(options.allowedOrigins); errOrigins != nil {
		return nil, errOrigins
	}

	allowedOrigins := make(map[string]struct{}, len(options.allowedOrigins))
	for _, origin := range options.allowedOrigins {
		allowedOrigins[origin] = struct{}{}
	}

	officialClient := options.officialClient
	if officialClient == nil {
		proxyURL, errProxy := environmentProxyURL(validatedOfficial)
		if errProxy != nil {
			return nil, fmt.Errorf("resolve official HTTP proxy: %w", errProxy)
		}
		officialClient = helps.NewUtlsHTTPClientWithProxyURL(context.Background(), proxyURL, 0)
	}
	relayClient := options.relayClient
	if relayClient == nil {
		relayClient = &http.Client{Transport: newDirectHTTPTransport()}
	}
	officialClient = withoutRedirects(officialClient)
	if officialClient.Jar == nil {
		officialCookies := options.officialCookies
		if officialCookies == nil {
			var errCookies error
			officialCookies, errCookies = newChatGPTCloudflareCookieJar()
			if errCookies != nil {
				return nil, fmt.Errorf("create ChatGPT Cloudflare cookie jar: %w", errCookies)
			}
		}
		officialClient.Jar = officialCookies
	}
	relayClient = withoutRedirects(relayClient)

	return &httpBridge{
		routes:          routes,
		discovery:       options.discovery,
		officialBaseURL: validatedOfficial,
		relayBaseURL:    validatedRelay,
		relayAPIKey:     relayAPIKey,
		fastModels:      options.fastModels,
		relayAgents:     options.relayAgents,
		maxRequestBody:  options.maxRequestBody,
		allowedOrigins:  allowedOrigins,
		officialClient:  officialClient,
		relayClient:     relayClient,
		logging:         options.logging,
	}, nil
}

func environmentProxyURL(target string) (string, error) {
	return resolveEnvironmentProxyURL(target, http.ProxyFromEnvironment)
}

func resolveEnvironmentProxyURL(target string, resolve func(*http.Request) (*url.URL, error)) (string, error) {
	parsed, errParse := url.Parse(target)
	if errParse != nil {
		return "", errParse
	}
	proxyURL, errProxy := resolve(&http.Request{URL: parsed})
	if errProxy != nil {
		return "", errors.New("environment proxy configuration is invalid")
	}
	if proxyURL == nil {
		return "", nil
	}
	switch proxyURL.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return "", errors.New("environment HTTPS proxy scheme is unsupported")
	}
	if proxyURL.Host == "" {
		return "", errors.New("environment HTTPS proxy URL is invalid")
	}
	return proxyURL.String(), nil
}

func validateHTTPBaseURL(rawURL, label string) (string, error) {
	parsed, errParse := url.Parse(strings.TrimSpace(rawURL))
	if errParse != nil {
		return "", fmt.Errorf("parse %s HTTP base URL: %w", label, errParse)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("%s HTTP base URL scheme %q must be http or https", label, parsed.Scheme)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("%s HTTP base URL host is empty", label)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawPath != "" {
		return "", fmt.Errorf("%s HTTP base URL contains unsupported URL components", label)
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/")
	return parsed.String(), nil
}

func newDirectHTTPTransport() *http.Transport {
	return &http.Transport{
		Proxy:              nil,
		DialContext:        (&net.Dialer{}).DialContext,
		ForceAttemptHTTP2:  true,
		DisableCompression: true,
	}
}

func withoutRedirects(source *http.Client) *http.Client {
	if source == nil {
		source = &http.Client{}
	}
	client := *source
	client.Timeout = 0
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &client
}

func (b *httpBridge) ServeResponses(response http.ResponseWriter, request *http.Request) {
	setAccessTransport(request.Context(), "http_sse", "responses")
	b.serve(response, request, httpEndpointResponses)
}

func (b *httpBridge) ServeCompact(response http.ResponseWriter, request *http.Request) {
	setAccessTransport(request.Context(), "http_compact", "responses/compact")
	b.serve(response, request, httpEndpointCompact)
}

func (b *httpBridge) serve(response http.ResponseWriter, request *http.Request, endpoint httpEndpoint) {
	if request.Method != http.MethodPost {
		response.Header().Set("Allow", http.MethodPost)
		writeHTTPError(response, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed", "")
		return
	}
	if request.URL.RawQuery != "" || request.URL.ForceQuery {
		writeHTTPError(response, http.StatusBadRequest, "invalid_request_error", "query parameters are unsupported", "")
		return
	}
	if len(headerValuesCaseInsensitive(request.Header, rootHopHeader)) != 0 {
		writeHTTPError(response, http.StatusLoopDetected, "invalid_request_error", "root proxy loop detected", "")
		return
	}
	if !b.originAllowed(request.Header) {
		writeHTTPError(response, http.StatusForbidden, "invalid_request_error", "origin is not allowed", "")
		return
	}
	if _, errAuthorization := singleBearerAuthorization(request.Header); errAuthorization != nil {
		response.Header().Set("WWW-Authenticate", "Bearer")
		writeHTTPError(response, http.StatusUnauthorized, "authentication_error", "Desktop bearer authorization is required", "")
		return
	}
	if errContentType := requireJSONContentType(request.Header); errContentType != nil {
		writeHTTPError(response, http.StatusUnsupportedMediaType, "invalid_request_error", errContentType.Error(), "")
		return
	}

	rawBody, decodedBody, contentEncoding, errBody := readHTTPBody(request, b.maxRequestBody)
	if errBody != nil {
		var tooLarge *requestBodyTooLargeError
		if errors.As(errBody, &tooLarge) {
			writeHTTPError(response, http.StatusRequestEntityTooLarge, "invalid_request_error", tooLarge.Error(), "")
		} else {
			writeHTTPError(response, http.StatusBadRequest, "invalid_request_error", errBody.Error(), "")
		}
		return
	}
	setAccessRequestBytes(request.Context(), int64(len(rawBody)))
	model, errModel := inspectHTTPModel(decodedBody)
	if errModel != nil {
		writeHTTPError(response, http.StatusBadRequest, "invalid_request_error", errModel.Error(), "model")
		return
	}
	stream, errStream := inspectHTTPStream(decodedBody)
	if errStream != nil {
		writeHTTPError(response, http.StatusBadRequest, "invalid_request_error", errStream.Error(), "stream")
		return
	}
	// Without this a Relay that was unreachable at startup would leave every
	// Relay model looking like an official one.
	b.discovery.ensure(request.Context())
	selected, errRoute := b.routes.classify(model)
	if errRoute != nil {
		writeHTTPError(response, http.StatusBadRequest, "invalid_request_error", "model is not routable", "model")
		return
	}
	transport := "http_sse"
	endpointName := "responses"
	if endpoint == httpEndpointCompact {
		transport = "http_compact"
		endpointName = "responses/compact"
	}
	updateAccessSelection(request.Context(), transport, endpointName, selected, model)
	var exchange *stockExchange
	if selected == routeOfficial {
		exchange = b.logging.beginStockExchange(request.Context(), selected, transport, endpointName, model)
		exchange.recordPayload("request", "desktop_to_root", decodedBody, map[string]any{
			"representation":          "decoded_inspection",
			"source_content_encoding": contentEncoding,
		})
		defer exchange.finish("incomplete", false, "handler_returned_before_exchange_completed")
	}
	if selected == routeRelay {
		createsCompaction := endpoint == httpEndpointCompact
		if errState := validateRelayPayloadState(decodedBody, b.routes.relayProvider(model), createsCompaction); errState != nil {
			writeHTTPError(response, http.StatusConflict, "cross_provider_compaction_not_portable", "Compaction state is not portable to the selected Relay provider; start a new conversation chain", "input")
			return
		}
	}

	forwardBody := rawBody
	forwardEncoding := contentEncoding
	switch endpoint {
	case httpEndpointResponses:
		if !stream.present || stream.isNull || !stream.value {
			writeStockHTTPError(response, exchange, http.StatusBadRequest, "invalid_request_error", "HTTP Responses requires stream true", "stream", "rejected", "streaming_required")
			return
		}
		if selected == routeOfficial {
			parentBody := normalizeRelayMultiAgentParentPayload(decodedBody, b.relayAgents)
			normalizedBody, errPrepare := prepareOfficialPayload(parentBody)
			if errPrepare != nil {
				writeStockHTTPError(response, exchange, http.StatusBadRequest, "invalid_request_error", errPrepare.Error(), "input", "rejected", "official_payload_invalid")
				return
			}
			var errTier error
			normalizedBody, errTier = applyOfficialFastServiceTier(normalizedBody, model, b.fastModels)
			if errTier != nil {
				writeStockHTTPError(response, exchange, http.StatusBadRequest, "invalid_request_error", errTier.Error(), "service_tier", "rejected", "official_service_tier_not_applied")
				return
			}
			if !bytes.Equal(normalizedBody, decodedBody) {
				forwardBody = normalizedBody
				forwardEncoding = ""
			}
		}
	case httpEndpointCompact:
		if stream.present && !stream.isNull && stream.value {
			writeStockHTTPError(response, exchange, http.StatusBadRequest, "invalid_request_error", "streaming is unsupported for compact responses", "stream", "rejected", "compact_streaming_unsupported")
			return
		}
		normalizedBody := decodedBody
		if stream.present {
			var errDelete error
			normalizedBody, errDelete = sjson.DeleteBytes(normalizedBody, "stream")
			if errDelete != nil {
				writeStockHTTPError(response, exchange, http.StatusBadRequest, "invalid_request_error", "compact stream field could not be normalized", "stream", "rejected", "compact_stream_normalization_failed")
				return
			}
		}
		if selected == routeOfficial {
			var errPrepare error
			normalizedBody, errPrepare = prepareOfficialPayload(normalizedBody)
			if errPrepare != nil {
				writeStockHTTPError(response, exchange, http.StatusBadRequest, "invalid_request_error", errPrepare.Error(), "input", "rejected", "official_payload_invalid")
				return
			}
		}
		if contentEncoding == "zstd" && bytes.Equal(normalizedBody, decodedBody) {
			forwardBody = rawBody
			forwardEncoding = contentEncoding
		} else {
			forwardBody = normalizedBody
			forwardEncoding = ""
		}
	default:
		writeStockHTTPError(response, exchange, http.StatusNotFound, "invalid_request_error", "endpoint is unavailable", "", "rejected", "endpoint_unavailable")
		return
	}

	if selected == routeOfficial {
		exchange.recordPayload("request", "root_to_official", forwardBody, map[string]any{
			"representation":   "forwarded",
			"content_encoding": forwardEncoding,
		})
	}
	b.forward(response, request, selected, endpoint, forwardBody, forwardEncoding, exchange)
}

func (b *httpBridge) originAllowed(headers http.Header) bool {
	origins := headerValuesCaseInsensitive(headers, "Origin")
	if len(origins) == 0 {
		return true
	}
	if len(origins) != 1 {
		return false
	}
	_, allowed := b.allowedOrigins[origins[0]]
	return allowed
}

func requireJSONContentType(headers http.Header) error {
	values := headerValuesCaseInsensitive(headers, "Content-Type")
	if len(values) != 1 {
		return errors.New("Content-Type application/json is required")
	}
	mediaType, _, errParse := mime.ParseMediaType(values[0])
	if errParse != nil || !strings.EqualFold(mediaType, "application/json") {
		return errors.New("Content-Type application/json is required")
	}
	return nil
}

type requestBodyTooLargeError struct {
	which string
}

func (e *requestBodyTooLargeError) Error() string {
	return e.which + " request body exceeds the configured limit"
}

func readHTTPBody(request *http.Request, limit int64) ([]byte, []byte, string, error) {
	if request.Body == nil {
		return nil, nil, "", errors.New("request body is empty")
	}
	if request.ContentLength > limit {
		return nil, nil, "", &requestBodyTooLargeError{which: "raw"}
	}
	raw, errRead := io.ReadAll(io.LimitReader(request.Body, limit+1))
	if errRead != nil {
		return nil, nil, "", fmt.Errorf("read request body: %w", errRead)
	}
	if int64(len(raw)) > limit {
		return nil, nil, "", &requestBodyTooLargeError{which: "raw"}
	}
	encoding, errEncoding := requestContentEncoding(request.Header)
	if errEncoding != nil {
		return nil, nil, "", errEncoding
	}
	if encoding != "zstd" {
		return raw, raw, encoding, nil
	}
	decoder, errDecoder := zstd.NewReader(
		bytes.NewReader(raw),
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderLowmem(true),
		zstd.WithDecoderMaxMemory(uint64(limit+1)),
	)
	if errDecoder != nil {
		return nil, nil, "", fmt.Errorf("create zstd request decoder: %w", errDecoder)
	}
	defer decoder.Close()
	decoded, errDecoded := io.ReadAll(io.LimitReader(decoder, limit+1))
	if errDecoded != nil {
		return nil, nil, "", fmt.Errorf("decode zstd request body: %w", errDecoded)
	}
	if int64(len(decoded)) > limit {
		return nil, nil, "", &requestBodyTooLargeError{which: "decoded"}
	}
	return raw, decoded, encoding, nil
}

func requestContentEncoding(headers http.Header) (string, error) {
	values := headerValuesCaseInsensitive(headers, "Content-Encoding")
	if len(values) == 0 {
		return "", nil
	}
	if len(values) != 1 || strings.Contains(values[0], ",") {
		return "", errors.New("exactly one request content encoding is supported")
	}
	encoding := strings.ToLower(strings.TrimSpace(values[0]))
	switch encoding {
	case "", "identity":
		return encoding, nil
	case "zstd":
		return encoding, nil
	default:
		return "", fmt.Errorf("unsupported request content encoding %q", encoding)
	}
}

func inspectHTTPStream(payload []byte) (optionalBool, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	opening, errOpening := decoder.Token()
	if errOpening != nil {
		return optionalBool{}, fmt.Errorf("decode request: %w", errOpening)
	}
	openingObject, ok := opening.(json.Delim)
	if !ok || openingObject != '{' {
		return optionalBool{}, errors.New("request must be a JSON object")
	}

	var stream optionalBool
	for decoder.More() {
		keyToken, errKey := decoder.Token()
		if errKey != nil {
			return optionalBool{}, fmt.Errorf("decode request key: %w", errKey)
		}
		key, okKey := keyToken.(string)
		if !okKey {
			return optionalBool{}, errors.New("request contains a non-string key")
		}
		var raw json.RawMessage
		if errValue := decoder.Decode(&raw); errValue != nil {
			return optionalBool{}, fmt.Errorf("decode request field %q: %w", key, errValue)
		}
		if key != "stream" {
			continue
		}
		if stream.present {
			return optionalBool{}, errors.New("request contains duplicate stream fields")
		}
		stream.present = true
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			stream.isNull = true
			continue
		}
		if errBool := json.Unmarshal(raw, &stream.value); errBool != nil {
			return optionalBool{}, errors.New("request stream must be a boolean or null")
		}
	}
	if _, errClosing := decoder.Token(); errClosing != nil {
		return optionalBool{}, fmt.Errorf("decode request closing token: %w", errClosing)
	}
	var trailing any
	if errTrailing := decoder.Decode(&trailing); !errors.Is(errTrailing, io.EOF) {
		if errTrailing == nil {
			return optionalBool{}, errors.New("request contains multiple JSON values")
		}
		return optionalBool{}, fmt.Errorf("decode trailing request: %w", errTrailing)
	}
	return stream, nil
}

func (b *httpBridge) forward(response http.ResponseWriter, inbound *http.Request, selected route, endpoint httpEndpoint, body []byte, contentEncoding string, exchange *stockExchange) {
	headers, errHeaders := buildUpstreamHeaders(inbound.Header, selected, b.relayAPIKey)
	if errHeaders != nil {
		setAccessOutcome(inbound.Context(), "failed", "upstream_headers_unavailable", 0)
		writeStockHTTPError(response, exchange, http.StatusUnauthorized, "authentication_error", "required authentication is unavailable", "", "failed", "upstream_headers_unavailable")
		return
	}
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept-Encoding", "identity")
	path := "/responses"
	streamResponse := true
	if endpoint == httpEndpointCompact {
		path = "/responses/compact"
		streamResponse = false
		headers.Set("Accept", "application/json")
	} else {
		headers.Set("Accept", "text/event-stream")
	}
	if contentEncoding != "" {
		headers.Set("Content-Encoding", contentEncoding)
	}

	baseURL := b.relayBaseURL
	client := b.relayClient
	if selected == routeOfficial {
		baseURL = b.officialBaseURL
		client = b.officialClient
	}
	upstreamRequest, errRequest := http.NewRequestWithContext(
		inbound.Context(),
		http.MethodPost,
		baseURL+path,
		io.NopCloser(bytes.NewReader(body)),
	)
	if errRequest != nil {
		setAccessOutcome(inbound.Context(), "failed", "upstream_request_creation_failed", 0)
		writeStockHTTPError(response, exchange, http.StatusBadGateway, "upstream_error", "upstream request could not be created", "", "failed", "upstream_request_creation_failed")
		return
	}
	upstreamRequest.Header = headers
	upstreamRequest.ContentLength = int64(len(body))
	upstreamRequest.GetBody = nil

	upstreamResponse, errDo := client.Do(upstreamRequest)
	if errDo != nil {
		if inbound.Context().Err() != nil {
			exchange.finish("canceled", false, "downstream_context_canceled")
			setAccessOutcome(inbound.Context(), "canceled", "downstream_context_canceled", 0)
			return
		}
		setAccessOutcome(inbound.Context(), "failed", "upstream_unavailable", 0)
		log.WithError(errDo).WithField("provider", selected.String()).Warn("root proxy HTTP upstream request failed")
		writeStockHTTPError(response, exchange, http.StatusBadGateway, "upstream_error", "upstream unavailable", "", "failed", "upstream_unavailable")
		return
	}
	defer func() {
		if errClose := upstreamResponse.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("root proxy failed to close HTTP upstream response")
		}
	}()
	setAccessUpstreamStatus(inbound.Context(), upstreamResponse.StatusCode)
	exchange.writeEvent(map[string]any{
		"kind":   "response_start",
		"status": upstreamResponse.StatusCode,
	})

	copyHTTPResponseHeaders(response.Header(), upstreamResponse.Header)
	response.WriteHeader(upstreamResponse.StatusCode)
	responseContentEncoding := strings.Join(headerValuesCaseInsensitive(upstreamResponse.Header, "Content-Encoding"), ",")
	copyResult := b.copyResponseBody(response, upstreamResponse.Body, streamResponse, responseContentEncoding, exchange)
	if copyResult.complete {
		exchange.finish(capturedHTTPOutcome(upstreamResponse.StatusCode), true, "")
		return
	}
	if copyResult.downstreamFailure {
		exchange.finish("incomplete", false, "downstream_write_failed")
		setAccessOutcome(inbound.Context(), "canceled", "downstream_write_failed", 0)
		return
	}
	exchange.finish("incomplete", false, "upstream_read_failed")
	setAccessOutcome(inbound.Context(), "failed", "upstream_read_failed", 0)
}

func capturedHTTPOutcome(status int) string {
	switch {
	case status >= http.StatusInternalServerError:
		return "failed"
	case status >= http.StatusBadRequest:
		return "rejected"
	default:
		return "completed"
	}
}

type responseCopyResult struct {
	complete          bool
	downstreamFailure bool
}

func (b *httpBridge) copyResponseBody(response http.ResponseWriter, body io.Reader, flush bool, contentEncoding string, exchange *stockExchange) responseCopyResult {
	buffer := make([]byte, 32<<10)
	flusher, canFlush := response.(http.Flusher)
	sequence := 0
	for {
		count, errRead := body.Read(buffer)
		if count > 0 {
			sequence++
			exchange.recordPayload("response_chunk", "official_to_root", buffer[:count], map[string]any{
				"chunk":            sequence,
				"content_encoding": strings.TrimSpace(contentEncoding),
			})
			written, errWrite := response.Write(buffer[:count])
			if errWrite != nil || written != count {
				return responseCopyResult{downstreamFailure: true}
			}
			if flush && canFlush {
				flusher.Flush()
			}
		}
		if errRead != nil {
			if !errors.Is(errRead, io.EOF) {
				log.WithError(errRead).Debug("root proxy HTTP upstream stream ended")
				return responseCopyResult{}
			}
			return responseCopyResult{complete: true}
		}
	}
}

func copyHTTPResponseHeaders(target, source http.Header) {
	connectionHeaders := make(map[string]struct{})
	for _, value := range headerValuesCaseInsensitive(source, "Connection") {
		for _, name := range strings.Split(value, ",") {
			if trimmed := strings.ToLower(strings.TrimSpace(name)); trimmed != "" {
				connectionHeaders[trimmed] = struct{}{}
			}
		}
	}
	for name, values := range source {
		lowerName := strings.ToLower(strings.TrimSpace(name))
		if responseHeaderIsUnsafe(lowerName, connectionHeaders) {
			continue
		}
		for _, value := range values {
			target.Add(name, value)
		}
	}
}

func responseHeaderIsUnsafe(lowerName string, connectionHeaders map[string]struct{}) bool {
	if _, nominated := connectionHeaders[lowerName]; nominated {
		return true
	}
	switch lowerName {
	case "alt-svc", "connection", "content-length", "keep-alive", "proxy-authenticate", "proxy-authorization", "proxy-connection", "proxy-status", "server", "set-cookie", "te", "trailer", "transfer-encoding", "upgrade", "via", "x-powered-by":
		return true
	}
	return strings.HasPrefix(lowerName, "access-control-") ||
		strings.HasPrefix(lowerName, "cf-") ||
		strings.HasPrefix(lowerName, "x-cliproxy-") ||
		strings.HasPrefix(lowerName, "x-forwarded-") ||
		strings.HasPrefix(lowerName, "x-envoy-")
}

func writeHTTPError(response http.ResponseWriter, status int, code, message, parameter string) {
	payload := encodeHTTPError(code, message, parameter)
	writeEncodedHTTPError(response, status, payload)
}

func writeStockHTTPError(response http.ResponseWriter, exchange *stockExchange, status int, code, message, parameter, outcome, detail string) {
	payload := encodeHTTPError(code, message, parameter)
	exchange.recordPayload("response", "root_to_desktop", payload, map[string]any{
		"representation":   "generated",
		"content_encoding": "",
		"status":           status,
	})
	written := writeEncodedHTTPError(response, status, payload)
	exchange.finish(outcome, written, detail)
}

func encodeHTTPError(code, message, parameter string) []byte {
	payload := struct {
		Error struct {
			Message string  `json:"message"`
			Type    string  `json:"type"`
			Code    string  `json:"code"`
			Param   *string `json:"param"`
		} `json:"error"`
	}{}
	payload.Error.Message = message
	payload.Error.Type = code
	payload.Error.Code = code
	if parameter != "" {
		payload.Error.Param = &parameter
	}
	var encoded bytes.Buffer
	if errEncode := json.NewEncoder(&encoded).Encode(payload); errEncode != nil {
		log.WithError(errEncode).Debug("root proxy failed to write HTTP error")
		return nil
	}
	return encoded.Bytes()
}

func writeEncodedHTTPError(response http.ResponseWriter, status int, payload []byte) bool {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	written, errWrite := response.Write(payload)
	if errWrite != nil {
		log.WithError(errWrite).Debug("root proxy failed to write HTTP error")
	}
	return errWrite == nil && written == len(payload)
}
