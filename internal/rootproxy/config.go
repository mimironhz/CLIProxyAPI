package rootproxy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	defaultHost                      = "127.0.0.1"
	defaultPort                      = 8317
	defaultRelayBaseURL              = "http://127.0.0.1:8318/v1"
	defaultRelayAPIKeyEnv            = "CPA_RELAY_API_KEY"
	defaultMaxMessageBytes     int64 = 64 << 20
	defaultMaxPendingRoutes          = 64
	defaultMaxRequestBodyBytes int64 = 64 << 20
	defaultLogDirectory              = "logs/root"
	defaultLogMaxFileSizeMB          = 32
	defaultLogMaxBackups             = 15
	defaultLogMaxAgeDays             = 7
	defaultLogMaxTotalSizeMB         = 512
	stockPayloadFormatBase64         = "base64"
	stockPayloadFormatAuto           = "auto"
	officialWebsocketURL             = "wss://chatgpt.com/backend-api/codex/responses"
	websocketModeHTTPFallback        = "http-fallback"
	websocketModeFirstMessage        = "first-message"
)

var environmentVariableName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Config is the intentionally small configuration surface for the Root Proxy.
// It is separate from the full CPA configuration because Root only routes and
// enforces credential boundaries; it does not execute provider translations.
type Config struct {
	Host      string          `yaml:"host"`
	Port      int             `yaml:"port"`
	Debug     bool            `yaml:"debug"`
	Relay     RelayConfig     `yaml:"relay"`
	Routing   RoutingConfig   `yaml:"routing"`
	Websocket WebsocketConfig `yaml:"websocket"`
	HTTP      HTTPConfig      `yaml:"http"`
	Logging   LoggingConfig   `yaml:"logging"`

	relayAPIKey             string
	relayWebsocketURL       string
	configDirectory         string
	fastModels              map[string]struct{}
	multiAgentV2Models      map[string]struct{}
	multiAgentV2RelayModels map[string]struct{}
}

// RelayConfig identifies the local CPA Relay and the environment variable
// containing the API key Root uses for its authenticated hop.
type RelayConfig struct {
	BaseURL   string `yaml:"base-url"`
	APIKeyEnv string `yaml:"api-key-env"`
}

// RoutingConfig contains exact and disjoint model identifiers.
//
// Discovery selects how the lists are interpreted. In the default "static" mode
// they are the complete and authoritative routing surface. In "auto" mode the
// Relay half is discovered from the Relay catalog and the lists degrade to
// optional pins: a non-empty StockModels list is protected from Relay
// collisions, a non-empty RelayModels list narrows the discovered catalog, and
// RelayModelProviders overrides the catalog's own attribution.
//
// FastModels is independent of routing: it names stock models whose turns are
// forced onto the ChatGPT "Fast" (priority) service tier instead of waiting for
// Desktop to select it per turn.
//
// MultiAgentV2Models and MultiAgentV2Relay are likewise independent of routing.
// They advertise multi_agent_version: v2 so a Codex multi-agent v2 parent will
// accept the model as a spawn_agent target; see advertiseMultiAgentV2 for why
// the client rejects anything else.
type RoutingConfig struct {
	Discovery           string            `yaml:"discovery"`
	StockModels         []string          `yaml:"stock-models"`
	RelayModels         []string          `yaml:"relay-models"`
	RelayModelProviders map[string]string `yaml:"relay-model-providers"`
	FastModels          []string          `yaml:"fast-models"`
	MultiAgentV2Models  []string          `yaml:"multi-agent-v2-models"`

	MultiAgentV2Relay MultiAgentV2RelaySelection `yaml:"multi-agent-v2-relay"`
}

// MultiAgentV2RelaySelection chooses which Relay models are advertised as
// multi-agent v2. It accepts either a bool, covering the whole discovered Relay
// half, or a list of provider-qualified model identifiers:
//
//	multi-agent-v2-relay: true
//	multi-agent-v2-relay: ["xai/grok-4.5", "deepseek/deepseek-v4-flash"]
//
// The prefix names the Relay provider whose executor will carry the
// collaboration tools, which is the property that actually decides whether a
// model can serve as a subagent. It is also validated at config load against the
// closed provider set, which a bare slug could not be: under auto discovery the
// Relay catalog is only known at runtime.
type MultiAgentV2RelaySelection struct {
	// All advertises every discovered Relay model.
	All bool
	// Models holds the configured provider-qualified identifiers verbatim.
	Models []string
}

