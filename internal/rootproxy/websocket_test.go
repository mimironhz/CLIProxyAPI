package rootproxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

type capturedMessage struct {
	header      http.Header
	messageType int
	payload     []byte
}

func TestWebsocketBridgeRoutesStockAndRelayWithCredentialIsolation(t *testing.T) {
	stockCapture := make(chan capturedMessage, 2)
	relayCapture := make(chan capturedMessage, 2)
	var stockConnections atomic.Int32
	var relayConnections atomic.Int32
	stockServer := newEchoWebsocketServer(t, &stockConnections, stockCapture)
	relayServer := newEchoWebsocketServer(t, &relayConnections, relayCapture)

	tests := []struct {
		name           string
		path           string
		model          string
		messageType    int
		selected       <-chan capturedMessage
		wantStockCount int32
		wantRelayCount int32
		checkHeaders   func(*testing.T, http.Header)
	}{
		{
			name:           "stock keeps OAuth only on official arm",
			path:           "/v1/responses",
			model:          "gpt-stock",
			messageType:    websocket.TextMessage,
			selected:       stockCapture,
			wantStockCount: 1,
			wantRelayCount: 0,
			checkHeaders: func(t *testing.T, headers http.Header) {
				assertHeader(t, headers, "Authorization", "Bearer desktop-oauth")
				assertHeader(t, headers, "ChatGPT-Account-ID", "account-1")
				assertHeader(t, headers, "X-Codex-Turn-State", "turn-state")
				assertHeaderAbsent(t, headers, rootHopHeader)
				assertHeaderAbsent(t, headers, "Cookie")
				assertHeaderAbsent(t, headers, "X-Api-Key")
				assertHeaderAbsent(t, headers, "Sec-WebSocket-Extensions")
			},
		},
		{
			name:           "relay replaces every Desktop credential",
			path:           "/backend-api/codex/responses",
			model:          "relay-model",
			messageType:    websocket.BinaryMessage,
			selected:       relayCapture,
			wantStockCount: 1,
			wantRelayCount: 1,
			checkHeaders: func(t *testing.T, headers http.Header) {
				assertHeader(t, headers, "Authorization", "Bearer relay-secret")
				assertHeader(t, headers, "X-Codex-Turn-State", "turn-state")
				assertHeader(t, headers, rootHopHeader, "1")
				for _, name := range []string{
					"ChatGPT-Account-ID",
					"Cookie",
					"OpenAI-Organization",
					"OpenAI-Project",
					"X-Api-Key",
					"Sec-WebSocket-Extensions",
				} {
					assertHeaderAbsent(t, headers, name)
				}
			},
		},
	}

	bridge := newTestBridge(t, websocketURL(stockServer.URL), websocketURL(relayServer.URL), nil)
	rootServer := httptest.NewServer(bridge)
	t.Cleanup(rootServer.Close)

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			headers := http.Header{
				"Authorization":       {"Bearer desktop-oauth"},
				"ChatGPT-Account-ID":  {"account-1"},
				"Cookie":              {"session=desktop-secret"},
				"OpenAI-Beta":         {"responses_websockets=2026-02-06"},
				"OpenAI-Organization": {"org-secret"},
				"OpenAI-Project":      {"project-secret"},
				"Originator":          {"Codex Desktop"},
				"Session_id":          {"session-1"},
				"X-Api-Key":           {"alternate-secret"},
				"X-Codex-Turn-State":  {"turn-state"},
			}
			dialer := websocket.Dialer{
				EnableCompression: true,
				// The small buffer forces the large first application message
				// across continuation frames before Root reassembles it.
				WriteBufferSize: 32,
			}
			connection, response, errDial := dialer.Dial(websocketURL(rootServer.URL)+test.path, headers)
			if errDial != nil {
				t.Fatalf("dial Root: %v", errDial)
			}
			defer func() { _ = connection.Close() }()
			setTestReadDeadline(t, connection)
			if got := response.Header.Get("Sec-WebSocket-Extensions"); got != "" {
				t.Fatalf("downstream compression negotiated: %q", got)
			}

			payload := []byte(` {"type":"response.create","model":"` + test.model + `","padding":"` + strings.Repeat("x", 512) + `"} `)
			if errWrite := connection.WriteMessage(test.messageType, payload); errWrite != nil {
				t.Fatalf("write first message: %v", errWrite)
			}
			gotType, gotPayload, errRead := connection.ReadMessage()
			if errRead != nil {
				t.Fatalf("read echoed message: %v", errRead)
			}
			if gotType != test.messageType || string(gotPayload) != string(payload) {
				t.Fatalf("echo = type %d payload %q, want type %d payload %q", gotType, gotPayload, test.messageType, payload)
			}

			capture := receiveCapture(t, test.selected)
			if capture.messageType != test.messageType || string(capture.payload) != string(payload) {
				t.Fatalf("upstream received type %d payload %q", capture.messageType, capture.payload)
			}
			test.checkHeaders(t, capture.header)
			if got := stockConnections.Load(); got != test.wantStockCount {
				t.Fatalf("stock connections = %d, want %d", got, test.wantStockCount)
			}
			if got := relayConnections.Load(); got != test.wantRelayCount {
				t.Fatalf("relay connections = %d, want %d", got, test.wantRelayCount)
			}
		})
	}
}

func TestWebsocketBridgeForwardsPlaintextCollaborationMessageSchemasToOfficial(t *testing.T) {
	stockCapture := make(chan capturedMessage, 1)
	relayCapture := make(chan capturedMessage, 1)
	var stockConnections atomic.Int32
	var relayConnections atomic.Int32
	stockServer := newEchoWebsocketServer(t, &stockConnections, stockCapture)
	relayServer := newEchoWebsocketServer(t, &relayConnections, relayCapture)
	bridge := newTestBridge(t, websocketURL(stockServer.URL), websocketURL(relayServer.URL), func(options *bridgeOptions) {
		options.relayAgents = true
	})
	rootServer := httptest.NewServer(bridge)
	t.Cleanup(rootServer.Close)

	connection := dialRootWebsocket(t, rootServer.URL, "/v1/responses", desktopHTTPHeaders())
	defer func() { _ = connection.Close() }()
	payload := []byte(`{"type":"response.create","model":"gpt-stock","input":[],"tools":[{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"spawn_agent","parameters":{"properties":{"message":{"type":"string","encrypted":true}}}},{"type":"function","name":"followup_task","parameters":{"properties":{"message":{"type":"string","encrypted":true}}}},{"type":"function","name":"send_message","parameters":{"properties":{"message":{"type":"string","encrypted":true}}}}]},{"type":"namespace","name":"mail","tools":[{"type":"function","name":"send_message","parameters":{"properties":{"message":{"type":"string","encrypted":true}}}}]}]}`)
	if errWrite := connection.WriteMessage(websocket.TextMessage, payload); errWrite != nil {
		t.Fatalf("write first message: %v", errWrite)
	}

	captured := receiveCapture(t, stockCapture)
	assertDelegationMessageSchemaRewrite(t, captured.payload)
	messageType, echoed, errRead := connection.ReadMessage()
	if errRead != nil {
		t.Fatalf("read rewritten echo: %v", errRead)
	}
	if messageType != websocket.TextMessage {
		t.Fatalf("echo message type = %d, want text", messageType)
	}
	assertDelegationMessageSchemaRewrite(t, echoed)
}

func TestWebsocketBridgeRejectsInvalidFirstMessagesWithoutDialing(t *testing.T) {
	var officialDials atomic.Int32
	var relayDials atomic.Int32
	failingDial := func(counter *atomic.Int32) websocketDialFunc {
		return func(context.Context, string, http.Header) (*websocket.Conn, *http.Response, error) {
			counter.Add(1)
			return nil, nil, errors.New("unexpected dial")
		}
	}
	bridge := newTestBridge(t, "ws://127.0.0.1:1/responses", "ws://127.0.0.1:2/responses", func(options *bridgeOptions) {
		options.dialOfficial = failingDial(&officialDials)
		options.dialRelay = failingDial(&relayDials)
	})
	rootServer := httptest.NewServer(bridge)
	t.Cleanup(rootServer.Close)

	tests := map[string]string{
		"unknown model":   `{"type":"response.create","model":"unknown"}`,
		"missing model":   `{"type":"response.create"}`,
		"wrong type":      `{"type":"response.append","model":"gpt-stock"}`,
		"duplicate model": `{"type":"response.create","model":"gpt-stock","model":"relay-model"}`,
		"malformed":       `{"type":"response.create","model":"gpt-stock"`,
	}
	for name, payload := range tests {
		t.Run(name, func(t *testing.T) {
			connection := dialRootWebsocket(t, rootServer.URL, "/v1/responses", http.Header{
				"Authorization": {"Bearer desktop-oauth"},
			})
			defer func() { _ = connection.Close() }()
			if errWrite := connection.WriteMessage(websocket.TextMessage, []byte(payload)); errWrite != nil {
				t.Fatalf("write invalid first message: %v", errWrite)
			}
			assertCloseCode(t, connection, websocket.ClosePolicyViolation)
		})
	}

	connection, response, errDial := websocket.DefaultDialer.Dial(websocketURL(rootServer.URL)+"/v1/responses", nil)
	if connection != nil {
		_ = connection.Close()
	}
	if errDial == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated dial = response %#v error %v, want 401", response, errDial)
	}
	if response.Body != nil {
		_ = response.Body.Close()
	}
	if got := officialDials.Load(); got != 0 {
		t.Fatalf("official dials = %d, want 0", got)
	}
	if got := relayDials.Load(); got != 0 {
		t.Fatalf("relay dials = %d, want 0", got)
	}
}

