package rootproxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/tidwall/gjson"
)

type capturedHTTPRequest struct {
	path     string
	header   http.Header
	body     []byte
	encoding string
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestResolveEnvironmentProxyURLFailsClosedWithoutLeakingCredentials(t *testing.T) {
	const proxyCredentials = "proxy-user:proxy-secret"
	unsupportedProxy, errParse := url.Parse("ftp://" + proxyCredentials + "@proxy.example.com:21")
	if errParse != nil {
		t.Fatalf("parse unsupported proxy URL: %v", errParse)
	}

	tests := []struct {
		name    string
		resolve func(*http.Request) (*url.URL, error)
		wantErr string
	}{
		{
			name: "unsupported scheme",
			resolve: func(*http.Request) (*url.URL, error) {
				return unsupportedProxy, nil
			},
			wantErr: "environment HTTPS proxy scheme is unsupported",
		},
		{
			name: "resolver error",
			resolve: func(*http.Request) (*url.URL, error) {
				return nil, errors.New("invalid proxy " + proxyCredentials)
			},
			wantErr: "environment proxy configuration is invalid",
		},
		{
			name: "missing host",
			resolve: func(*http.Request) (*url.URL, error) {
				return &url.URL{Scheme: "https", User: url.UserPassword("proxy-user", "proxy-secret")}, nil
			},
			wantErr: "environment HTTPS proxy URL is invalid",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolved, errResolve := resolveEnvironmentProxyURL(officialHTTPBaseURL, test.resolve)
			if errResolve == nil {
				t.Fatalf("resolveEnvironmentProxyURL() = %q, nil; want error", resolved)
			}
			if errResolve.Error() != test.wantErr {
				t.Fatalf("resolveEnvironmentProxyURL() error = %q, want %q", errResolve, test.wantErr)
			}
			for _, secret := range []string{"proxy-user", "proxy-secret", proxyCredentials} {
				if strings.Contains(errResolve.Error(), secret) {
					t.Fatalf("proxy resolution error exposes credentials: %q", errResolve)
				}
			}
		})
	}
}

func TestResolveEnvironmentProxyURLAcceptsUtlsProxySchemes(t *testing.T) {
	tests := []string{
		"http://proxy-user:proxy-secret@proxy.example.com:8080",
		"https://proxy.example.com:8443",
		"socks5://proxy.example.com:1080",
		"socks5h://proxy.example.com:1080",
	}
	for _, rawProxy := range tests {
		t.Run(strings.SplitN(rawProxy, ":", 2)[0], func(t *testing.T) {
			proxyURL, errParse := url.Parse(rawProxy)
			if errParse != nil {
				t.Fatalf("parse proxy URL: %v", errParse)
			}
			resolved, errResolve := resolveEnvironmentProxyURL(officialHTTPBaseURL, func(request *http.Request) (*url.URL, error) {
				if request.URL.String() != officialHTTPBaseURL {
					t.Fatalf("proxy target = %q, want %q", request.URL, officialHTTPBaseURL)
				}
				return proxyURL, nil
			})
			if errResolve != nil {
				t.Fatalf("resolveEnvironmentProxyURL() error = %v", errResolve)
			}
			if resolved != rawProxy {
				t.Fatalf("resolved proxy URL = %q, want %q", resolved, rawProxy)
			}
		})
	}
}