func (s *MultiAgentV2RelaySelection) UnmarshalYAML(value *yaml.Node) error {
	if value == nil {
		return nil
	}
	switch value.Kind {
	case yaml.ScalarNode:
		if value.ShortTag() == "!!null" {
			*s = MultiAgentV2RelaySelection{}
			return nil
		}
		var all bool
		if errDecode := value.Decode(&all); errDecode != nil || value.ShortTag() != "!!bool" {
			return errors.New("routing multi-agent-v2-relay must be a bool or a list of provider-qualified models")
		}
		*s = MultiAgentV2RelaySelection{All: all}
		return nil
	case yaml.SequenceNode:
		var models []string
		if errDecode := value.Decode(&models); errDecode != nil {
			return errors.New("routing multi-agent-v2-relay list must contain provider-qualified model strings")
		}
		*s = MultiAgentV2RelaySelection{Models: models}
		return nil
	default:
		return errors.New("routing multi-agent-v2-relay must be a bool or a list of provider-qualified models")
	}
}

// WebsocketConfig controls transport selection plus message and
// unestablished-route capacity bounds. AllowedOrigins is shared by WebSocket
// and HTTP routes. Root deliberately installs no network deadlines.
type WebsocketConfig struct {
	Mode             string   `yaml:"mode"`
	MaxMessageBytes  int64    `yaml:"max-message-bytes"`
	MaxPendingRoutes int      `yaml:"max-pending-routes"`
	AllowedOrigins   []string `yaml:"allowed-origins"`
}

// HTTPConfig bounds the raw and decoded request bodies inspected before
// forwarding HTTP Responses and compaction requests.
type HTTPConfig struct {
	MaxRequestBodyBytes int64 `yaml:"max-request-body-bytes"`
}

// LoggingConfig controls Root-owned application, access, and stock traffic
// logs. Traffic capture is deliberately opt-in because it contains complete
// conversation and tool payloads for stock requests.
type LoggingConfig struct {
	LoggingToFile           bool   `yaml:"logging-to-file"`
	Directory               string `yaml:"directory"`
	RequestAccessLog        bool   `yaml:"request-access-log"`
	StockRequestResponseLog bool   `yaml:"stock-request-response-log"`
	StockPayloadFormat      string `yaml:"stock-payload-format"`
	MaxFileSizeMB           int    `yaml:"max-file-size-mb"`
	MaxBackups              int    `yaml:"max-backups"`
	MaxAgeDays              int    `yaml:"max-age-days"`
	MaxTotalSizeMB          int    `yaml:"max-total-size-mb"`
	Compress                bool   `yaml:"compress"`
}

func defaultConfig() Config {
	return Config{
		Host: defaultHost,
		Port: defaultPort,
		Relay: RelayConfig{
			BaseURL:   defaultRelayBaseURL,
			APIKeyEnv: defaultRelayAPIKeyEnv,
		},
		Websocket: WebsocketConfig{
			Mode:             websocketModeHTTPFallback,
			MaxMessageBytes:  defaultMaxMessageBytes,
			MaxPendingRoutes: defaultMaxPendingRoutes,
		},
		HTTP: HTTPConfig{
			MaxRequestBodyBytes: defaultMaxRequestBodyBytes,
		},
		Logging: LoggingConfig{
			Directory:          defaultLogDirectory,
			StockPayloadFormat: stockPayloadFormatBase64,
			MaxFileSizeMB:      defaultLogMaxFileSizeMB,
			MaxBackups:         defaultLogMaxBackups,
			MaxAgeDays:         defaultLogMaxAgeDays,
			MaxTotalSizeMB:     defaultLogMaxTotalSizeMB,
			Compress:           true,
		},
	}
}

// LoadConfig reads a strict single-document YAML file and resolves the Relay
// key from the configured environment variable. Inline keys are intentionally
// unsupported so they cannot accidentally enter source control or logs.
func LoadConfig(path string) (*Config, error) {
	return loadConfig(path, os.LookupEnv)
}

