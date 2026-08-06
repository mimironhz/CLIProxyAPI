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
		"fast relay model": `
routing:
  stock-models: ["gpt-stock"]
  relay-models: ["relay-model"]
  fast-models: ["relay-model"]
`,
		"fast model outside the static stock surface": `
routing:
  stock-models: ["gpt-stock"]
  relay-models: ["relay-model"]
  fast-models: ["gpt-unlisted"]
`,
		"duplicate fast model": `
routing:
  stock-models: ["gpt-stock"]
  relay-models: ["relay-model"]
  fast-models: ["gpt-stock", "gpt-stock"]
`,
		"fast model wildcard": `
routing:
  discovery: "auto"
  stock-models: ["gpt-stock"]
  relay-models: ["relay-model"]
  fast-models: ["gpt-*"]
`,
		"multi-agent-v2 relay model": `
routing:
  stock-models: ["gpt-stock"]
  relay-models: ["relay-model"]
  multi-agent-v2-models: ["relay-model"]
`,
		"multi-agent-v2 model outside the static stock surface": `
routing:
  stock-models: ["gpt-stock"]
  relay-models: ["relay-model"]
  multi-agent-v2-models: ["gpt-unlisted"]
`,
		"duplicate multi-agent-v2 model": `
routing:
  stock-models: ["gpt-stock"]
  relay-models: ["relay-model"]
  multi-agent-v2-models: ["gpt-stock", "gpt-stock"]
`,
		"multi-agent-v2 model wildcard": `
routing:
  discovery: "auto"
  stock-models: ["gpt-stock"]
  relay-models: ["relay-model"]
  multi-agent-v2-models: ["gpt-*"]
`,
		"multi-agent-v2 relay entry without a provider prefix": `
routing:
  stock-models: ["gpt-stock"]
  relay-models: ["relay-model"]
  multi-agent-v2-relay: ["relay-model"]
`,
		"multi-agent-v2 relay entry with an unknown provider": `
routing:
  stock-models: ["gpt-stock"]
  relay-models: ["relay-model"]
  multi-agent-v2-relay: ["openai/relay-model"]
`,
		"multi-agent-v2 relay model outside the static relay surface": `
routing:
  stock-models: ["gpt-stock"]
  relay-models: ["relay-model"]
  multi-agent-v2-relay: ["xai/unlisted-model"]
`,
		"duplicate multi-agent-v2 relay model": `
routing:
  stock-models: ["gpt-stock"]
  relay-models: ["relay-model"]
  multi-agent-v2-relay: ["xai/relay-model", "xai/relay-model"]
`,
		"multi-agent-v2 relay model wildcard": `
routing:
  discovery: "auto"
  stock-models: ["gpt-stock"]
  relay-models: ["relay-model"]
  multi-agent-v2-relay: ["xai/relay-*"]
`,
		"multi-agent-v2 relay of the wrong type": `
routing:
  stock-models: ["gpt-stock"]
  relay-models: ["relay-model"]
  multi-agent-v2-relay: {xai: true}
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

func TestLoadConfigResolvesFastModelsForBothBridges(t *testing.T) {
	path := writeRootConfig(t, `
routing:
  stock-models: ["gpt-stock", "gpt-standard"]
  relay-models: ["relay-model"]
  fast-models: ["gpt-stock"]
`)
	config, errLoad := loadConfig(path, staticEnvironment("relay-secret"))
	if errLoad != nil {
		t.Fatalf("loadConfig() error = %v", errLoad)
	}
	for name, resolved := range map[string]map[string]struct{}{
		"http":      config.httpBridgeOptions().fastModels,
		"websocket": config.bridgeOptions().fastModels,
	} {
		if len(resolved) != 1 {
			t.Fatalf("%s fast models = %#v, want exactly gpt-stock", name, resolved)
		}
		if _, fast := resolved["gpt-stock"]; !fast {
			t.Fatalf("%s fast models = %#v, want gpt-stock", name, resolved)
		}
	}
	// A model may be forced under auto discovery before it is pinned, because
	// the stock half is only known once the merged catalog lands.
	autoPath := writeRootConfig(t, `
routing:
  discovery: "auto"
  fast-models: ["gpt-5.6-luna"]
