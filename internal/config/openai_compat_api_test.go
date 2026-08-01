package config

import "testing"

func TestNormalizeOpenAICompatAPI(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "", want: OpenAICompatAPIChatCompletions},
		{in: "responses", want: OpenAICompatAPIResponses},
		{in: "  Responses  ", want: OpenAICompatAPIResponses},
		{in: "chat-completions", want: OpenAICompatAPIChatCompletions},
		{in: "response", want: OpenAICompatAPIChatCompletions},
	}
	for _, tt := range tests {
		if got := NormalizeOpenAICompatAPI(tt.in); got != tt.want {
			t.Errorf("NormalizeOpenAICompatAPI(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// A typo must not silently look like the default, so sanitization canonicalizes
// the value it will actually act on.
func TestSanitizeOpenAICompatibilityNormalizesAPI(t *testing.T) {
	cfg := &Config{OpenAICompatibility: []OpenAICompatibility{
		{Name: "deepseek", BaseURL: "https://api.deepseek.com/v1", API: " RESPONSES "},
		{Name: "typo", BaseURL: "https://example.com/v1", API: "respones"},
		{Name: "default", BaseURL: "https://example.com/v1"},
	}}
	cfg.SanitizeOpenAICompatibility()

	if len(cfg.OpenAICompatibility) != 3 {
		t.Fatalf("expected 3 providers, got %d", len(cfg.OpenAICompatibility))
	}
	if got := cfg.OpenAICompatibility[0].API; got != OpenAICompatAPIResponses {
		t.Errorf("provider 0 api = %q, want %q", got, OpenAICompatAPIResponses)
	}
	if got := cfg.OpenAICompatibility[1].API; got != OpenAICompatAPIChatCompletions {
		t.Errorf("provider 1 api = %q, want %q", got, OpenAICompatAPIChatCompletions)
	}
	if got := cfg.OpenAICompatibility[2].API; got != "" {
		t.Errorf("provider 2 api = %q, want it left unset", got)
	}
}