func TestHTTPBridgeRoutesSSEWithRawBodiesAndCredentialIsolation(t *testing.T) {
	officialCapture := make(chan capturedHTTPRequest, 1)
	relayCapture := make(chan capturedHTTPRequest, 1)
	officialServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		captureHTTPRequest(t, request, officialCapture)
		response.Header().Set("Content-Type", "text/event-stream")
		response.Header().Set("Cache-Control", "no-cache")
		response.Header().Set("X-Request-ID", "official-request")
		response.Header().Set("X-Reasoning-Included", "true")
		response.Header().Set("X-Models-Etag", `"models-v1"`)
		response.Header().Set("OpenAI-Model", "gpt-stock")
		response.Header().Set("Connection", "X-Upstream-Secret")
		response.Header().Set("X-Upstream-Secret", "drop-me")
		response.Header().Set("Set-Cookie", "upstream=secret")
		response.Header().Set("Access-Control-Allow-Origin", "*")
		response.Header().Set("Alt-Svc", `h3=":443"`)
		response.Header().Set("Server", "upstream")
		response.Header().Set("X-CLIProxy-Upstream", "relay-fingerprint")
		response.WriteHeader(http.StatusAccepted)
		_, _ = response.Write([]byte("data: official\n\n"))
	}))
	t.Cleanup(officialServer.Close)
	relayServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		captureHTTPRequest(t, request, relayCapture)
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = response.Write([]byte("data: relay\n\n"))
	}))
	t.Cleanup(relayServer.Close)

	bridge := newTestHTTPBridge(t, officialServer, relayServer, nil)
	rootServer := newTestHTTPRootServer(t, bridge)

	stockBody := []byte(" { \"model\" : \"gpt-stock\", \"stream\" : true, \"input\" : [] } \n")
	stockResponse := performRootPOST(t, rootServer.URL+"/v1/responses", stockBody, "", desktopHTTPHeaders())
	if stockResponse.StatusCode != http.StatusAccepted {
		t.Fatalf("stock status = %d, want %d", stockResponse.StatusCode, http.StatusAccepted)
	}
	if got := readAndClose(t, stockResponse); got != "data: official\n\n" {
		t.Fatalf("stock response body = %q", got)
	}
	assertHeader(t, stockResponse.Header, "Content-Type", "text/event-stream")
	assertHeader(t, stockResponse.Header, "Cache-Control", "no-cache")
	assertHeader(t, stockResponse.Header, "X-Request-ID", "official-request")
	assertHeader(t, stockResponse.Header, "X-Reasoning-Included", "true")
	assertHeader(t, stockResponse.Header, "X-Models-Etag", `"models-v1"`)
	assertHeader(t, stockResponse.Header, "OpenAI-Model", "gpt-stock")
	for _, name := range []string{"X-Upstream-Secret", "Set-Cookie", "Access-Control-Allow-Origin", "Alt-Svc", "Server", "X-CLIProxy-Upstream", "Content-Length"} {
		assertHeaderAbsent(t, stockResponse.Header, name)
	}
	stockRequest := receiveWithTimeout(t, officialCapture)
	if stockRequest.path != "/backend-api/codex/responses" || !bytes.Equal(stockRequest.body, stockBody) {
		t.Fatalf("stock upstream request = path %q body %q", stockRequest.path, stockRequest.body)
	}
	assertHeader(t, stockRequest.header, "Authorization", "Bearer desktop-oauth")
	assertHeader(t, stockRequest.header, "ChatGPT-Account-ID", "account-1")
	assertHeaderAbsent(t, stockRequest.header, rootHopHeader)
	assertHeaderAbsent(t, stockRequest.header, "Cookie")

	relayBody := []byte(`{"model":"relay-model","stream":true,"input":[{"type":"message","role":"user","content":"hello"}]}`)
	compressedRelayBody := encodeZstd(t, relayBody)
	relayResponse := performRootPOST(t, rootServer.URL+"/backend-api/codex/responses", compressedRelayBody, "zstd", desktopHTTPHeaders())
	if relayResponse.StatusCode != http.StatusOK {
		t.Fatalf("Relay status = %d, want 200", relayResponse.StatusCode)
	}
	if got := readAndClose(t, relayResponse); got != "data: relay\n\n" {
		t.Fatalf("Relay response body = %q", got)
	}
	relayRequest := receiveWithTimeout(t, relayCapture)
	if relayRequest.path != "/v1/responses" || !bytes.Equal(relayRequest.body, compressedRelayBody) {
		t.Fatalf("Relay upstream request = path %q body length %d", relayRequest.path, len(relayRequest.body))
	}
	if relayRequest.encoding != "zstd" {
		t.Fatalf("Relay Content-Encoding = %q, want zstd", relayRequest.encoding)
	}
	assertHeader(t, relayRequest.header, "Authorization", "Bearer relay-secret")
	assertHeader(t, relayRequest.header, rootHopHeader, "1")
	for _, name := range []string{"ChatGPT-Account-ID", "Cookie", "X-Api-Key", "Origin"} {
		assertHeaderAbsent(t, relayRequest.header, name)
	}
	select {
	case unexpected := <-officialCapture:
		t.Fatalf("unexpected second official request: %#v", unexpected)
	default:
	}
}

func TestHTTPBridgeSanitizesOfficialSSEReplayState(t *testing.T) {
	officialCapture := make(chan capturedHTTPRequest, 1)
	officialServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		captureHTTPRequest(t, request, officialCapture)
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = response.Write([]byte("data: done\n\n"))
	}))
	t.Cleanup(officialServer.Close)
	relayServer := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(relayServer.Close)
	bridge := newTestHTTPBridge(t, officialServer, relayServer, nil)
	rootServer := newTestHTTPRootServer(t, bridge)

	foreignReasoning := []byte(`{"model":"gpt-stock","stream":true,"store":false,"input":[{"type":"reasoning","id":"reasoning-relay","encrypted_content":"foreign-state","summary":[]},{"type":"message","role":"user","content":"continue"}]}`)
	response := performRootPOST(t, rootServer.URL+"/v1/responses", encodeZstd(t, foreignReasoning), "zstd", desktopHTTPHeaders())
	_ = readAndClose(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("sanitized SSE status = %d, want 200", response.StatusCode)
	}
	capture := receiveWithTimeout(t, officialCapture)
	if capture.encoding != "" {
		t.Fatalf("rewritten official Content-Encoding = %q, want absent", capture.encoding)
	}
	reasoning := gjson.GetBytes(capture.body, "input.0")
	if reasoning.Get("encrypted_content").Exists() || reasoning.Get("id").Exists() {
		t.Fatalf("foreign reasoning reached official SSE: %s", capture.body)
	}

	foreignCompaction := []byte(`{"model":"gpt-stock","stream":true,"input":[{"type":"compaction","encrypted_content":"foreign-state"}]}`)
	rejected := performRootPOST(t, rootServer.URL+"/v1/responses", foreignCompaction, "", desktopHTTPHeaders())
	_ = readAndClose(t, rejected)
	if rejected.StatusCode != http.StatusBadRequest {
		t.Fatalf("foreign SSE compaction status = %d, want 400", rejected.StatusCode)
	}
	select {
	case unexpected := <-officialCapture:
		t.Fatalf("foreign compaction reached official SSE: %s", unexpected.body)
	default:
	}
}