`)
	if _, errAuto := loadConfig(autoPath, staticEnvironment("relay-secret")); errAuto != nil {
		t.Fatalf("loadConfig() under auto discovery error = %v", errAuto)
	}
}

func TestLoadConfigResolvesMultiAgentV2Models(t *testing.T) {
	path := writeRootConfig(t, `
routing:
  stock-models: ["gpt-stock", "gpt-standard"]
  relay-models: ["relay-model"]
  multi-agent-v2-models: ["gpt-stock"]
  multi-agent-v2-relay: true
`)
	config, errLoad := loadConfig(path, staticEnvironment("relay-secret"))
	if errLoad != nil {
		t.Fatalf("loadConfig() error = %v", errLoad)
	}
	if len(config.multiAgentV2Models) != 1 {
		t.Fatalf("multi-agent-v2 models = %#v, want exactly gpt-stock", config.multiAgentV2Models)
	}
	if _, advertised := config.multiAgentV2Models["gpt-stock"]; !advertised {
		t.Fatalf("multi-agent-v2 models = %#v, want gpt-stock", config.multiAgentV2Models)
	}
	if !config.Routing.MultiAgentV2Relay.All {
		t.Fatal("multi-agent-v2-relay = false, want true")
	}

	// The Relay switch stands alone: advertising the discovered half must not
	// require naming any stock model.
	relayOnlyPath := writeRootConfig(t, `
routing:
  stock-models: ["gpt-stock"]
  relay-models: ["relay-model"]
  multi-agent-v2-relay: true
`)
	relayOnly, errRelayOnly := loadConfig(relayOnlyPath, staticEnvironment("relay-secret"))
	if errRelayOnly != nil {
		t.Fatalf("loadConfig() with relay-only advertisement error = %v", errRelayOnly)
	}
	if len(relayOnly.multiAgentV2Models) != 0 {
		t.Fatalf("multi-agent-v2 models = %#v, want empty", relayOnly.multiAgentV2Models)
	}

	// The same key also accepts a provider-qualified list, which resolves to an
	// explicit set rather than the whole discovered half.
	selectivePath := writeRootConfig(t, `
routing:
  stock-models: ["gpt-stock"]
  relay-models: ["relay-model", "other-model"]
  multi-agent-v2-relay: ["xai/relay-model"]
`)
	selective, errSelective := loadConfig(selectivePath, staticEnvironment("relay-secret"))
	if errSelective != nil {
		t.Fatalf("loadConfig() with a qualified relay list error = %v", errSelective)
	}
	if selective.Routing.MultiAgentV2Relay.All {
		t.Fatal("multi-agent-v2-relay All = true, want false for a list")
	}
	if len(selective.multiAgentV2RelayModels) != 1 {
		t.Fatalf("multi-agent-v2 relay models = %#v, want exactly xai/relay-model", selective.multiAgentV2RelayModels)
	}
	if _, advertised := selective.multiAgentV2RelayModels[relayModelKey(relayProviderXAI, "relay-model")]; !advertised {
		t.Fatalf("multi-agent-v2 relay models = %#v, want xai/relay-model", selective.multiAgentV2RelayModels)
	}

	// A vendor-qualified slug keeps its own slashes; only the first separator
	// delimits the provider prefix.
	nestedPath := writeRootConfig(t, `
routing:
  discovery: "auto"
  multi-agent-v2-relay: ["xai/x-ai/grok-4.5"]
`)
	nested, errNested := loadConfig(nestedPath, staticEnvironment("relay-secret"))
	if errNested != nil {
		t.Fatalf("loadConfig() with a vendor-qualified slug error = %v", errNested)
	}
	if _, advertised := nested.multiAgentV2RelayModels[relayModelKey(relayProviderXAI, "x-ai/grok-4.5")]; !advertised {
		t.Fatalf("multi-agent-v2 relay models = %#v, want xai/x-ai/grok-4.5", nested.multiAgentV2RelayModels)
	}

	// A stock model may be advertised under auto discovery before it is pinned,
	// because the stock half is only known once the merged catalog lands.
	autoPath := writeRootConfig(t, `
routing:
  discovery: "auto"
  multi-agent-v2-models: ["gpt-5.6-luna"]
`)
	if _, errAuto := loadConfig(autoPath, staticEnvironment("relay-secret")); errAuto != nil {
		t.Fatalf("loadConfig() under auto discovery error = %v", errAuto)
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
