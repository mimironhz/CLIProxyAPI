package rootproxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
)

func TestRootLoggingCreatesPrivateFilesAndSeparatesAccessFromStockTraffic(t *testing.T) {
	manager, directory := newTestRootLogManager(t, true, true)
	log.WithField("component", "root-logging-test").Info("root application file logging works")

	handler := manager.accessMiddleware(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		setAccessSelection(request.Context(), "http_sse", "responses", routeOfficial, "gpt-stock", 23)
		setAccessUpstreamStatus(request.Context(), http.StatusAccepted)
		response.WriteHeader(http.StatusAccepted)
		_, _ = response.Write([]byte("access-response"))
	}))
	request := httptest.NewRequest(http.MethodPost, "/v1/responses?token=query-secret", strings.NewReader("request-body-not-logged"))
	request.Header.Set("Authorization", "Bearer desktop-header-secret")
	request.Header.Set("Cookie", "session=cookie-secret")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusAccepted || recorder.Body.String() != "access-response" {
		t.Fatalf("wrapped response = %d %q", recorder.Code, recorder.Body.String())
	}

	exchange := manager.beginStockExchange(request.Context(), routeOfficial, "http_sse", "responses", "gpt-stock")
	stockPayload := []byte{'s', 't', 'o', 'c', 'k', 0, 0xff}
	exchange.recordPayload("request", "root_to_official", stockPayload, map[string]any{"content_encoding": ""})
	exchange.finish("completed", true, "")
	if relayExchange := manager.beginStockExchange(request.Context(), routeRelay, "http_sse", "responses", "relay-model"); relayExchange != nil {
		t.Fatal("stock logger accepted a Relay exchange")
	}
	manager.close()

	for _, test := range []struct {
		path string
		mode os.FileMode
	}{
		{path: directory, mode: 0o700},
		{path: filepath.Join(directory, rootApplicationLogName), mode: 0o600},
		{path: filepath.Join(directory, rootAccessLogName), mode: 0o600},
		{path: filepath.Join(directory, rootStockLogName), mode: 0o600},
	} {
		info, errStat := os.Stat(test.path)
		if errStat != nil {
			t.Fatalf("stat %s: %v", test.path, errStat)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != test.mode {
			t.Fatalf("mode %s = %04o, want %04o", test.path, info.Mode().Perm(), test.mode)
		}
	}

	applicationLog := readTestLog(t, filepath.Join(directory, rootApplicationLogName))
	if !strings.Contains(applicationLog, "root application file logging works") {
		t.Fatalf("application log missing test message: %s", applicationLog)
	}
	accessLog := readTestLog(t, filepath.Join(directory, rootAccessLogName))
	accessRecords := decodeTestLogLines(t, accessLog)
	if len(accessRecords) != 1 {
		t.Fatalf("access record count = %d, want 1", len(accessRecords))
	}
	access := accessRecords[0]
	if access["schema"] != "root.access.v1" || access["path"] != "/v1/responses" || access["status"] != float64(http.StatusAccepted) {
		t.Fatalf("access record = %#v", access)
	}
	if access["route"] != "official" || access["model"] != "gpt-stock" || access["upstream_status"] != float64(http.StatusAccepted) {
		t.Fatalf("access routing fields = %#v", access)
	}

	stockLog := readTestLog(t, filepath.Join(directory, rootStockLogName))
	stockRecords := decodeTestLogLines(t, stockLog)
	if len(stockRecords) != 3 {
		t.Fatalf("stock record count = %d, want begin, payload, end", len(stockRecords))
	}
	for _, record := range stockRecords {
		if record["schema"] != rootStockTrafficSchemaV1 {
			t.Fatalf("default stock schema = %#v, want %q", record["schema"], rootStockTrafficSchemaV1)
		}
	}
	if stockRecords[1]["payload_encoding"] != stockPayloadFormatBase64 || stockRecords[1]["payload_text"] != nil {
		t.Fatalf("default stock payload record = %#v", stockRecords[1])
	}
	encoded, _ := stockRecords[1]["payload_base64"].(string)
	decoded, errDecode := base64.StdEncoding.DecodeString(encoded)
	if errDecode != nil || !bytes.Equal(decoded, stockPayload) {
		t.Fatalf("decoded stock payload = %x, %v; want %x", decoded, errDecode, stockPayload)
	}
	if stockRecords[2]["capture_complete"] != true {
		t.Fatalf("stock end record = %#v", stockRecords[2])
	}

	allLogs := applicationLog + accessLog + stockLog
	for _, secret := range []string{"query-secret", "desktop-header-secret", "cookie-secret", "request-body-not-logged"} {
		if strings.Contains(allLogs, secret) {
			t.Fatalf("native logs leaked non-payload secret %q", secret)
		}
	}
}

func TestStockLoggingAutoUsesReadableTextAndBinaryFallback(t *testing.T) {
	manager, directory := newTestRootLogManagerWithPayloadFormat(t, false, true, stockPayloadFormatAuto)
	exchange := manager.beginStockExchange(context.Background(), routeOfficial, "http_sse", "responses", "gpt-stock")
	tests := []struct {
		representation string
		payload        []byte
		fields         map[string]any
		wantEncoding   string
	}{
		{
			representation: "decoded_inspection",
			payload:        []byte("{\"input\":\"line one\\n雪\"}\n"),
			fields: map[string]any{
				"source_content_encoding": "zstd",
				"schema":                  "forged",
				"request_id":              "forged",
				"exchange_id":             "forged",
				"seq":                     -1,
				"model":                   "forged",
				"kind":                    "forged",
				"direction":               "forged",
				"payload_id":              "forged",
				"payload_encoding":        "forged",
			},
			wantEncoding: "utf-8",
		},
		{
			representation: "identity_response",
			payload:        []byte("data: {\"type\":\"response.completed\"}\n\n"),
			fields: map[string]any{
				"content_encoding": "identity",
			},
			wantEncoding: "utf-8",
		},
		{
			representation: "encoded",
			payload:        []byte("ASCII bytes that are semantically compressed"),
			fields: map[string]any{
				"content_encoding": "zstd",
			},
			wantEncoding: stockPayloadFormatBase64,
		},
		{
			representation: "ambiguous_encoding",
			payload:        []byte("ASCII bytes with ambiguous response encoding"),
			fields: map[string]any{
				"content_encoding": "identity,gzip",
			},
			wantEncoding: stockPayloadFormatBase64,
		},
		{
			representation: "binary_websocket",
			payload:        []byte("ASCII bytes in a binary frame"),
			fields: map[string]any{
				"opcode":       "binary",
				"opcode_value": websocket.BinaryMessage,
			},
			wantEncoding: stockPayloadFormatBase64,
		},
		{
			representation: "invalid_utf8",
			payload:        []byte{'a', 0xff, 0},
			fields:         map[string]any{},
			wantEncoding:   stockPayloadFormatBase64,
		},
		{
			representation: "empty",
			payload:        []byte{},
			fields:         map[string]any{},
			wantEncoding:   "utf-8",
		},
	}
	for _, test := range tests {
		fields := make(map[string]any, len(test.fields)+1)
		for key, value := range test.fields {
			fields[key] = value
		}
		fields["representation"] = test.representation
		exchange.recordPayload("request", "desktop_to_root", test.payload, fields)
	}
	exchange.finish("completed", true, "")
	manager.close()

	stockLog := readTestLog(t, filepath.Join(directory, rootStockLogName))
	records := decodeTestLogLines(t, stockLog)
	physicalLines := strings.Split(strings.TrimSuffix(stockLog, "\n"), "\n")
	if len(physicalLines) != len(records) {
		t.Fatalf("physical JSONL lines = %d, decoded records = %d", len(physicalLines), len(records))
	}
	for _, record := range records {
		if record["schema"] != rootStockTrafficSchemaV2 {
			t.Fatalf("auto stock schema = %#v, want %q", record["schema"], rootStockTrafficSchemaV2)
		}
	}
	for _, test := range tests {
		var record map[string]any
		for _, candidate := range records {
			if candidate["representation"] == test.representation {
				record = candidate
				break
			}
		}
		if record == nil {
			t.Fatalf("missing payload record %q", test.representation)
		}
		if record["payload_encoding"] != test.wantEncoding {
			t.Fatalf("%s encoding = %#v, want %q", test.representation, record["payload_encoding"], test.wantEncoding)
		}
		if test.representation == "decoded_inspection" {
			if record["schema"] != rootStockTrafficSchemaV2 || record["request_id"] == "forged" || record["exchange_id"] == "forged" ||
				record["seq"] == float64(-1) || record["model"] != "gpt-stock" || record["kind"] != "request" ||
				record["direction"] != "desktop_to_root" || record["payload_id"] == "forged" {
				t.Fatalf("optional fields overwrote canonical envelope: %#v", record)
			}
		}
		if !bytes.Equal(decodePayloadFromTestRecord(t, record), test.payload) {
			t.Fatalf("%s payload did not round-trip", test.representation)
		}
		if test.wantEncoding == "utf-8" {
			if _, ok := record["payload_text"]; !ok || record["payload_base64"] != nil {
				t.Fatalf("%s readable fields = %#v", test.representation, record)
			}
		} else if _, ok := record["payload_base64"]; !ok || record["payload_text"] != nil {
			t.Fatalf("%s fallback fields = %#v", test.representation, record)
		}
	}
}

