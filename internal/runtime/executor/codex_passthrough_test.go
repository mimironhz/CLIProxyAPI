package executor

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// fakeOAuthToken builds a JWT-shaped bearer, which is the shape ChatGPT OAuth
// access tokens use and the only shape passthrough is allowed to forward.
func fakeOAuthToken(t *testing.T) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"user-1"}`))
	return header + "." + payload + ".c2ln"
}

func TestCodexLooksLikeOAuthToken(t *testing.T) {
	if !codexLooksLikeOAuthToken(fakeOAuthToken(t)) {
		t.Fatalf("codexLooksLikeOAuthToken(jwt) = false, want true")
	}
	for _, token := range []string{
		"",
		"sk-proxy-api-key",
		"not.a.jwt",
		"only.two",
		"a.b.c.d",
	} {
		if codexLooksLikeOAuthToken(token) {
			t.Fatalf("codexLooksLikeOAuthToken(%q) = true, want false", token)
		}
	}
}

func TestCodexPassthroughTokenRequiresFlagAndJWT(t *testing.T) {
	token := fakeOAuthToken(t)
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+token)

	disabled := &config.Config{}
	if got := codexPassthroughToken(disabled, headers); got != "" {
		t.Fatalf("passthrough token with flag off = %q, want empty", got)
	}

	enabled := &config.Config{}
	enabled.Codex.PassthroughClientToken = true
	if got := codexPassthroughToken(enabled, headers); got != token {
		t.Fatalf("passthrough token = %q, want %q", got, token)
	}

	// A configured proxy API key is also presented as a bearer and must never be
	// forwarded upstream as if it were a ChatGPT credential.
	apiKeyHeaders := http.Header{}
	apiKeyHeaders.Set("Authorization", "Bearer sk-my-proxy-key")
	if got := codexPassthroughToken(enabled, apiKeyHeaders); got != "" {
		t.Fatalf("passthrough forwarded a non-JWT bearer = %q, want empty", got)
	}

	if got := codexPassthroughToken(enabled, nil); got != "" {
		t.Fatalf("passthrough token without headers = %q, want empty", got)
	}
}

func TestApplyCodexHeadersUsesPassthroughBearer(t *testing.T) {
	clientToken := fakeOAuthToken(t)
	ginHeaders := http.Header{}
	ginHeaders.Set("Authorization", "Bearer "+clientToken)
	ginHeaders.Set("Chatgpt-Account-Id", "acct-from-client")

	cfg := &config.Config{}
	cfg.Codex.PassthroughClientToken = true

	auth := &cliproxyauth.Auth{
		Provider:   "codex",
		Attributes: map[string]string{"api_key": "placeholder"},
		Metadata:   map[string]any{"account_id": "acct-from-store"},
	}

	req, err := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	applyCodexHeadersFromSources(req, auth, "stored-token", true, cfg, ginHeaders)

	if got := req.Header.Get("Authorization"); got != "Bearer "+clientToken {
		t.Fatalf("Authorization = %q, want the client bearer", got)
	}
	// The placeholder credential is an api-key, but a passthrough request is
	// OAuth and must carry OAuth identity headers.
	if got := req.Header.Get("Chatgpt-Account-Id"); got != "acct-from-client" {
		t.Fatalf("Chatgpt-Account-Id = %q, want acct-from-client", got)
	}
	if got := req.Header.Get("Originator"); got != codexOriginator {
		t.Fatalf("Originator = %q, want %q", got, codexOriginator)
	}
}

func TestApplyCodexHeadersKeepsStoredTokenWhenPassthroughDisabled(t *testing.T) {
	ginHeaders := http.Header{}
	ginHeaders.Set("Authorization", "Bearer "+fakeOAuthToken(t))

	req, err := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	applyCodexHeadersFromSources(req, &cliproxyauth.Auth{Provider: "codex"}, "stored-token", true, &config.Config{}, ginHeaders)

	if got := req.Header.Get("Authorization"); got != "Bearer stored-token" {
		t.Fatalf("Authorization = %q, want the stored token", got)
	}
}

// The passthrough bearer belongs to ChatGPT and must never reach xAI, whose
// executor authenticates with its own SuperGrok credential.
func TestXAIExecutorIgnoresPassthroughBearer(t *testing.T) {
	var gotAuthorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":0,\"status\":\"completed\",\"model\":\"grok-4.3\",\"output\":[]}}\n\n"))
	}))
	defer server.Close()

	cfg := &config.Config{}
	cfg.Codex.PassthroughClientToken = true

	ginRequest, err := http.NewRequest(http.MethodPost, "http://127.0.0.1/v1/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	ginRequest.Header.Set("Authorization", "Bearer "+fakeOAuthToken(t))

	exec := NewXAIExecutor(cfg)
	auth := &cliproxyauth.Auth{
		Provider:   "xai",
		Attributes: map[string]string{"base_url": server.URL},
		Metadata:   map[string]any{"access_token": "xai-token"},
	}

	result, err := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "grok-4.3",
		Payload: []byte(`{"model":"grok-4.3","input":[{"role":"user","content":"hello"}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
	}

	if !strings.Contains(gotAuthorization, "xai-token") {
		t.Fatalf("xAI Authorization = %q, want the xAI credential", gotAuthorization)
	}
}
