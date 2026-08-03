package rootproxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const shutdownGracePeriod = 10 * time.Second

// Server hosts only Root-owned endpoints.
type Server struct {
	address    string
	bridge     *websocketBridge
	httpBridge *httpBridge
	discovery  *relayDiscovery
	handler    http.Handler
	logging    *rootLogManager
	closeOnce  sync.Once
}

type responsesEndpointHandler struct {
	websocket     *websocketBridge
	http          *httpBridge
	websocketMode string
}

// NewServer creates the lightweight Root server from a validated configuration.
func NewServer(config *Config) (*Server, error) {
	if config == nil {
		return nil, errors.New("root proxy config is nil")
	}
	if errValidate := config.validateAndResolve(os.LookupEnv); errValidate != nil {
		return nil, fmt.Errorf("validate root proxy config: %w", errValidate)
	}
	loggingManager, errLogging := newRootLogManager(config)
	if errLogging != nil {
		return nil, fmt.Errorf("configure root proxy logging: %w", errLogging)
	}
	completed := false
	defer func() {
		if !completed {
			loggingManager.close()
		}
	}()
	officialCookies, errCookies := newChatGPTCloudflareCookieJar()
	if errCookies != nil {
		return nil, fmt.Errorf("create ChatGPT Cloudflare cookie jar: %w", errCookies)
	}
	mode, errMode := config.discoveryMode()
	if errMode != nil {
		return nil, errMode
	}

	// In auto mode a single resolver is shared by both bridges and the catalog
	// handler so one discovery refresh updates every routing decision at once.
	var (
		resolver  *routeResolver
		discovery *relayDiscovery
	)
	if mode == discoveryAuto {
		var errResolver error
		resolver, errResolver = newRouteResolver(mode, config.Routing.StockModels, config.Routing.RelayModels, config.Routing.RelayModelProviders)
		if errResolver != nil {
			return nil, fmt.Errorf("create root route resolver: %w", errResolver)
		}
	}

	websocketOptions := config.bridgeOptions()
	websocketOptions.officialCookies = officialCookies
	websocketOptions.logging = loggingManager
	websocketOptions.resolver = resolver
	httpOptions := config.httpBridgeOptions()
	httpOptions.officialCookies = officialCookies
	httpOptions.logging = loggingManager
	httpOptions.resolver = resolver
	httpBridge, errHTTPBridge := newHTTPBridge(httpOptions)
	if errHTTPBridge != nil {
		return nil, fmt.Errorf("create root HTTP bridge: %w", errHTTPBridge)
	}
	if mode == discoveryAuto {
		var errDiscovery error
		discovery, errDiscovery = newRelayDiscovery(config.Relay.BaseURL, config.relayAPIKey, httpBridge.relayClient, resolver)
		if errDiscovery != nil {
			return nil, fmt.Errorf("create relay discovery: %w", errDiscovery)
		}
		httpBridge.discovery = discovery
		websocketOptions.discovery = discovery
	}
	bridge, errBridge := newWebsocketBridge(websocketOptions)
	if errBridge != nil {
		return nil, fmt.Errorf("create root websocket bridge: %w", errBridge)
	}
	models, errModels := newModelsHandlerWithDiscovery(config, resolver, discovery, httpBridge.officialBaseURL, httpBridge.officialClient)
	if errModels != nil {
		return nil, fmt.Errorf("create root model catalog: %w", errModels)
	}
	mux := http.NewServeMux()
	responses := &responsesEndpointHandler{
		websocket:     bridge,
		http:          httpBridge,
		websocketMode: config.Websocket.Mode,
	}
	mux.Handle("/v1/responses", responses)
	mux.Handle("/backend-api/codex/responses", responses)
	mux.HandleFunc("/v1/responses/compact", httpBridge.ServeCompact)
	mux.HandleFunc("/backend-api/codex/responses/compact", httpBridge.ServeCompact)
	mux.Handle("/v1/models", models)
	mux.Handle("/backend-api/codex/models", models)
	mux.HandleFunc("/healthz", healthHandler)
	var handler http.Handler = mux
	if loggingManager != nil {
		handler = loggingManager.accessMiddleware(handler)
	}
	server := &Server{
		address:    config.listenAddress(),
		bridge:     bridge,
		httpBridge: httpBridge,
		discovery:  discovery,
		handler:    handler,
		logging:    loggingManager,
	}
	completed = true
	return server, nil
}