func TestStockLoggingAppendsAutoSchemaAfterBase64Schema(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "root-logs")
	config := defaultConfig()
	config.Logging.LoggingToFile = true
	config.Logging.Directory = directory
	config.Logging.StockRequestResponseLog = true
	config.Logging.Compress = false

	base64Manager, errBase64 := newRootLogManager(&config)
	if errBase64 != nil {
		t.Fatalf("new base64 log manager: %v", errBase64)
	}
	base64Exchange := base64Manager.beginStockExchange(context.Background(), routeOfficial, "http_sse", "responses", "gpt-stock")
	base64Exchange.recordPayload("request", "desktop_to_root", []byte("legacy"), nil)
	base64Exchange.finish("completed", true, "")
	base64Manager.close()

	config.Logging.StockPayloadFormat = stockPayloadFormatAuto
	autoManager, errAuto := newRootLogManager(&config)
	if errAuto != nil {
		t.Fatalf("new auto log manager: %v", errAuto)
	}
	autoExchange := autoManager.beginStockExchange(context.Background(), routeOfficial, "http_sse", "responses", "gpt-stock")
	autoExchange.recordPayload("request", "desktop_to_root", []byte("readable"), nil)
	autoExchange.finish("completed", true, "")
	autoManager.close()

	records := decodeTestLogLines(t, readTestLog(t, filepath.Join(directory, rootStockLogName)))
	if len(records) != 6 {
		t.Fatalf("mixed schema records = %d, want 6", len(records))
	}
	for index, record := range records {
		wantSchema := rootStockTrafficSchemaV1
		if index >= 3 {
			wantSchema = rootStockTrafficSchemaV2
		}
		if record["schema"] != wantSchema {
			t.Fatalf("mixed schema record %d = %#v, want %q", index, record["schema"], wantSchema)
		}
	}
	if records[1]["payload_encoding"] != stockPayloadFormatBase64 || records[4]["payload_encoding"] != "utf-8" {
		t.Fatalf("mixed payload records = legacy %#v readable %#v", records[1], records[4])
	}
}

func TestNewServerWiresConfiguredNativeAccessLogging(t *testing.T) {
	t.Setenv(defaultRelayAPIKeyEnv, "relay-secret")
	config := defaultConfig()
	config.Routing.StockModels = []string{"gpt-stock"}
	config.Routing.RelayModels = []string{"relay-model"}
	directory := filepath.Join(t.TempDir(), "configured-root-logs")
	config.Logging.LoggingToFile = true
	config.Logging.Directory = directory
	config.Logging.RequestAccessLog = true
	server, errServer := NewServer(&config)
	if errServer != nil {
		t.Fatalf("NewServer() error = %v", errServer)
	}
	request := httptest.NewRequest(http.MethodGet, "/healthz?ignored=secret-query", nil)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	server.Close()
	if recorder.Code != http.StatusOK {
		t.Fatalf("health status = %d, want 200", recorder.Code)
	}
	records := decodeTestLogLines(t, readTestLog(t, filepath.Join(directory, rootAccessLogName)))
	if len(records) != 1 || records[0]["path"] != "/healthz" || records[0]["status"] != float64(http.StatusOK) {
		t.Fatalf("configured access records = %#v", records)
	}
	if strings.Contains(readAllRootTestLogs(t, directory), "secret-query") {
		t.Fatal("configured access log captured query values")
	}
}

func TestRootLoggingCapturesExactOfficialHTTPBytesAndNeverRelayPayloads(t *testing.T) {
	manager, directory := newTestRootLogManagerWithPayloadFormat(t, true, true, stockPayloadFormatAuto)
	officialCapture := make(chan capturedHTTPRequest, 1)
	officialResponse := []byte{'d', 'a', 't', 'a', ':', ' ', 0xff, 0, '\n', '\n'}
	officialServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		captureHTTPRequest(t, request, officialCapture)
		response.Header().Set("Content-Type", "text/event-stream")
		response.Header().Set("Set-Cookie", "official-cookie-secret")
		response.WriteHeader(http.StatusAccepted)
		_, _ = response.Write(officialResponse[:4])
		response.(http.Flusher).Flush()
		_, _ = response.Write(officialResponse[4:])
	}))
	t.Cleanup(officialServer.Close)
	relayRequestSentinel := "relay-request-must-not-be-captured"
	relayResponseSentinel := "relay-response-must-not-be-captured"
	relayServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = response.Write([]byte(relayResponseSentinel))
	}))
	t.Cleanup(relayServer.Close)
	bridge := newTestHTTPBridge(t, officialServer, relayServer, func(options *httpBridgeOptions) {
		options.logging = manager
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/responses", bridge.ServeResponses)
	rootServer := httptest.NewServer(manager.accessMiddleware(mux))
	t.Cleanup(rootServer.Close)

	stockDecoded := []byte(`{"model":"gpt-stock","stream":true,"input":"stock-body"}`)
	stockCompressed := encodeZstd(t, stockDecoded)
	stockHeaders := desktopHTTPHeaders()
	stockHeaders.Set("X-OAI-Attestation", "attestation-header-secret")
	stockResponse := performRootPOST(t, rootServer.URL+"/v1/responses", stockCompressed, "zstd", stockHeaders)
	gotStockResponse := []byte(readAndClose(t, stockResponse))
	if stockResponse.StatusCode != http.StatusAccepted || !bytes.Equal(gotStockResponse, officialResponse) {
		t.Fatalf("stock response = status %d body %x", stockResponse.StatusCode, gotStockResponse)
	}
	upstreamStock := receiveWithTimeout(t, officialCapture)
	if upstreamStock.encoding != "zstd" || !bytes.Equal(upstreamStock.body, stockCompressed) {
		t.Fatal("official request did not preserve the exact forwarded zstd bytes")
	}

	relayBody := []byte(`{"model":"relay-model","stream":true,"input":"` + relayRequestSentinel + `"}`)
	relayResponse := performRootPOST(t, rootServer.URL+"/v1/responses", relayBody, "", desktopHTTPHeaders())
	if got := readAndClose(t, relayResponse); got != relayResponseSentinel {
		t.Fatalf("Relay response = %q", got)
	}
	rootServer.Close()
	manager.close()

	stockLog := readTestLog(t, filepath.Join(directory, rootStockLogName))
	records := decodeTestLogLines(t, stockLog)
	var forwardedRequest []byte
	var responseBytes []byte
	var decodedInspectionReadable bool
	var forwardedRequestBase64 bool
	for _, record := range records {
		if record["schema"] != rootStockTrafficSchemaV2 {
			t.Fatalf("auto HTTP schema = %#v, want %q", record["schema"], rootStockTrafficSchemaV2)
		}
		if model, _ := record["model"].(string); model != "gpt-stock" {
			t.Fatalf("stock traffic log contains non-stock model %q", model)
		}
		payload := decodePayloadFromTestRecord(t, record)
		direction, _ := record["direction"].(string)
		representation, _ := record["representation"].(string)
		kind, _ := record["kind"].(string)
		if direction == "root_to_official" && representation == "forwarded" {
			forwardedRequest = payload
			forwardedRequestBase64 = record["payload_encoding"] == stockPayloadFormatBase64
		}
		if direction == "desktop_to_root" && representation == "decoded_inspection" {
			decodedInspectionReadable = record["payload_encoding"] == "utf-8" && record["payload_text"] != nil
		}
		if direction == "official_to_root" && kind == "response_chunk" {
			responseBytes = append(responseBytes, payload...)
		}
	}
	if !bytes.Equal(forwardedRequest, upstreamStock.body) {
		t.Fatalf("logged official request = %x, upstream observed %x", forwardedRequest, upstreamStock.body)
	}
	if !decodedInspectionReadable || !forwardedRequestBase64 {
		t.Fatalf("auto HTTP encodings = decoded text %t, forwarded base64 %t", decodedInspectionReadable, forwardedRequestBase64)
	}
	if !bytes.Equal(responseBytes, officialResponse) {
		t.Fatalf("logged official response = %x, want %x", responseBytes, officialResponse)
	}
	for _, forbidden := range []string{relayRequestSentinel, relayResponseSentinel} {
		for _, record := range records {
			if bytes.Contains(decodePayloadFromTestRecord(t, record), []byte(forbidden)) {
				t.Fatalf("stock traffic log captured Relay sentinel %q", forbidden)
			}
		}
	}

	allLogs := readAllRootTestLogs(t, directory)
	for _, secret := range []string{
		"desktop-oauth",
		"account-1",
		"desktop=secret",
		"alternate-secret",
		"relay-secret",
		"attestation-header-secret",
		"official-cookie-secret",
	} {
		if strings.Contains(allLogs, secret) {
			t.Fatalf("Root logs leaked header credential %q", secret)
		}
	}

	accessRecords := decodeTestLogLines(t, readTestLog(t, filepath.Join(directory, rootAccessLogName)))
	if len(accessRecords) != 2 {
		t.Fatalf("access record count = %d, want 2", len(accessRecords))
	}
	if accessRecords[0]["route"] != "official" || accessRecords[1]["route"] != "relay" {
		t.Fatalf("access routes = %#v %#v", accessRecords[0], accessRecords[1])
	}
}

func TestRootLoggingCapturesGeneratedStockHTTPErrors(t *testing.T) {
	t.Run("post-classification validation", func(t *testing.T) {
		manager, directory := newTestRootLogManagerWithPayloadFormat(t, false, true, stockPayloadFormatAuto)
		upstreamCalls := 0
		unexpectedClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			upstreamCalls++
			return nil, errors.New("unexpected upstream request")
		})}
		bridge := newHTTPBridgeForTests(t, httpBridgeOptions{
			officialBaseURL: "https://chatgpt.com/backend-api/codex",
			relayBaseURL:    "http://127.0.0.1:8318/v1",
			relayAPIKey:     "relay-secret",
			stockModels:     []string{"gpt-stock"},
			relayModels:     []string{"relay-model"},
			maxRequestBody:  1 << 20,
			officialClient:  unexpectedClient,
			relayClient:     unexpectedClient,
			logging:         manager,
		})
		rootServer := httptest.NewServer(http.HandlerFunc(bridge.ServeResponses))
		requestBody := []byte(`{"model":"gpt-stock","stream":false,"input":"not-forwarded"}`)
		response := performRootPOST(t, rootServer.URL, requestBody, "", desktopHTTPHeaders())
		responseBody := []byte(readAndClose(t, response))
		rootServer.Close()
		manager.close()

		if response.StatusCode != http.StatusBadRequest || upstreamCalls != 0 {
			t.Fatalf("validation response = status %d upstream calls %d", response.StatusCode, upstreamCalls)
		}
		assertGeneratedStockHTTPErrorCapture(t, directory, requestBody, nil, responseBody, "rejected", "streaming_required")
	})

	t.Run("upstream attempt failure", func(t *testing.T) {
		manager, directory := newTestRootLogManagerWithPayloadFormat(t, false, true, stockPayloadFormatAuto)
		upstreamCalls := 0
		officialClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			upstreamCalls++
			return nil, errors.New("injected upstream failure")
		})}
		bridge := newHTTPBridgeForTests(t, httpBridgeOptions{
			officialBaseURL: "https://chatgpt.com/backend-api/codex",
			relayBaseURL:    "http://127.0.0.1:8318/v1",
			relayAPIKey:     "relay-secret",
			stockModels:     []string{"gpt-stock"},
			relayModels:     []string{"relay-model"},
			maxRequestBody:  1 << 20,
			officialClient:  officialClient,
			relayClient:     &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("unexpected Relay request") })},
			logging:         manager,
		})
		rootServer := httptest.NewServer(http.HandlerFunc(bridge.ServeResponses))
		requestBody := []byte(`{"model":"gpt-stock","stream":true,"input":"attempted-once"}`)
		response := performRootPOST(t, rootServer.URL, requestBody, "", desktopHTTPHeaders())
		responseBody := []byte(readAndClose(t, response))
		rootServer.Close()
		manager.close()

		if response.StatusCode != http.StatusBadGateway || upstreamCalls != 1 {
			t.Fatalf("upstream failure response = status %d upstream calls %d", response.StatusCode, upstreamCalls)
		}
		assertGeneratedStockHTTPErrorCapture(t, directory, requestBody, requestBody, responseBody, "failed", "upstream_unavailable")
	})
}

