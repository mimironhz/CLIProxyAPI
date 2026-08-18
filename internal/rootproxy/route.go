package rootproxy

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
)

type route uint8

const (
	routeOfficial route = iota + 1
	routeRelay
)

func (r route) String() string {
	switch r {
	case routeOfficial:
		return "official"
	case routeRelay:
		return "relay"
	default:
		return "unknown"
	}
}

type relayProvider string

const (
	relayProviderXAI      relayProvider = "xai"
	relayProviderKimi     relayProvider = "kimi"
	relayProviderDeepSeek relayProvider = "deepseek"
)

// discoveryMode selects how the route table is populated. In static mode the
// configured lists are the complete and authoritative routing surface. In auto
// mode they degrade to optional pins and the Relay half is discovered from the
// Relay catalog at runtime.
type discoveryMode uint8

const (
	discoveryStatic discoveryMode = iota + 1
	discoveryAuto
)

func (m discoveryMode) String() string {
	switch m {
	case discoveryStatic:
		return "static"
	case discoveryAuto:
		return "auto"
	default:
		return "unknown"
	}
}

// routeTable is an immutable routing snapshot. In auto mode it is replaced
// wholesale by the resolver whenever Relay discovery reports a new catalog.
type routeTable struct {
	mode           discoveryMode
	stock          map[string]struct{}
	relay          map[string]struct{}
	relayProviders map[string]relayProvider
	// relayWindows is published with the same snapshot as relay/providers so a
	// catalog refresh cannot advertise one observation's routes and another's
	// context metadata.
	relayWindows map[string]int
}

func buildRouteTable(stockModels, relayModels []string, configuredProviders map[string]string) (routeTable, error) {
	return buildRouteTableForMode(discoveryStatic, stockModels, relayModels, configuredProviders)
}

func buildRouteTableForMode(mode discoveryMode, stockModels, relayModels []string, configuredProviders map[string]string) (routeTable, error) {
	// Auto mode treats both lists as optional pins, so an empty list simply
	// means "do not constrain this half".
	if mode == discoveryStatic {
		if len(stockModels) == 0 {
			return routeTable{}, errors.New("routing stock-models must not be empty")
		}
		if len(relayModels) == 0 {
			return routeTable{}, errors.New("routing relay-models must not be empty")
		}
	}

	table := routeTable{
		mode:           mode,
		stock:          make(map[string]struct{}, len(stockModels)),
		relay:          make(map[string]struct{}, len(relayModels)),
		relayProviders: make(map[string]relayProvider, len(configuredProviders)),
	}
	for _, model := range stockModels {
		if errModel := addExactModel(table.stock, model, "stock"); errModel != nil {
			return routeTable{}, errModel
		}
	}
	for _, model := range relayModels {
		if errModel := addExactModel(table.relay, model, "relay"); errModel != nil {
			return routeTable{}, errModel
		}
		if _, overlaps := table.stock[model]; overlaps {
			return routeTable{}, fmt.Errorf("model %q appears in both stock-models and relay-models", model)
		}
	}
	for model, rawProvider := range configuredProviders {
		// In auto mode a configured provider is an override for a model that may
		// only appear once Relay discovery runs, so it cannot be cross-checked
		// against the pin list here.
		if _, configured := table.relay[model]; !configured && mode == discoveryStatic {
			return routeTable{}, fmt.Errorf("relay provider is configured for non-relay model %q", model)
		}
		provider, errProvider := parseRelayProvider(rawProvider)
		if errProvider != nil {
			return routeTable{}, fmt.Errorf("relay model %q: %w", model, errProvider)
		}
		table.relayProviders[model] = provider
	}
	return table, nil
}

// buildFastModelSet resolves the models whose stock turns are forced onto the
// ChatGPT "Fast" (priority) service tier. Only the official arm honours the
// tier, so a Relay model can never be listed; under static mode the model must
// also belong to the configured stock surface. Under auto mode the stock half
// is discovered, so membership is left to the runtime route decision.
func buildFastModelSet(mode discoveryMode, fastModels, stockModels, relayModels []string) (map[string]struct{}, error) {
	if len(fastModels) == 0 {
		return nil, nil
	}
	stock := make(map[string]struct{}, len(stockModels))
	for _, model := range stockModels {
		stock[model] = struct{}{}
	}
	relay := make(map[string]struct{}, len(relayModels))
	for _, model := range relayModels {
		relay[model] = struct{}{}
	}
	fast := make(map[string]struct{}, len(fastModels))
	for _, model := range fastModels {
		if errModel := addExactModel(fast, model, "fast"); errModel != nil {
			return nil, errModel
		}
		if _, isRelay := relay[model]; isRelay {
			return nil, fmt.Errorf("fast model %q is a relay model; the Fast service tier is official-only", model)
		}
		if _, isStock := stock[model]; !isStock && mode == discoveryStatic {
			return nil, fmt.Errorf("fast model %q is not configured in stock-models", model)
		}
	}
	return fast, nil
}