func TestWebsocketBridgeBoundsPreRouteState(t *testing.T) {
	t.Run("pending route capacity", func(t *testing.T) {
		bridge := newTestBridge(t, "ws://127.0.0.1:1/responses", "ws://127.0.0.1:2/responses", func(options *bridgeOptions) {
			options.maxPendingRoutes = 1
		})
		rootServer := httptest.NewServer(bridge)
		t.Cleanup(rootServer.Close)
		first := dialRootWebsocket(t, rootServer.URL, "/v1/responses", desktopBearerHeaders())
		defer func() { _ = first.Close() }()
		second := dialRootWebsocket(t, rootServer.URL, "/v1/responses", desktopBearerHeaders())
		defer func() { _ = second.Close() }()
		assertCloseCode(t, first, websocket.CloseTryAgainLater)
		if errWrite := second.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"unknown"}`)); errWrite != nil {
			t.Fatalf("write second pending route: %v", errWrite)
		}
		assertCloseCode(t, second, websocket.ClosePolicyViolation)
	})

	t.Run("in-flight dial capacity", func(t *testing.T) {
		dialStarted := make(chan struct{}, 1)
		dialCanceled := make(chan struct{}, 1)
		bridge := newTestBridge(t, "ws://127.0.0.1:1/responses", "ws://127.0.0.1:2/responses", func(options *bridgeOptions) {
			options.maxPendingRoutes = 1
			options.dialRelay = func(ctx context.Context, _ string, _ http.Header) (*websocket.Conn, *http.Response, error) {
				dialStarted <- struct{}{}
				<-ctx.Done()
				dialCanceled <- struct{}{}
				return nil, nil, ctx.Err()
			}
		})
		rootServer := httptest.NewServer(bridge)
		t.Cleanup(rootServer.Close)

		first := dialRootWebsocket(t, rootServer.URL, "/v1/responses", desktopBearerHeaders())
		defer func() { _ = first.Close() }()
		if errWrite := first.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"relay-model"}`)); errWrite != nil {
			t.Fatalf("write first route: %v", errWrite)
		}
		receiveWithTimeout(t, dialStarted)

		second := dialRootWebsocket(t, rootServer.URL, "/v1/responses", desktopBearerHeaders())
		defer func() { _ = second.Close() }()
		assertCloseCode(t, first, websocket.CloseTryAgainLater)
		receiveWithTimeout(t, dialCanceled)
	})

	t.Run("established route releases capacity", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			connection, errUpgrade := testUpgrader().Upgrade(response, request, nil)
			if errUpgrade != nil {
				return
			}
			defer func() { _ = connection.Close() }()
			for {
				messageType, payload, errRead := connection.ReadMessage()
				if errRead != nil {
					return
				}
				if errWrite := connection.WriteMessage(messageType, payload); errWrite != nil {
					return
				}
			}
		}))
		t.Cleanup(upstream.Close)
		bridge := newTestBridge(t, "ws://127.0.0.1:1/responses", websocketURL(upstream.URL), func(options *bridgeOptions) {
			options.maxPendingRoutes = 1
		})
		rootServer := httptest.NewServer(bridge)
		t.Cleanup(rootServer.Close)

		first := dialRootWebsocket(t, rootServer.URL, "/v1/responses", desktopBearerHeaders())
		defer func() { _ = first.Close() }()
		writeAndExpectEcho(t, first, websocket.TextMessage, []byte(`{"type":"response.create","model":"relay-model"}`))

		second := dialRootWebsocket(t, rootServer.URL, "/v1/responses", desktopBearerHeaders())
		defer func() { _ = second.Close() }()
		writeAndExpectEcho(t, first, websocket.TextMessage, []byte(`{"type":"response.create","previous_response_id":"resp_1"}`))
	})

	t.Run("message too big", func(t *testing.T) {
		bridge := newTestBridge(t, "ws://127.0.0.1:1/responses", "ws://127.0.0.1:2/responses", func(options *bridgeOptions) {
			options.maxMessageBytes = 96
		})
		rootServer := httptest.NewServer(bridge)
		t.Cleanup(rootServer.Close)
		connection := dialRootWebsocket(t, rootServer.URL, "/v1/responses", http.Header{
			"Authorization": {"Bearer desktop-oauth"},
		})
		defer func() { _ = connection.Close() }()
		payload := []byte(`{"type":"response.create","model":"gpt-stock","padding":"` + strings.Repeat("x", 128) + `"}`)
		if errWrite := connection.WriteMessage(websocket.TextMessage, payload); errWrite != nil {
			t.Fatalf("write oversized message: %v", errWrite)
		}
		assertCloseCode(t, connection, websocket.CloseMessageTooBig)
	})
}

func TestWebsocketBridgeForcesFastServiceTierOnTurnCreationOnly(t *testing.T) {
	stockCapture := make(chan capturedMessage, 8)
	relayCapture := make(chan capturedMessage, 4)
	stockServer := newTurnCapturingWebsocketServer(t, stockCapture, `{"type":"response.completed","response":{"id":"stock"}}`)
	relayServer := newTurnCapturingWebsocketServer(t, relayCapture, `{"type":"response.completed","response":{"id":"relay"}}`)
	bridge := newTestBridge(t, websocketURL(stockServer.URL), websocketURL(relayServer.URL), func(options *bridgeOptions) {
		options.stockModels = []string{"gpt-stock", "gpt-standard"}
		options.fastModels = map[string]struct{}{"gpt-stock": {}}
	})
	rootServer := httptest.NewServer(bridge)
	t.Cleanup(rootServer.Close)

	connection := dialRootWebsocket(t, rootServer.URL, "/v1/responses", desktopBearerHeaders())
	defer func() { _ = connection.Close() }()

	turns := []struct {
		name     string
		payload  string
		wantTier string
	}{
		{
			name:     "configured stock model",
			payload:  `{"type":"response.create","model":"gpt-stock","input":[]}`,
			wantTier: officialFastServiceTier,
		},
		{
			name:    "unlisted stock model keeps the Desktop choice",
			payload: `{"type":"response.create","model":"gpt-standard","input":[]}`,
		},
	}
	for _, turn := range turns {
		t.Run(turn.name, func(t *testing.T) {
			if errWrite := connection.WriteMessage(websocket.TextMessage, []byte(turn.payload)); errWrite != nil {
				t.Fatalf("write turn: %v", errWrite)
			}
			capture := receiveCapture(t, stockCapture)
			if got := gjson.GetBytes(capture.payload, "service_tier").String(); got != turn.wantTier {
				t.Fatalf("service_tier = %q, want %q; payload=%s", got, turn.wantTier, capture.payload)
			}
			_, terminal, errRead := connection.ReadMessage()
			if errRead != nil || !upstreamEventIsTerminal(terminal) {
				t.Fatalf("terminal = %s, error %v", terminal, errRead)
			}
		})
	}

	// A frame that does not create a turn is not a request and must reach the
	// upstream exactly as Desktop wrote it, even while the model is forced.
	followUp := []byte(`{"type":"response.cancel"}`)
	if errWrite := connection.WriteMessage(websocket.TextMessage, followUp); errWrite != nil {
		t.Fatalf("write follow-up frame: %v", errWrite)
	}
	capture := receiveCapture(t, stockCapture)
	if string(capture.payload) != string(followUp) {
		t.Fatalf("non-create frame was rewritten: %s", capture.payload)
	}

	// Returning to the forced model through a route handoff opens a new upstream
	// connection, which must carry the tier just like the first turn did.
	relayTurn := []byte(`{"type":"response.create","model":"relay-model","input":[]}`)
	if errWrite := connection.WriteMessage(websocket.TextMessage, relayTurn); errWrite != nil {
		t.Fatalf("write relay turn: %v", errWrite)
	}
	relayRequest := receiveCapture(t, relayCapture)
	if !bytes.Equal(relayRequest.payload, relayTurn) {
		t.Fatalf("Relay turn was rewritten: %s", relayRequest.payload)
	}
	if _, terminal, errRead := connection.ReadMessage(); errRead != nil || !upstreamEventIsTerminal(terminal) {
		t.Fatalf("relay terminal = %s, error %v", terminal, errRead)
	}
	if errWrite := connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-stock","input":[]}`)); errWrite != nil {
		t.Fatalf("write handoff turn: %v", errWrite)
	}
	handoff := receiveCapture(t, stockCapture)
	if got := gjson.GetBytes(handoff.payload, "service_tier").String(); got != officialFastServiceTier {
		t.Fatalf("handoff service_tier = %q, want %q; payload=%s", got, officialFastServiceTier, handoff.payload)
	}
}

func TestWebsocketBridgeSwitchesRoutesOnOneDownstreamConnection(t *testing.T) {
	stockCapture := make(chan capturedMessage, 2)
	relayCapture := make(chan capturedMessage, 1)
	stockServer := newTerminalWebsocketServer(t, stockCapture, `{"type":"response.completed","response":{"id":"stock"}}`)
	relayServer := newTerminalWebsocketServer(t, relayCapture, `{"type":"response.completed","response":{"id":"relay"}}`)
	bridge := newTestBridge(t, websocketURL(stockServer.URL), websocketURL(relayServer.URL), nil)
	rootServer := httptest.NewServer(bridge)
	t.Cleanup(rootServer.Close)

	headers := desktopBearerHeaders()
	headers.Set("ChatGPT-Account-ID", "account-1")
	connection := dialRootWebsocket(t, rootServer.URL, "/v1/responses", headers)
	defer func() { _ = connection.Close() }()

	turns := []struct {
		payload []byte
		capture <-chan capturedMessage
		arm     route
	}{
		{payload: []byte(`{"type":"response.create","model":"gpt-stock","input":[]}`), capture: stockCapture, arm: routeOfficial},
		{payload: []byte(`{"type":"response.create","model":"relay-model","input":[]}`), capture: relayCapture, arm: routeRelay},
		{payload: []byte(`{"type":"response.create","model":"gpt-stock","input":[]}`), capture: stockCapture, arm: routeOfficial},
	}
	for index, turn := range turns {
		if errWrite := connection.WriteMessage(websocket.TextMessage, turn.payload); errWrite != nil {
			t.Fatalf("write turn %d: %v", index, errWrite)
		}
		capture := receiveCapture(t, turn.capture)
		if string(capture.payload) != string(turn.payload) {
			t.Fatalf("turn %d upstream payload = %s, want %s", index, capture.payload, turn.payload)
		}
		if turn.arm == routeOfficial {
			assertHeader(t, capture.header, "Authorization", "Bearer desktop-oauth")
			assertHeader(t, capture.header, "ChatGPT-Account-ID", "account-1")
			assertHeaderAbsent(t, capture.header, rootHopHeader)
		} else {
			assertHeader(t, capture.header, "Authorization", "Bearer relay-secret")
			assertHeader(t, capture.header, rootHopHeader, "1")
			assertHeaderAbsent(t, capture.header, "ChatGPT-Account-ID")
		}
		_, terminalPayload, errRead := connection.ReadMessage()
		if errRead != nil || !upstreamEventIsTerminal(terminalPayload) {
			t.Fatalf("turn %d terminal = %s, error %v", index, terminalPayload, errRead)
		}
	}
}