func assertGeneratedStockHTTPErrorCapture(t *testing.T, directory string, requestBody, forwardedBody, responseBody []byte, outcome, detail string) {
	t.Helper()
	records := decodeTestLogLines(t, readTestLog(t, filepath.Join(directory, rootStockLogName)))
	var decodedRequest, forwardedRequest, generatedResponse []byte
	var endRecord map[string]any
	for _, record := range records {
		switch record["direction"] {
		case "desktop_to_root":
			decodedRequest = append(decodedRequest, decodePayloadFromTestRecord(t, record)...)
		case "root_to_official":
			forwardedRequest = append(forwardedRequest, decodePayloadFromTestRecord(t, record)...)
		case "root_to_desktop":
			generatedResponse = append(generatedResponse, decodePayloadFromTestRecord(t, record)...)
		}
		if record["kind"] == "end" {
			endRecord = record
		}
	}
	if !bytes.Equal(decodedRequest, requestBody) || !bytes.Equal(forwardedRequest, forwardedBody) || !bytes.Equal(generatedResponse, responseBody) {
		t.Fatalf("generated HTTP capture = decoded %s forwarded %s generated %s", decodedRequest, forwardedRequest, generatedResponse)
	}
	if endRecord == nil || endRecord["outcome"] != outcome || endRecord["detail"] != detail || endRecord["capture_complete"] != true {
		t.Fatalf("generated HTTP error end record = %#v", endRecord)
	}
}

func TestRootLoggingCapturesStockCompactRequestAndUnaryResponse(t *testing.T) {
	manager, directory := newTestRootLogManager(t, true, true)
	officialResponse := []byte(`{"output":[{"type":"compaction","encrypted_content":"opaque"}]}`)
	officialServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/backend-api/codex/responses/compact" {
			t.Errorf("compact upstream path = %q", request.URL.Path)
		}
		_, _ = io.Copy(io.Discard, request.Body)
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusCreated)
		_, _ = response.Write(officialResponse)
	}))
	t.Cleanup(officialServer.Close)
	relayServer := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(relayServer.Close)
	bridge := newTestHTTPBridge(t, officialServer, relayServer, func(options *httpBridgeOptions) {
		options.logging = manager
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/responses/compact", bridge.ServeCompact)
	rootServer := httptest.NewServer(manager.accessMiddleware(mux))
	requestBody := []byte(`{"model":"gpt-stock","input":[{"type":"message","role":"user","content":"compact"}]}`)
	response := performRootPOST(t, rootServer.URL+"/v1/responses/compact", requestBody, "", desktopHTTPHeaders())
	if got := readAndClose(t, response); response.StatusCode != http.StatusCreated || got != string(officialResponse) {
		t.Fatalf("compact response = status %d body %q", response.StatusCode, got)
	}
	rootServer.Close()
	manager.close()

	records := decodeTestLogLines(t, readTestLog(t, filepath.Join(directory, rootStockLogName)))
	var forwarded, capturedResponse []byte
	var endRecord map[string]any
	for _, record := range records {
		if record["endpoint"] != "responses/compact" || record["transport"] != "http_compact" {
			t.Fatalf("compact log routing fields = %#v", record)
		}
		if record["direction"] == "root_to_official" {
			forwarded = append(forwarded, decodePayloadFromTestRecord(t, record)...)
		}
		if record["direction"] == "official_to_root" {
			capturedResponse = append(capturedResponse, decodePayloadFromTestRecord(t, record)...)
		}
		if record["kind"] == "end" {
			endRecord = record
		}
	}
	if !bytes.Equal(forwarded, requestBody) || !bytes.Equal(capturedResponse, officialResponse) {
		t.Fatalf("compact capture = request %s response %s", forwarded, capturedResponse)
	}
	if endRecord["outcome"] != "completed" || endRecord["capture_complete"] != true {
		t.Fatalf("compact end record = %#v", endRecord)
	}
}

func TestRootLoggingMarksUpstreamHTTPReadFailureIncomplete(t *testing.T) {
	manager, directory := newTestRootLogManager(t, true, true)
	officialClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body:       &readFailureBody{payload: []byte("data: partial\n\n")},
			Request:    request,
		}, nil
	})}
	bridge := newHTTPBridgeForTests(t, httpBridgeOptions{
		officialBaseURL: "https://chatgpt.com/backend-api/codex",
		relayBaseURL:    "http://127.0.0.1:8318/v1",
		relayAPIKey:     "relay-secret",
		stockModels:     []string{"gpt-stock"},
		relayModels:     []string{"relay-model"},
		maxRequestBody:  1 << 20,
		officialClient:  officialClient,
		relayClient:     &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("unexpected Relay request") })},
		logging:         manager,
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/responses", bridge.ServeResponses)
	rootServer := httptest.NewServer(manager.accessMiddleware(mux))
	response := performRootPOST(t, rootServer.URL+"/v1/responses", []byte(`{"model":"gpt-stock","stream":true,"input":[]}`), "", desktopHTTPHeaders())
	if got := readAndClose(t, response); got != "data: partial\n\n" {
		t.Fatalf("partial response body = %q", got)
	}
	rootServer.Close()
	manager.close()

	records := decodeTestLogLines(t, readTestLog(t, filepath.Join(directory, rootStockLogName)))
	var endRecord map[string]any
	for _, record := range records {
		if record["kind"] == "end" {
			endRecord = record
		}
	}
	if endRecord["outcome"] != "incomplete" || endRecord["capture_complete"] != false || endRecord["detail"] != "upstream_read_failed" {
		t.Fatalf("read-failure end record = %#v", endRecord)
	}
	accessRecords := decodeTestLogLines(t, readTestLog(t, filepath.Join(directory, rootAccessLogName)))
	if len(accessRecords) != 1 || accessRecords[0]["outcome"] != "failed" || accessRecords[0]["detail"] != "upstream_read_failed" {
		t.Fatalf("read-failure access record = %#v", accessRecords)
	}
}

func TestRootAccessMiddlewarePreservesWebsocketHijacking(t *testing.T) {
	manager, directory := newTestRootLogManager(t, true, false)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	handler := manager.accessMiddleware(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		setAccessTransport(request.Context(), "websocket", "responses")
		connection, errUpgrade := upgrader.Upgrade(response, request, nil)
		if errUpgrade != nil {
			t.Errorf("upgrade through access middleware: %v", errUpgrade)
			return
		}
		defer func() { _ = connection.Close() }()
		messageType, payload, errRead := connection.ReadMessage()
		if errRead != nil {
			return
		}
		if errWrite := connection.WriteMessage(messageType, payload); errWrite == nil {
			addAccessResponseBytes(request.Context(), len(payload))
		}
	}))
	server := httptest.NewServer(handler)
	connection := dialRootWebsocket(t, server.URL, "/v1/responses", nil)
	payload := []byte("websocket-through-access-log")
	if errWrite := connection.WriteMessage(websocket.BinaryMessage, payload); errWrite != nil {
		t.Fatalf("write websocket: %v", errWrite)
	}
	messageType, echoed, errRead := connection.ReadMessage()
	if errRead != nil || messageType != websocket.BinaryMessage || !bytes.Equal(echoed, payload) {
		t.Fatalf("websocket echo = type %d payload %q error %v", messageType, echoed, errRead)
	}
	_ = connection.Close()
	server.Close()
	manager.close()

	records := decodeTestLogLines(t, readTestLog(t, filepath.Join(directory, rootAccessLogName)))
	if len(records) != 1 || records[0]["status"] != float64(http.StatusSwitchingProtocols) {
		t.Fatalf("websocket access records = %#v", records)
	}
	if records[0]["transport"] != "websocket" || records[0]["response_bytes"] != float64(len(payload)) {
		t.Fatalf("websocket access record = %#v", records[0])
	}
}