func loadConfig(path string, lookupEnv func(string) (string, bool)) (*Config, error) {
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		return nil, fmt.Errorf("read root proxy config: %w", errRead)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, errors.New("root proxy config is empty")
	}

	cfg := defaultConfig()
	absolutePath, errAbsolute := filepath.Abs(path)
	if errAbsolute != nil {
		return nil, fmt.Errorf("resolve root proxy config path: %w", errAbsolute)
	}
	cfg.configDirectory = filepath.Dir(absolutePath)
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if errDecode := decoder.Decode(&cfg); errDecode != nil {
		return nil, fmt.Errorf("decode root proxy config: %w", errDecode)
	}
	var trailing any
	if errTrailing := decoder.Decode(&trailing); !errors.Is(errTrailing, io.EOF) {
		if errTrailing == nil {
			return nil, errors.New("root proxy config must contain exactly one YAML document")
		}
		return nil, fmt.Errorf("decode trailing root proxy config: %w", errTrailing)
	}

	if errValidate := cfg.validateAndResolve(lookupEnv); errValidate != nil {
		return nil, errValidate
	}
	return &cfg, nil
}

func (c *Config) validateAndResolve(lookupEnv func(string) (string, bool)) error {
	if c == nil {
		return errors.New("root proxy config is nil")
	}
	c.Host = strings.TrimSpace(c.Host)
	if !isLoopbackHost(c.Host) {
		return fmt.Errorf("root proxy host %q is not loopback", c.Host)
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("root proxy port %d is outside 1-65535", c.Port)
	}

	c.Relay.BaseURL = strings.TrimSpace(c.Relay.BaseURL)
	relayURL, errRelayURL := relayWebsocketURL(c.Relay.BaseURL, c.Port)
	if errRelayURL != nil {
		return errRelayURL
	}
	c.relayWebsocketURL = relayURL

	c.Relay.APIKeyEnv = strings.TrimSpace(c.Relay.APIKeyEnv)
	if !environmentVariableName.MatchString(c.Relay.APIKeyEnv) {
		return fmt.Errorf("relay api-key-env %q is not a valid environment variable name", c.Relay.APIKeyEnv)
	}
	if lookupEnv == nil {
		return errors.New("environment lookup is unavailable")
	}
	relayKey, ok := lookupEnv(c.Relay.APIKeyEnv)
	if !ok || strings.TrimSpace(relayKey) == "" {
		return fmt.Errorf("relay API key environment variable %s is unset or empty", c.Relay.APIKeyEnv)
	}
	c.relayAPIKey = strings.TrimSpace(relayKey)
	if len(strings.Fields(c.relayAPIKey)) != 1 || strings.Contains(c.relayAPIKey, ",") {
		return fmt.Errorf("relay API key environment variable %s contains invalid whitespace or separators", c.Relay.APIKeyEnv)
	}

	mode, errDiscovery := c.discoveryMode()
	if errDiscovery != nil {
		return errDiscovery
	}
	if _, errModels := buildRouteTableForMode(mode, c.Routing.StockModels, c.Routing.RelayModels, c.Routing.RelayModelProviders); errModels != nil {
		return errModels
	}
	fastModels, errFast := buildFastModelSet(mode, c.Routing.FastModels, c.Routing.StockModels, c.Routing.RelayModels)
	if errFast != nil {
		return errFast
	}
	c.fastModels = fastModels
	multiAgentV2Models, errMultiAgentV2 := buildMultiAgentV2ModelSet(mode, c.Routing.MultiAgentV2Models, c.Routing.StockModels, c.Routing.RelayModels)
	if errMultiAgentV2 != nil {
		return errMultiAgentV2
	}
	c.multiAgentV2Models = multiAgentV2Models
	multiAgentV2RelayModels, errMultiAgentV2Relay := buildMultiAgentV2RelaySet(mode, c.Routing.MultiAgentV2Relay, c.Routing.RelayModels)
	if errMultiAgentV2Relay != nil {
		return errMultiAgentV2Relay
	}
	c.multiAgentV2RelayModels = multiAgentV2RelayModels
	switch c.Websocket.Mode {
	case websocketModeHTTPFallback, websocketModeFirstMessage:
	default:
		return fmt.Errorf("websocket mode %q must be %q or %q", c.Websocket.Mode, websocketModeHTTPFallback, websocketModeFirstMessage)
	}
	if c.Websocket.MaxMessageBytes <= 0 {
		return errors.New("websocket max-message-bytes must be positive")
	}
	if c.Websocket.MaxPendingRoutes <= 0 {
		return errors.New("websocket max-pending-routes must be positive")
	}
	if errOrigins := validateOrigins(c.Websocket.AllowedOrigins); errOrigins != nil {
		return errOrigins
	}
	if c.HTTP.MaxRequestBodyBytes <= 0 {
		return errors.New("http max-request-body-bytes must be positive")
	}
	if errLogging := c.validateLogging(); errLogging != nil {
		return errLogging
	}
	return nil
}