func TestWebsocketBridgeHandlesPrewarmCompactionAndTurnAcrossRoutes(t *testing.T) {
	stockCapture := make(chan capturedMessage, 1)
	relayCapture := make(chan capturedMessage, 2)
	stockServer := newTerminalWebsocketServer(t, stockCapture, `{"type":"response.completed","response":{"id":"stock"}}`)
	relayServer := newTerminalWebsocketServer(t, relayCapture, `{"type":"response.completed","response":{"id":"relay"}}`)
	bridge := newTestBridge(t, websocketURL(stockServer.URL), websocketURL(relayServer.URL), nil)
	rootServer := httptest.NewServer(bridge)
	t.Cleanup(rootServer.Close)

	headers := desktopBearerHeaders()
	headers.Set("ChatGPT-Account-ID", "account-1")
	connection := dialRootWebsocket(t, rootServer.URL, "/v1/responses", headers)
	defer func() { _ = connection.Close() }()

	requests := []struct {
		name    string
		payload []byte
		capture <-chan capturedMessage
		arm     route
	}{
		{
			name:    "new Relay model prewarm without a turn ID",
			payload: []byte(`{"type":"response.create","model":"relay-model","generate":false,"input":[],"client_metadata":{"x-codex-turn-metadata":"{\"request_kind\":\"prewarm\",\"turn_id\":\"\"}"}}`),
			capture: relayCapture,
			arm:     routeRelay,
		},
		{
			name:    "previous stock model compaction for the active turn",
			payload: []byte(`{"type":"response.create","model":"gpt-stock","input":[{"type":"compaction_trigger"}],"client_metadata":{"x-codex-turn-metadata":"{\"request_kind\":\"compaction\",\"turn_id\":\"turn-1\",\"compaction\":{\"reason\":\"comp_hash_changed\"}}"}}`),
			capture: stockCapture,
			arm:     routeOfficial,
		},
		{
			name:    "new Relay model turn after compaction",
			payload: []byte(`{"type":"response.create","model":"relay-model","input":[{"type":"message","role":"user","content":"continue"}],"client_metadata":{"x-codex-turn-metadata":"{\"request_kind\":\"turn\",\"turn_id\":\"turn-1\"}"}}`),
			capture: relayCapture,
			arm:     routeRelay,
		},
	}

	for _, request := range requests {
		t.Run(request.name, func(t *testing.T) {
			if errWrite := connection.WriteMessage(websocket.TextMessage, request.payload); errWrite != nil {
				t.Fatalf("write request: %v", errWrite)
			}
			capture := receiveCapture(t, request.capture)
			if string(capture.payload) != string(request.payload) {
				t.Fatalf("upstream payload = %s, want %s", capture.payload, request.payload)
			}
			if request.arm == routeOfficial {
				assertHeader(t, capture.header, "Authorization", "Bearer desktop-oauth")
				assertHeader(t, capture.header, "ChatGPT-Account-ID", "account-1")
				assertHeaderAbsent(t, capture.header, rootHopHeader)
			} else {
				assertHeader(t, capture.header, "Authorization", "Bearer relay-secret")
				assertHeader(t, capture.header, rootHopHeader, "1")
				assertHeaderAbsent(t, capture.header, "ChatGPT-Account-ID")
			}
			_, terminalPayload, errRead := connection.ReadMessage()
			if errRead != nil || !upstreamEventIsTerminal(terminalPayload) {
				t.Fatalf("terminal = %s, error %v", terminalPayload, errRead)
			}
		})
	}
}

func TestWebsocketBridgeRecoversStatefulCrossRouteTurnWithFullReplay(t *testing.T) {
	stockCapture := make(chan capturedMessage, 1)
	relayCapture := make(chan capturedMessage, 1)
	stockServer := newTerminalWebsocketServer(t, stockCapture, `{"type":"response.completed"}`)
	relayServer := newTerminalWebsocketServer(t, relayCapture, `{"type":"response.completed"}`)
	bridge := newTestBridge(t, websocketURL(stockServer.URL), websocketURL(relayServer.URL), nil)
	rootServer := httptest.NewServer(bridge)
	t.Cleanup(rootServer.Close)
	connection := dialRootWebsocket(t, rootServer.URL, "/v1/responses", desktopBearerHeaders())
	defer func() { _ = connection.Close() }()

	first := []byte(`{"type":"response.create","model":"relay-model","input":[]}`)
	if errWrite := connection.WriteMessage(websocket.TextMessage, first); errWrite != nil {
		t.Fatalf("write first turn: %v", errWrite)
	}
	receiveCapture(t, relayCapture)
	if _, _, errRead := connection.ReadMessage(); errRead != nil {
		t.Fatalf("read first terminal: %v", errRead)
	}

	statefulSwitch := []byte(`{"type":"response.create","model":"gpt-stock","previous_response_id":"resp_relay","input":[]}`)
	if errWrite := connection.WriteMessage(websocket.TextMessage, statefulSwitch); errWrite != nil {
		t.Fatalf("write stateful switch: %v", errWrite)
	}
	_, errorPayload, errRead := connection.ReadMessage()
	if errRead != nil {
		t.Fatalf("read replay request: %v", errRead)
	}
	if !strings.Contains(string(errorPayload), `"code":"previous_response_not_found"`) {
		t.Fatalf("replay event = %s", errorPayload)
	}
	select {
	case unexpected := <-stockCapture:
		t.Fatalf("stateful turn leaked to official upstream: %s", unexpected.payload)
	default:
	}

	fullReplay := []byte(`{"type":"response.create","model":"gpt-stock","previous_response_id":null,"input":[{"role":"user","content":"full history"}]}`)
	if errWrite := connection.WriteMessage(websocket.TextMessage, fullReplay); errWrite != nil {
		t.Fatalf("write full replay: %v", errWrite)
	}
	if got := receiveCapture(t, stockCapture); string(got.payload) != string(fullReplay) {
		t.Fatalf("official replay payload = %s, want %s", got.payload, fullReplay)
	}
	if _, _, errRead := connection.ReadMessage(); errRead != nil {
		t.Fatalf("read replay terminal: %v", errRead)
	}
}

func TestWebsocketBridgeFailsClosedForOfficialCompactionCrossingToRelay(t *testing.T) {
	stockCapture := make(chan capturedMessage, 1)
	relayCapture := make(chan capturedMessage, 1)
	stockServer := newTerminalWebsocketServer(t, stockCapture, `{"type":"response.completed"}`)
	relayServer := newTerminalWebsocketServer(t, relayCapture, `{"type":"response.completed"}`)
	bridge := newTestBridge(t, websocketURL(stockServer.URL), websocketURL(relayServer.URL), nil)
	rootServer := httptest.NewServer(bridge)
	t.Cleanup(rootServer.Close)
	connection := dialRootWebsocket(t, rootServer.URL, "/v1/responses", desktopBearerHeaders())
	defer func() { _ = connection.Close() }()

	if errWrite := connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-stock","input":[]}`)); errWrite != nil {
		t.Fatalf("write stock turn: %v", errWrite)
	}
	receiveCapture(t, stockCapture)
	if _, _, errRead := connection.ReadMessage(); errRead != nil {
		t.Fatalf("read stock terminal: %v", errRead)
	}

	compaction := validGPTReasoningEncryptedContentForRootTest()
	unsafeSwitch := []byte(`{"type":"response.create","model":"relay-model","input":[{"type":"compaction","encrypted_content":"` + compaction + `"},{"type":"message","role":"user","content":"continue"}]}`)
	if errWrite := connection.WriteMessage(websocket.TextMessage, unsafeSwitch); errWrite != nil {
		t.Fatalf("write compacted Relay switch: %v", errWrite)
	}
	_, errorPayload, errRead := connection.ReadMessage()
	if errRead != nil {
		t.Fatalf("read compaction portability error: %v", errRead)
	}
	if !strings.Contains(string(errorPayload), `"code":"cross_provider_compaction_not_portable"`) {
		t.Fatalf("compaction error = %s", errorPayload)
	}
	select {
	case leaked := <-relayCapture:
		t.Fatalf("official compaction reached Relay: %s", leaked.payload)
	default:
	}

	safeSwitch := []byte(`{"type":"response.create","model":"relay-model","input":[{"type":"message","role":"user","content":"new chain"}]}`)
	if errWrite := connection.WriteMessage(websocket.TextMessage, safeSwitch); errWrite != nil {
		t.Fatalf("write safe Relay switch: %v", errWrite)
	}
	if got := receiveCapture(t, relayCapture); string(got.payload) != string(safeSwitch) {
		t.Fatalf("safe Relay payload = %s, want %s", got.payload, safeSwitch)
	}
}

func TestWebsocketBridgeRejectsGPTCompactionOnInitialRelayRoute(t *testing.T) {
	var relayDials atomic.Int32
	bridge := newTestBridge(t, "ws://127.0.0.1:1/responses", "ws://127.0.0.1:2/responses", func(options *bridgeOptions) {
		options.dialRelay = func(context.Context, string, http.Header) (*websocket.Conn, *http.Response, error) {
			relayDials.Add(1)
			return nil, nil, errors.New("GPT compaction must be rejected before Relay dial")
		}
	})
	rootServer := httptest.NewServer(bridge)
	t.Cleanup(rootServer.Close)
	connection := dialRootWebsocket(t, rootServer.URL, "/v1/responses", desktopBearerHeaders())
	defer func() { _ = connection.Close() }()
	compaction := validGPTReasoningEncryptedContentForRootTest()
	payload := []byte(`{"type":"response.create","model":"relay-model","input":[{"type":"compaction","encrypted_content":"` + compaction + `"}]}`)
	if errWrite := connection.WriteMessage(websocket.TextMessage, payload); errWrite != nil {
		t.Fatalf("write initial Relay compaction: %v", errWrite)
	}
	_, errorPayload, errRead := connection.ReadMessage()
	if errRead != nil {
		t.Fatalf("read initial compaction error: %v", errRead)
	}
	if !strings.Contains(string(errorPayload), `"code":"cross_provider_compaction_not_portable"`) {
		t.Fatalf("initial compaction error = %s", errorPayload)
	}
	if got := relayDials.Load(); got != 0 {
		t.Fatalf("Relay dials = %d, want 0", got)
	}
}