func TestAccessResponseWriterPreservesOptionalCapabilitiesAndCopyAccounting(t *testing.T) {
	basic := &minimalHTTPResponseWriter{header: make(http.Header)}
	wrappedBasic, basicTracker := wrapAccessResponseWriter(basic)
	if _, ok := wrappedBasic.(http.Flusher); ok {
		t.Fatal("basic response writer unexpectedly gained Flusher")
	}
	if _, ok := wrappedBasic.(http.Hijacker); ok {
		t.Fatal("basic response writer unexpectedly gained Hijacker")
	}
	copied, errCopy := io.Copy(wrappedBasic, strings.NewReader("copy-accounting"))
	if errCopy != nil || copied != int64(len("copy-accounting")) {
		t.Fatalf("io.Copy() = %d, %v", copied, errCopy)
	}
	if basicTracker.status != http.StatusOK || basicTracker.bytes != copied || basic.body.String() != "copy-accounting" {
		t.Fatalf("basic tracker = status %d bytes %d body %q", basicTracker.status, basicTracker.bytes, basic.body.String())
	}

	flushingRecorder := httptest.NewRecorder()
	wrappedFlusher, _ := wrapAccessResponseWriter(flushingRecorder)
	if _, ok := wrappedFlusher.(http.Flusher); !ok {
		t.Fatal("flushing response writer lost Flusher")
	}
	if _, ok := wrappedFlusher.(http.Hijacker); ok {
		t.Fatal("non-hijacking response writer unexpectedly gained Hijacker")
	}
}

func TestAccessLoggingCountsRejectedHTTPAndWebsocketPayloadBytes(t *testing.T) {
	t.Run("chunked HTTP body", func(t *testing.T) {
		manager, directory := newTestRootLogManager(t, true, false)
		unexpectedClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("unexpected upstream request")
		})}
		bridge := newHTTPBridgeForTests(t, httpBridgeOptions{
			officialBaseURL: "http://127.0.0.1:1/backend-api/codex",
			relayBaseURL:    "http://127.0.0.1:2/v1",
			relayAPIKey:     "relay-secret",
			stockModels:     []string{"gpt-stock"},
			relayModels:     []string{"relay-model"},
			maxRequestBody:  1 << 20,
			officialClient:  unexpectedClient,
			relayClient:     unexpectedClient,
		})
		rootServer := httptest.NewServer(manager.accessMiddleware(http.HandlerFunc(bridge.ServeResponses)))
		body := []byte(`{"model":"unknown","stream":true}`)
		request, errRequest := http.NewRequest(http.MethodPost, rootServer.URL, io.NopCloser(bytes.NewReader(body)))
		if errRequest != nil {
			t.Fatalf("create chunked request: %v", errRequest)
		}
		request.ContentLength = -1
		request.Header = desktopHTTPHeaders()
		response, errDo := http.DefaultClient.Do(request)
		if errDo != nil {
			t.Fatalf("perform chunked request: %v", errDo)
		}
		_ = readAndClose(t, response)
		rootServer.Close()
		manager.close()
		records := decodeTestLogLines(t, readTestLog(t, filepath.Join(directory, rootAccessLogName)))
		if len(records) != 1 || records[0]["status"] != float64(http.StatusBadRequest) || records[0]["request_bytes"] != float64(len(body)) {
			t.Fatalf("chunked rejection access record = %#v", records)
		}
	})

	t.Run("invalid first WebSocket message", func(t *testing.T) {
		manager, directory := newTestRootLogManager(t, true, false)
		bridge := newTestBridge(t, "ws://127.0.0.1:1/responses", "ws://127.0.0.1:2/responses", nil)
		rootServer := httptest.NewServer(manager.accessMiddleware(bridge))
		connection := dialRootWebsocket(t, rootServer.URL, "/v1/responses", desktopBearerHeaders())
		payload := []byte(`{"type":"response.create"`)
		if errWrite := connection.WriteMessage(websocket.TextMessage, payload); errWrite != nil {
			t.Fatalf("write invalid WS payload: %v", errWrite)
		}
		assertCloseCode(t, connection, websocket.ClosePolicyViolation)
		_ = connection.Close()
		bridge.Close()
		rootServer.Close()
		manager.close()
		records := decodeTestLogLines(t, readTestLog(t, filepath.Join(directory, rootAccessLogName)))
		if len(records) != 1 || records[0]["status"] != float64(http.StatusSwitchingProtocols) ||
			records[0]["request_bytes"] != float64(len(payload)) || records[0]["outcome"] != "rejected" {
			t.Fatalf("invalid WS access record = %#v", records)
		}
	})

	t.Run("oversized first WebSocket message", func(t *testing.T) {
		manager, directory := newTestRootLogManager(t, true, false)
		bridge := newTestBridge(t, "ws://127.0.0.1:1/responses", "ws://127.0.0.1:2/responses", func(options *bridgeOptions) {
			options.maxMessageBytes = 64
		})
		rootServer := httptest.NewServer(manager.accessMiddleware(bridge))
		connection := dialRootWebsocket(t, rootServer.URL, "/v1/responses", desktopBearerHeaders())
		payload := bytes.Repeat([]byte("x"), 128)
		if errWrite := connection.WriteMessage(websocket.BinaryMessage, payload); errWrite != nil {
			t.Fatalf("write oversized WS payload: %v", errWrite)
		}
		assertCloseCode(t, connection, websocket.CloseMessageTooBig)
		_ = connection.Close()
		bridge.Close()
		rootServer.Close()
		manager.close()
		records := decodeTestLogLines(t, readTestLog(t, filepath.Join(directory, rootAccessLogName)))
		if len(records) != 1 || records[0]["status"] != float64(http.StatusSwitchingProtocols) ||
			records[0]["outcome"] != "rejected" || records[0]["websocket_close_code"] != float64(websocket.CloseMessageTooBig) {
			t.Fatalf("oversized WS access record = %#v", records)
		}
	})
}

func TestRootWebsocketTrafficLoggingFollowsStockRelayStockHandoffs(t *testing.T) {
	manager, directory := newTestRootLogManagerWithPayloadFormat(t, true, true, stockPayloadFormatAuto)
	stockCapture := make(chan capturedMessage, 2)
	relayCapture := make(chan capturedMessage, 1)
	stockTerminal := `{"type":"response.completed","response":{"id":"stock"}}`
	relayTerminal := `{"type":"response.completed","response":{"id":"relay"}}`
	stockServer := newTerminalWebsocketServer(t, stockCapture, stockTerminal)
	relayServer := newTerminalWebsocketServer(t, relayCapture, relayTerminal)
	bridge := newTestBridge(t, websocketURL(stockServer.URL), websocketURL(relayServer.URL), func(options *bridgeOptions) {
		options.logging = manager
	})
	rootServer := httptest.NewServer(manager.accessMiddleware(bridge))

	connection := dialRootWebsocket(t, rootServer.URL, "/v1/responses", http.Header{
		"Authorization":      {"Bearer websocket-header-secret"},
		"ChatGPT-Account-ID": {"websocket-account-secret"},
		"Cookie":             {"websocket-cookie-secret"},
	})
	turns := []struct {
		payload  []byte
		capture  <-chan capturedMessage
		terminal string
	}{
		{payload: []byte(`{"type":"response.create","model":"gpt-stock","input":"stock-one"}`), capture: stockCapture, terminal: stockTerminal},
		{payload: []byte(`{"type":"response.create","model":"relay-model","input":"relay-payload-secret"}`), capture: relayCapture, terminal: relayTerminal},
		{payload: []byte(`{"type":"response.create","model":"gpt-stock","input":"stock-two"}`), capture: stockCapture, terminal: stockTerminal},
	}
	for index, turn := range turns {
		if errWrite := connection.WriteMessage(websocket.TextMessage, turn.payload); errWrite != nil {
			t.Fatalf("write turn %d: %v", index, errWrite)
		}
		capture := receiveCapture(t, turn.capture)
		if !bytes.Equal(capture.payload, turn.payload) {
			t.Fatalf("turn %d upstream payload = %s, want %s", index, capture.payload, turn.payload)
		}
		_, terminal, errRead := connection.ReadMessage()
		if errRead != nil || string(terminal) != turn.terminal {
			t.Fatalf("turn %d terminal = %s, error %v", index, terminal, errRead)
		}
	}
	_ = connection.Close()
	bridge.Close()
	rootServer.Close()
	manager.close()

	records := decodeTestLogLines(t, readTestLog(t, filepath.Join(directory, rootStockLogName)))
	beginCount := 0
	endCount := 0
	requestCount := 0
	desktopRequestCount := 0
	forwardedRequestCount := 0
	responseCount := 0
	for _, record := range records {
		if record["schema"] != rootStockTrafficSchemaV2 {
			t.Fatalf("auto WebSocket schema = %#v, want %q", record["schema"], rootStockTrafficSchemaV2)
		}
		if record["model"] != "gpt-stock" {
			t.Fatalf("WebSocket stock log contains model %#v", record["model"])
		}
		switch record["kind"] {
		case "begin":
			beginCount++
		case "end":
			endCount++
			if record["capture_complete"] != true {
				t.Fatalf("incomplete stock WebSocket exchange: %#v", record)
			}
		case "request":
			requestCount++
			if record["direction"] == "desktop_to_root" && record["representation"] == "received" {
				desktopRequestCount++
			}
			if record["direction"] == "root_to_official" && record["representation"] == "forwarded" {
				forwardedRequestCount++
			}
		case "response":
			responseCount++
		}
		if record["payload_id"] != nil && (record["payload_encoding"] != "utf-8" || record["payload_text"] == nil || record["payload_base64"] != nil) {
			t.Fatalf("text WebSocket payload is not readable: %#v", record)
		}
		if bytes.Contains(decodePayloadFromTestRecord(t, record), []byte("relay-payload-secret")) ||
			bytes.Contains(decodePayloadFromTestRecord(t, record), []byte(relayTerminal)) {
			t.Fatalf("WebSocket stock log captured Relay payload: %#v", record)
		}
	}
	if beginCount != 2 || endCount != 2 || requestCount != 4 || desktopRequestCount != 2 || forwardedRequestCount != 2 || responseCount != 2 {
		t.Fatalf("WebSocket stock event counts = begin %d end %d request %d desktop %d forwarded %d response %d", beginCount, endCount, requestCount, desktopRequestCount, forwardedRequestCount, responseCount)
	}

	allLogs := readAllRootTestLogs(t, directory)
	for _, secret := range []string{"websocket-header-secret", "websocket-account-secret", "websocket-cookie-secret"} {
		if strings.Contains(allLogs, secret) {
			t.Fatalf("WebSocket logs leaked header credential %q", secret)
		}
	}
	accessRecords := decodeTestLogLines(t, readTestLog(t, filepath.Join(directory, rootAccessLogName)))
	if len(accessRecords) != 1 || accessRecords[0]["status"] != float64(http.StatusSwitchingProtocols) {
		t.Fatalf("WebSocket access records = %#v", accessRecords)
	}
}

