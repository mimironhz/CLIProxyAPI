package rootproxy

import "testing"

func TestParseRelayProviderAcceptsDeepSeek(t *testing.T) {
	provider, errParse := parseRelayProvider("deepseek")
	if errParse != nil {
		t.Fatalf("parseRelayProvider(\"deepseek\") error = %v", errParse)
	}
	if provider != relayProviderDeepSeek {
		t.Fatalf("provider = %q, want %q", provider, relayProviderDeepSeek)
	}
}

func TestSanitizeRelayModelMetadataSearchToolByProvider(t *testing.T) {
	tests := map[relayProvider]bool{
		relayProviderXAI:      true,
		relayProviderKimi:     true,
		relayProviderDeepSeek: true,
		relayProvider(""):     false,
	}
	for provider, want := range tests {
		model := map[string]any{"comp_hash": "abc"}
		sanitizeRelayModelMetadata(model, provider)
		if got, ok := model["supports_search_tool"].(bool); !ok || got != want {
			t.Errorf("provider %q supports_search_tool = %#v, want %v", provider, model["supports_search_tool"], want)
		}
	}
}
