package rootproxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigAppliesDefaultsAndResolvesRelayKey(t *testing.T) {
	path := writeRootConfig(t, `
routing:
  stock-models: ["gpt-stock"]
  relay-models: ["relay-model"]
`)
	config, errLoad := loadConfig(path, func(name string) (string, bool) {
		if name != defaultRelayAPIKeyEnv {
			t.Fatalf("environment variable = %q, want %q", name, defaultRelayAPIKeyEnv)
		}
		return " relay-secret ", true
	})
	if errLoad != nil {
		t.Fatalf("loadConfig() error = %v", errLoad)
	}
	if got := config.listenAddress(); got != "127.0.0.1:8317" {
		t.Fatalf("listenAddress() = %q", got)
	}
	if got := config.relayWebsocketURL; got != "ws://127.0.0.1:8318/v1/responses" {
		t.Fatalf("relay websocket URL = %q", got)
	}
	if got := config.relayAPIKey; got != "relay-secret" {
		t.Fatalf("resolved Relay key = %q", got)
	}
	options := config.bridgeOptions()
	if options.officialURL != officialWebsocketURL {
		t.Fatalf("official URL = %q", options.officialURL)
	}
	if options.maxMessageBytes != defaultMaxMessageBytes {
		t.Fatalf("max message bytes = %d", options.maxMessageBytes)
	}
	if options.maxPendingRoutes != defaultMaxPendingRoutes {
		t.Fatalf("maximum pending routes = %d", options.maxPendingRoutes)
	}
	if got := config.HTTP.MaxRequestBodyBytes; got != defaultMaxRequestBodyBytes {
		t.Fatalf("maximum HTTP request body bytes = %d", got)
	}
	if got := config.Websocket.Mode; got != websocketModeHTTPFallback {
		t.Fatalf("websocket mode = %q, want %q", got, websocketModeHTTPFallback)
	}
	if config.Logging.LoggingToFile || config.Logging.RequestAccessLog || config.Logging.StockRequestResponseLog {
		t.Fatal("Root logging defaults must remain opt-in")
	}
	if got := config.Logging.StockPayloadFormat; got != stockPayloadFormatBase64 {
		t.Fatalf("stock payload format = %q, want %q", got, stockPayloadFormatBase64)
	}
	if got := config.Logging.Directory; got != defaultLogDirectory {
		t.Fatalf("logging directory = %q, want %q", got, defaultLogDirectory)
	}
	resolvedLogDirectory, errDirectory := config.logDirectory()
	if errDirectory != nil {
		t.Fatalf("logDirectory() error = %v", errDirectory)
	}
	if want := filepath.Join(filepath.Dir(path), defaultLogDirectory); resolvedLogDirectory != want {
		t.Fatalf("resolved logging directory = %q, want %q", resolvedLogDirectory, want)
	}
}

func TestLoadConfigAcceptsPrivateLoggingConfiguration(t *testing.T) {
	logDirectory := filepath.Join(t.TempDir(), "root-logs")
	path := writeRootConfig(t, `
routing:
  stock-models: ["gpt-stock"]
  relay-models: ["relay-model"]
logging:
  logging-to-file: true
  directory: "`+logDirectory+`"
  request-access-log: true
  stock-request-response-log: true
  stock-payload-format: auto
  max-file-size-mb: 64
  max-backups: 9
  max-age-days: 5
  max-total-size-mb: 256
  compress: false
`)
	config, errLoad := loadConfig(path, staticEnvironment("relay-secret"))
	if errLoad != nil {
		t.Fatalf("loadConfig() error = %v", errLoad)
	}
	if !config.Logging.LoggingToFile || !config.Logging.RequestAccessLog || !config.Logging.StockRequestResponseLog {
		t.Fatalf("logging configuration not retained: %#v", config.Logging)
	}
	if got := config.Logging.StockPayloadFormat; got != stockPayloadFormatAuto {
		t.Fatalf("stock payload format = %q, want %q", got, stockPayloadFormatAuto)
	}
	if got, errDirectory := config.logDirectory(); errDirectory != nil || got != logDirectory {
		t.Fatalf("logDirectory() = %q, %v; want %q", got, errDirectory, logDirectory)
	}
}

func TestLoadConfigAcceptsExplicitFirstMessageWebsocketMode(t *testing.T) {
	path := writeRootConfig(t, `
routing:
  stock-models: ["gpt-stock"]
  relay-models: ["relay-model"]
websocket:
  mode: "first-message"
`)
	config, errLoad := loadConfig(path, staticEnvironment("relay-secret"))
	if errLoad != nil {
		t.Fatalf("loadConfig() error = %v", errLoad)
	}
	if got := config.Websocket.Mode; got != websocketModeFirstMessage {
		t.Fatalf("websocket mode = %q, want %q", got, websocketModeFirstMessage)
	}
}

