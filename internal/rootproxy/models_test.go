package rootproxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestModelsHandlerBuildsExactCutoverCatalog(t *testing.T) {
	handler := newTestModelsHandler(t, []string{
		"gpt-5.6-sol",
		"gpt-5.6-terra",
		"gpt-5.6-luna",
	}, []string{
		"grok-4.5",
		"kimi-k3",
	}, map[string]string{
		"grok-4.5": "xai",
		"kimi-k3":  "kimi",
	}, nil)

	response := serveModels(t, handler, http.MethodGet, "/v1/models?client_version=0.146.0", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if etag := response.Header().Get("ETag"); !strings.HasPrefix(etag, `"`) || !strings.HasSuffix(etag, `"`) {
		t.Fatalf("ETag = %q, want quoted strong tag", etag)
	}

	var payload rootModelsPayload
	if errDecode := json.Unmarshal(response.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode model catalog: %v", errDecode)
	}
	wantSlugs := []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "grok-4.5", "kimi-k3"}
	gotSlugs := make([]string, 0, len(payload.Models))
	for index, model := range payload.Models {
		slug, _ := model["slug"].(string)
		gotSlugs = append(gotSlugs, slug)
		if priority, ok := model["priority"].(float64); !ok || int(priority) != index+1 {
			t.Errorf("%s priority = %#v, want %d", slug, model["priority"], index+1)
		}
		if prefer, ok := model["prefer_websockets"].(bool); !ok || prefer {
			t.Errorf("%s prefer_websockets = %#v, want false", slug, model["prefer_websockets"])
		}
		for _, required := range []string{
			"slug",
			"display_name",
			"supported_reasoning_levels",
			"shell_type",
			"visibility",
			"supported_in_api",
			"priority",
			"base_instructions",
			"support_verbosity",
			"truncation_policy",
			"supports_parallel_tool_calls",
			"experimental_supported_tools",
		} {
			if _, exists := model[required]; !exists {
				t.Errorf("%s is missing required Codex field %q", slug, required)
			}
		}
	}
	if !reflect.DeepEqual(gotSlugs, wantSlugs) {
		t.Fatalf("model slugs = %#v, want %#v", gotSlugs, wantSlugs)
	}

	for _, model := range payload.Models[3:] {
		slug := model["slug"].(string)
		if _, exists := model["comp_hash"]; exists {
			t.Errorf("%s inherited a GPT comp_hash", slug)
		}
		if tiers, ok := model["service_tiers"].([]any); !ok || len(tiers) != 0 {
			t.Errorf("%s service_tiers = %#v, want empty", slug, model["service_tiers"])
		}
		if tiers, ok := model["additional_speed_tiers"].([]any); !ok || len(tiers) != 0 {
			t.Errorf("%s additional_speed_tiers = %#v, want empty", slug, model["additional_speed_tiers"])
		}
		if supportsSearch, ok := model["supports_search_tool"].(bool); !ok || !supportsSearch {
			t.Errorf("%s supports_search_tool = %#v, want true", slug, model["supports_search_tool"])
		}
	}
	if _, exists := payload.Models[0]["comp_hash"]; !exists {
		t.Error("stock model lost its curated comp_hash")
	}
	if strings.Contains(response.Body.String(), "relay-secret") {
		t.Fatal("model catalog leaked Relay credentials")
	}
}

func TestModelsHandlerUsesStableConditionalETag(t *testing.T) {
	first := newTestModelsHandler(t, []string{"gpt-5.6-sol"}, []string{"grok-4.5", "kimi-k3"}, nil, nil)
	second := newTestModelsHandler(t, []string{"gpt-5.6-sol"}, []string{"grok-4.5", "kimi-k3"}, nil, nil)
	changed := newTestModelsHandler(t, []string{"gpt-5.6-sol"}, []string{"kimi-k3", "grok-4.5"}, nil, nil)
	if first.etag != second.etag {
		t.Fatalf("equal catalogs produced different ETags: %q and %q", first.etag, second.etag)
	}
	if first.etag == changed.etag {
		t.Fatalf("different catalog order produced the same ETag %q", first.etag)
	}

	response := serveModels(t, first, http.MethodGet, "/v1/models?client_version=0.146.0", http.Header{
		"If-None-Match": {first.etag},
	})
	if response.Code != http.StatusNotModified {
		t.Fatalf("conditional status = %d, want 304; body=%s", response.Code, response.Body.String())
	}
	if response.Body.Len() != 0 {
		t.Fatalf("conditional body = %q, want empty", response.Body.String())
	}
	if got := response.Header().Get("ETag"); got != first.etag {
		t.Fatalf("conditional ETag = %q, want %q", got, first.etag)
	}
}

