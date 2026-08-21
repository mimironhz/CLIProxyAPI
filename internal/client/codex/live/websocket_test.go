package live

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

type denyDirectAdmissionGate struct {
	exhausted bool
}

type recordingDirectQuotaGate struct {
	settled chan coreusage.Detail
}

func (*recordingDirectQuotaGate) BlockedForModel([]*auth.Auth, string, time.Time) (auth.QuotaWindowBlock, bool) {
	return auth.QuotaWindowBlock{}, false
}

func (*recordingDirectQuotaGate) Admit(*auth.Auth, string, time.Time) (string, bool) {
	return "realtime-reservation", true
}

func (g *recordingDirectQuotaGate) SettleQuotaWindowReservation(_ string, detail coreusage.Detail) {
	g.settled <- detail
}

func (g *denyDirectAdmissionGate) BlockedForModel(_ []*auth.Auth, _ string, now time.Time) (auth.QuotaWindowBlock, bool) {
	if !g.exhausted {
		return auth.QuotaWindowBlock{}, false
	}
	return auth.QuotaWindowBlock{Provider: "codex", Window: "workday", Exhausted: []string{"requests"}, AvailableAt: now.Add(time.Hour)}, true
}

func (g *denyDirectAdmissionGate) Admit(*auth.Auth, string, time.Time) (string, bool) {
	g.exhausted = true
	return "", false
}

func TestHandleDirectWebsocketRejectsClientSecretModelMismatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewHandler(auth.NewManager(nil, nil, nil), nil)
	router := gin.New()
	router.GET("/v1/realtime", func(c *gin.Context) {
		c.Set(ClientSecretSessionContextKey, json.RawMessage(`{"type":"realtime","model":"gpt-live-1-codex"}`))
		c.Set(ClientSecretPrincipalContextKey, "sess_123")
		c.Next()
	}, handler.HandleRealtimeWebsocket)
	request := httptest.NewRequest(http.MethodGet, "/v1/realtime?model=another-live-model", nil)
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusForbidden, recorder.Body.String())
	}
}