func TestHTTPBridgeRetainsOnlyOfficialCloudflareCookies(t *testing.T) {
	var calls atomic.Int32
	secondCookie := make(chan string, 1)
	officialClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		call := calls.Add(1)
		if call == 2 {
			secondCookie <- request.Header.Get("Cookie")
		}
		headers := http.Header{"Content-Type": {"text/event-stream"}}
		if call == 1 {
			headers["Set-Cookie"] = []string{
				"__cflb=west; Path=/; Secure; HttpOnly",
				"chatgpt_session=account-secret; Path=/; Secure; HttpOnly",
			}
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     headers,
			Body:       io.NopCloser(strings.NewReader("data: done\n\n")),
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
	})
	rootServer := newTestHTTPRootServer(t, bridge)
	body := []byte(`{"model":"gpt-stock","stream":true,"input":[]}`)
	for index := 0; index < 2; index++ {
		response := performRootPOST(t, rootServer.URL+"/v1/responses", body, "", desktopHTTPHeaders())
		_ = readAndClose(t, response)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("official cookie request %d status = %d", index+1, response.StatusCode)
		}
		assertHeaderAbsent(t, response.Header, "Set-Cookie")
	}
	if got := receiveWithTimeout(t, secondCookie); got != "__cflb=west" {
		t.Fatalf("second official Cookie = %q, want only __cflb", got)
	}
}

