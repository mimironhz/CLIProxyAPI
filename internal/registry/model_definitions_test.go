package registry

import "testing"

func TestModelOverrideHeadersFromEmbeddedModels(t *testing.T) {
	const wantUA = "codex-tui/0.144.0 (Mac OS 26.5.1; arm64) iTerm.app/3.6.11 (codex-tui; 0.144.0)"
	got := ModelOverrideHeaders("gpt-5.6-luna")
	if got == nil {
		t.Fatal("ModelOverrideHeaders(gpt-5.6-luna) = nil, want headers")
	}
	if got["user-agent"] != wantUA {
		t.Fatalf("user-agent = %q, want %q", got["user-agent"], wantUA)
	}
	if got := ModelOverrideHeaders("gpt-5.4"); got != nil {
		t.Fatalf("ModelOverrideHeaders(gpt-5.4) = %#v, want nil", got)
	}
}

// DeepSeek's own API accepts only "high" and "max" as reasoning_effort; "low"
// and "medium" collapse to "high" and "xhigh" to "max". Advertising the aliases
// would offer Codex four levels that behave as two, so the static definitions
// carry the effective pair.
func TestDeepSeekModelsExposeOfficialLimits(t *testing.T) {
	for _, modelID := range []string{"deepseek-v4-flash", "deepseek-v4-pro"} {
		info := LookupStaticModelInfo(modelID)
		if info == nil {
			t.Fatalf("LookupStaticModelInfo(%q) = nil, want model info", modelID)
		}
		if info.DisplayName == "" || info.DisplayName == modelID {
			t.Fatalf("%s display name = %q, want a readable name", modelID, info.DisplayName)
		}
		if info.ContextLength != 1048576 {
			t.Fatalf("%s context length = %d, want 1048576", modelID, info.ContextLength)
		}
		if info.MaxCompletionTokens != 393216 {
			t.Fatalf("%s max completion tokens = %d, want 393216", modelID, info.MaxCompletionTokens)
		}
		if info.Thinking == nil {
			t.Fatalf("%s thinking support = nil, want levels", modelID)
		}
		if want := []string{"high", "max"}; len(info.Thinking.Levels) != len(want) {
			t.Fatalf("%s thinking levels = %v, want %v", modelID, info.Thinking.Levels, want)
		} else {
			for index, level := range want {
				if info.Thinking.Levels[index] != level {
					t.Fatalf("%s thinking levels = %v, want %v", modelID, info.Thinking.Levels, want)
				}
			}
		}
	}
}

func TestGeminiVertexModelsUseFlashLiteReleaseID(t *testing.T) {
	const releaseID = "gemini-3.1-flash-lite"
	const previewID = releaseID + "-preview"

	for _, model := range GetGeminiVertexModels() {
		if model == nil {
			continue
		}
		if model.ID == previewID {
			t.Fatalf("Vertex model ID = %q, want release ID %q", model.ID, releaseID)
		}
		if model.ID == releaseID {
			return
		}
	}

	t.Fatalf("Vertex models do not contain %q", releaseID)
}

func TestWithXAIBuiltinsIncludesVideoPreviewModel(t *testing.T) {
	models := WithXAIBuiltins(nil)

	for _, model := range models {
		if model == nil {
			continue
		}
		if model.ID == xaiBuiltinVideo15PreviewModelID {
			return
		}
	}

	t.Fatalf("expected xAI builtin model %s", xaiBuiltinVideo15PreviewModelID)
}

func TestAntigravityWebSearchModelForRequiresRequestedModelCapability(t *testing.T) {
	registryRef := GetGlobalRegistry()
	registryRef.RegisterClient("test-antigravity-websearch-route", "antigravity", []*ModelInfo{
		{ID: "gemini-route-test"},
		{ID: "gemini-web-search-test", SupportsWebSearch: true},
	})
	registryRef.RegisterClient("test-gemini-websearch-route", "gemini", []*ModelInfo{
		{ID: "gemini-cross-provider-route"},
		{ID: "gemini-cross-provider-search", SupportsWebSearch: true},
	})
	t.Cleanup(func() {
		registryRef.UnregisterClient("test-antigravity-websearch-route")
		registryRef.UnregisterClient("test-gemini-websearch-route")
	})

	if got := AntigravityWebSearchModelFor("gemini-route-test"); got != "" {
		t.Fatalf("route model without web search support should not get fallback model, got %q", got)
	}
	if got := AntigravityWebSearchModelFor("gemini-route-test(high)"); got != "" {
		t.Fatalf("suffix route model without web search support should not get fallback model, got %q", got)
	}
	if got := AntigravityWebSearchModelFor("gemini-web-search-test"); got != "gemini-web-search-test" {
		t.Fatalf("AntigravityWebSearchModelFor capable model = %q, want itself", got)
	}
	if got := AntigravityWebSearchModelFor("gemini-cross-provider-route"); got != "" {
		t.Fatalf("cross-provider model should not get Antigravity web search model, got %q", got)
	}
	if got := AntigravityWebSearchModelFor("unknown-model"); got != "" {
		t.Fatalf("unknown model should not get Antigravity web search model, got %q", got)
	}
}