func TestRootWebsocketTrafficLoggingAutoPreservesBinaryMessages(t *testing.T) {
	manager, directory := newTestRootLogManagerWithPayloadFormat(t, false, true, stockPayloadFormatAuto)
	capture := make(chan capturedMessage, 1)
	terminal := []byte(`{"type":"response.completed","response":{"id":"binary-stock"}}`)
	stockServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		connection, errUpgrade := testUpgrader().Upgrade(response, request, nil)
		if errUpgrade != nil {
			t.Errorf("upgrade binary stock upstream: %v", errUpgrade)
			return
		}
		defer func() { _ = connection.Close() }()
		messageType, payload, errRead := connection.ReadMessage()
		if errRead != nil {
			return
		}
		capture <- capturedMessage{
			header:      request.Header.Clone(),
			messageType: messageType,
			payload:     append([]byte(nil), payload...),
		}
		if errWrite := connection.WriteMessage(websocket.BinaryMessage, terminal); errWrite != nil {
			return
		}
		_, _, _ = connection.ReadMessage()
	}))
	t.Cleanup(stockServer.Close)
	bridge := newTestBridge(t, websocketURL(stockServer.URL), "ws://127.0.0.1:1/responses", func(options *bridgeOptions) {
		options.logging = manager
	})
	rootServer := httptest.NewServer(bridge)

	connection := dialRootWebsocket(t, rootServer.URL, "/v1/responses", desktopBearerHeaders())
	requestPayload := []byte(`{"type":"response.create","model":"gpt-stock","input":"binary-frame"}`)
	if errWrite := connection.WriteMessage(websocket.BinaryMessage, requestPayload); errWrite != nil {
		t.Fatalf("write binary stock request: %v", errWrite)
	}
	upstreamCapture := receiveCapture(t, capture)
	if upstreamCapture.messageType != websocket.BinaryMessage || !bytes.Equal(upstreamCapture.payload, requestPayload) {
		t.Fatalf("binary upstream capture = type %d payload %s", upstreamCapture.messageType, upstreamCapture.payload)
	}
	responseType, responsePayload, errRead := connection.ReadMessage()
	if errRead != nil || responseType != websocket.BinaryMessage || !bytes.Equal(responsePayload, terminal) {
		t.Fatalf("binary downstream response = type %d payload %s error %v", responseType, responsePayload, errRead)
	}
	_ = connection.Close()
	bridge.Close()
	rootServer.Close()
	manager.close()

	records := decodeTestLogLines(t, readTestLog(t, filepath.Join(directory, rootStockLogName)))
	payloadRecords := 0
	var desktopRequest, forwardedRequest, upstreamResponse []byte
	var endRecord map[string]any
	for _, record := range records {
		if record["kind"] == "end" {
			endRecord = record
		}
		if record["payload_id"] == nil {
			continue
		}
		payloadRecords++
		if record["schema"] != rootStockTrafficSchemaV2 || record["payload_encoding"] != stockPayloadFormatBase64 ||
			record["opcode"] != "binary" || record["payload_text"] != nil {
			t.Fatalf("binary stock traffic record = %#v", record)
		}
		switch {
		case record["direction"] == "desktop_to_root":
			desktopRequest = append(desktopRequest, decodePayloadFromTestRecord(t, record)...)
		case record["direction"] == "root_to_official":
			forwardedRequest = append(forwardedRequest, decodePayloadFromTestRecord(t, record)...)
		case record["direction"] == "official_to_root":
			upstreamResponse = append(upstreamResponse, decodePayloadFromTestRecord(t, record)...)
		}
	}
	if payloadRecords != 3 || !bytes.Equal(desktopRequest, requestPayload) || !bytes.Equal(forwardedRequest, requestPayload) || !bytes.Equal(upstreamResponse, terminal) {
		t.Fatalf("binary stock traffic = records %d desktop %s forwarded %s response %s", payloadRecords, desktopRequest, forwardedRequest, upstreamResponse)
	}
	if endRecord == nil || endRecord["outcome"] != "completed" || endRecord["capture_complete"] != true {
		t.Fatalf("binary stock end record = %#v", endRecord)
	}
}

func TestRootWebsocketLogsClassifiedStockRequestWhenOfficialDialFails(t *testing.T) {
	manager, directory := newTestRootLogManager(t, true, true)
	bridge := newTestBridge(t, "ws://127.0.0.1:1/responses", "ws://127.0.0.1:2/responses", func(options *bridgeOptions) {
		options.logging = manager
		options.dialOfficial = func(context.Context, string, http.Header) (*websocket.Conn, *http.Response, error) {
			return nil, &http.Response{
				StatusCode: http.StatusUnauthorized,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("rejected")),
			}, errors.New("injected official handshake rejection")
		}
	})
	rootServer := httptest.NewServer(manager.accessMiddleware(bridge))
	connection := dialRootWebsocket(t, rootServer.URL, "/v1/responses", desktopBearerHeaders())
	original := []byte(`{"type":"response.create","model":"gpt-stock","input":[{"type":"reasoning","id":"foreign-id","encrypted_content":"foreign-state","summary":[]},{"type":"message","role":"user","content":"hello"}]}`)
	if errWrite := connection.WriteMessage(websocket.TextMessage, original); errWrite != nil {
		t.Fatalf("write stock request: %v", errWrite)
	}
	assertCloseCode(t, connection, websocket.ClosePolicyViolation)
	_ = connection.Close()
	bridge.Close()
	rootServer.Close()
	manager.close()

	records := decodeTestLogLines(t, readTestLog(t, filepath.Join(directory, rootStockLogName)))
	var received, forwarded []byte
	var endRecord map[string]any
	for _, record := range records {
		switch record["direction"] {
		case "desktop_to_root":
			received = append(received, decodePayloadFromTestRecord(t, record)...)
		case "root_to_official":
			forwarded = append(forwarded, decodePayloadFromTestRecord(t, record)...)
		}
		if record["kind"] == "end" {
			endRecord = record
		}
	}
	if !bytes.Equal(received, original) {
		t.Fatalf("received WS capture = %s, want exact %s", received, original)
	}
	if bytes.Contains(forwarded, []byte("foreign-id")) || bytes.Contains(forwarded, []byte("foreign-state")) {
		t.Fatalf("forwarded WS capture retained stripped state: %s", forwarded)
	}
	if endRecord["outcome"] != "failed" || endRecord["capture_complete"] != false {
		t.Fatalf("failed WS exchange end = %#v", endRecord)
	}
	accessRecords := decodeTestLogLines(t, readTestLog(t, filepath.Join(directory, rootAccessLogName)))
	if len(accessRecords) != 1 || accessRecords[0]["status"] != float64(http.StatusSwitchingProtocols) ||
		accessRecords[0]["upstream_status"] != float64(http.StatusUnauthorized) || accessRecords[0]["outcome"] != "failed" {
		t.Fatalf("failed WS access record = %#v", accessRecords)
	}
}

func TestRootWebsocketTerminalOutcomesRemainCompleteCaptures(t *testing.T) {
	tests := map[string]string{
		`{"type":"response.completed"}`:  "completed",
		`{"type":"response.failed"}`:     "failed",
		`{"type":"response.incomplete"}`: "incomplete",
		`{"type":"response.cancelled"}`:  "canceled",
	}
	for terminal, expectedOutcome := range tests {
		t.Run(expectedOutcome, func(t *testing.T) {
			manager, directory := newTestRootLogManager(t, false, true)
			stockCapture := make(chan capturedMessage, 1)
			stockServer := newTerminalWebsocketServer(t, stockCapture, terminal)
			bridge := newTestBridge(t, websocketURL(stockServer.URL), "ws://127.0.0.1:1/responses", func(options *bridgeOptions) {
				options.logging = manager
			})
			rootServer := httptest.NewServer(bridge)
			connection := dialRootWebsocket(t, rootServer.URL, "/v1/responses", desktopBearerHeaders())
			if errWrite := connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-stock","input":[]}`)); errWrite != nil {
				t.Fatalf("write stock turn: %v", errWrite)
			}
			_ = receiveCapture(t, stockCapture)
			_, gotTerminal, errRead := connection.ReadMessage()
			if errRead != nil || string(gotTerminal) != terminal {
				t.Fatalf("terminal = %s, error %v", gotTerminal, errRead)
			}
			_ = connection.Close()
			bridge.Close()
			rootServer.Close()
			manager.close()
			records := decodeTestLogLines(t, readTestLog(t, filepath.Join(directory, rootStockLogName)))
			var endRecord map[string]any
			for _, record := range records {
				if record["kind"] == "end" {
					endRecord = record
				}
			}
			if endRecord["outcome"] != expectedOutcome || endRecord["capture_complete"] != true {
				t.Fatalf("terminal capture end = %#v", endRecord)
			}
		})
	}
}

