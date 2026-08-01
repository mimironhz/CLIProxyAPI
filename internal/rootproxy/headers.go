package rootproxy

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

const rootHopHeader = "X-CLIProxy-Root-Hop"

var websocketMetadataHeaders = map[string]struct{}{
	"conversation_id":                        {},
	"idempotency-key":                        {},
	"openai-beta":                            {},
	"originator":                             {},
	"session-id":                             {},
	"session_id":                             {},
	"thread-id":                              {},
	"traceparent":                            {},
	"tracestate":                             {},
	"user-agent":                             {},
	"version":                                {},
	"x-client-request-id":                    {},
	"x-codex-beta-features":                  {},
	"x-codex-installation-id":                {},
	"x-codex-parent-thread-id":               {},
	"x-codex-turn-metadata":                  {},
	"x-codex-turn-state":                     {},
	"x-codex-window-id":                      {},
	"x-openai-internal-codex-responses-lite": {},
	"x-openai-memgen-request":                {},
	"x-openai-subagent":                      {},
	"x-request-id":                           {},
	"x-responsesapi-include-timing-metrics":  {},
}

var officialOnlyMetadataHeaders = []string{
	"X-OAI-Attestation",
	"X-OpenAI-FedRAMP",
	"X-OpenAI-Internal-Codex-Residency",
}

func buildUpstreamHeaders(inbound http.Header, selected route, relayAPIKey string) (http.Header, error) {
	result := make(http.Header)
	for name, values := range inbound {
		lowerName := strings.ToLower(strings.TrimSpace(name))
		if _, allowed := websocketMetadataHeaders[lowerName]; !allowed {
			continue
		}
		for _, value := range values {
			result.Add(name, value)
		}
	}
	authorization, errAuthorization := singleBearerAuthorization(inbound)
	if errAuthorization != nil {
		return nil, errAuthorization
	}

	switch selected {
	case routeOfficial:
		result.Set("Authorization", authorization)
		if errCopy := copyAtMostOneHeader(result, inbound, "ChatGPT-Account-ID"); errCopy != nil {
			return nil, errCopy
		}
		for _, name := range officialOnlyMetadataHeaders {
			if errCopy := copyAtMostOneHeader(result, inbound, name); errCopy != nil {
				return nil, errCopy
			}
		}
	case routeRelay:
		relayAPIKey = strings.TrimSpace(relayAPIKey)
		if relayAPIKey == "" {
			return nil, errors.New("relay API key is empty")
		}
		result.Set("Authorization", "Bearer "+relayAPIKey)
		result.Set(rootHopHeader, "1")
	default:
		return nil, fmt.Errorf("unsupported route %d", selected)
	}

	return result, nil
}

func singleBearerAuthorization(headers http.Header) (string, error) {
	values := headerValuesCaseInsensitive(headers, "Authorization")
	if len(values) != 1 {
		return "", errors.New("official route requires exactly one Authorization header")
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" || strings.Contains(parts[1], ",") {
		return "", errors.New("official route requires a Bearer Authorization header")
	}
	return "Bearer " + parts[1], nil
}

func copyAtMostOneHeader(target, source http.Header, name string) error {
	values := headerValuesCaseInsensitive(source, name)
	if len(values) > 1 {
		return fmt.Errorf("official route received multiple %s headers", name)
	}
	if len(values) == 1 && strings.TrimSpace(values[0]) != "" {
		target.Set(name, values[0])
	}
	return nil
}

func headerValuesCaseInsensitive(headers http.Header, name string) []string {
	var values []string
	for candidate, candidateValues := range headers {
		if strings.EqualFold(candidate, name) {
			values = append(values, candidateValues...)
		}
	}
	return values
}
