package rootproxy

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
)

const websocketCloseReasonMaxBytes = 123

type websocketDialFunc func(context.Context, string, http.Header) (*websocket.Conn, *http.Response, error)

type bridgeOptions struct {
	officialURL      string
	relayURL         string
	relayAPIKey      string
	stockModels      []string
	relayModels      []string
	relayProviders   map[string]string
	fastModels       map[string]struct{}
	relayAgents      bool
	resolver         *routeResolver
	discovery        *relayDiscovery
	maxMessageBytes  int64
	maxPendingRoutes int
	allowedOrigins   []string
	dialOfficial     websocketDialFunc
	dialRelay        websocketDialFunc
	officialCookies  http.CookieJar
	logging          *rootLogManager
}

type websocketBridge struct {
	upgrader          websocket.Upgrader
	routes            *routeResolver
	discovery         *relayDiscovery
	officialURL       string
	relayURL          string
	relayAPIKey       string
	fastModels        map[string]struct{}
	relayAgents       bool
	maxMessage        int64
	maxPending        int
	dialOfficial      websocketDialFunc
	dialRelay         websocketDialFunc
	officialCookies   http.CookieJar
	officialCookieURL *url.URL
	logging           *rootLogManager
	pendingMu         sync.Mutex
	pendingRoutes     []*websocketSession
	handoffSlots      chan struct{}

	sessionsMu sync.Mutex
	sessions   map[*websocketSession]struct{}
	sessionsWG sync.WaitGroup
	closing    bool
}

type websocketSession struct {
	mu         sync.Mutex
	downstream *websocketPeer
	upstreams  map[*websocketPeer]struct{}
	cancel     context.CancelFunc
	accessCtx  context.Context
	closed     bool
}

type websocketPeer struct {
	connection *websocket.Conn
	writeMu    sync.Mutex
	closing    atomic.Bool
}

type websocketReadSource uint8

const (
	readFromDownstream websocketReadSource = iota + 1
	readFromUpstream
)

type websocketReadResult struct {
	source      websocketReadSource
	peer        *websocketPeer
	generation  uint64
	messageType int
	payload     []byte
	err         error
}

type websocketControllerState struct {
	route            route
	relayProvider    relayProvider
	model            string
	peer             *websocketPeer
	generation       uint64
	outstandingTurns int
	stockExchanges   []*stockExchange
}

type pendingRouteSwitch struct {
	messageType int
	payload     []byte
	envelope    clientMessageEnvelope
}

type clientPolicyError struct {
	reason string
}

func (e *clientPolicyError) Error() string {
	return e.reason
}