func (c *Config) validateLogging() error {
	c.Logging.Directory = strings.TrimSpace(c.Logging.Directory)
	if c.Logging.Directory == "" {
		return errors.New("logging directory must not be empty")
	}
	if (c.Logging.RequestAccessLog || c.Logging.StockRequestResponseLog) && !c.Logging.LoggingToFile {
		return errors.New("logging logging-to-file must be enabled for access or stock request-response logs")
	}
	if c.Logging.StockPayloadFormat == "" {
		// Preserve directly constructed Config values from before this option was
		// introduced. LoadConfig already supplies the same backward-compatible
		// default before decoding YAML.
		c.Logging.StockPayloadFormat = stockPayloadFormatBase64
	} else {
		c.Logging.StockPayloadFormat = strings.TrimSpace(c.Logging.StockPayloadFormat)
	}
	switch c.Logging.StockPayloadFormat {
	case stockPayloadFormatBase64, stockPayloadFormatAuto:
	default:
		return fmt.Errorf("logging stock-payload-format %q must be %q or %q", c.Logging.StockPayloadFormat, stockPayloadFormatBase64, stockPayloadFormatAuto)
	}
	if c.Logging.MaxFileSizeMB <= 0 {
		return errors.New("logging max-file-size-mb must be positive")
	}
	if c.Logging.MaxBackups < 0 {
		return errors.New("logging max-backups must not be negative")
	}
	if c.Logging.MaxAgeDays < 0 {
		return errors.New("logging max-age-days must not be negative")
	}
	if c.Logging.MaxTotalSizeMB <= 0 {
		return errors.New("logging max-total-size-mb must be positive")
	}
	if c.Logging.LoggingToFile {
		activeFiles := 1
		if c.Logging.RequestAccessLog {
			activeFiles++
		}
		if c.Logging.StockRequestResponseLog {
			activeFiles++
		}
		minimumTotal := c.Logging.MaxFileSizeMB * activeFiles
		if c.Logging.MaxTotalSizeMB < minimumTotal {
			return fmt.Errorf("logging max-total-size-mb must be at least %d for %d active log files", minimumTotal, activeFiles)
		}
	}
	return nil
}

// discoveryMode resolves routing.discovery. It defaults to static so an existing
// configuration keeps its exact routing surface after an upgrade.
func (c *Config) discoveryMode() (discoveryMode, error) {
	switch strings.TrimSpace(c.Routing.Discovery) {
	case "", discoveryStatic.String():
		return discoveryStatic, nil
	case discoveryAuto.String():
		return discoveryAuto, nil
	default:
		return 0, fmt.Errorf("routing discovery %q must be %q or %q", c.Routing.Discovery, discoveryStatic, discoveryAuto)
	}
}

func (c *Config) logDirectory() (string, error) {
	if c == nil {
		return "", errors.New("root proxy config is nil")
	}
	directory := c.Logging.Directory
	if !filepath.IsAbs(directory) {
		base := c.configDirectory
		if base == "" {
			var errWorkingDirectory error
			base, errWorkingDirectory = os.Getwd()
			if errWorkingDirectory != nil {
				return "", fmt.Errorf("resolve logging directory: %w", errWorkingDirectory)
			}
		}
		directory = filepath.Join(base, directory)
	}
	absolute, errAbsolute := filepath.Abs(directory)
	if errAbsolute != nil {
		return "", fmt.Errorf("resolve logging directory: %w", errAbsolute)
	}
	return filepath.Clean(absolute), nil
}