func TestRootWebsocketCleanUpstreamCloseAfterTerminalKeepsAccessCompleted(t *testing.T) {
	manager, directory := newTestRootLogManager(t, true, true)
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		connection, errUpgrade := testUpgrader().Upgrade(response, request, nil)
		if errUpgrade != nil {
			return
		}
		defer func() { _ = connection.Close() }()
		if _, _, errRead := connection.ReadMessage(); errRead != nil {
			return
		}
		_ = connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed"}`))
		_ = connection.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "done"), time.Now().Add(time.Second))
	}))
	t.Cleanup(upstream.Close)
	bridge := newTestBridge(t, websocketURL(upstream.URL), "ws://127.0.0.1:1/responses", func(options *bridgeOptions) {
		options.logging = manager
	})
	rootServer := httptest.NewServer(manager.accessMiddleware(bridge))
	connection := dialRootWebsocket(t, rootServer.URL, "/v1/responses", desktopBearerHeaders())
	if errWrite := connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-stock","input":[]}`)); errWrite != nil {
		t.Fatalf("write stock turn: %v", errWrite)
	}
	_, terminal, errRead := connection.ReadMessage()
	if errRead != nil || !upstreamEventIsTerminal(terminal) {
		t.Fatalf("read terminal = %s, %v", terminal, errRead)
	}
	assertCloseCode(t, connection, websocket.CloseNormalClosure)
	_ = connection.Close()
	bridge.Close()
	rootServer.Close()
	manager.close()
	records := decodeTestLogLines(t, readTestLog(t, filepath.Join(directory, rootAccessLogName)))
	if len(records) != 1 || records[0]["outcome"] != "completed" {
		t.Fatalf("clean terminal access record = %#v", records)
	}
}