func TestHTTPBridgeRejectsInvalidResponsesRequestsBeforeDial(t *testing.T) {
	var dials atomic.Int32
	failingClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		dials.Add(1)
		return nil, errors.New("unexpected upstream request")
	})}
	bridge := newHTTPBridgeForTests(t, httpBridgeOptions{
		officialBaseURL: "http://127.0.0.1:1/backend-api/codex",
		relayBaseURL:    "http://127.0.0.1:2/v1",
		relayAPIKey:     "relay-secret",
		stockModels:     []string{"gpt-stock"},
		relayModels:     []string{"relay-model"},
		maxRequestBody:  128,
		allowedOrigins:  []string{"https://desktop.example"},
		officialClient:  failingClient,
		relayClient:     failingClient,
	})
	server := newTestHTTPRootServer(t, bridge)

	tests := []struct {
		name     string
		body     []byte
		encoding string
		headers  http.Header
		path     string
		status   int
	}{
		{name: "missing stream", body: []byte(`{"model":"gpt-stock"}`), headers: desktopHTTPHeaders(), path: "/v1/responses", status: 400},
		{name: "false stream", body: []byte(`{"model":"gpt-stock","stream":false}`), headers: desktopHTTPHeaders(), path: "/v1/responses", status: 400},
		{name: "null stream", body: []byte(`{"model":"gpt-stock","stream":null}`), headers: desktopHTTPHeaders(), path: "/v1/responses", status: 400},
		{name: "non boolean stream", body: []byte(`{"model":"gpt-stock","stream":"true"}`), headers: desktopHTTPHeaders(), path: "/v1/responses", status: 400},
		{name: "duplicate stream", body: []byte(`{"model":"gpt-stock","stream":true,"stream":true}`), headers: desktopHTTPHeaders(), path: "/v1/responses", status: 400},
		{name: "duplicate model", body: []byte(`{"model":"gpt-stock","model":"relay-model","stream":true}`), headers: desktopHTTPHeaders(), path: "/v1/responses", status: 400},
		{name: "trailing JSON", body: []byte(`{"model":"gpt-stock","stream":true}{}`), headers: desktopHTTPHeaders(), path: "/v1/responses", status: 400},
		{name: "unknown model", body: []byte(`{"model":"unknown","stream":true}`), headers: desktopHTTPHeaders(), path: "/v1/responses", status: 400},
		{name: "unsupported encoding", body: []byte(`{"model":"gpt-stock","stream":true}`), encoding: "gzip", headers: desktopHTTPHeaders(), path: "/v1/responses", status: 400},
		{name: "raw body too large", body: bytes.Repeat([]byte("x"), 129), headers: desktopHTTPHeaders(), path: "/v1/responses", status: 413},
		{name: "missing bearer", body: []byte(`{"model":"gpt-stock","stream":true}`), headers: http.Header{"Content-Type": {"application/json"}}, path: "/v1/responses", status: 401},
		{name: "bad origin", body: []byte(`{"model":"gpt-stock","stream":true}`), headers: func() http.Header { h := desktopHTTPHeaders(); h.Set("Origin", "https://evil.example"); return h }(), path: "/v1/responses", status: 403},
		{name: "root hop", body: []byte(`{"model":"gpt-stock","stream":true}`), headers: func() http.Header { h := desktopHTTPHeaders(); h.Set(rootHopHeader, "1"); return h }(), path: "/v1/responses", status: 508},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, errRequest := http.NewRequest(http.MethodPost, server.URL+test.path, bytes.NewReader(test.body))
			if errRequest != nil {
				t.Fatalf("create request: %v", errRequest)
			}
			request.Header = test.headers.Clone()
			if test.encoding != "" {
				request.Header.Set("Content-Encoding", test.encoding)
			}
			response, errDo := http.DefaultClient.Do(request)
			if errDo != nil {
				t.Fatalf("perform request: %v", errDo)
			}
			_ = readAndClose(t, response)
			if response.StatusCode != test.status {
				t.Fatalf("status = %d, want %d", response.StatusCode, test.status)
			}
		})
	}

	largeDecoded := []byte(`{"model":"gpt-stock","stream":true,"padding":"` + strings.Repeat("x", 256) + `"}`)
	response := performRootPOST(t, server.URL+"/v1/responses", encodeZstd(t, largeDecoded), "zstd", desktopHTTPHeaders())
	_ = readAndClose(t, response)
	if response.StatusCode < 400 {
		t.Fatalf("oversized decoded status = %d, want rejection", response.StatusCode)
	}
	if got := dials.Load(); got != 0 {
		t.Fatalf("upstream dials = %d, want 0", got)
	}

	gptCompaction := validGPTReasoningEncryptedContentForRootTest()
	unsafeRelayBody := []byte(`{"model":"relay-model","stream":true,"input":[{"type":"compaction","encrypted_content":"` + gptCompaction + `"}]}`)
	compactionBridge := newHTTPBridgeForTests(t, httpBridgeOptions{
		officialBaseURL: "http://127.0.0.1:1/backend-api/codex",
		relayBaseURL:    "http://127.0.0.1:2/v1",
		relayAPIKey:     "relay-secret",
		stockModels:     []string{"gpt-stock"},
		relayModels:     []string{"relay-model"},
		maxRequestBody:  1 << 20,
		officialClient:  failingClient,
		relayClient:     failingClient,
	})
	compactionServer := newTestHTTPRootServer(t, compactionBridge)
	unsafeRelay := performRootPOST(t, compactionServer.URL+"/v1/responses", unsafeRelayBody, "", desktopHTTPHeaders())
	_ = readAndClose(t, unsafeRelay)
	if unsafeRelay.StatusCode != http.StatusConflict {
		t.Fatalf("GPT compaction to Relay status = %d, want 409", unsafeRelay.StatusCode)
	}
	if got := dials.Load(); got != 0 {
		t.Fatalf("upstream dials after GPT compaction = %d, want 0", got)
	}
}