// buildMultiAgentV2ModelSet resolves the stock models advertised as multi-agent
// v2. The Relay half is discovered at runtime and cannot be enumerated here, so
// it is covered by the routing-wide multi-agent-v2-relay switch instead; a Relay
// identifier listed here is therefore rejected as a configuration mistake. Under
// static mode the model must belong to the configured stock surface, matching
// buildFastModelSet.
func buildMultiAgentV2ModelSet(mode discoveryMode, multiAgentV2Models, stockModels, relayModels []string) (map[string]struct{}, error) {
	if len(multiAgentV2Models) == 0 {
		return nil, nil
	}
	stock := make(map[string]struct{}, len(stockModels))
	for _, model := range stockModels {
		stock[model] = struct{}{}
	}
	relay := make(map[string]struct{}, len(relayModels))
	for _, model := range relayModels {
		relay[model] = struct{}{}
	}
	advertised := make(map[string]struct{}, len(multiAgentV2Models))
	for _, model := range multiAgentV2Models {
		if errModel := addExactModel(advertised, model, "multi-agent-v2"); errModel != nil {
			return nil, errModel
		}
		if _, isRelay := relay[model]; isRelay {
			return nil, fmt.Errorf("multi-agent-v2 model %q is a relay model; use multi-agent-v2-relay instead", model)
		}
		if _, isStock := stock[model]; !isStock && mode == discoveryStatic {
			return nil, fmt.Errorf("multi-agent-v2 model %q is not configured in stock-models", model)
		}
	}
	return advertised, nil
}

// relayModelKey is the canonical provider-qualified identifier that matches a
// discovered Relay model against configuration.
func relayModelKey(provider relayProvider, model string) string {
	return string(provider) + "/" + model
}

// buildMultiAgentV2RelaySet resolves the provider-qualified Relay models
// advertised as multi-agent v2. A bare bool covers the whole discovered half and
// resolves to no set. The provider half is validated here; the model half cannot
// be under auto discovery, where the Relay catalog is only known at runtime, so
// an unmatched entry stays inert exactly as an unpinned fast model does.
func buildMultiAgentV2RelaySet(mode discoveryMode, selection MultiAgentV2RelaySelection, relayModels []string) (map[string]struct{}, error) {
	if len(selection.Models) == 0 {
		return nil, nil
	}
	relay := make(map[string]struct{}, len(relayModels))
	for _, model := range relayModels {
		relay[model] = struct{}{}
	}
	advertised := make(map[string]struct{}, len(selection.Models))
	for _, entry := range selection.Models {
		provider, model, errEntry := parseQualifiedRelayModel(entry)
		if errEntry != nil {
			return nil, errEntry
		}
		key := relayModelKey(provider, model)
		if _, duplicate := advertised[key]; duplicate {
			return nil, fmt.Errorf("multi-agent-v2 relay model %q is duplicated", entry)
		}
		advertised[key] = struct{}{}
		if _, isRelay := relay[model]; !isRelay && mode == discoveryStatic {
			return nil, fmt.Errorf("multi-agent-v2 relay model %q is not configured in relay-models", entry)
		}
	}
	return advertised, nil
}

// parseQualifiedRelayModel splits "provider/model" on the first separator. The
// model half may itself contain slashes, because a Relay catalog can publish
// vendor-qualified identifiers such as "x-ai/grok-4.6".
func parseQualifiedRelayModel(entry string) (relayProvider, string, error) {
	if entry == "" || strings.TrimSpace(entry) != entry {
		return "", "", fmt.Errorf("multi-agent-v2 relay model %q is empty or has surrounding whitespace", entry)
	}
	providerName, model, qualified := strings.Cut(entry, "/")
	if !qualified {
		return "", "", fmt.Errorf(`multi-agent-v2 relay model %q must be provider-qualified, for example "xai/grok-4.6"`, entry)
	}
	provider, errProvider := parseRelayProvider(providerName)
	if errProvider != nil {
		return "", "", fmt.Errorf("multi-agent-v2 relay model %q: %w", entry, errProvider)
	}
	if model == "" || strings.ContainsAny(model, "*?") {
		return "", "", fmt.Errorf("multi-agent-v2 relay model %q must name exactly one model", entry)
	}
	return provider, model, nil
}