func TestValidateDirectConfigDefaultsEmptyStockPayloadFormatToBase64(t *testing.T) {
	config := defaultConfig()
	config.Routing.StockModels = []string{"gpt-stock"}
	config.Routing.RelayModels = []string{"relay-model"}
	config.Logging.StockPayloadFormat = ""
	if errValidate := config.validateAndResolve(staticEnvironment("relay-secret")); errValidate != nil {
		t.Fatalf("validateAndResolve() error = %v", errValidate)
	}
	if config.Logging.StockPayloadFormat != stockPayloadFormatBase64 {
		t.Fatalf("direct stock payload format = %q, want %q", config.Logging.StockPayloadFormat, stockPayloadFormatBase64)
	}
}

func TestLoadConfigResolvesExactRelayModelProviders(t *testing.T) {
	path := writeRootConfig(t, `
routing:
  stock-models: ["gpt-stock"]
  relay-models: ["grok-a", "grok-b", "kimi-a"]
  relay-model-providers:
    grok-a: xai
    grok-b: xai
    kimi-a: kimi
`)
	config, errLoad := loadConfig(path, staticEnvironment("relay-secret"))
	if errLoad != nil {
		t.Fatalf("loadConfig() error = %v", errLoad)
	}
	if got := config.bridgeOptions().relayProviders["grok-b"]; got != "xai" {
		t.Fatalf("grok-b provider = %q, want xai", got)
	}
	if got := config.httpBridgeOptions().relayProviders["kimi-a"]; got != "kimi" {
		t.Fatalf("kimi-a provider = %q, want kimi", got)
	}

	options := config.bridgeOptions()
	options.relayProviders["grok-a"] = "kimi"
	if got := config.Routing.RelayModelProviders["grok-a"]; got != "xai" {
		t.Fatalf("bridge options mutated config provider = %q, want xai", got)
	}
}

func TestLoadConfigRejectsUnknownAndTrailingYAML(t *testing.T) {
	tests := map[string]string{
		"unknown inline key": `
relay:
  api-key: "must-not-be-supported"
routing:
  stock-models: ["gpt-stock"]
  relay-models: ["relay-model"]
`,
		"trailing document": `
routing:
  stock-models: ["gpt-stock"]
  relay-models: ["relay-model"]
---
routing: {}
`,
	}
	for name, contents := range tests {
		t.Run(name, func(t *testing.T) {
			path := writeRootConfig(t, contents)
			if _, errLoad := loadConfig(path, staticEnvironment("relay-secret")); errLoad == nil {
				t.Fatal("loadConfig() succeeded, want strict YAML error")
			}
		})
	}
}

func TestLoadConfigRejectsUnsafeTopology(t *testing.T) {
	tests := map[string]string{
		"public root":        `host: "0.0.0.0"`,
		"remote relay":       `relay: {base-url: "http://relay.example/v1"}`,
		"relay without port": `relay: {base-url: "http://127.0.0.1/v1"}`,
		"relay wrong path":   `relay: {base-url: "http://127.0.0.1:8318/api"}`,
		"root loop":          `relay: {base-url: "http://localhost:8317/v1"}`,
	}
	for name, override := range tests {
		t.Run(name, func(t *testing.T) {
			contents := override + `
routing:
  stock-models: ["gpt-stock"]
  relay-models: ["relay-model"]
`
			path := writeRootConfig(t, contents)
			if _, errLoad := loadConfig(path, staticEnvironment("relay-secret")); errLoad == nil {
				t.Fatal("loadConfig() succeeded, want topology error")
			}
		})
	}
}