func TestHTTPBridgeValidatesRelayCompactionProvider(t *testing.T) {
	var relayCalls atomic.Int32
	relayServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		relayCalls.Add(1)
		if strings.HasSuffix(request.URL.Path, "/compact") {
			response.Header().Set("Content-Type", "application/json")
			_, _ = response.Write([]byte(`{"output":[]}`))
			return
		}
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = response.Write([]byte("data: done\n\n"))
	}))
	t.Cleanup(relayServer.Close)
	bridge := newHTTPBridgeForTests(t, httpBridgeOptions{
		officialBaseURL: "http://127.0.0.1:1/backend-api/codex",
		relayBaseURL:    relayServer.URL + "/v1",
		relayAPIKey:     "relay-secret",
		stockModels:     []string{"gpt-stock"},
		relayModels:     []string{"grok-a", "kimi-a", "unclassified"},
		relayProviders:  map[string]string{"grok-a": "xai", "kimi-a": "kimi"},
		maxRequestBody:  1 << 20,
		officialClient:  &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("unexpected official request") })},
		relayClient:     relayServer.Client(),
	})
	rootServer := newTestHTTPRootServer(t, bridge)
	grok := validGrokEncryptedContentForRootTest()
	kimi := validKimiCompactionForRootTest("summary")

	tests := []struct {
		name   string
		path   string
		body   string
		status int
	}{
		{name: "Grok to xai", path: "/v1/responses", body: `{"model":"grok-a","stream":true,"input":[{"type":"compaction","encrypted_content":"` + grok + `"}]}`, status: http.StatusOK},
		{name: "Kimi to kimi", path: "/v1/responses", body: `{"model":"kimi-a","stream":true,"input":[{"type":"compaction","encrypted_content":"` + kimi + `"}]}`, status: http.StatusOK},
		{name: "Kimi to xai rejected", path: "/v1/responses", body: `{"model":"grok-a","stream":true,"input":[{"type":"compaction","encrypted_content":"` + kimi + `"}]}`, status: http.StatusConflict},
		{name: "Grok to kimi rejected", path: "/v1/responses", body: `{"model":"kimi-a","stream":true,"input":[{"type":"compaction","encrypted_content":"` + grok + `"}]}`, status: http.StatusConflict},
		{name: "unclassified replay rejected", path: "/v1/responses", body: `{"model":"unclassified","stream":true,"input":[{"type":"compaction","encrypted_content":"` + kimi + `"}]}`, status: http.StatusConflict},
		{name: "unclassified trigger rejected", path: "/v1/responses", body: `{"model":"unclassified","stream":true,"input":[{"type":"compaction_trigger"}]}`, status: http.StatusConflict},
		{name: "unclassified compact request rejected", path: "/v1/responses/compact", body: `{"model":"unclassified","input":[]}`, status: http.StatusConflict},
		{name: "classified compact request accepted", path: "/v1/responses/compact", body: `{"model":"kimi-a","input":[]}`, status: http.StatusOK},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := performRootPOST(t, rootServer.URL+test.path, []byte(test.body), "", desktopHTTPHeaders())
			_ = readAndClose(t, response)
			if response.StatusCode != test.status {
				t.Fatalf("status = %d, want %d", response.StatusCode, test.status)
			}
		})
	}
	if got := relayCalls.Load(); got != 3 {
		t.Fatalf("Relay calls = %d, want 3 accepted same-provider requests", got)
	}
}

func TestHTTPBridgeCompactNormalizesOfficialAndPreservesOutput(t *testing.T) {
	var officialCalls atomic.Int32
	officialCapture := make(chan capturedHTTPRequest, 2)
	relayCapture := make(chan capturedHTTPRequest, 1)
	compactOutput := " {\n  \"output\": [{\"type\":\"compaction\",\"encrypted_content\":\"opaque\"}]\n}\n"
	officialServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		officialCalls.Add(1)
		captureHTTPRequest(t, request, officialCapture)
		response.Header().Set("Content-Type", "application/json")
		response.Header().Set("X-Request-ID", "compact-request")
		response.WriteHeader(http.StatusCreated)
		_, _ = response.Write([]byte(compactOutput))
	}))
	t.Cleanup(officialServer.Close)
	relayServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		captureHTTPRequest(t, request, relayCapture)
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"output":[]}`))
	}))
	t.Cleanup(relayServer.Close)
	bridge := newTestHTTPBridge(t, officialServer, relayServer, nil)
	rootServer := newTestHTTPRootServer(t, bridge)

	officialBody := []byte(`{"model":"gpt-stock","store":false,"stream":false,"input":[{"type":"reasoning","id":"reasoning-foreign","encrypted_content":"foreign-state","summary":[]},{"type":"message","role":"user","content":"continue"}]}`)
	response := performRootPOST(t, rootServer.URL+"/v1/responses/compact", officialBody, "", desktopHTTPHeaders())
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("official compact status = %d", response.StatusCode)
	}
	if got := readAndClose(t, response); got != compactOutput {
		t.Fatalf("compact output changed: %q", got)
	}
	assertHeader(t, response.Header, "X-Request-ID", "compact-request")
	capture := receiveWithTimeout(t, officialCapture)
	if capture.path != "/backend-api/codex/responses/compact" {
		t.Fatalf("official compact path = %q", capture.path)
	}
	if gjson.GetBytes(capture.body, "stream").Exists() {
		t.Fatalf("stream field reached official compact: %s", capture.body)
	}
	reasoning := gjson.GetBytes(capture.body, "input.0")
	if reasoning.Get("encrypted_content").Exists() || reasoning.Get("id").Exists() {
		t.Fatalf("foreign reasoning state reached official compact: %s", capture.body)
	}

	relayBody := []byte(`{"model":"relay-model","input":[]}`)
	relayCompressed := encodeZstd(t, relayBody)
	relayResponse := performRootPOST(t, rootServer.URL+"/backend-api/codex/responses/compact", relayCompressed, "zstd", desktopHTTPHeaders())
	if relayResponse.StatusCode != http.StatusOK {
		t.Fatalf("Relay compact status = %d", relayResponse.StatusCode)
	}
	_ = readAndClose(t, relayResponse)
	relayRequest := receiveWithTimeout(t, relayCapture)
	if relayRequest.encoding != "zstd" || !bytes.Equal(relayRequest.body, relayCompressed) {
		t.Fatal("unchanged Relay compact body was not forwarded in its original zstd representation")
	}

	invalidCompaction := []byte(`{"model":"gpt-stock","input":[{"type":"compaction","encrypted_content":"foreign-state"}]}`)
	rejected := performRootPOST(t, rootServer.URL+"/v1/responses/compact", invalidCompaction, "", desktopHTTPHeaders())
	_ = readAndClose(t, rejected)
	if rejected.StatusCode != http.StatusBadRequest {
		t.Fatalf("non-portable compaction status = %d, want 400", rejected.StatusCode)
	}
	if got := officialCalls.Load(); got != 1 {
		t.Fatalf("official compact calls = %d, want 1", got)
	}

	streamingCompact := performRootPOST(t, rootServer.URL+"/v1/responses/compact", []byte(`{"model":"gpt-stock","stream":true}`), "", desktopHTTPHeaders())
	_ = readAndClose(t, streamingCompact)
	if streamingCompact.StatusCode != http.StatusBadRequest {
		t.Fatalf("streaming compact status = %d, want 400", streamingCompact.StatusCode)
	}
}

func TestHTTPBridgeFlushesSSEBeforeUpstreamCompletes(t *testing.T) {
	firstWritten := make(chan struct{})
	release := make(chan struct{})
	officialServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = response.Write([]byte("data: first\n\n"))
		response.(http.Flusher).Flush()
		close(firstWritten)
		<-release
		_, _ = response.Write([]byte("data: second\n\n"))
	}))
	t.Cleanup(officialServer.Close)
	relayServer := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(relayServer.Close)
	bridge := newTestHTTPBridge(t, officialServer, relayServer, nil)
	rootServer := newTestHTTPRootServer(t, bridge)

	request, errRequest := http.NewRequest(http.MethodPost, rootServer.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-stock","stream":true}`))
	if errRequest != nil {
		t.Fatalf("create request: %v", errRequest)
	}
	request.Header = desktopHTTPHeaders()
	response, errDo := http.DefaultClient.Do(request)
	if errDo != nil {
		t.Fatalf("perform request: %v", errDo)
	}
	defer func() { _ = response.Body.Close() }()
	receiveWithTimeout(t, firstWritten)
	line, errLine := bufio.NewReader(response.Body).ReadString('\n')
	if errLine != nil {
		t.Fatalf("read first SSE line before release: %v", errLine)
	}
	if line != "data: first\n" {
		t.Fatalf("first SSE line = %q", line)
	}
	close(release)
}