func newWebsocketBridge(options bridgeOptions) (*websocketBridge, error) {
	if errURL := validateWebsocketURL(options.officialURL, "official"); errURL != nil {
		return nil, errURL
	}
	if errURL := validateWebsocketURL(options.relayURL, "relay"); errURL != nil {
		return nil, errURL
	}
	if strings.TrimSpace(options.relayAPIKey) == "" {
		return nil, errors.New("relay API key is empty")
	}
	if options.maxMessageBytes <= 0 {
		return nil, errors.New("maximum websocket message size must be positive")
	}
	if options.maxPendingRoutes <= 0 {
		return nil, errors.New("maximum pending websocket routes must be positive")
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
	officialCookieURL, errCookieURL := websocketCookieURL(options.officialURL)
	if errCookieURL != nil {
		return nil, fmt.Errorf("create official websocket cookie URL: %w", errCookieURL)
	}
	officialCookies := options.officialCookies
	if officialCookies == nil {
		var errCookies error
		officialCookies, errCookies = newChatGPTCloudflareCookieJar()
		if errCookies != nil {
			return nil, fmt.Errorf("create ChatGPT Cloudflare cookie jar: %w", errCookies)
		}
	}

	allowedOrigins := make(map[string]struct{}, len(options.allowedOrigins))
	for _, origin := range options.allowedOrigins {
		allowedOrigins[origin] = struct{}{}
	}
	checkOrigin := func(request *http.Request) bool {
		origins := request.Header.Values("Origin")
		if len(origins) == 0 {
			return true
		}
		if len(origins) != 1 {
			return false
		}
		_, allowed := allowedOrigins[origins[0]]
		return allowed
	}

	officialDialer := &websocket.Dialer{
		Proxy:             http.ProxyFromEnvironment,
		EnableCompression: false,
		ReadBufferSize:    4096,
		WriteBufferSize:   4096,
	}
	relayDialer := &websocket.Dialer{
		Proxy:             nil,
		EnableCompression: false,
		ReadBufferSize:    4096,
		WriteBufferSize:   4096,
	}
	if options.dialOfficial == nil {
		options.dialOfficial = officialDialer.DialContext
	}
	if options.dialRelay == nil {
		options.dialRelay = relayDialer.DialContext
	}

	return &websocketBridge{
		upgrader: websocket.Upgrader{
			ReadBufferSize:    4096,
			WriteBufferSize:   4096,
			EnableCompression: false,
			CheckOrigin:       checkOrigin,
		},
		routes:            routes,
		discovery:         options.discovery,
		officialURL:       options.officialURL,
		relayURL:          options.relayURL,
		relayAPIKey:       strings.TrimSpace(options.relayAPIKey),
		fastModels:        options.fastModels,
		relayAgents:       options.relayAgents,
		maxMessage:        options.maxMessageBytes,
		maxPending:        options.maxPendingRoutes,
		dialOfficial:      options.dialOfficial,
		dialRelay:         options.dialRelay,
		officialCookies:   officialCookies,
		officialCookieURL: officialCookieURL,
		logging:           options.logging,
		handoffSlots:      make(chan struct{}, options.maxPendingRoutes),
		sessions:          make(map[*websocketSession]struct{}),
	}, nil
}

func validateWebsocketURL(rawURL, label string) error {
	parsed, errParse := url.Parse(rawURL)
	if errParse != nil {
		return fmt.Errorf("parse %s websocket URL: %w", label, errParse)
	}
	if parsed.Scheme != "ws" && parsed.Scheme != "wss" {
		return fmt.Errorf("%s websocket URL scheme %q must be ws or wss", label, parsed.Scheme)
	}
	if parsed.Host == "" {
		return fmt.Errorf("%s websocket URL host is empty", label)
	}
	if parsed.User != nil || parsed.Fragment != "" {
		return fmt.Errorf("%s websocket URL must not contain user information or a fragment", label)
	}
	return nil
}

func websocketCookieURL(rawURL string) (*url.URL, error) {
	parsed, errParse := url.Parse(rawURL)
	if errParse != nil {
		return nil, errParse
	}
	switch parsed.Scheme {
	case "wss":
		parsed.Scheme = "https"
	case "ws":
		parsed.Scheme = "http"
	default:
		return nil, fmt.Errorf("unsupported websocket cookie URL scheme %q", parsed.Scheme)
	}
	return parsed, nil
}

func (b *websocketBridge) addOfficialCookies(selected route, headers http.Header) {
	if b == nil || selected != routeOfficial || b.officialCookies == nil || b.officialCookieURL == nil {
		return
	}
	request := &http.Request{Header: headers}
	for _, cookie := range b.officialCookies.Cookies(b.officialCookieURL) {
		request.AddCookie(cookie)
	}
}

func (b *websocketBridge) storeOfficialCookies(selected route, response *http.Response) {
	if b == nil || selected != routeOfficial || response == nil || b.officialCookies == nil || b.officialCookieURL == nil {
		return
	}
	b.officialCookies.SetCookies(b.officialCookieURL, response.Cookies())
}

func (b *websocketBridge) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	setAccessTransport(request.Context(), "websocket", "responses")
	if !b.validateDownstreamHandshake(response, request) {
		return
	}

	downstream, errUpgrade := b.upgrader.Upgrade(response, request, nil)
	if errUpgrade != nil {
		return
	}
	downstream.EnableWriteCompression(false)
	downstream.SetReadLimit(b.maxMessage)
	sessionContext, cancelSession := context.WithCancel(request.Context())
	session := &websocketSession{
		downstream: &websocketPeer{connection: downstream},
		upstreams:  make(map[*websocketPeer]struct{}),
		cancel:     cancelSession,
		accessCtx:  request.Context(),
	}
	if !b.register(session) {
		session.forceClose()
		return
	}
	var readers sync.WaitGroup
	defer b.unregister(session)
	defer func() {
		session.forceClose()
		readers.Wait()
	}()
	b.addPendingRoute(session)
	registrationPending := true
	defer func() {
		if registrationPending {
			b.removePendingRoute(session)
		}
	}()

	messageType, firstPayload, errFirst := b.readFirstMessage(downstream)
	if errFirst != nil {
		b.closeForFirstMessageError(session, errFirst)
		return
	}
	addAccessRequestBytes(request.Context(), len(firstPayload))
	model, errEnvelope := inspectFirstClientMessage(firstPayload)
	if errEnvelope != nil {
		setAccessOutcome(request.Context(), "rejected", "invalid_first_response_create", websocket.ClosePolicyViolation)
		session.terminate(websocket.ClosePolicyViolation, "invalid response.create", true, false)
		return
	}
	// Without this a Relay that was unreachable at startup would leave every
	// Relay model looking like an official one.
	b.discovery.ensure(request.Context())
	selected, errRoute := b.routes.classify(model)
	if errRoute != nil {
		setAccessOutcome(request.Context(), "rejected", "model_not_routable", websocket.ClosePolicyViolation)
		session.terminate(websocket.ClosePolicyViolation, "model is not routable", true, false)
		return
	}
	updateAccessSelection(request.Context(), "websocket", "responses", selected, model)
	var stockExchanges []*stockExchange
	var initialExchange *stockExchange
	if selected == routeOfficial {
		initialExchange = b.logging.beginStockExchange(request.Context(), selected, "websocket", "responses", model)
		recordWebsocketPayload(initialExchange, "request", "desktop_to_root", "received", messageType, firstPayload)
		if initialExchange != nil {
			stockExchanges = append(stockExchanges, initialExchange)
			defer initialExchange.finish("incomplete", false, "websocket_handler_returned_before_exchange_completed")
		}
	}
	selectedRelayProvider := relayProvider("")
	if selected == routeRelay {
		selectedRelayProvider = b.routes.relayProvider(model)
		if errState := validateRelayPayloadState(firstPayload, selectedRelayProvider, false); errState != nil {
			if b.writeNonPortableStateError(session) {
				setAccessOutcome(request.Context(), "rejected", "relay_request_state_not_portable", websocket.CloseNormalClosure)
				session.terminate(websocket.CloseNormalClosure, "non-portable compaction rejected", true, false)
			}
			return
		}
	}
	upstreamPayload := firstPayload
	if selected == routeOfficial {
		upstreamPayload = normalizeRelayMultiAgentParentPayload(firstPayload, b.relayAgents)
		upstreamPayload, errEnvelope = prepareOfficialPayload(upstreamPayload)
		if errEnvelope == nil {
			// The first message is always a response.create, so it is a turn.
			upstreamPayload, errEnvelope = applyOfficialFastServiceTier(upstreamPayload, model, b.fastModels)
		}
		if errEnvelope != nil {
			initialExchange.finish("rejected", false, "official_request_state_not_portable")
			setAccessOutcome(request.Context(), "rejected", "official_request_state_not_portable", websocket.ClosePolicyViolation)
			session.terminate(websocket.ClosePolicyViolation, "official request state is not portable", true, false)
			return
		}
		recordWebsocketPayload(initialExchange, "request", "root_to_official", "forwarded", messageType, upstreamPayload)
	}

	downstreamResults := make(chan websocketReadResult, 1)
	upstreamResults := make(chan websocketReadResult, b.maxPending+2)
	readers.Add(1)
	go b.readWebsocketLoop(sessionContext, session, session.downstream, readFromDownstream, 0, downstreamResults, &readers)

	upstreamHeaders, errHeaders := buildUpstreamHeaders(request.Header, selected, b.relayAPIKey)
	if errHeaders != nil {
		initialExchange.finish("failed", false, "upstream_headers_unavailable")
		setAccessOutcome(request.Context(), "failed", "upstream_headers_unavailable", websocket.ClosePolicyViolation)
		session.terminate(websocket.ClosePolicyViolation, "required authentication is unavailable", true, false)
		return
	}
	b.addOfficialCookies(selected, upstreamHeaders)
	targetURL, dial := b.target(selected)
	upstream, handshakeResponse, errDial := dial(sessionContext, targetURL, upstreamHeaders)
	b.storeOfficialCookies(selected, handshakeResponse)
	closeCode, closeReason := upstreamHandshakeClose(handshakeResponse)
	if handshakeResponse != nil {
		setAccessUpstreamStatus(request.Context(), handshakeResponse.StatusCode)
	}
	closeHandshakeResponse(handshakeResponse)
	if errDial != nil {
		outcome := "failed"
		if sessionContext.Err() != nil {
			outcome = "canceled"
		}
		initialExchange.finish(outcome, false, "upstream_websocket_dial_failed")
		setAccessOutcome(request.Context(), outcome, "upstream_websocket_dial_failed", closeCode)
		if !b.terminateFromBufferedDownstream(session, downstreamResults) {
			session.terminate(closeCode, closeReason, true, false)
		}
		return
	}
	upstream.EnableWriteCompression(false)
	upstream.SetReadLimit(b.maxMessage)
	setAccessUpstreamStatus(request.Context(), http.StatusSwitchingProtocols)
	upstreamPeer, okUpstream := session.addUpstream(upstream)
	if !okUpstream {
		initialExchange.finish("canceled", false, "session_closed_before_upstream_tracking")
		return
	}
	if errWrite := upstreamPeer.writeMessage(messageType, upstreamPayload); errWrite != nil {
		initialExchange.finish("failed", false, "upstream_websocket_write_failed")
		setAccessOutcome(request.Context(), "failed", "upstream_websocket_write_failed", websocket.CloseInternalServerErr)
		if !b.terminateFromBufferedDownstream(session, downstreamResults) {
			session.terminate(websocket.CloseInternalServerErr, "upstream unavailable", true, false)
		}
		return
	}
	readers.Add(1)
	go b.readWebsocketLoop(sessionContext, session, upstreamPeer, readFromUpstream, 1, upstreamResults, &readers)
	b.removePendingRoute(session)
	registrationPending = false

	log.WithFields(log.Fields{"provider": selected.String(), "relay_provider": selectedRelayProvider, "model": model}).Info("root proxy websocket route selected")
	b.runController(sessionContext, session, request.Header.Clone(), websocketControllerState{
		route:            selected,
		relayProvider:    selectedRelayProvider,
		model:            model,
		peer:             upstreamPeer,
		generation:       1,
		outstandingTurns: 1,
		stockExchanges:   stockExchanges,
	}, downstreamResults, upstreamResults, &readers)
}