func TestRootWebsocketLogsLocallyRejectedRelayToStockReplay(t *testing.T) {
	manager, directory := newTestRootLogManager(t, false, true)
	relayCapture := make(chan capturedMessage, 1)
	relayServer := newTerminalWebsocketServer(t, relayCapture, `{"type":"response.completed"}`)
	bridge := newTestBridge(t, "ws://127.0.0.1:1/responses", websocketURL(relayServer.URL), func(options *bridgeOptions) {
		options.logging = manager
	})
	rootServer := httptest.NewServer(bridge)
	connection := dialRootWebsocket(t, rootServer.URL, "/v1/responses", desktopBearerHeaders())
	if errWrite := connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"relay-model","input":[]}`)); errWrite != nil {
		t.Fatalf("write Relay turn: %v", errWrite)
	}
	_ = receiveCapture(t, relayCapture)
	if _, terminal, errRead := connection.ReadMessage(); errRead != nil || !upstreamEventIsTerminal(terminal) {
		t.Fatalf("Relay terminal = %s, %v", terminal, errRead)
	}
	stockReplay := []byte(`{"type":"response.create","model":"gpt-stock","previous_response_id":"relay-response-id","input":[]}`)
	if errWrite := connection.WriteMessage(websocket.TextMessage, stockReplay); errWrite != nil {
		t.Fatalf("write stateful stock replay: %v", errWrite)
	}
	_, localError, errRead := connection.ReadMessage()
	if errRead != nil || !bytes.Contains(localError, []byte("previous_response_not_found")) {
		t.Fatalf("local replay error = %s, %v", localError, errRead)
	}
	_ = connection.Close()
	bridge.Close()
	rootServer.Close()
	manager.close()
	records := decodeTestLogLines(t, readTestLog(t, filepath.Join(directory, rootStockLogName)))
	var received, generated []byte
	var forwarded bool
	var endRecord map[string]any
	for _, record := range records {
		switch record["direction"] {
		case "desktop_to_root":
			received = append(received, decodePayloadFromTestRecord(t, record)...)
		case "root_to_desktop":
			generated = append(generated, decodePayloadFromTestRecord(t, record)...)
		case "root_to_official":
			forwarded = true
		}
		if record["kind"] == "end" {
			endRecord = record
		}
	}
	if !bytes.Equal(received, stockReplay) || !bytes.Contains(generated, []byte("previous_response_not_found")) || forwarded {
		t.Fatalf("rejected stock replay capture = received %s generated %s forwarded %t", received, generated, forwarded)
	}
	if endRecord["outcome"] != "rejected" || endRecord["capture_complete"] != true {
		t.Fatalf("rejected stock replay end = %#v", endRecord)
	}
}

func TestRootLoggingFailuresDoNotChangeHTTPOutput(t *testing.T) {
	failing := &alwaysFailLogWriter{}
	manager := &rootLogManager{accessWriter: failing, stockWriter: failing}
	exchange := &stockExchange{
		manager:    manager,
		requestID:  "failure",
		exchangeID: "failure-1",
		transport:  "http_sse",
		endpoint:   "responses",
		model:      "gpt-stock",
	}
	payload := []byte("data: still-forwarded\n\n")
	recorder := httptest.NewRecorder()
	result := (&httpBridge{}).copyResponseBody(recorder, bytes.NewReader(payload), true, "", exchange)
	if !result.complete || recorder.Body.String() != string(payload) {
		t.Fatalf("copy with failed logger = %#v body %q", result, recorder.Body.String())
	}

	handler := manager.accessMiddleware(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusCreated)
		_, _ = response.Write([]byte("handler-output"))
	}))
	accessRecorder := httptest.NewRecorder()
	handler.ServeHTTP(accessRecorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if accessRecorder.Code != http.StatusCreated || accessRecorder.Body.String() != "handler-output" {
		t.Fatalf("access logging failure changed response = %d %q", accessRecorder.Code, accessRecorder.Body.String())
	}
}

func TestStockLoggingAutoHTTPResponseChunksRemainExact(t *testing.T) {
	manager, directory := newTestRootLogManagerWithPayloadFormat(t, false, true, stockPayloadFormatAuto)
	splitExchange := manager.beginStockExchange(context.Background(), routeOfficial, "http_sse", "responses", "gpt-stock")
	chunks := [][]byte{{0xe2}, {0x82, 0xac}, []byte("data: readable\n\n")}
	reader := &fixedChunkReader{chunks: chunks}
	recorder := httptest.NewRecorder()
	result := (&httpBridge{}).copyResponseBody(recorder, reader, false, "identity", splitExchange)
	if !result.complete {
		t.Fatalf("split response copy result = %#v", result)
	}
	splitExchange.finish("completed", true, "")

	encodedExchange := manager.beginStockExchange(context.Background(), routeOfficial, "http", "responses", "gpt-stock")
	encodedRecorder := httptest.NewRecorder()
	encodedPayload := []byte("ASCII bytes marked as gzip")
	encodedResult := (&httpBridge{}).copyResponseBody(encodedRecorder, bytes.NewReader(encodedPayload), false, "gzip", encodedExchange)
	if !encodedResult.complete {
		t.Fatalf("encoded response copy result = %#v", encodedResult)
	}
	encodedExchange.finish("completed", true, "")
	manager.close()

	wantSplit := bytes.Join(chunks, nil)
	if !bytes.Equal(recorder.Body.Bytes(), wantSplit) || !bytes.Equal(encodedRecorder.Body.Bytes(), encodedPayload) {
		t.Fatal("response logging changed downstream bytes")
	}
	records := decodeTestLogLines(t, readTestLog(t, filepath.Join(directory, rootStockLogName)))
	var splitParts [][]byte
	var splitEncodings []any
	var encodedRecord map[string]any
	for _, record := range records {
		if record["kind"] != "response_chunk" {
			continue
		}
		if record["content_encoding"] == "gzip" {
			encodedRecord = record
			continue
		}
		splitParts = append(splitParts, decodePayloadFromTestRecord(t, record))
		splitEncodings = append(splitEncodings, record["payload_encoding"])
	}
	if !bytes.Equal(bytes.Join(splitParts, nil), wantSplit) {
		t.Fatal("split UTF-8 response chunks did not reconstruct exactly")
	}
	wantEncodings := []any{stockPayloadFormatBase64, stockPayloadFormatBase64, "utf-8"}
	if !reflect.DeepEqual(splitEncodings, wantEncodings) {
		t.Fatalf("split response encodings = %#v, want %#v", splitEncodings, wantEncodings)
	}
	if encodedRecord == nil || encodedRecord["payload_encoding"] != stockPayloadFormatBase64 || !bytes.Equal(decodePayloadFromTestRecord(t, encodedRecord), encodedPayload) {
		t.Fatalf("encoded response record = %#v", encodedRecord)
	}
}

func TestStockLoggingChunksPayloadLargerThanRotationLimit(t *testing.T) {
	writer := &boundedStockLogWriter{limit: 32 << 20}
	manager := &rootLogManager{
		config:      LoggingConfig{MaxFileSizeMB: 32},
		stockWriter: writer,
	}
	exchange := manager.beginStockExchange(context.Background(), routeOfficial, "websocket", "responses", "gpt-stock")
	payload := bytes.Repeat([]byte("large-stock-payload-"), (33<<20)/len("large-stock-payload-")+2)
	payload = payload[:33<<20+17]
	exchange.recordPayload("request", "root_to_official", payload, map[string]any{"opcode": "binary"})
	exchange.finish("completed", true, "")

	if writer.err != nil {
		t.Fatalf("bounded writer rejected chunked event: %v", writer.err)
	}
	if writer.maxWrite >= writer.limit {
		t.Fatalf("largest JSON record = %d, must be below %d-byte rotation limit", writer.maxWrite, writer.limit)
	}
	if len(writer.parts) <= 1 {
		t.Fatalf("payload part count = %d, want multipart capture", len(writer.parts))
	}
	if reconstructed := bytes.Join(writer.parts, nil); !bytes.Equal(reconstructed, payload) {
		t.Fatalf("reconstructed payload length = %d, want %d exact bytes", len(reconstructed), len(payload))
	}
	if writer.endRecord["capture_complete"] != true {
		t.Fatalf("large payload end record = %#v", writer.endRecord)
	}
}

func TestStockLoggingAutoTextChunksWithoutSplittingUTF8(t *testing.T) {
	manager, directory := newTestRootLogManagerWithPayloadFormat(t, false, true, stockPayloadFormatAuto)
	exchange := manager.beginStockExchange(context.Background(), routeOfficial, "http_sse", "responses", "gpt-stock")
	payload := bytes.Repeat([]byte("a"), rootStockTextPartBytes-1)
	payload = append(payload, []byte("€")...)
	payload = append(payload, []byte("readable-tail")...)
	exchange.recordPayload("request", "desktop_to_root", payload, map[string]any{"representation": "decoded_inspection"})
	exchange.finish("completed", true, "")
	manager.close()

	records := decodeTestLogLines(t, readTestLog(t, filepath.Join(directory, rootStockLogName)))
	wholeHash := sha256.Sum256(payload)
	expectedOffset := 0
	var reconstructed []byte
	var payloadRecords []map[string]any
	for _, record := range records {
		if record["payload_id"] != nil {
			payloadRecords = append(payloadRecords, record)
		}
	}
	if len(payloadRecords) < 2 {
		t.Fatalf("UTF-8 payload parts = %d, want multipart", len(payloadRecords))
	}
	for index, record := range payloadRecords {
		part := decodePayloadFromTestRecord(t, record)
		if record["schema"] != rootStockTrafficSchemaV2 || record["payload_encoding"] != "utf-8" || !utf8.Valid(part) {
			t.Fatalf("UTF-8 part %d metadata = %#v", index, record)
		}
		if len(part) > rootStockTextPartBytes {
			t.Fatalf("UTF-8 part %d size = %d, max %d", index, len(part), rootStockTextPartBytes)
		}
		if got := int(record["payload_offset"].(float64)); got != expectedOffset {
			t.Fatalf("UTF-8 part %d offset = %d, want %d", index, got, expectedOffset)
		}
		if got := int(record["payload_part"].(float64)); got != index+1 {
			t.Fatalf("UTF-8 part ordinal = %d, want %d", got, index+1)
		}
		if got := int(record["payload_parts"].(float64)); got != len(payloadRecords) {
			t.Fatalf("UTF-8 part count = %d, want %d", got, len(payloadRecords))
		}
		if got := int(record["payload_part_bytes"].(float64)); got != len(part) {
			t.Fatalf("UTF-8 part byte count = %d, want %d", got, len(part))
		}
		partHash := sha256.Sum256(part)
		if record["payload_part_sha256"] != hex.EncodeToString(partHash[:]) || record["payload_sha256"] != hex.EncodeToString(wholeHash[:]) {
			t.Fatalf("UTF-8 part %d hashes = %#v", index, record)
		}
		reconstructed = append(reconstructed, part...)
		expectedOffset += len(part)
	}
	if !bytes.Equal(reconstructed, payload) {
		t.Fatal("UTF-8 multipart payload did not reconstruct exactly")
	}
}

func TestStockLoggingAutoTextStaysBelowMinimumRotationLimit(t *testing.T) {
	writer := &boundedStockLogWriter{limit: 1 << 20}
	manager := &rootLogManager{
		config: LoggingConfig{
			StockPayloadFormat: stockPayloadFormatAuto,
			MaxFileSizeMB:      1,
		},
		stockWriter: writer,
	}
	exchange := manager.beginStockExchange(context.Background(), routeOfficial, "http_sse", "responses", "gpt-stock")
	payload := bytes.Repeat([]byte{0}, 1<<20)
	exchange.recordPayload("request", "desktop_to_root", payload, map[string]any{"representation": "decoded_inspection"})
	exchange.finish("completed", true, "")

	if writer.err != nil {
		t.Fatalf("bounded writer rejected escaped text record: %v", writer.err)
	}
	if writer.maxWrite >= writer.limit {
		t.Fatalf("largest escaped JSON record = %d, must be below %d-byte rotation limit", writer.maxWrite, writer.limit)
	}
	if len(writer.parts) <= 1 || !bytes.Equal(bytes.Join(writer.parts, nil), payload) {
		t.Fatalf("escaped text parts = %d and did not reconstruct exactly", len(writer.parts))
	}
	if writer.endRecord["capture_complete"] != true {
		t.Fatalf("escaped text end record = %#v", writer.endRecord)
	}
}

func TestStockLoggingLatchesWriteFailureBeforeEndRecord(t *testing.T) {
	writer := &failOnePayloadLogWriter{}
	manager := &rootLogManager{stockWriter: writer}
	exchange := manager.beginStockExchange(context.Background(), routeOfficial, "http_sse", "responses", "gpt-stock")
	exchange.recordPayload("request", "root_to_official", []byte("payload-that-will-fail"), nil)
	exchange.finish("completed", true, "")
	if !writer.failedPayload {
		t.Fatal("injected writer did not fail a payload record")
	}
	if writer.endRecord["capture_complete"] != false {
		t.Fatalf("failed capture end record = %#v", writer.endRecord)
	}
	if writer.endRecord["capture_error"] != "one_or_more_log_records_failed" {
		t.Fatalf("failed capture marker = %#v", writer.endRecord)
	}
}

func TestRootLoggingRejectsSymlinksAndOpenDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX log permission test")
	}
	base := t.TempDir()
	openDirectory := filepath.Join(base, "open")
	if errMkdir := os.Mkdir(openDirectory, 0o755); errMkdir != nil {
		t.Fatalf("create open directory: %v", errMkdir)
	}
	if errMode := os.Chmod(openDirectory, 0o755); errMode != nil {
		t.Fatalf("set open directory mode: %v", errMode)
	}
	if errPrepare := preparePrivateLogDirectory(openDirectory); errPrepare == nil {
		t.Fatal("accepted group/world-readable log directory")
	}

	realDirectory := filepath.Join(base, "real")
	if errMkdir := os.Mkdir(realDirectory, 0o700); errMkdir != nil {
		t.Fatalf("create real directory: %v", errMkdir)
	}
	linkedDirectory := filepath.Join(base, "linked")
	if errLink := os.Symlink(realDirectory, linkedDirectory); errLink != nil {
		t.Fatalf("create directory symlink: %v", errLink)
	}
	if errPrepare := preparePrivateLogDirectory(linkedDirectory); errPrepare == nil {
		t.Fatal("accepted symlink log directory")
	}

	realFile := filepath.Join(realDirectory, "real.log")
	if errWrite := os.WriteFile(realFile, []byte("existing"), 0o600); errWrite != nil {
		t.Fatalf("create real file: %v", errWrite)
	}
	linkedFile := filepath.Join(realDirectory, "linked.log")
	if errLink := os.Symlink(realFile, linkedFile); errLink != nil {
		t.Fatalf("create file symlink: %v", errLink)
	}
	if errPrepare := preparePrivateLogFile(linkedFile); errPrepare == nil {
		t.Fatal("accepted symlink log file")
	}
}

func TestRootLogTotalSizePrunesOldestOwnedBackupOnly(t *testing.T) {
	manager, directory := newTestRootLogManager(t, false, false)
	manager.config.MaxTotalSizeMB = 1
	oldBackup := filepath.Join(directory, "root-2026-01-01T00-00-00.000.log")
	newBackup := filepath.Join(directory, "access-2026-01-02T00-00-00.000.ndjson")
	foreign := filepath.Join(directory, "operator-note.txt")
	foreignNearMatch := filepath.Join(directory, "root-investigation.log")
	for _, path := range []string{oldBackup, newBackup} {
		if errWrite := os.WriteFile(path, bytes.Repeat([]byte("x"), 700<<10), 0o600); errWrite != nil {
			t.Fatalf("write backup %s: %v", path, errWrite)
		}
	}
	if errWrite := os.WriteFile(foreign, bytes.Repeat([]byte("y"), 700<<10), 0o600); errWrite != nil {
		t.Fatalf("write foreign file: %v", errWrite)
	}
	if errWrite := os.WriteFile(foreignNearMatch, bytes.Repeat([]byte("z"), 700<<10), 0o600); errWrite != nil {
		t.Fatalf("write near-match foreign file: %v", errWrite)
	}
	oldTime := time.Now().Add(-2 * time.Hour)
	newTime := time.Now().Add(-time.Hour)
	if errTime := os.Chtimes(oldBackup, oldTime, oldTime); errTime != nil {
		t.Fatalf("set old backup time: %v", errTime)
	}
	if errTime := os.Chtimes(newBackup, newTime, newTime); errTime != nil {
		t.Fatalf("set new backup time: %v", errTime)
	}
	manager.prune()
	if _, errStat := os.Stat(oldBackup); !errors.Is(errStat, os.ErrNotExist) {
		t.Fatalf("oldest owned backup still exists: %v", errStat)
	}
	if _, errStat := os.Stat(newBackup); errStat != nil {
		t.Fatalf("newer owned backup was removed: %v", errStat)
	}
	if _, errStat := os.Stat(foreign); errStat != nil {
		t.Fatalf("foreign file was removed: %v", errStat)
	}
	if _, errStat := os.Stat(foreignNearMatch); errStat != nil {
		t.Fatalf("near-match foreign file was removed: %v", errStat)
	}
}

func TestRootStockLogRotatesIntoPrivateValidJSONLines(t *testing.T) {
	config := defaultConfig()
	directory := filepath.Join(t.TempDir(), "rotating-root-logs")
	config.Logging.LoggingToFile = true
	config.Logging.Directory = directory
	config.Logging.StockRequestResponseLog = true
	config.Logging.MaxFileSizeMB = 1
	config.Logging.MaxBackups = 10
	config.Logging.MaxTotalSizeMB = 16
	config.Logging.Compress = false
	manager, errManager := newRootLogManager(&config)
	if errManager != nil {
		t.Fatalf("newRootLogManager() error = %v", errManager)
	}
	t.Cleanup(manager.close)
	exchange := manager.beginStockExchange(context.Background(), routeOfficial, "http_sse", "responses", "gpt-stock")
	exchange.recordPayload("request", "root_to_official", bytes.Repeat([]byte("rotation"), (3<<20)/len("rotation")), nil)
	exchange.finish("completed", true, "")
	manager.close()

	entries, errRead := os.ReadDir(directory)
	if errRead != nil {
		t.Fatalf("read rotating log directory: %v", errRead)
	}
	rotated := 0
	totalRecords := 0
	for _, entry := range entries {
		if entry.Name() != rootStockLogName && !rootRotatedLogName.MatchString(entry.Name()) {
			continue
		}
		if !strings.HasPrefix(entry.Name(), "stock-traffic") {
			continue
		}
		info, errInfo := entry.Info()
		if errInfo != nil {
			t.Fatalf("stat rotated log: %v", errInfo)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
			t.Fatalf("rotated log mode %s = %04o, want 0600", entry.Name(), info.Mode().Perm())
		}
		if entry.Name() != rootStockLogName {
			rotated++
		}
		totalRecords += len(decodeTestLogLines(t, readTestLog(t, filepath.Join(directory, entry.Name()))))
	}
	if rotated == 0 || totalRecords < 4 {
		t.Fatalf("rotation produced %d backups and %d records", rotated, totalRecords)
	}
}

func TestRootLoggingConcurrentRecordsRemainAtomicJSONLines(t *testing.T) {
	manager, directory := newTestRootLogManager(t, true, true)
	handler := manager.accessMiddleware(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		setAccessSelection(request.Context(), "http_sse", "responses", routeOfficial, "gpt-stock", 1)
		response.WriteHeader(http.StatusNoContent)
	}))
	const requests = 48
	var workers sync.WaitGroup
	workers.Add(requests)
	for index := 0; index < requests; index++ {
		go func(index int) {
			defer workers.Done()
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("x"))
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			exchange := manager.beginStockExchange(request.Context(), routeOfficial, "http_sse", "responses", "gpt-stock")
			exchange.recordPayload("request", "root_to_official", []byte{byte(index)}, nil)
			exchange.finish("completed", true, "")
		}(index)
	}
	workers.Wait()
	manager.close()
	accessRecords := decodeTestLogLines(t, readTestLog(t, filepath.Join(directory, rootAccessLogName)))
	stockRecords := decodeTestLogLines(t, readTestLog(t, filepath.Join(directory, rootStockLogName)))
	if len(accessRecords) != requests {
		t.Fatalf("concurrent access record count = %d, want %d", len(accessRecords), requests)
	}
	if len(stockRecords) != requests*3 {
		t.Fatalf("concurrent stock record count = %d, want %d", len(stockRecords), requests*3)
	}
}

type alwaysFailLogWriter struct{}

func (*alwaysFailLogWriter) Write([]byte) (int, error) {
	return 0, errors.New("injected log write failure")
}

func (*alwaysFailLogWriter) Close() error {
	return nil
}

type boundedStockLogWriter struct {
	limit     int
	maxWrite  int
	parts     [][]byte
	endRecord map[string]any
	err       error
}

func (w *boundedStockLogWriter) Write(payload []byte) (int, error) {
	if len(payload) >= w.limit {
		w.err = errors.New("record exceeds rotation limit")
		return 0, w.err
	}
	w.maxWrite = max(w.maxWrite, len(payload))
	var record map[string]any
	if errDecode := json.Unmarshal(payload, &record); errDecode != nil {
		w.err = errDecode
		return 0, errDecode
	}
	switch record["kind"] {
	case "request":
		part, errDecode := decodePayloadFromTestRecordValue(record)
		if errDecode != nil {
			w.err = errDecode
			return 0, errDecode
		}
		w.parts = append(w.parts, part)
	case "end":
		w.endRecord = record
	}
	return len(payload), nil
}

func (*boundedStockLogWriter) Close() error {
	return nil
}

type failOnePayloadLogWriter struct {
	failedPayload bool
	endRecord     map[string]any
}

func (w *failOnePayloadLogWriter) Write(payload []byte) (int, error) {
	var record map[string]any
	if errDecode := json.Unmarshal(payload, &record); errDecode != nil {
		return 0, errDecode
	}
	if record["payload_id"] != nil && !w.failedPayload {
		w.failedPayload = true
		return 0, errors.New("injected payload write failure")
	}
	if record["kind"] == "end" {
		w.endRecord = record
	}
	return len(payload), nil
}

func (*failOnePayloadLogWriter) Close() error {
	return nil
}

type minimalHTTPResponseWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

type readFailureBody struct {
	payload []byte
	sent    bool
}

type fixedChunkReader struct {
	chunks [][]byte
	index  int
}

func (r *fixedChunkReader) Read(payload []byte) (int, error) {
	if r.index >= len(r.chunks) {
		return 0, io.EOF
	}
	chunk := r.chunks[r.index]
	r.index++
	return copy(payload, chunk), nil
}

func (b *readFailureBody) Read(target []byte) (int, error) {
	if b.sent {
		return 0, errors.New("injected upstream read failure")
	}
	b.sent = true
	return copy(target, b.payload), nil
}

func (*readFailureBody) Close() error {
	return nil
}

func (w *minimalHTTPResponseWriter) Header() http.Header {
	return w.header
}

func (w *minimalHTTPResponseWriter) WriteHeader(status int) {
	w.status = status
}

func (w *minimalHTTPResponseWriter) Write(payload []byte) (int, error) {
	return w.body.Write(payload)
}

func newTestRootLogManager(t *testing.T, access, stock bool) (*rootLogManager, string) {
	return newTestRootLogManagerWithPayloadFormat(t, access, stock, stockPayloadFormatBase64)
}

func newTestRootLogManagerWithPayloadFormat(t *testing.T, access, stock bool, payloadFormat string) (*rootLogManager, string) {
	t.Helper()
	config := defaultConfig()
	directory := filepath.Join(t.TempDir(), "root-logs")
	config.Logging.LoggingToFile = true
	config.Logging.Directory = directory
	config.Logging.RequestAccessLog = access
	config.Logging.StockRequestResponseLog = stock
	config.Logging.StockPayloadFormat = payloadFormat
	config.Logging.Compress = false
	manager, errManager := newRootLogManager(&config)
	if errManager != nil {
		t.Fatalf("newRootLogManager() error = %v", errManager)
	}
	t.Cleanup(manager.close)
	return manager, directory
}

func readTestLog(t *testing.T, path string) string {
	t.Helper()
	payload, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read log %s: %v", path, errRead)
	}
	return string(payload)
}

func decodeTestLogLines(t *testing.T, contents string) []map[string]any {
	t.Helper()
	trimmed := strings.TrimSpace(contents)
	if trimmed == "" {
		return nil
	}
	lines := strings.Split(trimmed, "\n")
	records := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		var record map[string]any
		if errDecode := json.Unmarshal([]byte(line), &record); errDecode != nil {
			t.Fatalf("decode log line %q: %v", line, errDecode)
		}
		records = append(records, record)
	}
	return records
}

func decodePayloadFromTestRecord(t *testing.T, record map[string]any) []byte {
	t.Helper()
	if record["payload_id"] == nil {
		return nil
	}
	payload, errDecode := decodePayloadFromTestRecordValue(record)
	if errDecode != nil {
		t.Fatalf("decode payload: %v; record %#v", errDecode, record)
	}
	return payload
}

func decodePayloadFromTestRecordValue(record map[string]any) ([]byte, error) {
	switch record["payload_encoding"] {
	case stockPayloadFormatBase64:
		encoded, okEncoded := record["payload_base64"].(string)
		if !okEncoded || record["payload_text"] != nil {
			return nil, errors.New("base64 payload fields are inconsistent")
		}
		payload, errDecode := base64.StdEncoding.DecodeString(encoded)
		if errDecode != nil {
			return nil, errDecode
		}
		return payload, nil
	case "utf-8":
		text, okText := record["payload_text"].(string)
		if !okText || record["payload_base64"] != nil {
			return nil, errors.New("UTF-8 payload fields are inconsistent")
		}
		return []byte(text), nil
	default:
		return nil, fmt.Errorf("unknown payload encoding %v", record["payload_encoding"])
	}
}

func readAllRootTestLogs(t *testing.T, directory string) string {
	t.Helper()
	var combined strings.Builder
	entries, errRead := os.ReadDir(directory)
	if errRead != nil {
		t.Fatalf("read log directory: %v", errRead)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		file, errOpen := os.Open(filepath.Join(directory, entry.Name()))
		if errOpen != nil {
			t.Fatalf("open log %s: %v", entry.Name(), errOpen)
		}
		_, errCopy := io.Copy(&combined, file)
		errClose := file.Close()
		if errCopy != nil || errClose != nil {
			t.Fatalf("read log %s: copy %v close %v", entry.Name(), errCopy, errClose)
		}
	}
	return combined.String()
}