func TestHTTPBridgeDoesNotFollowRedirectOrFallback(t *testing.T) {
	var relayCalls atomic.Int32
	relayServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		relayCalls.Add(1)
	}))
	t.Cleanup(relayServer.Close)
	officialServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Location", relayServer.URL+"/v1/responses")
		response.WriteHeader(http.StatusTemporaryRedirect)
	}))
	t.Cleanup(officialServer.Close)
	bridge := newTestHTTPBridge(t, officialServer, relayServer, nil)
	rootServer := newTestHTTPRootServer(t, bridge)

	request, errRequest := http.NewRequest(http.MethodPost, rootServer.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-stock","stream":true}`))
	if errRequest != nil {
		t.Fatalf("create redirect request: %v", errRequest)
	}
	request.Header = desktopHTTPHeaders()
	response, errDo := withoutRedirects(http.DefaultClient).Do(request)
	if errDo != nil {
		t.Fatalf("perform redirect request: %v", errDo)
	}
	_ = readAndClose(t, response)
	if response.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("redirect status = %d, want 307", response.StatusCode)
	}
	if got := relayCalls.Load(); got != 0 {
		t.Fatalf("Relay calls after official redirect = %d, want 0", got)
	}

	var officialAttempts atomic.Int32
	var replayableBody atomic.Bool
	failingOfficial := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		officialAttempts.Add(1)
		replayableBody.Store(request.GetBody != nil)
		return nil, errors.New("official unavailable")
	})}
	failedBridge := newHTTPBridgeForTests(t, httpBridgeOptions{
		officialBaseURL: officialServer.URL + "/backend-api/codex",
		relayBaseURL:    relayServer.URL + "/v1",
		relayAPIKey:     "relay-secret",
		stockModels:     []string{"gpt-stock"},
		relayModels:     []string{"relay-model"},
		maxRequestBody:  1 << 20,
		officialClient:  failingOfficial,
		relayClient:     relayServer.Client(),
	})
	failedServer := newTestHTTPRootServer(t, failedBridge)
	failedResponse := performRootPOST(t, failedServer.URL+"/v1/responses", []byte(`{"model":"gpt-stock","stream":true}`), "", desktopHTTPHeaders())
	_ = readAndClose(t, failedResponse)
	if failedResponse.StatusCode != http.StatusBadGateway {
		t.Fatalf("failed official status = %d, want 502", failedResponse.StatusCode)
	}
	if got := officialAttempts.Load(); got != 1 {
		t.Fatalf("official attempts = %d, want 1", got)
	}
	if replayableBody.Load() {
		t.Fatal("upstream POST exposed GetBody and could be replayed after an ambiguous write")
	}
	if got := relayCalls.Load(); got != 0 {
		t.Fatalf("Relay calls after official failure = %d, want 0", got)
	}
}