func (b *websocketBridge) validateDownstreamHandshake(response http.ResponseWriter, request *http.Request) bool {
	if request.Method != http.MethodGet {
		response.Header().Set("Allow", http.MethodGet)
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	if request.URL.RawQuery != "" || request.URL.ForceQuery {
		http.Error(response, "websocket query parameters are unsupported", http.StatusBadRequest)
		return false
	}
	if len(request.Header.Values(rootHopHeader)) != 0 {
		http.Error(response, "root proxy loop detected", http.StatusLoopDetected)
		return false
	}
	if len(websocket.Subprotocols(request)) != 0 {
		http.Error(response, "websocket subprotocols are unsupported", http.StatusBadRequest)
		return false
	}
	if _, errAuthorization := singleBearerAuthorization(request.Header); errAuthorization != nil {
		response.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(response, "Desktop bearer authorization is required", http.StatusUnauthorized)
		return false
	}
	if !websocket.IsWebSocketUpgrade(request) {
		http.Error(response, "invalid websocket upgrade request", http.StatusBadRequest)
		return false
	}
	if !headerContainsToken(request.Header, "Sec-WebSocket-Version", "13") {
		http.Error(response, "unsupported websocket version", http.StatusBadRequest)
		return false
	}
	if b.upgrader.CheckOrigin == nil || !b.upgrader.CheckOrigin(request) {
		http.Error(response, "websocket origin is not allowed", http.StatusForbidden)
		return false
	}
	challenge, errChallenge := base64.StdEncoding.DecodeString(request.Header.Get("Sec-WebSocket-Key"))
	if errChallenge != nil || len(challenge) != 16 {
		http.Error(response, "invalid websocket challenge key", http.StatusBadRequest)
		return false
	}
	return true
}

func headerContainsToken(header http.Header, name, token string) bool {
	for _, value := range headerValuesCaseInsensitive(header, name) {
		for _, candidate := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(candidate), token) {
				return true
			}
		}
	}
	return false
}

func upstreamHandshakeClose(response *http.Response) (int, string) {
	if response == nil {
		return websocket.CloseInternalServerErr, "upstream unavailable"
	}
	switch response.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return websocket.ClosePolicyViolation, "upstream authentication rejected"
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		return websocket.CloseTryAgainLater, "upstream temporarily unavailable"
	default:
		return websocket.CloseInternalServerErr, "upstream unavailable"
	}
}

func (b *websocketBridge) readFirstMessage(downstream *websocket.Conn) (int, []byte, error) {
	messageType, payload, errRead := downstream.ReadMessage()
	if errRead != nil {
		return 0, nil, errRead
	}
	if messageType != websocket.TextMessage && messageType != websocket.BinaryMessage {
		return 0, nil, &clientPolicyError{reason: "first client message must contain JSON"}
	}
	return messageType, payload, nil
}

func (b *websocketBridge) closeForFirstMessageError(session *websocketSession, err error) {
	switch {
	case errors.Is(err, websocket.ErrReadLimit):
		setAccessOutcomeIfUnset(session.accessCtx, "rejected", "downstream_message_too_big", websocket.CloseMessageTooBig)
		session.terminate(websocket.CloseMessageTooBig, "message too big", false, false)
	default:
		var policy *clientPolicyError
		if errors.As(err, &policy) {
			session.terminate(websocket.ClosePolicyViolation, "invalid response.create", true, false)
			return
		}
		var closeError *websocket.CloseError
		if errors.As(err, &closeError) {
			session.terminate(0, "", false, false)
			return
		}
		session.terminate(websocket.CloseInternalServerErr, "websocket read failed", true, false)
	}
}

func closeHandshakeResponse(response *http.Response) {
	if response == nil || response.Body == nil {
		return
	}
	if errClose := response.Body.Close(); errClose != nil {
		log.WithError(errClose).Debug("root proxy failed to close upstream handshake response")
	}
}

func (b *websocketBridge) target(selected route) (string, websocketDialFunc) {
	if selected == routeOfficial {
		return b.officialURL, b.dialOfficial
	}
	return b.relayURL, b.dialRelay
}