func (h *responsesEndpointHandler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet:
		if h.websocketMode == websocketModeHTTPFallback {
			setAccessTransport(request.Context(), "websocket_fallback", "responses")
			if !h.websocket.validateDownstreamHandshake(response, request) {
				return
			}
			response.Header().Set("Cache-Control", "no-store")
			writeHTTPError(response, http.StatusUpgradeRequired, "websocket_transport_unavailable", "Responses WebSocket transport is disabled; retry over HTTP", "")
			return
		}
		h.websocket.ServeHTTP(response, request)
	case http.MethodPost:
		h.http.ServeResponses(response, request)
	default:
		response.Header().Set("Allow", "GET, POST")
		writeHTTPError(response, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed", "")
	}
}

// Handler exposes the Root HTTP handler for focused tests and embedding.
func (s *Server) Handler() http.Handler {
	if s == nil {
		return http.NotFoundHandler()
	}
	return s.handler
}

// Run listens until the context is cancelled or the HTTP server exits.
func (s *Server) Run(ctx context.Context) error {
	if s == nil || s.bridge == nil || s.httpBridge == nil || s.handler == nil {
		return errors.New("root proxy server is not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	defer s.Close()
	// Populate the Relay half before the listener accepts traffic where possible;
	// the first inference request also forces a refresh if this has not completed.
	s.discovery.start(ctx)
	listener, errListen := net.Listen("tcp", s.address)
	if errListen != nil {
		errWrapped := fmt.Errorf("listen on %s: %w", s.address, errListen)
		log.WithError(errWrapped).Error("root proxy failed to start")
		return errWrapped
	}
	errRun := s.runWithListener(ctx, listener)
	if errRun != nil {
		log.WithError(errRun).Error("root proxy stopped with an error")
	}
	return errRun
}

// Close releases WebSocket sessions and native log files. It is safe to call
// more than once and is primarily useful for embedded servers and tests; Run
// closes the server automatically when it returns.
func (s *Server) Close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		if s.bridge != nil {
			s.bridge.Close()
		}
		s.logging.close()
	})
}

func (s *Server) runWithListener(ctx context.Context, listener net.Listener) error {
	if listener == nil {
		return errors.New("root proxy listener is nil")
	}
	httpServer := &http.Server{Handler: s.handler}
	serveResult := make(chan error, 1)
	go func() {
		serveResult <- httpServer.Serve(listener)
	}()
	log.WithField("address", listener.Addr().String()).Info("root proxy started")

	select {
	case errServe := <-serveResult:
		s.bridge.Close()
		if errors.Is(errServe, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve root proxy: %w", errServe)
	case <-ctx.Done():
	}

	s.bridge.Close()
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownGracePeriod)
	defer cancelShutdown()
	errShutdown := httpServer.Shutdown(shutdownCtx)
	errServe := <-serveResult
	if errShutdown != nil {
		return fmt.Errorf("shutdown root proxy: %w", errShutdown)
	}
	if errServe != nil && !errors.Is(errServe, http.ErrServerClosed) {
		return fmt.Errorf("serve root proxy during shutdown: %w", errServe)
	}
	log.Info("root proxy stopped")
	return nil
}

func healthHandler(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		response.Header().Set("Allow", "GET, HEAD")
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(http.StatusOK)
	if request.Method == http.MethodHead {
		return
	}
	if _, errWrite := response.Write([]byte("{\"status\":\"ok\"}\n")); errWrite != nil {
		log.WithError(errWrite).Debug("root proxy failed to write health response")
	}
}