func TestWebsocketBridgeKeepsRelayCompactionWithinProvider(t *testing.T) {
	var relayConnections atomic.Int32
	relayCapture := make(chan capturedMessage, 8)
	relayServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		connectionID := relayConnections.Add(1)
		connection, errUpgrade := testUpgrader().Upgrade(response, request, nil)
		if errUpgrade != nil {
			return
		}
		defer func() { _ = connection.Close() }()
		for {
			messageType, payload, errRead := connection.ReadMessage()
			if errRead != nil {
				return
			}
			headers := request.Header.Clone()
			headers.Set("X-Test-Connection", fmt.Sprintf("%d", connectionID))
			relayCapture <- capturedMessage{header: headers, messageType: messageType, payload: append([]byte(nil), payload...)}
			if errWrite := connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed"}`)); errWrite != nil {
				return
			}
		}
	}))
	t.Cleanup(relayServer.Close)
	bridge := newTestBridge(t, "ws://127.0.0.1:1/responses", websocketURL(relayServer.URL), func(options *bridgeOptions) {
		options.relayModels = []string{"grok-a", "grok-b", "kimi-a", "unclassified"}
		options.relayProviders = map[string]string{"grok-a": "xai", "grok-b": "xai", "kimi-a": "kimi"}
	})
	rootServer := httptest.NewServer(bridge)
	t.Cleanup(rootServer.Close)
	grok := validGrokEncryptedContentForRootTest()
	kimi := validKimiCompactionForRootTest("summary")

	for name, payload := range map[string]string{
		"mismatched initial state": `{"type":"response.create","model":"grok-a","input":[{"type":"compaction","encrypted_content":"` + kimi + `"}]}`,
		"unclassified trigger":     `{"type":"response.create","model":"unclassified","input":[{"type":"compaction_trigger"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			connection := dialRootWebsocket(t, rootServer.URL, "/v1/responses", desktopBearerHeaders())
			defer func() { _ = connection.Close() }()
			if errWrite := connection.WriteMessage(websocket.TextMessage, []byte(payload)); errWrite != nil {
				t.Fatalf("write rejected initial turn: %v", errWrite)
			}
			_, errorPayload, errRead := connection.ReadMessage()
			if errRead != nil || !strings.Contains(string(errorPayload), `"code":"cross_provider_compaction_not_portable"`) {
				t.Fatalf("initial error = %s, read error %v", errorPayload, errRead)
			}
		})
	}
	if got := relayConnections.Load(); got != 0 {
		t.Fatalf("Relay connections after rejected initial state = %d, want 0", got)
	}

	connection := dialRootWebsocket(t, rootServer.URL, "/v1/responses", desktopBearerHeaders())
	defer func() { _ = connection.Close() }()
	first := []byte(`{"type":"response.create","model":"grok-a","input":[{"type":"compaction","encrypted_content":"` + grok + `"}]}`)
	if errWrite := connection.WriteMessage(websocket.TextMessage, first); errWrite != nil {
		t.Fatalf("write initial Grok replay: %v", errWrite)
	}
	if capture := receiveCapture(t, relayCapture); capture.header.Get("X-Test-Connection") != "1" {
		t.Fatalf("initial Grok connection = %q, want 1", capture.header.Get("X-Test-Connection"))
	}
	if _, _, errRead := connection.ReadMessage(); errRead != nil {
		t.Fatalf("read initial Grok terminal: %v", errRead)
	}

	sameProvider := []byte(`{"type":"response.create","model":"grok-b","input":[{"type":"compaction","encrypted_content":"` + grok + `"}]}`)
	if errWrite := connection.WriteMessage(websocket.TextMessage, sameProvider); errWrite != nil {
		t.Fatalf("write same-provider Grok replay: %v", errWrite)
	}
	if capture := receiveCapture(t, relayCapture); capture.header.Get("X-Test-Connection") != "1" {
		t.Fatalf("same-provider Grok connection = %q, want reused connection 1", capture.header.Get("X-Test-Connection"))
	}
	if _, _, errRead := connection.ReadMessage(); errRead != nil {
		t.Fatalf("read same-provider terminal: %v", errRead)
	}

	statefulSwitch := []byte(`{"type":"response.create","model":"kimi-a","previous_response_id":"resp_xai","input":[]}`)
	if errWrite := connection.WriteMessage(websocket.TextMessage, statefulSwitch); errWrite != nil {
		t.Fatalf("write stateful provider switch: %v", errWrite)
	}
	_, replayError, errRead := connection.ReadMessage()
	if errRead != nil || !strings.Contains(string(replayError), `"code":"previous_response_not_found"`) {
		t.Fatalf("state replay error = %s, read error %v", replayError, errRead)
	}
	conversationSwitch := []byte(`{"type":"response.create","model":"kimi-a","conversation":{"id":"conv_xai"},"input":[]}`)
	if errWrite := connection.WriteMessage(websocket.TextMessage, conversationSwitch); errWrite != nil {
		t.Fatalf("write conversation provider switch: %v", errWrite)
	}
	_, conversationError, errRead := connection.ReadMessage()
	if errRead != nil || !strings.Contains(string(conversationError), `"code":"previous_response_not_found"`) || !strings.Contains(string(conversationError), `"param":"conversation"`) {
		t.Fatalf("conversation replay error = %s, read error %v", conversationError, errRead)
	}

	compactedStatefulSwitch := []byte(`{"type":"response.create","model":"kimi-a","previous_response_id":"resp_xai","input":[{"type":"compaction","encrypted_content":"` + kimi + `"}]}`)
	if errWrite := connection.WriteMessage(websocket.TextMessage, compactedStatefulSwitch); errWrite != nil {
		t.Fatalf("write compacted provider switch: %v", errWrite)
	}
	_, compactionError, errRead := connection.ReadMessage()
	if errRead != nil || !strings.Contains(string(compactionError), `"code":"cross_provider_compaction_not_portable"`) {
		t.Fatalf("compaction error = %s, read error %v", compactionError, errRead)
	}
	if strings.Contains(string(compactionError), `"code":"previous_response_not_found"`) {
		t.Fatalf("state-ID error won over non-portable compaction: %s", compactionError)
	}

	safeKimi := []byte(`{"type":"response.create","model":"kimi-a","previous_response_id":null,"input":[{"type":"message","role":"user","content":"full history"}]}`)
	if errWrite := connection.WriteMessage(websocket.TextMessage, safeKimi); errWrite != nil {
		t.Fatalf("write safe Kimi switch: %v", errWrite)
	}
	if capture := receiveCapture(t, relayCapture); capture.header.Get("X-Test-Connection") != "2" {
		t.Fatalf("Kimi handoff connection = %q, want redialed connection 2", capture.header.Get("X-Test-Connection"))
	}
	if _, _, errRead := connection.ReadMessage(); errRead != nil {
		t.Fatalf("read Kimi terminal: %v", errRead)
	}

	sameKimi := []byte(`{"type":"response.create","model":"kimi-a","input":[{"type":"compaction","encrypted_content":"` + kimi + `"}]}`)
	if errWrite := connection.WriteMessage(websocket.TextMessage, sameKimi); errWrite != nil {
		t.Fatalf("write same-provider Kimi replay: %v", errWrite)
	}
	if capture := receiveCapture(t, relayCapture); capture.header.Get("X-Test-Connection") != "2" {
		t.Fatalf("same-provider Kimi connection = %q, want reused connection 2", capture.header.Get("X-Test-Connection"))
	}
	if _, _, errRead := connection.ReadMessage(); errRead != nil {
		t.Fatalf("read same-provider Kimi terminal: %v", errRead)
	}
}

func TestWebsocketBridgeWaitsForTerminalBeforeRelayProviderRedial(t *testing.T) {
	relayCapture := make(chan capturedMessage, 2)
	release := make(chan struct{})
	relayServer := newControlledTerminalWebsocketServer(t, relayCapture, release)
	bridge := newTestBridge(t, "ws://127.0.0.1:1/responses", websocketURL(relayServer.URL), func(options *bridgeOptions) {
		options.relayModels = []string{"grok-a", "kimi-a"}
		options.relayProviders = map[string]string{"grok-a": "xai", "kimi-a": "kimi"}
	})
	rootServer := httptest.NewServer(bridge)
	t.Cleanup(rootServer.Close)
	connection := dialRootWebsocket(t, rootServer.URL, "/v1/responses", desktopBearerHeaders())
	defer func() { _ = connection.Close() }()

	if errWrite := connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"grok-a","input":[]}`)); errWrite != nil {
		t.Fatalf("write Grok turn: %v", errWrite)
	}
	receiveCapture(t, relayCapture)
	if errWrite := connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"kimi-a","input":[]}`)); errWrite != nil {
		t.Fatalf("write queued Kimi turn: %v", errWrite)
	}
	select {
	case early := <-relayCapture:
		t.Fatalf("provider handoff ran before terminal: %s", early.payload)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if _, _, errRead := connection.ReadMessage(); errRead != nil {
		t.Fatalf("read Grok terminal: %v", errRead)
	}
	if capture := receiveCapture(t, relayCapture); !strings.Contains(string(capture.payload), `"model":"kimi-a"`) {
		t.Fatalf("redialed payload = %s, want Kimi turn", capture.payload)
	}
	if _, _, errRead := connection.ReadMessage(); errRead != nil {
		t.Fatalf("read Kimi terminal: %v", errRead)
	}
}

func TestWebsocketTargetChangedSeparatesUnclassifiedModels(t *testing.T) {
	state := websocketControllerState{route: routeRelay, model: "unknown-a"}
	if !websocketTargetChanged(state, routeRelay, "", "unknown-b") {
		t.Fatal("distinct unclassified Relay models were treated as the same logical target")
	}
	if websocketTargetChanged(state, routeRelay, "", "unknown-a") {
		t.Fatal("the same unclassified Relay model did not reuse its logical target")
	}
}

func TestWebsocketBridgeReconnectsBeforeAttestedOfficialHandoff(t *testing.T) {
	stockCapture := make(chan capturedMessage, 1)
	relayCapture := make(chan capturedMessage, 1)
	stockServer := newTerminalWebsocketServer(t, stockCapture, `{"type":"response.completed"}`)
	relayServer := newTerminalWebsocketServer(t, relayCapture, `{"type":"response.completed"}`)
	bridge := newTestBridge(t, websocketURL(stockServer.URL), websocketURL(relayServer.URL), nil)
	rootServer := httptest.NewServer(bridge)
	t.Cleanup(rootServer.Close)

	headers := desktopBearerHeaders()
	headers.Set("X-OAI-Attestation", "attestation-1")
	connection := dialRootWebsocket(t, rootServer.URL, "/v1/responses", headers)
	if errWrite := connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"relay-model","input":[]}`)); errWrite != nil {
		t.Fatalf("write Relay turn: %v", errWrite)
	}
	receiveCapture(t, relayCapture)
	if _, _, errRead := connection.ReadMessage(); errRead != nil {
		t.Fatalf("read Relay terminal: %v", errRead)
	}
	if errWrite := connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-stock","input":[]}`)); errWrite != nil {
		t.Fatalf("write attested official handoff: %v", errWrite)
	}
	assertCloseCode(t, connection, websocket.CloseServiceRestart)
	_ = connection.Close()
	select {
	case leaked := <-stockCapture:
		t.Fatalf("stale attestation reached official upstream: %s", leaked.payload)
	default:
	}

	freshHeaders := desktopBearerHeaders()
	freshHeaders.Set("X-OAI-Attestation", "attestation-2")
	freshConnection := dialRootWebsocket(t, rootServer.URL, "/v1/responses", freshHeaders)
	defer func() { _ = freshConnection.Close() }()
	stockTurn := []byte(`{"type":"response.create","model":"gpt-stock","input":[]}`)
	if errWrite := freshConnection.WriteMessage(websocket.TextMessage, stockTurn); errWrite != nil {
		t.Fatalf("write stock turn after reconnect: %v", errWrite)
	}
	stockRequest := receiveCapture(t, stockCapture)
	if string(stockRequest.payload) != string(stockTurn) {
		t.Fatalf("stock reconnect payload = %s, want %s", stockRequest.payload, stockTurn)
	}
	assertHeader(t, stockRequest.header, "X-OAI-Attestation", "attestation-2")
}

