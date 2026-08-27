package registry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPreserveLocalProviderSectionsKeepsDeepSeekWhenRemoteOmitsIt(t *testing.T) {
	current := &staticModelsJSON{DeepSeek: []*ModelInfo{{ID: "deepseek-v4-pro"}}}
	fetched := &staticModelsJSON{}

	preserveLocalProviderSections(current, fetched)

	if len(fetched.DeepSeek) != 1 || fetched.DeepSeek[0] == nil || fetched.DeepSeek[0].ID != "deepseek-v4-pro" {
		t.Fatalf("DeepSeek models = %+v, want preserved deepseek-v4-pro", fetched.DeepSeek)
	}
	current.DeepSeek[0].ID = "mutated"
	if fetched.DeepSeek[0].ID != "deepseek-v4-pro" {
		t.Fatalf("preserved DeepSeek model shares source storage: got %q", fetched.DeepSeek[0].ID)
	}
	fetched.DeepSeek[0].ID = "deepseek-v4-pro"
	current.DeepSeek[0].ID = "deepseek-v4-pro"
	if changed := detectChangedProviders(current, fetched); len(changed) != 0 {
		t.Fatalf("changed providers = %v, want none after preserving equal DeepSeek data", changed)
	}

	current.DeepSeek[0].ID = "mutated"
	if changed := detectChangedProviders(current, fetched); len(changed) != 1 || changed[0] != "deepseek" {
		t.Fatalf("changed providers = %v, want [deepseek]", changed)
	}
}

func TestPreserveLocalProviderSectionsPrefersRemoteDeepSeek(t *testing.T) {
	current := &staticModelsJSON{DeepSeek: []*ModelInfo{{ID: "local-deepseek"}}}
	fetched := &staticModelsJSON{DeepSeek: []*ModelInfo{{ID: "remote-deepseek"}}}

	preserveLocalProviderSections(current, fetched)

	if len(fetched.DeepSeek) != 1 || fetched.DeepSeek[0] == nil || fetched.DeepSeek[0].ID != "remote-deepseek" {
		t.Fatalf("DeepSeek models = %+v, want remote section", fetched.DeepSeek)
	}
}

func TestPreserveLocalProviderSectionsKeepsGrok46XHighWhenRemoteLags(t *testing.T) {
	current := &staticModelsJSON{XAI: []*ModelInfo{{
		ID:       "grok-4.6",
		Thinking: &ThinkingSupport{Levels: []string{"low", "medium", "high", "xhigh"}},
	}}}
	fetched := &staticModelsJSON{XAI: []*ModelInfo{{
		ID:          "grok-4.6",
		DisplayName: "Remote Grok 4.6",
		Thinking:    &ThinkingSupport{Levels: []string{"low", "medium", "high"}},
	}}}

	preserveLocalProviderSections(current, fetched)

	if got := fetched.XAI[0].DisplayName; got != "Remote Grok 4.6" {
		t.Fatalf("display name = %q, want remote metadata retained", got)
	}
	assertStringSlice(t, "Grok 4.6 thinking levels", fetched.XAI[0].Thinking.Levels, []string{"low", "medium", "high", "xhigh"})
	assertStringSlice(t, "local Grok 4.6 thinking levels", current.XAI[0].Thinking.Levels, []string{"low", "medium", "high", "xhigh"})
}

func TestPreserveLocalProviderSectionsKeepsGrok46WhenRemoteOmitsIt(t *testing.T) {
	current := &staticModelsJSON{XAI: []*ModelInfo{
		{ID: "remote-model"},
		{ID: "grok-4.6", Thinking: &ThinkingSupport{Levels: []string{"low", "medium", "high", "xhigh"}}},
	}}
	fetched := &staticModelsJSON{XAI: []*ModelInfo{{ID: "remote-model"}}}

	preserveLocalProviderSections(current, fetched)

	if len(fetched.XAI) != 2 || fetched.XAI[1] == nil || fetched.XAI[1].ID != "grok-4.6" {
		t.Fatalf("xAI models = %+v, want remote model plus local grok-4.6", fetched.XAI)
	}
	fetched.XAI[1].Thinking.Levels[0] = "mutated"
	if current.XAI[1].Thinking.Levels[0] != "low" {
		t.Fatalf("preserved Grok 4.6 shares source storage: %+v", current.XAI[1].Thinking.Levels)
	}
}

func TestRefreshModelsPreservesDeepSeekWhenRemoteCatalogOmitsIt(t *testing.T) {
	originalCatalog := getModels()
	originalURLs := modelsURLs
	SetModelRefreshCallback(nil)
	t.Cleanup(func() {
		modelsURLs = originalURLs
		modelsCatalogStore.mu.Lock()
		modelsCatalogStore.data = originalCatalog
		modelsCatalogStore.mu.Unlock()
		refreshCallbackMu.Lock()
		pendingRefreshChanges = nil
		refreshCallback = nil
		refreshCallbackMu.Unlock()
	})

	remoteCatalog := *originalCatalog
	remoteCatalog.DeepSeek = nil
	remoteData, errMarshal := json.Marshal(&remoteCatalog)
	if errMarshal != nil {
		t.Fatalf("marshal remote catalog: %v", errMarshal)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(remoteData)
	}))
	defer server.Close()
	modelsURLs = []string{server.URL}

	tryRefreshModels(context.Background(), "test model refresh")

	info := LookupStaticModelInfo("deepseek-v4-pro")
	if info == nil {
		t.Fatal("DeepSeek V4 Pro missing after remote refresh")
	}
	if info.ContextLength != 1048576 || info.MaxCompletionTokens != 393216 {
		t.Fatalf("DeepSeek V4 Pro limits = context %d, completion %d", info.ContextLength, info.MaxCompletionTokens)
	}
	if info.Thinking == nil || len(info.Thinking.Levels) != 2 || info.Thinking.Levels[0] != "high" || info.Thinking.Levels[1] != "max" {
		t.Fatalf("DeepSeek V4 Pro thinking = %+v, want [high max]", info.Thinking)
	}
}

func TestRefreshModelsPreservesGrok46XHighWhenRemoteCatalogLags(t *testing.T) {
	originalCatalog := getModels()
	originalURLs := modelsURLs
	SetModelRefreshCallback(nil)
	t.Cleanup(func() {
		modelsURLs = originalURLs
		modelsCatalogStore.mu.Lock()
		modelsCatalogStore.data = originalCatalog
		modelsCatalogStore.mu.Unlock()
		refreshCallbackMu.Lock()
		pendingRefreshChanges = nil
		refreshCallback = nil
		refreshCallbackMu.Unlock()
	})

	remoteCatalog := *originalCatalog
	remoteCatalog.XAI = cloneModelInfos(originalCatalog.XAI)
	for _, model := range remoteCatalog.XAI {
		if model != nil && model.ID == "grok-4.6" {
			model.Thinking.Levels = []string{"low", "medium", "high"}
		}
	}
	remoteData, errMarshal := json.Marshal(&remoteCatalog)
	if errMarshal != nil {
		t.Fatalf("marshal remote catalog: %v", errMarshal)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(remoteData)
	}))
	defer server.Close()
	modelsURLs = []string{server.URL}

	tryRefreshModels(context.Background(), "test model refresh")

	info := LookupStaticModelInfo("grok-4.6")
	if info == nil || info.Thinking == nil {
		t.Fatal("Grok 4.6 thinking metadata missing after remote refresh")
	}
	assertStringSlice(t, "Grok 4.6 thinking levels after refresh", info.Thinking.Levels, []string{"low", "medium", "high", "xhigh"})
}