func parseRelayProvider(raw string) (relayProvider, error) {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return "", fmt.Errorf("provider %q is empty or has surrounding whitespace", raw)
	}
	provider := relayProvider(raw)
	switch provider {
	case relayProviderXAI, relayProviderKimi, relayProviderDeepSeek:
		return provider, nil
	default:
		return "", fmt.Errorf("provider %q is unsupported; expected xai, kimi or deepseek", raw)
	}
}

func addExactModel(target map[string]struct{}, model, group string) error {
	trimmed := strings.TrimSpace(model)
	if trimmed == "" || trimmed != model {
		return fmt.Errorf("%s model %q is empty or has surrounding whitespace", group, model)
	}
	if strings.ContainsAny(model, "*?") {
		return fmt.Errorf("%s model %q contains a wildcard", group, model)
	}
	if _, duplicate := target[model]; duplicate {
		return fmt.Errorf("duplicate %s model %q", group, model)
	}
	target[model] = struct{}{}
	return nil
}

func (t routeTable) classify(model string) (route, error) {
	_, stock := t.stock[model]
	_, relay := t.relay[model]
	if t.mode == discoveryAuto {
		// A pinned stock model always wins, so a Relay catalog that starts
		// advertising an official model name cannot divert that conversation to a
		// third party. Discovery drops such collisions too; this is the backstop.
		switch {
		case stock:
			return routeOfficial, nil
		case relay:
			return routeRelay, nil
		default:
			// Anything Relay does not serve belongs to the official arm, which
			// validates the model itself. Root no longer rejects it locally.
			return routeOfficial, nil
		}
	}
	switch {
	case stock && relay:
		return 0, fmt.Errorf("model %q has overlapping routes", model)
	case stock:
		return routeOfficial, nil
	case relay:
		return routeRelay, nil
	default:
		return 0, fmt.Errorf("model %q is not configured", model)
	}
}

func (t routeTable) relayProvider(model string) relayProvider {
	return t.relayProviders[model]
}

// routeResolver holds the active route table plus the configured pins needed to
// rebuild it when Relay discovery reports a new catalog. Reads are lock-free so
// a refresh never blocks an in-flight request.
type routeResolver struct {
	mode              discoveryMode
	stockPin          map[string]struct{}
	relayPin          map[string]struct{}
	providerOverrides map[string]relayProvider
	current           atomic.Pointer[routeTable]
}

func newRouteResolver(mode discoveryMode, stockModels, relayModels []string, configuredProviders map[string]string) (*routeResolver, error) {
	table, errTable := buildRouteTableForMode(mode, stockModels, relayModels, configuredProviders)
	if errTable != nil {
		return nil, errTable
	}
	resolver := &routeResolver{
		mode:              mode,
		stockPin:          table.stock,
		relayPin:          table.relay,
		providerOverrides: table.relayProviders,
	}
	resolver.current.Store(&table)
	return resolver, nil
}

func (r *routeResolver) table() routeTable {
	if r == nil {
		return routeTable{}
	}
	if current := r.current.Load(); current != nil {
		return *current
	}
	return routeTable{}
}

func (r *routeResolver) classify(model string) (route, error) {
	return r.table().classify(model)
}

func (r *routeResolver) relayProvider(model string) relayProvider {
	return r.table().relayProvider(model)
}

// applyRelayCatalog rebuilds the routing snapshot from a freshly discovered
// Relay catalog. It reports the models that were dropped because a pinned stock
// model claims the same identifier. It is a no-op in static mode.
func (r *routeResolver) applyRelayCatalog(models []discoveredRelayModel) (accepted []string, collisions []string) {
	if r == nil || r.mode != discoveryAuto {
		return nil, nil
	}
	relay := make(map[string]struct{}, len(models))
	providers := make(map[string]relayProvider, len(models))
	windows := make(map[string]int, len(models))
	for _, model := range models {
		if _, pinnedStock := r.stockPin[model.id]; pinnedStock {
			collisions = append(collisions, model.id)
			continue
		}
		// An explicit relay-models pin narrows the discovered catalog.
		if len(r.relayPin) > 0 {
			if _, pinned := r.relayPin[model.id]; !pinned {
				continue
			}
		}
		relay[model.id] = struct{}{}
		// A configured provider overrides the catalog's owned_by attribution.
		if override, ok := r.providerOverrides[model.id]; ok {
			providers[model.id] = override
		} else if model.provider != "" {
			providers[model.id] = model.provider
		}
		if model.contextWindow > 0 {
			windows[model.id] = model.contextWindow
		}
		accepted = append(accepted, model.id)
	}
	table := routeTable{
		mode:           r.mode,
		stock:          r.stockPin,
		relay:          relay,
		relayProviders: providers,
		relayWindows:   windows,
	}
	r.current.Store(&table)
	return accepted, collisions
}
