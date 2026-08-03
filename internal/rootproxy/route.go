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
		accepted = append(accepted, model.id)
	}
	table := routeTable{
		mode:           r.mode,
		stock:          r.stockPin,
		relay:          relay,
		relayProviders: providers,
	}
	r.current.Store(&table)
	return accepted, collisions
}