func TestHTTPBridgeForwardsOfficialAuthenticationFailureWithoutRetry(t *testing.T) {
	var officialCalls atomic.Int32
	var relayCalls atomic.Int32
	officialServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		officialCalls.Add(1)
		response.Header().Set("Content-Type", "application/json")
		response.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		response.Header().Set("X-Request-ID", "auth-request")
		response.WriteHeader(http.StatusUnauthorized)
		_, _ = response.Write([]byte(`{"error":{"type":"authentication_error","message":"expired"}}`))
	}))
	t.Cleanup(officialServer.Close)
	relayServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		relayCalls.Add(1)
	}))
	t.Cleanup(relayServer.Close)
	bridge := newTestHTTPBridge(t, officialServer, relayServer, nil)
	rootServer := newTestHTTPRootServer(t, bridge)

	response := performRootPOST(t, rootServer.URL+"/v1/responses", []byte(`{"model":"gpt-stock","stream":true}`), "", desktopHTTPHeaders())
	body := readAndClose(t, response)
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.StatusCode)
	}
	if body != `{"error":{"type":"authentication_error","message":"expired"}}` {
		t.Fatalf("body = %q", body)
	}
	assertHeader(t, response.Header, "WWW-Authenticate", `Bearer error="invalid_token"`)
	assertHeader(t, response.Header, "X-Request-ID", "auth-request")
	if got := officialCalls.Load(); got != 1 {
		t.Fatalf("official calls = %d, want 1", got)
	}
	if got := relayCalls.Load(); got != 0 {
		t.Fatalf("Relay calls = %d, want 0", got)
	}
}

func TestHTTPBridgeForcesFastServiceTierOnConfiguredStockTurnsOnly(t *testing.T) {
	officialCapture := make(chan capturedHTTPRequest, 4)
	officialServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		captureHTTPRequest(t, request, officialCapture)
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = response.Write([]byte("data: done\n\n"))
	}))
	t.Cleanup(officialServer.Close)
	relayCapture := make(chan capturedHTTPRequest, 1)
	relayServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		captureHTTPRequest(t, request, relayCapture)
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = response.Write([]byte("data: done\n\n"))
	}))
	t.Cleanup(relayServer.Close)
	bridge := newTestHTTPBridge(t, officialServer, relayServer, func(options *httpBridgeOptions) {
		options.stockModels = []string{"gpt-stock", "gpt-standard"}
		options.fastModels = map[string]struct{}{"gpt-stock": {}}
	})
	rootServer := newTestHTTPRootServer(t, bridge)

	forced := performRootPOST(t, rootServer.URL+"/v1/responses", []byte(`{"model":"gpt-stock","stream":true,"input":[]}`), "", desktopHTTPHeaders())
	_ = readAndClose(t, forced)
	if forced.StatusCode != http.StatusOK {
		t.Fatalf("forced stock status = %d, want 200", forced.StatusCode)
	}
	capture := receiveWithTimeout(t, officialCapture)
	if got := gjson.GetBytes(capture.body, "service_tier").String(); got != officialFastServiceTier {
		t.Fatalf("forced stock service_tier = %q, want %q; body=%s", got, officialFastServiceTier, capture.body)
	}

	// An unlisted stock model keeps Desktop's own per-turn choice.
	standard := performRootPOST(t, rootServer.URL+"/v1/responses", []byte(`{"model":"gpt-standard","stream":true,"input":[]}`), "", desktopHTTPHeaders())
	_ = readAndClose(t, standard)
	standardCapture := receiveWithTimeout(t, officialCapture)
	if gjson.GetBytes(standardCapture.body, "service_tier").Exists() {
		t.Fatalf("unlisted stock model was forced: %s", standardCapture.body)
	}

	// Compaction is background summarization and must not spend the tier.
	compact := performRootPOST(t, rootServer.URL+"/v1/responses/compact", []byte(`{"model":"gpt-stock","input":[]}`), "", desktopHTTPHeaders())
	_ = readAndClose(t, compact)
	compactCapture := receiveWithTimeout(t, officialCapture)
	if compactCapture.path != "/backend-api/codex/responses/compact" {
		t.Fatalf("compact path = %q", compactCapture.path)
	}
	if gjson.GetBytes(compactCapture.body, "service_tier").Exists() {
		t.Fatalf("compaction was forced onto the fast tier: %s", compactCapture.body)
	}

	// The Relay arm has no such tier, and its bytes stay untouched.
	relayBody := []byte(`{"model":"relay-model","stream":true,"input":[]}`)
	relayResponse := performRootPOST(t, rootServer.URL+"/v1/responses", relayBody, "", desktopHTTPHeaders())
	_ = readAndClose(t, relayResponse)
	relayRequest := receiveWithTimeout(t, relayCapture)
	if !bytes.Equal(relayRequest.body, relayBody) {
		t.Fatalf("Relay body was rewritten: %s", relayRequest.body)
	}
}