func (b *websocketBridge) readWebsocketLoop(
	ctx context.Context,
	session *websocketSession,
	peer *websocketPeer,
	source websocketReadSource,
	generation uint64,
	results chan<- websocketReadResult,
	readers *sync.WaitGroup,
) {
	defer readers.Done()
	for {
		messageType, payload, errRead := peer.connection.ReadMessage()
		result := websocketReadResult{
			source:      source,
			peer:        peer,
			generation:  generation,
			messageType: messageType,
			payload:     payload,
			err:         errRead,
		}
		if source == readFromDownstream && errRead != nil {
			session.interruptFromDownstream(errRead)
			select {
			case results <- result:
			default:
			}
			return
		}
		select {
		case results <- result:
		case <-ctx.Done():
			return
		}
		if errRead != nil {
			return
		}
	}
}

func (b *websocketBridge) runController(
	ctx context.Context,
	session *websocketSession,
	inboundHeaders http.Header,
	state websocketControllerState,
	downstreamResults <-chan websocketReadResult,
	upstreamResults chan websocketReadResult,
	readers *sync.WaitGroup,
) {
	defer finishAllStockExchanges(&state, "incomplete", false, "websocket_session_ended")
	var pending *pendingRouteSwitch
	for {
		select {
		case <-ctx.Done():
			if !b.terminateFromBufferedDownstream(session, downstreamResults) {
				session.forceClose()
			}
			return
		case result := <-downstreamResults:
			if result.err != nil {
				b.terminateForReadResult(session, result)
				return
			}
			addAccessRequestBytes(ctx, len(result.payload))
			if result.messageType != websocket.TextMessage && result.messageType != websocket.BinaryMessage {
				session.terminatePolicyViolation()
				return
			}
			envelope, errInspect := inspectClientMessage(result.payload)
			if errInspect != nil {
				session.terminatePolicyViolation()
				return
			}
			nextRoute := state.route
			nextRelayProvider := state.relayProvider
			nextModel := state.model
			if envelope.hasModel {
				var errRoute error
				nextRoute, errRoute = b.routes.classify(envelope.model)
				if errRoute != nil {
					session.terminatePolicyViolation()
					return
				}
				nextModel = envelope.model
				if nextRoute == routeRelay {
					nextRelayProvider = b.routes.relayProvider(envelope.model)
				} else {
					nextRelayProvider = ""
				}
			}
			isCreate := envelope.hasEventType && envelope.eventType == "response.create"
			targetChanged := websocketTargetChanged(state, nextRoute, nextRelayProvider, nextModel)
			if targetChanged {
				if !isCreate {
					session.terminatePolicyViolation()
					return
				}
				// Opaque compaction remains bound to the provider that produced the
				// current chain. Check it before replayable state identifiers because
				// a full replay cannot make the opaque item portable.
				if payloadContainsCompaction(result.payload) {
					if !b.writeNonPortableStateError(session) {
						return
					}
					b.logRejectedStockWebsocketRequest(ctx, nextRoute, nextModel, result.messageType, result.payload, nonPortableStateErrorPayload(), "compaction_not_portable")
					continue
				}
				if nextRoute == routeRelay {
					if errState := validateRelayPayloadState(result.payload, nextRelayProvider, false); errState != nil {
						if !b.writeNonPortableStateError(session) {
							return
						}
						continue
					}
				}
				if envelope.referencesUpstreamState() {
					localPayload := stateReplayErrorPayload(envelope)
					if errWrite := session.downstream.writeMessage(websocket.TextMessage, localPayload); errWrite != nil {
						session.forceClose()
						return
					}
					addAccessResponseBytes(session.accessCtx, len(localPayload))
					b.logRejectedStockWebsocketRequest(ctx, nextRoute, nextModel, result.messageType, result.payload, localPayload, "upstream_state_requires_replay")
					continue
				}
				switchRequest := &pendingRouteSwitch{
					messageType: result.messageType,
					payload:     append([]byte(nil), result.payload...),
					envelope:    envelope,
				}
				if pending != nil {
					session.terminatePolicyViolation()
					return
				}
				if state.outstandingTurns > 0 {
					pending = switchRequest
					continue
				}
				if !b.performHandoff(ctx, session, inboundHeaders, &state, switchRequest, downstreamResults, upstreamResults, readers) {
					return
				}
				continue
			}
			if pending != nil && isCreate {
				session.terminatePolicyViolation()
				return
			}

			upstreamPayload := result.payload
			var requestExchange *stockExchange
			if state.route == routeOfficial {
				if isCreate {
					requestExchange = b.logging.beginStockExchange(ctx, state.route, "websocket", "responses", nextModel)
					if requestExchange != nil {
						state.stockExchanges = append(state.stockExchanges, requestExchange)
					}
				} else {
					requestExchange = firstStockExchange(&state)
				}
				recordWebsocketPayload(requestExchange, "request", "desktop_to_root", "received", result.messageType, result.payload)
			}
			if state.route == routeRelay {
				if errState := validateRelayPayloadState(result.payload, nextRelayProvider, false); errState != nil {
					if !b.writeNonPortableStateError(session) {
						return
					}
					continue
				}
			}
			if state.route == routeOfficial {
				upstreamPayload = normalizeRelayMultiAgentParentPayload(result.payload, b.relayAgents)
				upstreamPayload, errInspect = prepareOfficialPayload(upstreamPayload)
				if errInspect == nil && isCreate {
					upstreamPayload, errInspect = applyOfficialFastServiceTier(upstreamPayload, nextModel, b.fastModels)
				}
				if errInspect != nil {
					if !b.writeNonPortableStateError(session) {
						finishStockExchange(&state, requestExchange, "incomplete", false, "local_error_write_failed")
						return
					}
					recordWebsocketPayload(requestExchange, "response", "root_to_desktop", "generated", websocket.TextMessage, nonPortableStateErrorPayload())
					if isCreate {
						finishStockExchange(&state, requestExchange, "rejected", true, "official_request_state_not_portable")
					}
					continue
				}
				recordWebsocketPayload(requestExchange, "request", "root_to_official", "forwarded", result.messageType, upstreamPayload)
			}
			if errWrite := state.peer.writeMessage(result.messageType, upstreamPayload); errWrite != nil {
				finishStockExchange(&state, requestExchange, "incomplete", false, "upstream_websocket_write_failed")
				setAccessOutcome(ctx, "failed", "upstream_websocket_write_failed", websocket.CloseInternalServerErr)
				if !b.terminateFromBufferedDownstream(session, downstreamResults) {
					session.terminate(websocket.CloseInternalServerErr, "upstream unavailable", true, false)
				}
				return
			}
			if isCreate {
				state.outstandingTurns++
				state.model = nextModel
				state.relayProvider = nextRelayProvider
				updateAccessSelection(ctx, "websocket", "responses", state.route, nextModel)
			}

		case result := <-upstreamResults:
			if result.peer != state.peer || result.generation != state.generation {
				if result.err != nil {
					session.removeUpstream(result.peer)
				}
				continue
			}
			if result.err != nil {
				finishFirstStockExchange(&state, "incomplete", false, "upstream_websocket_read_failed")
				if state.outstandingTurns == 0 {
					setAccessOutcomeIfUnset(ctx, "completed", "upstream_websocket_closed_after_terminal", websocket.CloseNormalClosure)
				} else {
					setAccessOutcomeIfUnset(ctx, "failed", "upstream_websocket_closed_before_terminal", websocket.CloseAbnormalClosure)
				}
				b.terminateForReadResult(session, result)
				return
			}
			if state.route == routeOfficial {
				recordWebsocketPayload(firstStockExchange(&state), "response", "official_to_root", "received", result.messageType, result.payload)
			}
			if errWrite := session.downstream.writeMessage(result.messageType, result.payload); errWrite != nil {
				if !b.terminateFromBufferedDownstream(session, downstreamResults) {
					session.forceClose()
				}
				return
			}
			addAccessResponseBytes(ctx, len(result.payload))
			if upstreamEventIsError(result.payload) {
				finishFirstStockExchange(&state, "failed", true, "upstream_error_event")
				setAccessOutcome(ctx, "failed", "upstream_error_event", websocket.CloseNormalClosure)
				session.terminate(websocket.CloseNormalClosure, "upstream error", true, false)
				return
			}
			terminalOutcome, terminal := upstreamTerminalOutcome(result.payload)
			if !terminal {
				continue
			}
			finishFirstStockExchange(&state, terminalOutcome, true, "")
			if terminalOutcome != "completed" {
				setAccessOutcome(ctx, terminalOutcome, "upstream_terminal_"+terminalOutcome, websocket.CloseNormalClosure)
			}
			if state.outstandingTurns > 0 {
				state.outstandingTurns--
			}
			if pending == nil || state.outstandingTurns > 0 {
				continue
			}
			switchRequest := pending
			pending = nil
			if !b.performHandoff(ctx, session, inboundHeaders, &state, switchRequest, downstreamResults, upstreamResults, readers) {
				return
			}
		}
	}
}