func TestHandleDirectWebsocketAppliesClientSecretSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstreamUpdate := make(chan []byte, 1)
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		connection, errUpgrade := upgrader.Upgrade(writer, request, nil)
		if errUpgrade != nil {
			return
		}
		defer func() { _ = connection.Close() }()
		_, payload, errRead := connection.ReadMessage()
		if errRead != nil {
			return
		}
		upstreamUpdate <- append([]byte(nil), payload...)
		_ = connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.created"}`))
	}))
	defer upstreamServer.Close()

	manager := auth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(&captureExecutor{})
	registerCredential(t, manager, &auth.Auth{
		ID:       "codex-oauth",
		Provider: "codex",
		Status:   auth.StatusActive,
		Metadata: map[string]any{"access_token": "oauth-token"},
	})
	handler := NewHandler(manager, nil)
	handler.sidebandAPIBaseURL = "ws" + strings.TrimPrefix(upstreamServer.URL, "http") + "/v1"
	router := gin.New()
	router.GET("/v1/realtime", func(c *gin.Context) {
		c.Set(ClientSecretSessionContextKey, json.RawMessage(`{"type":"realtime","model":"gpt-live-1-codex","instructions":"help"}`))
		c.Set(ClientSecretPrincipalContextKey, "sess_123")
		c.Next()
	}, handler.HandleRealtimeWebsocket)
	downstreamServer := httptest.NewServer(router)
	defer downstreamServer.Close()

	wsURL := "ws" + strings.TrimPrefix(downstreamServer.URL, "http") + "/v1/realtime?model=gpt-realtime"
	connection, _, errDial := websocket.DefaultDialer.Dial(wsURL, nil)
	if errDial != nil {
		t.Fatalf("dial downstream websocket: %v", errDial)
	}
	defer func() { _ = connection.Close() }()
	_, _, _ = connection.ReadMessage()

	select {
	case update := <-upstreamUpdate:
		var event struct {
			Type    string `json:"type"`
			Session struct {
				Model        string `json:"model"`
				Instructions string `json:"instructions"`
			} `json:"session"`
		}
		if errUnmarshal := json.Unmarshal(update, &event); errUnmarshal != nil {
			t.Fatalf("unmarshal session update: %v", errUnmarshal)
		}
		if event.Type != "session.update" || event.Session.Model != "" || event.Session.Instructions != "help" {
			t.Fatalf("session update = %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("session update not captured")
	}
}

func TestHandleDirectWebsocketRelaysStandardRealtimeFrames(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstreamRequest := make(chan *http.Request, 1)
	upstreamMessage := make(chan string, 1)
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		connection, errUpgrade := upgrader.Upgrade(writer, request, nil)
		if errUpgrade != nil {
			return
		}
		defer func() { _ = connection.Close() }()
		upstreamRequest <- request.Clone(request.Context())
		if errWrite := connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.created"}`)); errWrite != nil {
			return
		}
		messageType, payload, errRead := connection.ReadMessage()
		if errRead != nil {
			return
		}
		upstreamMessage <- string(payload)
		_ = connection.WriteMessage(messageType, append([]byte("echo:"), payload...))
	}))
	defer upstreamServer.Close()

	manager := auth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(&captureExecutor{})
	registerCredential(t, manager, &auth.Auth{
		ID:       "codex-oauth",
		Provider: "codex",
		Status:   auth.StatusActive,
		Metadata: map[string]any{
			"access_token": "oauth-token",
			"account_id":   "account-123",
		},
	})
	handler := NewHandler(manager, nil)
	handler.sidebandAPIBaseURL = "ws" + strings.TrimPrefix(upstreamServer.URL, "http") + "/v1"

	router := gin.New()
	router.GET("/v1/realtime", handler.HandleRealtimeWebsocket)
	downstreamServer := httptest.NewServer(router)
	defer downstreamServer.Close()

	wsURL := "ws" + strings.TrimPrefix(downstreamServer.URL, "http") + "/v1/realtime?model=gpt-realtime"
	downstreamHeaders := make(http.Header)
	downstreamHeaders.Set("OpenAI-Alpha", "quicksilver=v2")
	connection, _, errDial := websocket.DefaultDialer.Dial(wsURL, downstreamHeaders)
	if errDial != nil {
		t.Fatalf("dial downstream websocket: %v", errDial)
	}
	defer func() { _ = connection.Close() }()

	_, created, errRead := connection.ReadMessage()
	if errRead != nil {
		t.Fatalf("read session.created: %v", errRead)
	}
	if string(created) != `{"type":"session.created"}` {
		t.Fatalf("created event = %s", created)
	}
	const event = `{"type":"response.create"}`
	if errWrite := connection.WriteMessage(websocket.TextMessage, []byte(event)); errWrite != nil {
		t.Fatalf("write downstream event: %v", errWrite)
	}
	_, echoed, errRead := connection.ReadMessage()
	if errRead != nil {
		t.Fatalf("read echoed event: %v", errRead)
	}
	if string(echoed) != "echo:"+event {
		t.Fatalf("echoed event = %s", echoed)
	}

	select {
	case request := <-upstreamRequest:
		if request.Header.Get("Authorization") != "Bearer oauth-token" {
			t.Fatalf("Authorization = %q", request.Header.Get("Authorization"))
		}
		if request.Header.Get("Chatgpt-Account-Id") != "account-123" {
			t.Fatalf("Chatgpt-Account-Id = %q", request.Header.Get("Chatgpt-Account-Id"))
		}
		if request.Header.Get("OpenAI-Alpha") != "" {
			t.Fatalf("OpenAI-Alpha must not be forwarded, got %q", request.Header.Get("OpenAI-Alpha"))
		}
		query, errParse := url.ParseQuery(request.URL.RawQuery)
		if errParse != nil {
			t.Fatalf("parse upstream query: %v", errParse)
		}
		if query.Get("model") != "gpt-realtime" || query.Has("intent") {
			t.Fatalf("upstream query = %v", query)
		}
	case <-time.After(time.Second):
		t.Fatal("upstream request not captured")
	}
	select {
	case payload := <-upstreamMessage:
		if payload != event {
			t.Fatalf("upstream event = %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("upstream event not captured")
	}
}

func TestHandleDirectWebsocketQuotaAdmissionBlocksUpstreamDial(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstreamHit := make(chan struct{}, 1)
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		upstreamHit <- struct{}{}
	}))
	defer upstreamServer.Close()

	manager := auth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(&captureExecutor{})
	manager.SetQuotaWindowGate(&denyDirectAdmissionGate{})
	registerCredential(t, manager, &auth.Auth{
		ID:       "codex-oauth-quota",
		Provider: "codex",
		Status:   auth.StatusActive,
		Metadata: map[string]any{"access_token": "oauth-token"},
	})
	handler := NewHandler(manager, nil)
	handler.sidebandAPIBaseURL = "ws" + strings.TrimPrefix(upstreamServer.URL, "http") + "/v1"
	router := gin.New()
	router.GET("/v1/realtime", handler.HandleRealtimeWebsocket)
	downstreamServer := httptest.NewServer(router)
	defer downstreamServer.Close()

	wsURL := "ws" + strings.TrimPrefix(downstreamServer.URL, "http") + "/v1/realtime?model=gpt-realtime"
	connection, response, errDial := websocket.DefaultDialer.Dial(wsURL, nil)
	if connection != nil {
		_ = connection.Close()
	}
	if errDial == nil || response == nil || response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("dial error = %v, response = %#v; want 429", errDial, response)
	}
	if got := response.Header.Get("Retry-After"); got == "" {
		t.Fatal("Retry-After is empty")
	}
	if response.Body != nil {
		_ = response.Body.Close()
	}
	select {
	case <-upstreamHit:
		t.Fatal("quota-exhausted direct websocket reached upstream")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestHandleDirectWebsocketSettlesTerminalTokenUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		connection, errUpgrade := upgrader.Upgrade(writer, request, nil)
		if errUpgrade != nil {
			return
		}
		defer func() { _ = connection.Close() }()
		_ = connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7}}}`))
		_, _, _ = connection.ReadMessage()
	}))
	defer upstreamServer.Close()

	gate := &recordingDirectQuotaGate{settled: make(chan coreusage.Detail, 1)}
	manager := auth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(&captureExecutor{})
	manager.SetQuotaWindowGate(gate)
	registerCredential(t, manager, &auth.Auth{
		ID: "codex-oauth-token-quota", Provider: "codex", Status: auth.StatusActive, Metadata: map[string]any{"access_token": "oauth-token"},
	})
	handler := NewHandler(manager, nil)
	handler.sidebandAPIBaseURL = "ws" + strings.TrimPrefix(upstreamServer.URL, "http") + "/v1"
	router := gin.New()
	router.GET("/v1/realtime", handler.HandleRealtimeWebsocket)
	downstreamServer := httptest.NewServer(router)
	defer downstreamServer.Close()

	connection, _, errDial := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(downstreamServer.URL, "http")+"/v1/realtime?model=gpt-realtime", nil)
	if errDial != nil {
		t.Fatalf("dial downstream websocket: %v", errDial)
	}
	_, _, errRead := connection.ReadMessage()
	if errRead != nil {
		t.Fatalf("read terminal usage: %v", errRead)
	}
	_ = connection.Close()

	select {
	case detail := <-gate.settled:
		if detail.InputTokens != 3 || detail.OutputTokens != 4 || detail.TotalTokens != 7 {
			t.Fatalf("settled usage = %+v", detail)
		}
	case <-time.After(time.Second):
		t.Fatal("quota usage was not settled")
	}
}
