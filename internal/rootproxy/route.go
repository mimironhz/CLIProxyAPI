package rootproxy

import (
	"errors"
	"fmt"
	"strings"
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

type routeTable struct {
	stock          map[string]struct{}
	relay          map[string]struct{}
	relayProviders map[string]relayProvider
}

func buildRouteTable(stockModels, relayModels []string, configuredProviders map[string]string) (routeTable, error) {
	if len(stockModels) == 0 {
		return routeTable{}, errors.New("routing stock-models must not be empty")
	}
	if len(relayModels) == 0 {
		return routeTable{}, errors.New("routing relay-models must not be empty")
	}

	table := routeTable{
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
		if _, configured := table.relay[model]; !configured {
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