func TestLoadConfigRejectsInvalidRoutingAndBounds(t *testing.T) {
	tests := map[string]string{
		"missing stock": `
routing:
  relay-models: ["relay-model"]
`,
		"overlap": `
routing:
  stock-models: ["same"]
  relay-models: ["same"]
`,
		"duplicate": `
routing:
  stock-models: ["gpt-stock", "gpt-stock"]
  relay-models: ["relay-model"]
`,
		"wildcard": `
routing:
  stock-models: ["gpt-*"]
  relay-models: ["relay-model"]
`,
		"provider for non-relay model": `
routing:
  stock-models: ["gpt-stock"]
  relay-models: ["relay-model"]
  relay-model-providers: {unknown: xai}
`,
		"unsupported relay provider": `
routing:
  stock-models: ["gpt-stock"]
  relay-models: ["relay-model"]
  relay-model-providers: {relay-model: anthropic}
`,
		"relay provider with whitespace": `
routing:
  stock-models: ["gpt-stock"]
  relay-models: ["relay-model"]
  relay-model-providers: {relay-model: " xai"}
`,
		"zero message limit": `
routing:
  stock-models: ["gpt-stock"]
  relay-models: ["relay-model"]
websocket:
  max-message-bytes: 0
`,
		"zero pending routes": `
routing:
  stock-models: ["gpt-stock"]
  relay-models: ["relay-model"]
websocket:
  max-pending-routes: 0
`,
		"unsupported websocket mode": `
routing:
  stock-models: ["gpt-stock"]
  relay-models: ["relay-model"]
websocket:
  mode: "automatic"
`,
		"zero HTTP body limit": `
routing:
  stock-models: ["gpt-stock"]
  relay-models: ["relay-model"]
http:
  max-request-body-bytes: 0
`,
		"wildcard origin": `
routing:
  stock-models: ["gpt-stock"]
  relay-models: ["relay-model"]
websocket:
  allowed-origins: ["*"]
`,
		"access log without file logging": `
routing:
  stock-models: ["gpt-stock"]
  relay-models: ["relay-model"]
logging:
  request-access-log: true
`,
		"stock log without file logging": `
routing:
  stock-models: ["gpt-stock"]
  relay-models: ["relay-model"]
logging:
  stock-request-response-log: true
`,
		"unsupported stock payload format": `
routing:
  stock-models: ["gpt-stock"]
  relay-models: ["relay-model"]
logging:
  stock-payload-format: raw
`,
		"zero logging file size": `
routing:
  stock-models: ["gpt-stock"]
  relay-models: ["relay-model"]
logging:
  max-file-size-mb: 0
`,
		"negative logging backups": `
routing:
  stock-models: ["gpt-stock"]
  relay-models: ["relay-model"]
logging:
  max-backups: -1
`,
		"logging total below active files": `
routing:
  stock-models: ["gpt-stock"]
  relay-models: ["relay-model"]
logging:
  logging-to-file: true
  request-access-log: true
  stock-request-response-log: true
  max-file-size-mb: 32
  max-total-size-mb: 95
`,
	}
	for name, contents := range tests {
		t.Run(name, func(t *testing.T) {
			path := writeRootConfig(t, contents)
			if _, errLoad := loadConfig(path, staticEnvironment("relay-secret")); errLoad == nil {
				t.Fatal("loadConfig() succeeded, want validation error")
			}
		})
	}
}

func TestLoadConfigRequiresRelayKeyEnvironment(t *testing.T) {
	path := writeRootConfig(t, `
relay:
  api-key-env: "ROOT_TEST_RELAY_KEY"
routing:
  stock-models: ["gpt-stock"]
  relay-models: ["relay-model"]
`)
	_, errLoad := loadConfig(path, func(string) (string, bool) { return "", false })
	if errLoad == nil || !strings.Contains(errLoad.Error(), "ROOT_TEST_RELAY_KEY") {
		t.Fatalf("loadConfig() error = %v, want missing environment variable", errLoad)
	}
}

func TestBridgeOptionsUseConfiguredBounds(t *testing.T) {
	config := defaultConfig()
	config.Routing.StockModels = []string{"gpt-stock"}
	config.Routing.RelayModels = []string{"relay-model"}
	config.Websocket.MaxPendingRoutes = 2
	if errValidate := config.validateAndResolve(staticEnvironment("relay-secret")); errValidate != nil {
		t.Fatalf("validateAndResolve() error = %v", errValidate)
	}
	options := config.bridgeOptions()
	if options.maxPendingRoutes != 2 {
		t.Fatalf("maximum pending routes = %d", options.maxPendingRoutes)
	}
}

func TestNewServerRevalidatesMutableConfig(t *testing.T) {
	t.Setenv(defaultRelayAPIKeyEnv, "relay-secret")
	config := defaultConfig()
	config.Routing.StockModels = []string{"gpt-stock"}
	config.Routing.RelayModels = []string{"relay-model"}
	if errValidate := config.validateAndResolve(os.LookupEnv); errValidate != nil {
		t.Fatalf("validateAndResolve() error = %v", errValidate)
	}
	config.Host = "0.0.0.0"
	if _, errServer := NewServer(&config); errServer == nil {
		t.Fatal("NewServer() accepted a post-load public bind mutation")
	}
}

func writeRootConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "root-proxy.yaml")
	if errWrite := os.WriteFile(path, []byte(strings.TrimSpace(contents)+"\n"), 0o600); errWrite != nil {
		t.Fatalf("write config: %v", errWrite)
	}
	return path
}

func staticEnvironment(value string) func(string) (string, bool) {
	return func(string) (string, bool) { return value, true }
}
