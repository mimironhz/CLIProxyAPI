package rootproxy

import (
	"net/http"
	"testing"
)

func TestBuildOfficialHeadersUsesPositiveAllowlist(t *testing.T) {
	inbound := http.Header{
		"Authorization":                          {"Bearer desktop-oauth"},
		"ChatGPT-Account-ID":                     {"account-1"},
		"Cookie":                                 {"session=secret"},
		"OpenAI-Beta":                            {"responses_websockets=2026-02-06"},
		"OpenAI-Organization":                    {"org-secret"},
		"Originator":                             {"Codex Desktop"},
		"Session-Id":                             {"session-hyphen"},
		"Session_id":                             {"session-1"},
		"Thread-Id":                              {"thread-1"},
		"User-Agent":                             {"codex-desktop/test"},
		"X-Api-Key":                              {"alternate-secret"},
		"X-Codex-Api-Key":                        {"codex-prefix-secret"},
		"X-Codex-Parent-Thread-Id":               {"parent-1"},
		"X-Codex-Turn-State":                     {"turn-state"},
		"X-Forwarded-For":                        {"203.0.113.10"},
		"X-OAI-Attestation":                      {"attestation-secret"},
		"X-OpenAI-FedRAMP":                       {"true"},
		"X-OpenAI-Internal-Codex-Residency":      {"us"},
		"X-OpenAI-Internal-Codex-Responses-Lite": {"true"},
		"X-OpenAI-Subagent":                      {"collab_spawn"},
		"X-ResponsesAPI-Include-Timing-Metrics":  {"true"},
	}
	got, errBuild := buildUpstreamHeaders(inbound, routeOfficial, "relay-secret")
	if errBuild != nil {
		t.Fatalf("buildUpstreamHeaders() error = %v", errBuild)
	}
	assertHeader(t, got, "Authorization", "Bearer desktop-oauth")
	assertHeader(t, got, "ChatGPT-Account-ID", "account-1")
	assertHeader(t, got, "Originator", "Codex Desktop")
	assertHeader(t, got, "Session-Id", "session-hyphen")
	assertHeader(t, got, "Thread-Id", "thread-1")
	assertHeader(t, got, "X-Codex-Parent-Thread-Id", "parent-1")
	assertHeader(t, got, "X-Codex-Turn-State", "turn-state")
	assertHeader(t, got, "X-OAI-Attestation", "attestation-secret")
	assertHeader(t, got, "X-OpenAI-FedRAMP", "true")
	assertHeader(t, got, "X-OpenAI-Internal-Codex-Residency", "us")
	assertHeader(t, got, "X-OpenAI-Internal-Codex-Responses-Lite", "true")
	assertHeader(t, got, "X-OpenAI-Subagent", "collab_spawn")
	assertHeaderAbsent(t, got, rootHopHeader)
	for _, removed := range []string{"Cookie", "OpenAI-Organization", "X-Api-Key", "X-Codex-Api-Key", "X-Forwarded-For"} {
		if gotValue := got.Get(removed); gotValue != "" {
			t.Fatalf("%s leaked with value %q", removed, gotValue)
		}
	}
}

func TestBuildRelayHeadersReplacesDesktopCredentials(t *testing.T) {
	inbound := http.Header{
		"Authorization":                     {"Bearer desktop-oauth"},
		"ChatGPT-Account-ID":                {"account-1"},
		"Cookie":                            {"session=secret"},
		"OpenAI-Organization":               {"org-secret"},
		"OpenAI-Project":                    {"project-secret"},
		"Origin":                            {"https://chatgpt.com"},
		"Originator":                        {"Codex Desktop"},
		"Proxy-Authorization":               {"Basic proxy-secret"},
		"Sec-WebSocket-Protocol":            {"secret-protocol"},
		"Session_id":                        {"session-1"},
		"X-Api-Key":                         {"alternate-secret"},
		"X-Codex-Api-Key":                   {"codex-prefix-secret"},
		"X-Codex-Turn-State":                {"turn-state"},
		"X-Goog-Api-Key":                    {"google-secret"},
		"X-OAI-Attestation":                 {"attestation-secret"},
		"X-OpenAI-FedRAMP":                  {"true"},
		"X-OpenAI-Internal-Codex-Residency": {"us"},
	}
	got, errBuild := buildUpstreamHeaders(inbound, routeRelay, "relay-secret")
	if errBuild != nil {
		t.Fatalf("buildUpstreamHeaders() error = %v", errBuild)
	}
	assertHeader(t, got, "Authorization", "Bearer relay-secret")
	assertHeader(t, got, "Originator", "Codex Desktop")
	assertHeader(t, got, "X-Codex-Turn-State", "turn-state")
	for _, removed := range []string{
		"ChatGPT-Account-ID",
		"Cookie",
		"OpenAI-Organization",
		"OpenAI-Project",
		"Origin",
		"Proxy-Authorization",
		"Sec-WebSocket-Protocol",
		"X-Api-Key",
		"X-Codex-Api-Key",
		"X-Goog-Api-Key",
		"X-OAI-Attestation",
		"X-OpenAI-FedRAMP",
		"X-OpenAI-Internal-Codex-Residency",
	} {
		if gotValue := got.Get(removed); gotValue != "" {
			t.Fatalf("%s leaked with value %q", removed, gotValue)
		}
	}
}

func TestBuildOfficialHeadersRequiresUnambiguousBearer(t *testing.T) {
	tests := map[string]http.Header{
		"missing":  {},
		"basic":    {"Authorization": {"Basic abc"}},
		"multiple": {"Authorization": {"Bearer first", "Bearer second"}},
	}
	for name, headers := range tests {
		t.Run(name, func(t *testing.T) {
			if _, errBuild := buildUpstreamHeaders(headers, routeOfficial, "relay-secret"); errBuild == nil {
				t.Fatal("buildUpstreamHeaders() succeeded")
			}
		})
	}
}

func TestBuildRelayHeadersRequiresInboundBearer(t *testing.T) {
	if _, errBuild := buildUpstreamHeaders(http.Header{}, routeRelay, "relay-secret"); errBuild == nil {
		t.Fatal("buildUpstreamHeaders() succeeded without Desktop bearer")
	}
}

func assertHeader(t *testing.T, headers http.Header, name, want string) {
	t.Helper()
	if got := headers.Get(name); got != want {
		t.Fatalf("%s = %q, want %q", name, got, want)
	}
}