func TestWebsocketBridgeQueuesCrossRouteTurnUntilTerminalEvent(t *testing.T) {
	stockCapture := make(chan capturedMessage, 1)
	relayCapture := make(chan capturedMessage, 1)
	releaseStock := make(chan struct{})
	stockServer := newControlledTerminalWebsocketServer(t, stockCapture, releaseStock)
	relayServer := newTerminalWebsocketServer(t, relayCapture, `{"type":"response.completed"}`)
	bridge := newTestBridge(t, websocketURL(stockServer.URL), websocketURL(relayServer.URL), nil)
	rootServer := httptest.NewServer(bridge)
	t.Cleanup(rootServer.Close)
	connection := dialRootWebsocket(t, rootServer.URL, "/v1/responses", desktopBearerHeaders())
	defer func() { _ = connection.Close() }()

	if errWrite := connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-stock"}`)); errWrite != nil {
		t.Fatalf("write stock turn: %v", errWrite)
	}
	receiveCapture(t, stockCapture)
	queued := []byte(`{"type":"response.create","model":"relay-model","input":[]}`)
	if errWrite := connection.WriteMessage(websocket.TextMessage, queued); errWrite != nil {
		t.Fatalf("write queued Relay turn: %v", errWrite)
	}
	select {
	case leaked := <-relayCapture:
		t.Fatalf("Relay received turn before stock terminal: %s", leaked.payload)
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseStock)
	if _, terminal, errRead := connection.ReadMessage(); errRead != nil || !upstreamEventIsTerminal(terminal) {
		t.Fatalf("stock terminal = %s, error %v", terminal, errRead)
	}
	if got := receiveCapture(t, relayCapture); string(got.payload) != string(queued) {
		t.Fatalf("queued Relay payload = %s, want %s", got.payload, queued)
	}
}

func TestWebsocketBridgeWaitsForEveryAcceptedTurnAndForwardsPendingControls(t *testing.T) {
	stockCapture := make(chan capturedMessage, 3)
	relayCapture := make(chan capturedMessage, 1)
	stockTerminals := make(chan string, 2)
	stockServer := newSequencedWebsocketServer(t, stockCapture, stockTerminals)
	relayServer := newTerminalWebsocketServer(t, relayCapture, `{"type":"response.completed"}`)
	bridge := newTestBridge(t, websocketURL(stockServer.URL), websocketURL(relayServer.URL), nil)
	rootServer := httptest.NewServer(bridge)
	t.Cleanup(rootServer.Close)
	connection := dialRootWebsocket(t, rootServer.URL, "/v1/responses", desktopBearerHeaders())
	defer func() { _ = connection.Close() }()

	for index := 1; index <= 2; index++ {
		payload := []byte(fmt.Sprintf(`{"type":"response.create","model":"gpt-stock","input":[{"turn":%d}]}`, index))
		if errWrite := connection.WriteMessage(websocket.TextMessage, payload); errWrite != nil {
			t.Fatalf("write stock turn %d: %v", index, errWrite)
		}
		if got := receiveCapture(t, stockCapture); string(got.payload) != string(payload) {
			t.Fatalf("stock turn %d payload = %s, want %s", index, got.payload, payload)
		}
	}
	queued := []byte(`{"type":"response.create","model":"relay-model","input":[]}`)
	if errWrite := connection.WriteMessage(websocket.TextMessage, queued); errWrite != nil {
		t.Fatalf("write queued Relay turn: %v", errWrite)
	}
	control := []byte(`{"type":"response.inject","input":[{"type":"input_text","text":"continue old turn"}]}`)
	if errWrite := connection.WriteMessage(websocket.TextMessage, control); errWrite != nil {
		t.Fatalf("write pending control: %v", errWrite)
	}
	if got := receiveCapture(t, stockCapture); string(got.payload) != string(control) {
		t.Fatalf("pending control payload = %s, want %s", got.payload, control)
	}

	stockTerminals <- `{"type":"response.completed","response":{"id":"stock-1"}}`
	if _, terminal, errRead := connection.ReadMessage(); errRead != nil || !upstreamEventIsTerminal(terminal) {
		t.Fatalf("first stock terminal = %s, error %v", terminal, errRead)
	}
	select {
	case leaked := <-relayCapture:
		t.Fatalf("Relay received turn after only one of two terminals: %s", leaked.payload)
	case <-time.After(100 * time.Millisecond):
	}

	stockTerminals <- `{"type":"response.completed","response":{"id":"stock-2"}}`
	close(stockTerminals)
	if _, terminal, errRead := connection.ReadMessage(); errRead != nil || !upstreamEventIsTerminal(terminal) {
		t.Fatalf("second stock terminal = %s, error %v", terminal, errRead)
	}
	if got := receiveCapture(t, relayCapture); string(got.payload) != string(queued) {
		t.Fatalf("queued Relay payload = %s, want %s", got.payload, queued)
	}
}

func TestWebsocketBridgeDoesNotHandoffAfterFatalUpstreamError(t *testing.T) {
	stockCapture := make(chan capturedMessage, 1)
	relayCapture := make(chan capturedMessage, 1)
	stockEvents := make(chan string, 1)
	stockServer := newSequencedWebsocketServer(t, stockCapture, stockEvents)
	relayServer := newTerminalWebsocketServer(t, relayCapture, `{"type":"response.completed"}`)
	bridge := newTestBridge(t, websocketURL(stockServer.URL), websocketURL(relayServer.URL), nil)
	rootServer := httptest.NewServer(bridge)
	t.Cleanup(rootServer.Close)
	connection := dialRootWebsocket(t, rootServer.URL, "/v1/responses", desktopBearerHeaders())
	defer func() { _ = connection.Close() }()

	if errWrite := connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-stock"}`)); errWrite != nil {
		t.Fatalf("write stock turn: %v", errWrite)
	}
	receiveCapture(t, stockCapture)
	if errWrite := connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"relay-model","input":[]}`)); errWrite != nil {
		t.Fatalf("write queued Relay turn: %v", errWrite)
	}
	stockEvents <- `{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"bad_inject","message":"fatal"}}`
	close(stockEvents)
	_, errorPayload, errRead := connection.ReadMessage()
	if errRead != nil || !upstreamEventIsError(errorPayload) {
		t.Fatalf("upstream error event = %s, error %v", errorPayload, errRead)
	}
	assertCloseCode(t, connection, websocket.CloseNormalClosure)
	select {
	case leaked := <-relayCapture:
		t.Fatalf("queued Relay turn executed after fatal error: %s", leaked.payload)
	default:
	}
}

func TestWebsocketBridgeDoesNotFallbackAfterDialFailure(t *testing.T) {
	var officialDials atomic.Int32
	var relayDials atomic.Int32
	bridge := newTestBridge(t, "ws://127.0.0.1:1/responses", "ws://127.0.0.1:2/responses", func(options *bridgeOptions) {
		options.dialOfficial = func(context.Context, string, http.Header) (*websocket.Conn, *http.Response, error) {
			officialDials.Add(1)
			return nil, nil, errors.New("official unavailable")
		}
		options.dialRelay = func(context.Context, string, http.Header) (*websocket.Conn, *http.Response, error) {
			relayDials.Add(1)
			return nil, nil, errors.New("Relay must not be tried")
		}
	})
	rootServer := httptest.NewServer(bridge)
	t.Cleanup(rootServer.Close)
	connection := dialRootWebsocket(t, rootServer.URL, "/v1/responses", http.Header{
		"Authorization": {"Bearer desktop-oauth"},
	})
	defer func() { _ = connection.Close() }()
	if errWrite := connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-stock"}`)); errWrite != nil {
		t.Fatalf("write stock message: %v", errWrite)
	}
	assertCloseCode(t, connection, websocket.CloseInternalServerErr)
	if got := officialDials.Load(); got != 1 {
		t.Fatalf("official dials = %d, want 1", got)
	}
	if got := relayDials.Load(); got != 0 {
		t.Fatalf("relay dials = %d, want 0", got)
	}
}