func TestModelsHandlerAdvertisesConfiguredTransportMode(t *testing.T) {
	for _, test := range []struct {
		name             string
		mode             string
		preferWebsockets bool
	}{
		{name: "cutover HTTP fallback", mode: websocketModeHTTPFallback, preferWebsockets: false},
		{name: "experimental first message", mode: websocketModeFirstMessage, preferWebsockets: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := defaultConfig()
			config.Websocket.Mode = test.mode
			config.Routing.StockModels = []string{"gpt-5.6-sol"}
			config.Routing.RelayModels = []string{"grok-4.5"}
			handler, errHandler := newModelsHandler(&config)
			if errHandler != nil {
				t.Fatalf("newModelsHandler() error = %v", errHandler)
			}
			var payload rootModelsPayload
			if errDecode := json.Unmarshal(handler.body, &payload); errDecode != nil {
				t.Fatalf("decode model catalog: %v", errDecode)
			}
			for _, model := range payload.Models {
				if got, ok := model["prefer_websockets"].(bool); !ok || got != test.preferWebsockets {
					t.Errorf("%s prefer_websockets = %#v, want %t", model["slug"], model["prefer_websockets"], test.preferWebsockets)
				}
			}
		})
	}
}

func TestModelsHandlerEnforcesDesktopBoundary(t *testing.T) {
	handler := newTestModelsHandler(t, []string{"gpt-5.6-sol"}, []string{"grok-4.5"}, nil, []string{"https://desktop.example"})
	tests := []struct {
		name       string
		method     string
		target     string
		headers    http.Header
		wantStatus int
		wantAllow  string
	}{
		{name: "missing bearer", method: http.MethodGet, target: "/v1/models", headers: http.Header{"Authorization": {}}, wantStatus: http.StatusUnauthorized},
		{name: "wrong method", method: http.MethodPost, target: "/v1/models", wantStatus: http.StatusMethodNotAllowed, wantAllow: "GET, HEAD"},
		{name: "unknown query", method: http.MethodGet, target: "/v1/models?other=1", wantStatus: http.StatusBadRequest},
		{name: "duplicate version", method: http.MethodGet, target: "/v1/models?client_version=1&client_version=2", wantStatus: http.StatusBadRequest},
		{name: "denied origin", method: http.MethodGet, target: "/v1/models", headers: http.Header{"Origin": {"https://browser.example"}}, wantStatus: http.StatusForbidden},
		{name: "loop marker", method: http.MethodGet, target: "/v1/models", headers: http.Header{rootHopHeader: {"1"}}, wantStatus: http.StatusLoopDetected},
		{name: "allowed origin", method: http.MethodGet, target: "/v1/models?client_version=", headers: http.Header{"Origin": {"https://desktop.example"}}, wantStatus: http.StatusOK},
		{name: "head", method: http.MethodHead, target: "/v1/models?client_version=0.146.0", wantStatus: http.StatusOK},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := serveModels(t, handler, test.method, test.target, test.headers)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, test.wantStatus, response.Body.String())
			}
			if got := response.Header().Get("Allow"); got != test.wantAllow {
				t.Fatalf("Allow = %q, want %q", got, test.wantAllow)
			}
			if test.method == http.MethodHead && response.Body.Len() != 0 {
				t.Fatalf("HEAD body = %q, want empty", response.Body.String())
			}
		})
	}
}

func TestServerMountsBothModelsAliases(t *testing.T) {
	t.Setenv(defaultRelayAPIKeyEnv, "relay-secret")
	config := defaultConfig()
	config.Routing.StockModels = []string{"gpt-5.6-sol"}
	config.Routing.RelayModels = []string{"grok-4.5"}
	config.Routing.RelayModelProviders = map[string]string{"grok-4.5": "xai"}
	server, errServer := NewServer(&config)
	if errServer != nil {
		t.Fatalf("NewServer() error = %v", errServer)
	}
	defer server.bridge.Close()

	for _, path := range []string{"/v1/models", "/backend-api/codex/models"} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, path+"?client_version=0.146.0", nil)
		request.Header.Set("Authorization", "Bearer desktop-token")
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200; body=%s", path, response.Code, response.Body.String())
		}
		var payload rootModelsPayload
		if errDecode := json.Unmarshal(response.Body.Bytes(), &payload); errDecode != nil {
			t.Fatalf("decode %s catalog: %v", path, errDecode)
		}
		if len(payload.Models) != 2 {
			t.Fatalf("%s models = %d, want 2", path, len(payload.Models))
		}
	}
}

func newTestModelsHandler(
	t *testing.T,
	stockModels []string,
	relayModels []string,
	relayProviders map[string]string,
	allowedOrigins []string,
) *modelsHandler {
	t.Helper()
	config := defaultConfig()
	config.Routing.StockModels = append([]string(nil), stockModels...)
	config.Routing.RelayModels = append([]string(nil), relayModels...)
	config.Routing.RelayModelProviders = cloneStringMap(relayProviders)
	config.Websocket.AllowedOrigins = append([]string(nil), allowedOrigins...)
	handler, errHandler := newModelsHandler(&config)
	if errHandler != nil {
		t.Fatalf("newModelsHandler() error = %v", errHandler)
	}
	return handler
}

func serveModels(t *testing.T, handler http.Handler, method, target string, headers http.Header) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	request := httptest.NewRequest(method, target, nil)
	request.Header.Set("Authorization", "Bearer desktop-token")
	for name, values := range headers {
		request.Header.Del(name)
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	handler.ServeHTTP(response, request)
	return response
}
