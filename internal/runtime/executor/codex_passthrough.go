package executor

import (
	"encoding/base64"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// codexBearerFromHeaders extracts the bearer value from an Authorization header.
func codexBearerFromHeaders(headers http.Header) string {
	if headers == nil {
		return ""
	}
	value := strings.TrimSpace(headers.Get("Authorization"))
	if value == "" {
		return ""
	}
	const prefix = "bearer "
	if len(value) < len(prefix) || !strings.EqualFold(value[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(value[len(prefix):])
}

// codexLooksLikeOAuthToken reports whether a bearer is a JWT, which is the shape
// ChatGPT OAuth access tokens use. This is the guard that keeps a configured
// proxy API key — which clients also send as a bearer — from being forwarded
// upstream as if it were a ChatGPT credential.
func codexLooksLikeOAuthToken(token string) bool {
	token = strings.TrimSpace(token)
	segments := strings.Split(token, ".")
	if len(segments) != 3 {
		return false
	}
	for _, segment := range segments[:2] {
		if segment == "" {
			return false
		}
		if _, err := base64.RawURLEncoding.DecodeString(segment); err != nil {
			return false
		}
	}
	// The header segment must decode to JSON, not arbitrary base64.
	header, err := base64.RawURLEncoding.DecodeString(segments[0])
	if err != nil {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(string(header)), "{")
}

// codexPassthroughToken returns the inbound client's ChatGPT OAuth bearer when
// passthrough is enabled and the client actually presented one. An empty result
// means callers must fall back to the stored credential's token.
//
// This is deliberately confined to the Codex executors: a ChatGPT token must
// never be attached to an xAI request.
func codexPassthroughToken(cfg *config.Config, ginHeaders http.Header) string {
	if cfg == nil || !cfg.Codex.PassthroughClientToken {
		return ""
	}
	token := codexBearerFromHeaders(ginHeaders)
	if token == "" || !codexLooksLikeOAuthToken(token) {
		return ""
	}
	return token
}