func TestWebsocketBridgeDoesNotFallbackFromRelayToOfficial(t *testing.T) {
	var officialDials atomic.Int32
	var relayDials atomic.Int32
	bridge := newTestBridge(t, "ws://127.0.0.1:1/responses", "ws://127.0.0.1:2/responses", func(options *bridgeOptions) {
		options.dialOfficial = func(context.Context, string, http.Header) (*websocket.Conn, *http.Response, error) {
			officialDials.Add(1)
			return nil, nil, errors.New("Official must not be tried")
		}
		options.dialRelay = func(context.Context, string, http.Header) (*websocket.Conn, *http.Response, error) {
			relayDials.Add(1)
			return nil, nil, errors.New("Relay unavailable")
		}
	})
	rootServer := httptest.NewServer(bridge)
	t.Cleanup(rootServer.Close)
	connection := dialRootWebsocket(t, rootServer.URL, "/v1/responses", desktopBearerHeaders())
	defer func() { _ = connection.Close() }()
	if errWrite := connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"relay-model"}`)); errWrite != nil {
		t.Fatalf("write Relay message: %v", errWrite)
	}
	assertCloseCode(t, connection, websocket.CloseInternalServerErr)
	if got := relayDials.Load(); got != 1 {
		t.Fatalf("Relay dials = %d, want 1", got)
	}
	if got := officialDials.Load(); got != 0 {
		t.Fatalf("official dials = %d, want 0", got)
	}
}

func TestWebsocketBridgeDoesNotFallbackAfterHandoffDialFailure(t *testing.T) {
	relayCapture := make(chan capturedMessage, 1)
	relayServer := newTerminalWebsocketServer(t, relayCapture, `{"type":"response.completed"}`)
	var officialDials atomic.Int32
	bridge := newTestBridge(t, "ws://127.0.0.1:1/responses", websocketURL(relayServer.URL), func(options *bridgeOptions) {
		options.dialOfficial = func(context.Context, string, http.Header) (*websocket.Conn, *http.Response, error) {
			officialDials.Add(1)
			return nil, nil, errors.New("official unavailable")
		}
	})
	rootServer := httptest.NewServer(bridge)
	t.Cleanup(rootServer.Close)
	connection := dialRootWebsocket(t, rootServer.URL, "/v1/responses", desktopBearerHeaders())
	defer func() { _ = connection.Close() }()

	if errWrite := connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"relay-model"}`)); errWrite != nil {
		t.Fatalf("write Relay turn: %v", errWrite)
	}
	receiveCapture(t, relayCapture)
	if _, _, errRead := connection.ReadMessage(); errRead != nil {
		t.Fatalf("read Relay terminal: %v", errRead)
	}
	if errWrite := connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-stock","input":[]}`)); errWrite != nil {
		t.Fatalf("write official handoff: %v", errWrite)
	}
	assertCloseCode(t, connection, websocket.CloseInternalServerErr)
	if got := officialDials.Load(); got != 1 {
		t.Fatalf("official handoff dials = %d, want 1", got)
	}
}

func TestWebsocketBridgeMapsUpstreamHandshakeStatus(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		wantClose  int
	}{
		{name: "unauthorized", statusCode: http.StatusUnauthorized, wantClose: websocket.ClosePolicyViolation},
		{name: "forbidden", statusCode: http.StatusForbidden, wantClose: websocket.ClosePolicyViolation},
		{name: "rate limited", statusCode: http.StatusTooManyRequests, wantClose: websocket.CloseTryAgainLater},
		{name: "unavailable", statusCode: http.StatusServiceUnavailable, wantClose: websocket.CloseTryAgainLater},
		{name: "other", statusCode: http.StatusBadGateway, wantClose: websocket.CloseInternalServerErr},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bridge := newTestBridge(t, "ws://127.0.0.1:1/responses", "ws://127.0.0.1:2/responses", func(options *bridgeOptions) {
				options.dialRelay = func(context.Context, string, http.Header) (*websocket.Conn, *http.Response, error) {
					return nil, &http.Response{StatusCode: test.statusCode, Body: http.NoBody}, errors.New("handshake rejected")
				}
			})
			rootServer := httptest.NewServer(bridge)
			t.Cleanup(rootServer.Close)
			connection := dialRootWebsocket(t, rootServer.URL, "/v1/responses", desktopBearerHeaders())
			defer func() { _ = connection.Close() }()
			if errWrite := connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"relay-model"}`)); errWrite != nil {
				t.Fatalf("write Relay message: %v", errWrite)
			}
			assertCloseCode(t, connection, test.wantClose)
		})
	}
}

func TestWebsocketBridgeMirrorsUpstreamClose(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		connection, errUpgrade := testUpgrader().Upgrade(response, request, nil)
		if errUpgrade != nil {
			t.Errorf("upgrade upstream: %v", errUpgrade)
			return
		}
		defer func() { _ = connection.Close() }()
		if _, _, errRead := connection.ReadMessage(); errRead != nil {
			t.Errorf("read first message: %v", errRead)
			return
		}
		if errClose := connection.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseTryAgainLater, "busy"),
			time.Time{},
		); errClose != nil {
			t.Errorf("write upstream close: %v", errClose)
		}
	}))
	t.Cleanup(upstream.Close)
	bridge := newTestBridge(t, "ws://127.0.0.1:1/responses", websocketURL(upstream.URL), nil)
	rootServer := httptest.NewServer(bridge)
	t.Cleanup(rootServer.Close)
	connection := dialRootWebsocket(t, rootServer.URL, "/v1/responses", desktopBearerHeaders())
	defer func() { _ = connection.Close() }()
	if errWrite := connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"relay-model"}`)); errWrite != nil {
		t.Fatalf("write first message: %v", errWrite)
	}
	closeError := readCloseError(t, connection)
	if closeError.Code != websocket.CloseTryAgainLater || closeError.Text != "busy" {
		t.Fatalf("close = %d %q, want 1013 busy", closeError.Code, closeError.Text)
	}
}

func TestWebsocketBridgePreservesCloseDuringConcurrentWrite(t *testing.T) {
	triggerClose := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		connection, errUpgrade := testUpgrader().Upgrade(response, request, nil)
		if errUpgrade != nil {
			t.Errorf("upgrade upstream: %v", errUpgrade)
			return
		}
		defer func() { _ = connection.Close() }()
		messageType, payload, errRead := connection.ReadMessage()
		if errRead != nil {
			t.Errorf("read first message: %v", errRead)
			return
		}
		if errWrite := connection.WriteMessage(messageType, payload); errWrite != nil {
			t.Errorf("write ready message: %v", errWrite)
			return
		}
		<-triggerClose
		if errClose := connection.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseTryAgainLater, "busy during write"),
			time.Time{},
		); errClose != nil {
			t.Errorf("write upstream close: %v", errClose)
		}
	}))
	t.Cleanup(upstream.Close)
	bridge := newTestBridge(t, "ws://127.0.0.1:1/responses", websocketURL(upstream.URL), func(options *bridgeOptions) {
		options.maxMessageBytes = 2 << 20
	})
	rootServer := httptest.NewServer(bridge)
	t.Cleanup(rootServer.Close)
	connection := dialRootWebsocket(t, rootServer.URL, "/v1/responses", desktopBearerHeaders())
	defer func() { _ = connection.Close() }()
	writeAndExpectEcho(t, connection, websocket.TextMessage, []byte(`{"type":"response.create","model":"relay-model"}`))
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","input":"`+strings.Repeat("x", 1<<20)+`"}`))
	}()
	close(triggerClose)
	closeError := readCloseError(t, connection)
	if closeError.Code != websocket.CloseTryAgainLater || closeError.Text != "busy during write" {
		t.Fatalf("close = %d %q, want 1013 busy during write", closeError.Code, closeError.Text)
	}
	select {
	case <-writeDone:
	case <-time.After(time.Second):
		t.Fatal("concurrent downstream write did not exit")
	}
}

func TestWebsocketBridgeMirrorsDownstreamClose(t *testing.T) {
	upstreamClose := make(chan *websocket.CloseError, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		connection, errUpgrade := testUpgrader().Upgrade(response, request, nil)
		if errUpgrade != nil {
			t.Errorf("upgrade upstream: %v", errUpgrade)
			return
		}
		defer func() { _ = connection.Close() }()
		messageType, payload, errRead := connection.ReadMessage()
		if errRead != nil {
			t.Errorf("read first message: %v", errRead)
			return
		}
		if errWrite := connection.WriteMessage(messageType, payload); errWrite != nil {
			t.Errorf("write ready message: %v", errWrite)
			return
		}
		_, _, errRead = connection.ReadMessage()
		var closeError *websocket.CloseError
		if errors.As(errRead, &closeError) {
			upstreamClose <- closeError
			return
		}
		t.Errorf("upstream read error = %v, want close", errRead)
	}))
	t.Cleanup(upstream.Close)
	bridge := newTestBridge(t, "ws://127.0.0.1:1/responses", websocketURL(upstream.URL), nil)
	rootServer := httptest.NewServer(bridge)
	t.Cleanup(rootServer.Close)
	connection := dialRootWebsocket(t, rootServer.URL, "/v1/responses", desktopBearerHeaders())
	defer func() { _ = connection.Close() }()
	writeAndExpectEcho(t, connection, websocket.TextMessage, []byte(`{"type":"response.create","model":"relay-model"}`))
	if errClose := connection.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "done"),
		time.Time{},
	); errClose != nil {
		t.Fatalf("write downstream close: %v", errClose)
	}
	closeError := receiveWithTimeout(t, upstreamClose)
	if closeError.Code != websocket.CloseNormalClosure || closeError.Text != "done" {
		t.Fatalf("upstream close = %d %q", closeError.Code, closeError.Text)
	}
}

func TestWebsocketBridgeCloseTerminatesHijackedSessions(t *testing.T) {
	upstreamClose := make(chan error, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		connection, errUpgrade := testUpgrader().Upgrade(response, request, nil)
		if errUpgrade != nil {
			t.Errorf("upgrade upstream: %v", errUpgrade)
			return
		}
		defer func() { _ = connection.Close() }()
		messageType, payload, errRead := connection.ReadMessage()
		if errRead != nil {
			t.Errorf("read first message: %v", errRead)
			return
		}
		if errWrite := connection.WriteMessage(messageType, payload); errWrite != nil {
			t.Errorf("write first message: %v", errWrite)
			return
		}
		_, _, errRead = connection.ReadMessage()
		upstreamClose <- errRead
	}))
	t.Cleanup(upstream.Close)
	bridge := newTestBridge(t, "ws://127.0.0.1:1/responses", websocketURL(upstream.URL), nil)
	rootServer := httptest.NewServer(bridge)
	t.Cleanup(rootServer.Close)
	connection := dialRootWebsocket(t, rootServer.URL, "/v1/responses", desktopBearerHeaders())
	defer func() { _ = connection.Close() }()
	writeAndExpectEcho(t, connection, websocket.TextMessage, []byte(`{"type":"response.create","model":"relay-model"}`))
	closed := make(chan struct{})
	go func() {
		bridge.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("bridge.Close() blocked")
	}
	if _, _, errRead := connection.ReadMessage(); errRead == nil {
		t.Fatal("downstream remained open after bridge.Close()")
	}
	if errRead := receiveWithTimeout(t, upstreamClose); errRead == nil {
		t.Fatal("upstream remained open after bridge.Close()")
	}
}