func relayWebsocketURL(rawBaseURL string, rootPort int) (string, error) {
	parsed, errParse := url.Parse(rawBaseURL)
	if errParse != nil {
		return "", fmt.Errorf("parse relay base-url: %w", errParse)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("relay base-url scheme %q must be http or https", parsed.Scheme)
	}
	if parsed.User != nil {
		return "", errors.New("relay base-url must not contain user information")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", errors.New("relay base-url must not contain a query or fragment")
	}
	if parsed.RawPath != "" {
		return "", errors.New("relay base-url must not use an escaped path")
	}
	if !isLoopbackHost(parsed.Hostname()) {
		return "", fmt.Errorf("relay base-url host %q is not loopback", parsed.Hostname())
	}
	portText := parsed.Port()
	if portText == "" {
		return "", errors.New("relay base-url must include an explicit port")
	}
	relayPort, errPort := strconv.Atoi(portText)
	if errPort != nil || relayPort < 1 || relayPort > 65535 {
		return "", fmt.Errorf("relay base-url port %q is invalid", portText)
	}
	if relayPort == rootPort {
		return "", fmt.Errorf("relay base-url port %d would loop back to Root", relayPort)
	}
	normalizedPath := strings.TrimSuffix(parsed.Path, "/")
	if normalizedPath != "/v1" {
		return "", fmt.Errorf("relay base-url path %q must be /v1", parsed.Path)
	}
	if parsed.Scheme == "https" {
		parsed.Scheme = "wss"
	} else {
		parsed.Scheme = "ws"
	}
	parsed.Path = normalizedPath + "/responses"
	return parsed.String(), nil
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validateOrigins(origins []string) error {
	seen := make(map[string]struct{}, len(origins))
	for _, origin := range origins {
		trimmed := strings.TrimSpace(origin)
		if trimmed == "" || trimmed != origin {
			return fmt.Errorf("allowed origin %q is empty or has surrounding whitespace", origin)
		}
		if trimmed == "*" {
			return errors.New("allowed-origins must not contain a wildcard")
		}
		if _, ok := seen[trimmed]; ok {
			return fmt.Errorf("duplicate allowed origin %q", trimmed)
		}
		seen[trimmed] = struct{}{}
	}
	return nil
}

func (c *Config) listenAddress() string {
	return net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
}

func (c *Config) bridgeOptions() bridgeOptions {
	return bridgeOptions{
		officialURL:      officialWebsocketURL,
		relayURL:         c.relayWebsocketURL,
		relayAPIKey:      c.relayAPIKey,
		stockModels:      append([]string(nil), c.Routing.StockModels...),
		relayModels:      append([]string(nil), c.Routing.RelayModels...),
		relayProviders:   cloneStringMap(c.Routing.RelayModelProviders),
		fastModels:       cloneModelSet(c.fastModels),
		maxMessageBytes:  c.Websocket.MaxMessageBytes,
		maxPendingRoutes: c.Websocket.MaxPendingRoutes,
		allowedOrigins:   append([]string(nil), c.Websocket.AllowedOrigins...),
	}
}

func (c *Config) httpBridgeOptions() httpBridgeOptions {
	return httpBridgeOptions{
		relayBaseURL:   c.Relay.BaseURL,
		relayAPIKey:    c.relayAPIKey,
		stockModels:    append([]string(nil), c.Routing.StockModels...),
		relayModels:    append([]string(nil), c.Routing.RelayModels...),
		relayProviders: cloneStringMap(c.Routing.RelayModelProviders),
		fastModels:     cloneModelSet(c.fastModels),
		maxRequestBody: c.HTTP.MaxRequestBodyBytes,
		allowedOrigins: append([]string(nil), c.Websocket.AllowedOrigins...),
	}
}

func cloneModelSet(source map[string]struct{}) map[string]struct{} {
	if len(source) == 0 {
		return nil
	}
	cloned := make(map[string]struct{}, len(source))
	for key := range source {
		cloned[key] = struct{}{}
	}
	return cloned
}

func cloneStringMap(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}