func (b *websocketBridge) performHandoff(
	ctx context.Context,
	session *websocketSession,
	inboundHeaders http.Header,
	state *websocketControllerState,
	request *pendingRouteSwitch,
	downstreamResults <-chan websocketReadResult,
	upstreamResults chan<- websocketReadResult,
	readers *sync.WaitGroup,
) bool {
	select {
	case b.handoffSlots <- struct{}{}:
		defer func() { <-b.handoffSlots }()
	default:
		session.terminate(websocket.CloseTryAgainLater, "route handoff capacity exhausted", true, false)
		return false
	}

	nextRoute, errRoute := b.routes.classify(request.envelope.model)
	nextRelayProvider := relayProvider("")
	if nextRoute == routeRelay {
		nextRelayProvider = b.routes.relayProvider(request.envelope.model)
	}
	if errRoute != nil || !websocketTargetChanged(*state, nextRoute, nextRelayProvider, request.envelope.model) {
		session.terminatePolicyViolation()
		return false
	}
	var candidateExchange *stockExchange
	exchangeTransferred := false
	if nextRoute == routeOfficial {
		candidateExchange = b.logging.beginStockExchange(ctx, nextRoute, "websocket", "responses", request.envelope.model)
		recordWebsocketPayload(candidateExchange, "request", "desktop_to_root", "received", request.messageType, request.payload)
		defer func() {
			if !exchangeTransferred {
				candidateExchange.finish("incomplete", false, "websocket_handoff_not_completed")
			}
		}()
	}
	if nextRoute == routeOfficial && len(headerValuesCaseInsensitive(inboundHeaders, "X-OAI-Attestation")) != 0 {
		candidateExchange.finish("reconnect_required", false, "official_route_requires_fresh_handshake")
		setAccessOutcome(ctx, "reconnect_required", "official_route_requires_fresh_handshake", websocket.CloseServiceRestart)
		session.terminateForFreshOfficialHandshake()
		return false
	}
	payload := request.payload
	if payloadContainsCompaction(request.payload) {
		written := b.writeNonPortableStateError(session)
		if candidateExchange != nil && written {
			recordWebsocketPayload(candidateExchange, "response", "root_to_desktop", "generated", websocket.TextMessage, nonPortableStateErrorPayload())
			candidateExchange.finish("rejected", true, "compaction_not_portable")
		}
		return written
	}
	if nextRoute == routeRelay {
		if errState := validateRelayPayloadState(request.payload, nextRelayProvider, false); errState != nil {
			return b.writeNonPortableStateError(session)
		}
	}
	if nextRoute == routeOfficial {
		payload = normalizeRelayMultiAgentParentPayload(request.payload, b.relayAgents)
		payload, errRoute = prepareOfficialPayload(payload)
		if errRoute == nil {
			// A handoff only ever carries the response.create that changed target.
			payload, errRoute = applyOfficialFastServiceTier(payload, request.envelope.model, b.fastModels)
		}
		if errRoute != nil {
			written := b.writeNonPortableStateError(session)
			if written {
				recordWebsocketPayload(candidateExchange, "response", "root_to_desktop", "generated", websocket.TextMessage, nonPortableStateErrorPayload())
				candidateExchange.finish("rejected", true, "official_request_state_not_portable")
			}
			return written
		}
		recordWebsocketPayload(candidateExchange, "request", "root_to_official", "forwarded", request.messageType, payload)
	}
	headers, errHeaders := buildUpstreamHeaders(inboundHeaders, nextRoute, b.relayAPIKey)
	if errHeaders != nil {
		candidateExchange.finish("failed", false, "upstream_headers_unavailable")
		setAccessOutcome(ctx, "failed", "upstream_headers_unavailable", websocket.ClosePolicyViolation)
		session.terminatePolicyViolation()
		return false
	}
	b.addOfficialCookies(nextRoute, headers)
	targetURL, dial := b.target(nextRoute)
	candidateConnection, handshakeResponse, errDial := dial(ctx, targetURL, headers)
	b.storeOfficialCookies(nextRoute, handshakeResponse)
	closeCode, closeReason := upstreamHandshakeClose(handshakeResponse)
	if handshakeResponse != nil {
		setAccessUpstreamStatus(ctx, handshakeResponse.StatusCode)
	}
	closeHandshakeResponse(handshakeResponse)
	if errDial != nil {
		outcome := "failed"
		if ctx.Err() != nil {
			outcome = "canceled"
		}
		candidateExchange.finish(outcome, false, "upstream_websocket_handoff_dial_failed")
		setAccessOutcome(ctx, outcome, "upstream_websocket_handoff_dial_failed", closeCode)
		if !b.terminateFromBufferedDownstream(session, downstreamResults) {
			session.terminate(closeCode, closeReason, true, false)
		}
		return false
	}
	candidateConnection.EnableWriteCompression(false)
	candidateConnection.SetReadLimit(b.maxMessage)
	candidate, tracked := session.addUpstream(candidateConnection)
	if !tracked {
		candidateExchange.finish("canceled", false, "session_closed_before_handoff_tracking")
		return false
	}
	if errWrite := candidate.writeMessage(request.messageType, payload); errWrite != nil {
		candidateExchange.finish("failed", false, "upstream_websocket_handoff_write_failed")
		setAccessOutcome(ctx, "failed", "upstream_websocket_handoff_write_failed", websocket.CloseInternalServerErr)
		candidate.forceClose()
		session.removeUpstream(candidate)
		if !b.terminateFromBufferedDownstream(session, downstreamResults) {
			session.terminate(websocket.CloseInternalServerErr, "upstream unavailable", true, false)
		}
		return false
	}

	oldPeer := state.peer
	finishAllStockExchanges(state, "incomplete", false, "websocket_route_changed")
	state.generation++
	state.route = nextRoute
	state.relayProvider = nextRelayProvider
	state.model = request.envelope.model
	state.peer = candidate
	state.outstandingTurns = 1
	if candidateExchange != nil {
		state.stockExchanges = append(state.stockExchanges, candidateExchange)
		exchangeTransferred = true
	}
	updateAccessSelection(ctx, "websocket", "responses", nextRoute, request.envelope.model)
	setAccessUpstreamStatus(ctx, http.StatusSwitchingProtocols)
	readers.Add(1)
	go b.readWebsocketLoop(ctx, session, candidate, readFromUpstream, state.generation, upstreamResults, readers)
	readers.Add(1)
	go func() {
		defer readers.Done()
		defer session.removeUpstream(oldPeer)
		oldPeer.terminate(websocket.CloseNormalClosure, "model route changed", true)
	}()
	log.WithFields(log.Fields{"provider": nextRoute.String(), "relay_provider": nextRelayProvider, "model": state.model}).Info("root proxy websocket route changed")
	return true
}