func TestWebsocketBridgeCloseCancelsInFlightDial(t *testing.T) {
	dialStarted := make(chan struct{}, 1)
	bridge := newTestBridge(t, "ws://127.0.0.1:1/responses", "ws://127.0.0.1:2/responses", func(options *bridgeOptions) {
		options.dialRelay = func(ctx context.Context, _ string, _ http.Header) (*websocket.Conn, *http.Response, error) {
			dialStarted <- struct{}{}
			<-ctx.Done()
			return nil, nil, ctx.Err()
		}
	})
	rootServer := httptest.NewServer(bridge)
	t.Cleanup(rootServer.Close)
	connection := dialRootWebsocket(t, rootServer.URL, "/v1/responses", desktopBearerHeaders())
	defer func() { _ = connection.Close() }()
	if errWrite := connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"relay-model"}`)); errWrite != nil {
		t.Fatalf("write first message: %v", errWrite)
	}
	receiveWithTimeout(t, dialStarted)
	closed := make(chan struct{})
	go func() {
		bridge.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("bridge.Close() did not cancel the upstream dial")
	}
	if _, _, errRead := connection.ReadMessage(); errRead == nil {
		t.Fatal("downstream remained open after dial cancellation")
	}
}

func TestWebsocketBridgeDoesNotWriteAfterDownstreamCancellationRace(t *testing.T) {
	type upstreamRead struct {
		payload []byte
		err     error
	}
	upstreamReads := make(chan upstreamRead, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		connection, errUpgrade := testUpgrader().Upgrade(response, request, nil)
		if errUpgrade != nil {
			upstreamReads <- upstreamRead{err: errUpgrade}
			return
		}
		defer func() { _ = connection.Close() }()
		_, payload, errRead := connection.ReadMessage()
		upstreamReads <- upstreamRead{payload: payload, err: errRead}
	}))
	t.Cleanup(upstream.Close)
	dialStarted := make(chan struct{}, 1)
	bridge := newTestBridge(t, "ws://127.0.0.1:1/responses", websocketURL(upstream.URL), func(options *bridgeOptions) {
		options.dialRelay = func(ctx context.Context, target string, headers http.Header) (*websocket.Conn, *http.Response, error) {
			dialStarted <- struct{}{}
			<-ctx.Done()
			return websocket.DefaultDialer.Dial(target, headers)
		}
	})
	rootServer := httptest.NewServer(bridge)
	t.Cleanup(rootServer.Close)
	connection := dialRootWebsocket(t, rootServer.URL, "/v1/responses", desktopBearerHeaders())
	if errWrite := connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"relay-model"}`)); errWrite != nil {
		t.Fatalf("write first message: %v", errWrite)
	}
	receiveWithTimeout(t, dialStarted)
	if errClose := connection.Close(); errClose != nil {
		t.Fatalf("close downstream: %v", errClose)
	}
	observed := receiveWithTimeout(t, upstreamReads)
	if len(observed.payload) != 0 {
		t.Fatalf("canceled downstream payload reached upstream: %s", observed.payload)
	}
	if observed.err == nil {
		t.Fatal("late upstream connection remained open after downstream cancellation")
	}
}

func TestWebsocketBridgeRejectsUnsupportedHandshakeFeatures(t *testing.T) {
	bridge := newTestBridge(t, "ws://127.0.0.1:1/responses", "ws://127.0.0.1:2/responses", nil)
	rootServer := httptest.NewServer(bridge)
	t.Cleanup(rootServer.Close)

	tests := []struct {
		name         string
		path         string
		headers      http.Header
		subprotocols []string
		omitAuth     bool
		wantStatus   int
	}{
		{name: "missing bearer", path: "/v1/responses", omitAuth: true, wantStatus: http.StatusUnauthorized},
		{name: "query", path: "/v1/responses?token=secret", wantStatus: http.StatusBadRequest},
		{name: "hop marker", path: "/v1/responses", headers: http.Header{rootHopHeader: {"1"}}, wantStatus: http.StatusLoopDetected},
		{name: "untrusted origin", path: "/v1/responses", headers: http.Header{"Origin": {"https://evil.example"}}, wantStatus: http.StatusForbidden},
		{name: "subprotocol", path: "/v1/responses", subprotocols: []string{"required-protocol"}, wantStatus: http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			headers := desktopBearerHeaders()
			if test.omitAuth {
				headers = nil
			} else {
				for name, values := range test.headers {
					headers[name] = append([]string(nil), values...)
				}
			}
			dialer := websocket.Dialer{Subprotocols: test.subprotocols}
			connection, response, errDial := dialer.Dial(websocketURL(rootServer.URL)+test.path, headers)
			if connection != nil {
				_ = connection.Close()
			}
			if errDial == nil {
				t.Fatal("dial succeeded")
			}
			if response == nil || response.StatusCode != test.wantStatus {
				t.Fatalf("status = %#v, want %d", response, test.wantStatus)
			}
			if response.Body != nil {
				_ = response.Body.Close()
			}
		})
	}
}

func TestServerHealth(t *testing.T) {
	t.Setenv(defaultRelayAPIKeyEnv, "relay-secret")
	config := defaultConfig()
	config.Routing.StockModels = []string{"gpt-stock"}
	config.Routing.RelayModels = []string{"relay-model"}
	if errValidate := config.validateAndResolve(staticEnvironment("relay-secret")); errValidate != nil {
		t.Fatalf("validate config: %v", errValidate)
	}
	server, errServer := NewServer(&config)
	if errServer != nil {
		t.Fatalf("NewServer() error = %v", errServer)
	}
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "{\"status\":\"ok\"}\n" {
		t.Fatalf("health response = %d %q", recorder.Code, recorder.Body.String())
	}
}

func TestResponsesHandlerHTTPFallbackReturns426WithoutUpgradeOrUpstreamDial(t *testing.T) {
	var upstreamDials atomic.Int32
	bridge := newTestBridge(t, "ws://127.0.0.1:1/responses", "ws://127.0.0.1:2/responses", func(options *bridgeOptions) {
		options.dialOfficial = func(context.Context, string, http.Header) (*websocket.Conn, *http.Response, error) {
			upstreamDials.Add(1)
			return nil, nil, errors.New("unexpected official dial")
		}
		options.dialRelay = func(context.Context, string, http.Header) (*websocket.Conn, *http.Response, error) {
			upstreamDials.Add(1)
			return nil, nil, errors.New("unexpected Relay dial")
		}
	})
	rootServer := httptest.NewServer(&responsesEndpointHandler{
		websocket:     bridge,
		websocketMode: websocketModeHTTPFallback,
	})
	t.Cleanup(rootServer.Close)

	connection, response, errDial := websocket.DefaultDialer.Dial(
		websocketURL(rootServer.URL)+"/v1/responses",
		desktopBearerHeaders(),
	)
	if connection != nil {
		_ = connection.Close()
		t.Fatal("Root upgraded a cutover-safe HTTP fallback request")
	}
	if errDial == nil {
		t.Fatal("websocket dial succeeded, want HTTP 426")
	}
	if response == nil {
		t.Fatal("websocket dial returned no HTTP response")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("status = %d, want 426", response.StatusCode)
	}
	if got := response.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if got := upstreamDials.Load(); got != 0 {
		t.Fatalf("upstream dials = %d, want 0", got)
	}

	tests := []struct {
		name         string
		path         string
		headers      http.Header
		subprotocols []string
		wantStatus   int
	}{
		{name: "missing bearer", path: "/v1/responses", wantStatus: http.StatusUnauthorized},
		{name: "untrusted origin", path: "/v1/responses", headers: http.Header{"Authorization": {"Bearer desktop-oauth"}, "Origin": {"https://evil.example"}}, wantStatus: http.StatusForbidden},
		{name: "root hop", path: "/v1/responses", headers: http.Header{"Authorization": {"Bearer desktop-oauth"}, rootHopHeader: {"1"}}, wantStatus: http.StatusLoopDetected},
		{name: "query", path: "/v1/responses?route=relay", headers: desktopBearerHeaders(), wantStatus: http.StatusBadRequest},
		{name: "subprotocol", path: "/v1/responses", headers: desktopBearerHeaders(), subprotocols: []string{"route-model"}, wantStatus: http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dialer := websocket.Dialer{Subprotocols: test.subprotocols}
			connection, response, errDial := dialer.Dial(websocketURL(rootServer.URL)+test.path, test.headers)
			if connection != nil {
				_ = connection.Close()
				t.Fatal("invalid fallback handshake upgraded")
			}
			if errDial == nil || response == nil {
				t.Fatalf("dial error = %v response = %#v, want HTTP %d", errDial, response, test.wantStatus)
			}
			defer func() { _ = response.Body.Close() }()
			if response.StatusCode != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.StatusCode, test.wantStatus)
			}
		})
	}
	if got := upstreamDials.Load(); got != 0 {
		t.Fatalf("upstream dials after rejected handshakes = %d, want 0", got)
	}
}

func TestResponsesHandlerFirstMessageModeOptsIntoWebsocketBridge(t *testing.T) {
	var relayConnections atomic.Int32
	relayCapture := make(chan capturedMessage, 1)
	relayServer := newEchoWebsocketServer(t, &relayConnections, relayCapture)
	bridge := newTestBridge(t, "ws://127.0.0.1:1/responses", websocketURL(relayServer.URL), nil)
	rootServer := httptest.NewServer(&responsesEndpointHandler{
		websocket:     bridge,
		websocketMode: websocketModeFirstMessage,
	})
	t.Cleanup(rootServer.Close)

	connection := dialRootWebsocket(t, rootServer.URL, "/v1/responses", desktopBearerHeaders())
	defer func() { _ = connection.Close() }()
	writeAndExpectEcho(t, connection, websocket.TextMessage, []byte(`{"type":"response.create","model":"relay-model"}`))
	select {
	case <-relayCapture:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Relay WebSocket request")
	}
	if got := relayConnections.Load(); got != 1 {
		t.Fatalf("Relay connections = %d, want 1", got)
	}
}

