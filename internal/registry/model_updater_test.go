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