func websocketTargetChanged(state websocketControllerState, nextRoute route, nextRelayProvider relayProvider, nextModel string) bool {
	if nextRoute != state.route {
		return true
	}
	if nextRoute != routeRelay {
		return false
	}
	if nextRelayProvider != state.relayProvider {
		return true
	}
	// Without provider attribution, different model identifiers cannot safely
	// share provider-local WebSocket state.
	return nextRelayProvider == "" && nextModel != state.model
}

func recordWebsocketPayload(exchange *stockExchange, kind, direction, representation string, messageType int, payload []byte) {
	if exchange == nil {
		return
	}
	opcode := "unknown"
	switch messageType {
	case websocket.TextMessage:
		opcode = "text"
	case websocket.BinaryMessage:
		opcode = "binary"
	}
	fields := map[string]any{
		"opcode":       opcode,
		"opcode_value": messageType,
	}
	if representation != "" {
		fields["representation"] = representation
	}
	exchange.recordPayload(kind, direction, payload, fields)
}

func (b *websocketBridge) logRejectedStockWebsocketRequest(
	ctx context.Context,
	selected route,
	model string,
	messageType int,
	requestPayload, responsePayload []byte,
	detail string,
) {
	exchange := b.logging.beginStockExchange(ctx, selected, "websocket", "responses", model)
	if exchange == nil {
		return
	}
	recordWebsocketPayload(exchange, "request", "desktop_to_root", "received", messageType, requestPayload)
	recordWebsocketPayload(exchange, "response", "root_to_desktop", "generated", websocket.TextMessage, responsePayload)
	exchange.finish("rejected", true, detail)
}

func firstStockExchange(state *websocketControllerState) *stockExchange {
	if state == nil || len(state.stockExchanges) == 0 {
		return nil
	}
	return state.stockExchanges[0]
}

func finishFirstStockExchange(state *websocketControllerState, outcome string, terminalCaptured bool, detail string) {
	if state == nil || len(state.stockExchanges) == 0 {
		return
	}
	exchange := state.stockExchanges[0]
	copy(state.stockExchanges, state.stockExchanges[1:])
	state.stockExchanges[len(state.stockExchanges)-1] = nil
	state.stockExchanges = state.stockExchanges[:len(state.stockExchanges)-1]
	exchange.finish(outcome, terminalCaptured, detail)
}

func finishStockExchange(state *websocketControllerState, target *stockExchange, outcome string, terminalCaptured bool, detail string) {
	if state == nil || target == nil {
		return
	}
	for index, exchange := range state.stockExchanges {
		if exchange != target {
			continue
		}
		copy(state.stockExchanges[index:], state.stockExchanges[index+1:])
		state.stockExchanges[len(state.stockExchanges)-1] = nil
		state.stockExchanges = state.stockExchanges[:len(state.stockExchanges)-1]
		target.finish(outcome, terminalCaptured, detail)
		return
	}
	target.finish(outcome, terminalCaptured, detail)
}

func finishAllStockExchanges(state *websocketControllerState, outcome string, terminalCaptured bool, detail string) {
	if state == nil {
		return
	}
	for len(state.stockExchanges) != 0 {
		finishFirstStockExchange(state, outcome, terminalCaptured, detail)
	}
}