func TestServerRunStopsOnContextCancellation(t *testing.T) {
	upstreamClosed := make(chan error, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		connection, errUpgrade := testUpgrader().Upgrade(response, request, nil)
		if errUpgrade != nil {
			t.Errorf("upgrade upstream: %v", errUpgrade)
			return
		}
		defer func() { _ = connection.Close() }()
		messageType, payload, errRead := connection.ReadMessage()
		if errRead != nil {
			t.Errorf("read first message: %v", errRead)
			return
		}
		if errWrite := connection.WriteMessage(messageType, payload); errWrite != nil {
			t.Errorf("write first message: %v", errWrite)
			return
		}
		_, _, errRead = connection.ReadMessage()
		upstreamClosed <- errRead
	}))
	t.Cleanup(upstream.Close)
	bridge := newTestBridge(t, "ws://127.0.0.1:1/responses", websocketURL(upstream.URL), nil)
	mux := http.NewServeMux()
	mux.Handle("/v1/responses", bridge)
	server := &Server{bridge: bridge, handler: mux}
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatalf("listen: %v", errListen)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- server.runWithListener(ctx, listener)
	}()
	connection := dialRootWebsocket(t, "http://"+listener.Addr().String(), "/v1/responses", desktopBearerHeaders())
	defer func() { _ = connection.Close() }()
	writeAndExpectEcho(t, connection, websocket.TextMessage, []byte(`{"type":"response.create","model":"relay-model"}`))
	cancel()
	if errRun := receiveWithTimeout(t, done); errRun != nil {
		t.Fatalf("runWithListener() error = %v", errRun)
	}
	if _, _, errRead := connection.ReadMessage(); errRead == nil {
		t.Fatal("live downstream session remained open after cancellation")
	}
	if errRead := receiveWithTimeout(t, upstreamClosed); errRead == nil {
		t.Fatal("live upstream session remained open after cancellation")
	}
}

func newTerminalWebsocketServer(t *testing.T, captures chan<- capturedMessage, terminal string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		connection, errUpgrade := testUpgrader().Upgrade(response, request, nil)
		if errUpgrade != nil {
			t.Errorf("upgrade terminal upstream: %v", errUpgrade)
			return
		}
		defer func() { _ = connection.Close() }()
		messageType, payload, errRead := connection.ReadMessage()
		if errRead != nil {
			return
		}
		captures <- capturedMessage{
			header:      request.Header.Clone(),
			messageType: messageType,
			payload:     append([]byte(nil), payload...),
		}
		if errWrite := connection.WriteMessage(websocket.TextMessage, []byte(terminal)); errWrite != nil {
			return
		}
		_, _, _ = connection.ReadMessage()
	}))
	t.Cleanup(server.Close)
	return server
}

// newTurnCapturingWebsocketServer captures every inbound frame for the lifetime
// of the connection and answers only turn creation, so a session can be driven
// across several messages.
func newTurnCapturingWebsocketServer(t *testing.T, captures chan<- capturedMessage, terminal string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		connection, errUpgrade := testUpgrader().Upgrade(response, request, nil)
		if errUpgrade != nil {
			t.Errorf("upgrade turn capturing upstream: %v", errUpgrade)
			return
		}
		defer func() { _ = connection.Close() }()
		for {
			messageType, payload, errRead := connection.ReadMessage()
			if errRead != nil {
				return
			}
			captures <- capturedMessage{
				header:      request.Header.Clone(),
				messageType: messageType,
				payload:     append([]byte(nil), payload...),
			}
			envelope, errInspect := inspectClientMessage(payload)
			if errInspect != nil || !envelope.hasEventType || envelope.eventType != "response.create" {
				continue
			}
			if errWrite := connection.WriteMessage(websocket.TextMessage, []byte(terminal)); errWrite != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func newControlledTerminalWebsocketServer(t *testing.T, captures chan<- capturedMessage, release <-chan struct{}) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		connection, errUpgrade := testUpgrader().Upgrade(response, request, nil)
		if errUpgrade != nil {
			t.Errorf("upgrade controlled upstream: %v", errUpgrade)
			return
		}
		defer func() { _ = connection.Close() }()
		messageType, payload, errRead := connection.ReadMessage()
		if errRead != nil {
			return
		}
		captures <- capturedMessage{
			header:      request.Header.Clone(),
			messageType: messageType,
			payload:     append([]byte(nil), payload...),
		}
		<-release
		if errWrite := connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed"}`)); errWrite != nil {
			return
		}
		_, _, _ = connection.ReadMessage()
	}))
	t.Cleanup(server.Close)
	return server
}

func newSequencedWebsocketServer(t *testing.T, captures chan<- capturedMessage, terminals <-chan string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		connection, errUpgrade := testUpgrader().Upgrade(response, request, nil)
		if errUpgrade != nil {
			t.Errorf("upgrade sequenced upstream: %v", errUpgrade)
			return
		}
		defer func() { _ = connection.Close() }()
		done := make(chan struct{})
		defer close(done)
		go func() {
			for {
				select {
				case terminal, ok := <-terminals:
					if !ok {
						return
					}
					if errWrite := connection.WriteMessage(websocket.TextMessage, []byte(terminal)); errWrite != nil {
						return
					}
				case <-done:
					return
				}
			}
		}()
		for {
			messageType, payload, errRead := connection.ReadMessage()
			if errRead != nil {
				return
			}
			captures <- capturedMessage{
				header:      request.Header.Clone(),
				messageType: messageType,
				payload:     append([]byte(nil), payload...),
			}
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func newEchoWebsocketServer(t *testing.T, connections *atomic.Int32, captures chan<- capturedMessage) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		connections.Add(1)
		connection, errUpgrade := testUpgrader().Upgrade(response, request, nil)
		if errUpgrade != nil {
			t.Errorf("upgrade echo upstream: %v", errUpgrade)
			return
		}
		defer func() { _ = connection.Close() }()
		messageType, payload, errRead := connection.ReadMessage()
		if errRead != nil {
			t.Errorf("read echo upstream: %v", errRead)
			return
		}
		captures <- capturedMessage{header: request.Header.Clone(), messageType: messageType, payload: append([]byte(nil), payload...)}
		if errWrite := connection.WriteMessage(messageType, payload); errWrite != nil {
			t.Errorf("write echo upstream: %v", errWrite)
			return
		}
		_ = connection.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "done"),
			time.Time{},
		)
	}))
	t.Cleanup(server.Close)
	return server
}

func testUpgrader() *websocket.Upgrader {
	return &websocket.Upgrader{
		EnableCompression: true,
		CheckOrigin:       func(*http.Request) bool { return true },
	}
}

func newTestBridge(t *testing.T, officialURL, relayURL string, modify func(*bridgeOptions)) *websocketBridge {
	t.Helper()
	options := bridgeOptions{
		officialURL:      officialURL,
		relayURL:         relayURL,
		relayAPIKey:      "relay-secret",
		stockModels:      []string{"gpt-stock"},
		relayModels:      []string{"relay-model"},
		relayProviders:   map[string]string{"relay-model": "kimi"},
		maxMessageBytes:  1 << 20,
		maxPendingRoutes: 16,
	}
	if modify != nil {
		modify(&options)
	}
	bridge, errBridge := newWebsocketBridge(options)
	if errBridge != nil {
		t.Fatalf("newWebsocketBridge() error = %v", errBridge)
	}
	t.Cleanup(bridge.Close)
	return bridge
}

func dialRootWebsocket(t *testing.T, rootURL, path string, headers http.Header) *websocket.Conn {
	t.Helper()
	connection, response, errDial := websocket.DefaultDialer.Dial(websocketURL(rootURL)+path, headers)
	if errDial != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		t.Fatalf("dial Root websocket: %v", errDial)
	}
	setTestReadDeadline(t, connection)
	return connection
}

func desktopBearerHeaders() http.Header {
	return http.Header{"Authorization": {"Bearer desktop-oauth"}}
}

func setTestReadDeadline(t *testing.T, connection *websocket.Conn) {
	t.Helper()
	if errDeadline := connection.SetReadDeadline(time.Now().Add(3 * time.Second)); errDeadline != nil {
		t.Fatalf("set test websocket read deadline: %v", errDeadline)
	}
}

func websocketURL(httpURL string) string {
	return "ws" + strings.TrimPrefix(httpURL, "http")
}

func writeAndExpectEcho(t *testing.T, connection *websocket.Conn, messageType int, payload []byte) {
	t.Helper()
	if errWrite := connection.WriteMessage(messageType, payload); errWrite != nil {
		t.Fatalf("write message: %v", errWrite)
	}
	gotType, gotPayload, errRead := connection.ReadMessage()
	if errRead != nil {
		t.Fatalf("read echoed message: %v", errRead)
	}
	if gotType != messageType || string(gotPayload) != string(payload) {
		t.Fatalf("echo = type %d payload %q, want type %d payload %q", gotType, gotPayload, messageType, payload)
	}
}

func assertCloseCode(t *testing.T, connection *websocket.Conn, want int) {
	t.Helper()
	closeError := readCloseError(t, connection)
	if closeError.Code != want {
		t.Fatalf("close code = %d reason %q, want %d", closeError.Code, closeError.Text, want)
	}
}

func readCloseError(t *testing.T, connection *websocket.Conn) *websocket.CloseError {
	t.Helper()
	_, _, errRead := connection.ReadMessage()
	var closeError *websocket.CloseError
	if !errors.As(errRead, &closeError) {
		t.Fatalf("ReadMessage() error = %v, want websocket close", errRead)
	}
	return closeError
}

func receiveCapture(t *testing.T, channel <-chan capturedMessage) capturedMessage {
	t.Helper()
	return receiveWithTimeout(t, channel)
}

func receiveWithTimeout[T any](t *testing.T, channel <-chan T) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-time.After(2 * time.Second):
		var zero T
		t.Fatal("timed out waiting for test event")
		return zero
	}
}

func assertHeaderAbsent(t *testing.T, headers http.Header, name string) {
	t.Helper()
	if got := headers.Get(name); got != "" {
		t.Fatalf("%s = %q, want absent", name, got)
	}
}