func TestCapturedHTTPOutcomeSeparatesResponseStatusFromCaptureCompleteness(t *testing.T) {
	tests := map[int]string{
		http.StatusOK:                  "completed",
		http.StatusTemporaryRedirect:   "completed",
		http.StatusBadRequest:          "rejected",
		http.StatusTooManyRequests:     "rejected",
		http.StatusInternalServerError: "failed",
		http.StatusServiceUnavailable:  "failed",
	}
	for status, expected := range tests {
		if got := capturedHTTPOutcome(status); got != expected {
			t.Errorf("capturedHTTPOutcome(%d) = %q, want %q", status, got, expected)
		}
	}
}

func newTestHTTPBridge(t *testing.T, officialServer, relayServer *httptest.Server, modify func(*httpBridgeOptions)) *httpBridge {
	t.Helper()
	options := httpBridgeOptions{
		officialBaseURL: officialServer.URL + "/backend-api/codex",
		relayBaseURL:    relayServer.URL + "/v1",
		relayAPIKey:     "relay-secret",
		stockModels:     []string{"gpt-stock"},
		relayModels:     []string{"relay-model"},
		relayProviders:  map[string]string{"relay-model": "kimi"},
		maxRequestBody:  1 << 20,
		officialClient:  officialServer.Client(),
		relayClient:     relayServer.Client(),
	}
	if modify != nil {
		modify(&options)
	}
	return newHTTPBridgeForTests(t, options)
}

func newHTTPBridgeForTests(t *testing.T, options httpBridgeOptions) *httpBridge {
	t.Helper()
	bridge, errBridge := newHTTPBridge(options)
	if errBridge != nil {
		t.Fatalf("newHTTPBridge() error = %v", errBridge)
	}
	return bridge
}

func newTestHTTPRootServer(t *testing.T, bridge *httpBridge) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/responses", bridge.ServeResponses)
	mux.HandleFunc("/backend-api/codex/responses", bridge.ServeResponses)
	mux.HandleFunc("/v1/responses/compact", bridge.ServeCompact)
	mux.HandleFunc("/backend-api/codex/responses/compact", bridge.ServeCompact)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func desktopHTTPHeaders() http.Header {
	return http.Header{
		"Authorization":      {"Bearer desktop-oauth"},
		"ChatGPT-Account-ID": {"account-1"},
		"Content-Type":       {"application/json"},
		"Cookie":             {"desktop=secret"},
		"X-Api-Key":          {"alternate-secret"},
		"X-Codex-Turn-State": {"turn-state"},
	}
}

func performRootPOST(t *testing.T, target string, body []byte, encoding string, headers http.Header) *http.Response {
	t.Helper()
	request, errRequest := http.NewRequestWithContext(context.Background(), http.MethodPost, target, bytes.NewReader(body))
	if errRequest != nil {
		t.Fatalf("create POST request: %v", errRequest)
	}
	request.Header = headers.Clone()
	if encoding != "" {
		request.Header.Set("Content-Encoding", encoding)
	}
	response, errDo := http.DefaultClient.Do(request)
	if errDo != nil {
		t.Fatalf("perform POST request: %v", errDo)
	}
	return response
}

func captureHTTPRequest(t *testing.T, request *http.Request, target chan<- capturedHTTPRequest) {
	t.Helper()
	body, errRead := io.ReadAll(request.Body)
	if errRead != nil {
		t.Errorf("read captured request: %v", errRead)
		return
	}
	target <- capturedHTTPRequest{
		path:     request.URL.Path,
		header:   request.Header.Clone(),
		body:     body,
		encoding: request.Header.Get("Content-Encoding"),
	}
}

func readAndClose(t *testing.T, response *http.Response) string {
	t.Helper()
	body, errRead := io.ReadAll(response.Body)
	if errRead != nil {
		t.Fatalf("read response body: %v", errRead)
	}
	if errClose := response.Body.Close(); errClose != nil {
		t.Fatalf("close response body: %v", errClose)
	}
	return string(body)
}

func encodeZstd(t *testing.T, payload []byte) []byte {
	t.Helper()
	var encoded bytes.Buffer
	writer, errWriter := zstd.NewWriter(&encoded)
	if errWriter != nil {
		t.Fatalf("create zstd writer: %v", errWriter)
	}
	if _, errWrite := writer.Write(payload); errWrite != nil {
		t.Fatalf("write zstd payload: %v", errWrite)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("close zstd writer: %v", errClose)
	}
	return encoded.Bytes()
}