func stateReplayErrorPayload(envelope clientMessageEnvelope) []byte {
	if envelope.hasPreviousResponseID && !envelope.previousResponseIDIsNull {
		return []byte(`{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"previous_response_not_found","message":"The selected model uses a different upstream. Retry with full input and previous_response_id set to null.","param":"previous_response_id"}}`)
	}
	return []byte(`{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"previous_response_not_found","message":"The selected model uses a different upstream. Retry with full input and conversation set to null.","param":"conversation"}}`)
}

func (b *websocketBridge) writeNonPortableStateError(session *websocketSession) bool {
	payload := nonPortableStateErrorPayload()
	if errWrite := session.downstream.writeMessage(websocket.TextMessage, payload); errWrite != nil {
		session.forceClose()
		return false
	}
	addAccessResponseBytes(session.accessCtx, len(payload))
	return true
}

func nonPortableStateErrorPayload() []byte {
	return []byte(`{"type":"error","status":409,"error":{"type":"invalid_request_error","code":"cross_provider_compaction_not_portable","message":"Compacted state from another provider cannot be sent to this model. Start a new conversation chain.","param":"input"}}`)
}

func (b *websocketBridge) terminateFromBufferedDownstream(session *websocketSession, results <-chan websocketReadResult) bool {
	select {
	case result := <-results:
		if result.err == nil {
			return false
		}
		b.terminateForReadResult(session, result)
		return true
	default:
		return false
	}
}

func (b *websocketBridge) terminateForReadResult(session *websocketSession, result websocketReadResult) {
	var policy *clientPolicyError
	if errors.As(result.err, &policy) {
		setAccessOutcomeIfUnset(session.accessCtx, "rejected", "websocket_client_policy_error", websocket.ClosePolicyViolation)
		session.terminatePolicyViolation()
		return
	}
	if errors.Is(result.err, websocket.ErrReadLimit) {
		if result.source == readFromDownstream {
			setAccessOutcomeIfUnset(session.accessCtx, "rejected", "downstream_message_too_big", websocket.CloseMessageTooBig)
			session.terminate(websocket.CloseMessageTooBig, "message too big", false, true)
		} else {
			setAccessOutcomeIfUnset(session.accessCtx, "failed", "upstream_message_too_big", websocket.CloseMessageTooBig)
			session.terminate(websocket.CloseMessageTooBig, "message too big", true, false)
		}
		return
	}
	var closeError *websocket.CloseError
	if errors.As(result.err, &closeError) {
		code, reason := normalizedPeerClose(closeError)
		if result.source == readFromDownstream {
			setAccessOutcomeIfUnset(session.accessCtx, "completed", "downstream_websocket_closed", code)
			session.terminate(code, reason, false, true)
		} else {
			setAccessOutcomeIfUnset(session.accessCtx, "failed", "upstream_websocket_closed", code)
			session.terminate(code, reason, true, false)
		}
		return
	}
	if errors.Is(result.err, websocket.ErrCloseSent) || errors.Is(result.err, net.ErrClosed) || errors.Is(result.err, io.EOF) {
		if result.source == readFromUpstream {
			setAccessOutcomeIfUnset(session.accessCtx, "failed", "upstream_websocket_read_ended", websocket.CloseAbnormalClosure)
		} else {
			setAccessOutcomeIfUnset(session.accessCtx, "completed", "downstream_websocket_read_ended", websocket.CloseNormalClosure)
		}
		session.terminate(0, "", false, false)
		return
	}
	setAccessOutcomeIfUnset(session.accessCtx, "failed", "websocket_transport_failed", websocket.CloseInternalServerErr)
	session.terminate(websocket.CloseInternalServerErr, "websocket transport failed", true, true)
}

func normalizedPeerClose(closeError *websocket.CloseError) (int, string) {
	if closeError == nil || closeError.Code == websocket.CloseNoStatusReceived {
		return websocket.CloseNoStatusReceived, ""
	}
	if !validCloseCode(closeError.Code) {
		return websocket.CloseProtocolError, "invalid peer close"
	}
	return closeError.Code, truncateCloseReason(closeError.Text)
}

func validCloseCode(code int) bool {
	switch code {
	case websocket.CloseNormalClosure,
		websocket.CloseGoingAway,
		websocket.CloseProtocolError,
		websocket.CloseUnsupportedData,
		websocket.CloseInvalidFramePayloadData,
		websocket.ClosePolicyViolation,
		websocket.CloseMessageTooBig,
		websocket.CloseMandatoryExtension,
		websocket.CloseInternalServerErr,
		websocket.CloseServiceRestart,
		websocket.CloseTryAgainLater:
		return true
	default:
		return code >= 3000 && code <= 4999
	}
}

func truncateCloseReason(reason string) string {
	if len(reason) <= websocketCloseReasonMaxBytes && utf8.ValidString(reason) {
		return reason
	}
	for len(reason) > websocketCloseReasonMaxBytes || !utf8.ValidString(reason) {
		if len(reason) == 0 {
			return ""
		}
		reason = reason[:len(reason)-1]
	}
	return reason
}

func (p *websocketPeer) writeMessage(messageType int, payload []byte) error {
	if p == nil || p.connection == nil || p.closing.Load() {
		return websocket.ErrCloseSent
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if p.closing.Load() {
		return websocket.ErrCloseSent
	}
	return p.connection.WriteMessage(messageType, payload)
}

func (p *websocketPeer) terminate(code int, reason string, notify bool) {
	if p == nil || p.connection == nil {
		return
	}
	if !p.closing.CompareAndSwap(false, true) {
		p.forceClose()
		return
	}
	if !notify {
		p.forceClose()
		return
	}
	if !p.writeMu.TryLock() {
		p.forceClose()
		return
	}
	payload := []byte(nil)
	if code != websocket.CloseNoStatusReceived {
		payload = websocket.FormatCloseMessage(code, truncateCloseReason(reason))
	}
	errWrite := p.connection.WriteControl(websocket.CloseMessage, payload, time.Time{})
	errClose := p.connection.Close()
	p.writeMu.Unlock()
	if errWrite != nil && !errors.Is(errWrite, websocket.ErrCloseSent) && !errors.Is(errWrite, net.ErrClosed) {
		log.WithError(errWrite).Debug("root proxy failed to write websocket close")
	}
	if errClose != nil {
		log.WithError(errClose).Debug("root proxy failed to close websocket")
	}
}

func (p *websocketPeer) forceClose() {
	if p == nil || p.connection == nil {
		return
	}
	p.closing.Store(true)
	if errClose := p.connection.Close(); errClose != nil {
		log.WithError(errClose).Debug("root proxy failed to force-close websocket")
	}
}

func (s *websocketSession) addUpstream(connection *websocket.Conn) (*websocketPeer, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		if errClose := connection.Close(); errClose != nil {
			log.WithError(errClose).Debug("root proxy failed to close late upstream connection")
		}
		return nil, false
	}
	peer := &websocketPeer{connection: connection}
	if s.upstreams == nil {
		s.upstreams = make(map[*websocketPeer]struct{})
	}
	s.upstreams[peer] = struct{}{}
	return peer, true
}

func (s *websocketSession) removeUpstream(peer *websocketPeer) {
	s.mu.Lock()
	delete(s.upstreams, peer)
	s.mu.Unlock()
}

func (s *websocketSession) terminate(code int, reason string, notifyDownstream, notifyUpstream bool) {
	switch code {
	case websocket.ClosePolicyViolation:
		setAccessOutcomeIfUnset(s.accessCtx, "rejected", "websocket_policy_violation", code)
	case websocket.CloseInternalServerErr, websocket.CloseServiceRestart, websocket.CloseTryAgainLater:
		setAccessOutcomeIfUnset(s.accessCtx, "failed", "websocket_session_failed", code)
	}
	downstream, upstreams, ok := s.beginTerminate()
	if !ok {
		return
	}
	downstream.terminate(code, reason, notifyDownstream)
	for _, upstream := range upstreams {
		upstream.terminate(code, reason, notifyUpstream)
	}
}

func (s *websocketSession) terminatePolicyViolation() {
	setAccessOutcomeIfUnset(s.accessCtx, "rejected", "route_policy_violation", websocket.ClosePolicyViolation)
	downstream, upstreams, ok := s.beginTerminate()
	if !ok {
		return
	}
	downstream.terminate(websocket.ClosePolicyViolation, "route policy violation", true)
	for _, upstream := range upstreams {
		upstream.terminate(websocket.CloseNormalClosure, "client route ended", true)
	}
}

func (s *websocketSession) terminateForFreshOfficialHandshake() {
	setAccessOutcomeIfUnset(s.accessCtx, "reconnect_required", "official_route_requires_fresh_handshake", websocket.CloseServiceRestart)
	downstream, upstreams, ok := s.beginTerminate()
	if !ok {
		return
	}
	downstream.terminate(websocket.CloseServiceRestart, "official route requires a fresh handshake", true)
	for _, upstream := range upstreams {
		upstream.terminate(websocket.CloseNormalClosure, "client route reconnecting", true)
	}
}

func (s *websocketSession) forceClose() {
	s.mu.Lock()
	s.closed = true
	downstream := s.downstream
	upstreams := s.upstreamSnapshotLocked()
	cancel := s.cancel
	s.cancel = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	downstream.forceClose()
	for _, upstream := range upstreams {
		upstream.forceClose()
	}
}

func (s *websocketSession) interruptFromDownstream(errRead error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	upstreams := s.upstreamSnapshotLocked()
	cancel := s.cancel
	s.cancel = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	var closeError *websocket.CloseError
	if errors.As(errRead, &closeError) {
		code, reason := normalizedPeerClose(closeError)
		for _, upstream := range upstreams {
			upstream.terminate(code, reason, true)
		}
		return
	}
	if errors.Is(errRead, websocket.ErrReadLimit) {
		for _, upstream := range upstreams {
			upstream.terminate(websocket.CloseMessageTooBig, "message too big", true)
		}
		return
	}
	for _, upstream := range upstreams {
		upstream.forceClose()
	}
}

func (s *websocketSession) beginTerminate() (*websocketPeer, []*websocketPeer, bool) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, nil, false
	}
	s.closed = true
	downstream := s.downstream
	upstreams := s.upstreamSnapshotLocked()
	cancel := s.cancel
	s.cancel = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return downstream, upstreams, true
}

func (s *websocketSession) upstreamSnapshotLocked() []*websocketPeer {
	upstreams := make([]*websocketPeer, 0, len(s.upstreams))
	for upstream := range s.upstreams {
		upstreams = append(upstreams, upstream)
	}
	return upstreams
}

func (b *websocketBridge) addPendingRoute(session *websocketSession) {
	b.pendingMu.Lock()
	var evicted *websocketSession
	if len(b.pendingRoutes) >= b.maxPending {
		evicted = b.pendingRoutes[0]
		copy(b.pendingRoutes, b.pendingRoutes[1:])
		b.pendingRoutes[len(b.pendingRoutes)-1] = nil
		b.pendingRoutes = b.pendingRoutes[:len(b.pendingRoutes)-1]
	}
	b.pendingRoutes = append(b.pendingRoutes, session)
	b.pendingMu.Unlock()
	if evicted != nil {
		evicted.terminate(websocket.CloseTryAgainLater, "route capacity replaced", true, false)
	}
}

func (b *websocketBridge) removePendingRoute(session *websocketSession) {
	b.pendingMu.Lock()
	defer b.pendingMu.Unlock()
	for index, candidate := range b.pendingRoutes {
		if candidate == session {
			copy(b.pendingRoutes[index:], b.pendingRoutes[index+1:])
			b.pendingRoutes[len(b.pendingRoutes)-1] = nil
			b.pendingRoutes = b.pendingRoutes[:len(b.pendingRoutes)-1]
			return
		}
	}
}

func (b *websocketBridge) register(session *websocketSession) bool {
	b.sessionsMu.Lock()
	defer b.sessionsMu.Unlock()
	if b.closing {
		return false
	}
	b.sessions[session] = struct{}{}
	b.sessionsWG.Add(1)
	return true
}

func (b *websocketBridge) unregister(session *websocketSession) {
	b.sessionsMu.Lock()
	_, registered := b.sessions[session]
	if registered {
		delete(b.sessions, session)
	}
	b.sessionsMu.Unlock()
	if registered {
		b.sessionsWG.Done()
	}
}

func (b *websocketBridge) Close() {
	b.sessionsMu.Lock()
	if b.closing {
		b.sessionsMu.Unlock()
		return
	}
	b.closing = true
	sessions := make([]*websocketSession, 0, len(b.sessions))
	for session := range b.sessions {
		sessions = append(sessions, session)
	}
	b.sessionsMu.Unlock()

	for _, session := range sessions {
		session.forceClose()
	}
	b.sessionsWG.Wait()
}
