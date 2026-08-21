package quotawindow

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestGateCredentialScopeRotatesAndPersists(t *testing.T) {
	requestLimit := int64(1)
	cfg := &config.Config{ProviderQuota: map[string]config.ProviderQuota{"codex": {
		Scope: "credential",
		QuotaWindows: config.QuotaWindows{Timezone: "UTC", Windows: []config.QuotaWindow{{
			Name: "all-day", Start: "00:00", End: "23:59", Budget: &config.QuotaBudget{Requests: &requestLimit},
		}}},
	}}}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	authA := &coreauth.Auth{ID: "a", Provider: "codex", FileName: filepath.Join(t.TempDir(), "a.json")}
	authB := &coreauth.Auth{ID: "b", Provider: "codex", FileName: filepath.Join(t.TempDir(), "b.json")}
	authDir := t.TempDir()
	gate, errNew := New(cfg, manager, authDir)
	if errNew != nil {
		t.Fatalf("New() error = %v", errNew)
	}
	now := time.Date(2026, time.August, 21, 12, 0, 0, 0, time.UTC)
	if _, admitted := gate.Admit(authA, "gpt-5", now); !admitted {
		t.Fatal("Admit(authA) = false")
	}
	if _, blocked := gate.BlockedForModel([]*coreauth.Auth{authA, authB}, "gpt-5", now); blocked {
		t.Fatal("BlockedForModel() blocked while authB still has budget")
	}
	if _, admitted := gate.Admit(authB, "gpt-5", now); !admitted {
		t.Fatal("Admit(authB) = false")
	}
	block, blocked := gate.BlockedForModel([]*coreauth.Auth{authA, authB}, "gpt-5", now)
	if !blocked || block.Provider != "codex" {
		t.Fatalf("BlockedForModel() = %#v, %t", block, blocked)
	}
	if errClose := gate.Close(); errClose != nil {
		t.Fatalf("Close() error = %v", errClose)
	}

	restored, errRestore := New(cfg, manager, authDir)
	if errRestore != nil {
		t.Fatalf("restored New() error = %v", errRestore)
	}
	defer restored.Close()
	if _, blocked := restored.BlockedForModel([]*coreauth.Auth{authA, authB}, "gpt-5", now); !blocked {
		t.Fatal("restored BlockedForModel() = false")
	}
}

func TestModelSnapshotsComposeCredentialQuotaWithCooldown(t *testing.T) {
	requestLimit := int64(1)
	cfg := &config.Config{ProviderQuota: map[string]config.ProviderQuota{"codex": {
		Scope: "credential",
		QuotaWindows: config.QuotaWindows{Timezone: "UTC", Windows: []config.QuotaWindow{{
			Name: "all-day", Start: "00:00", End: "23:59", Budget: &config.QuotaBudget{Requests: &requestLimit},
		}}},
	}}}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	now := time.Date(2026, time.August, 21, 12, 0, 0, 0, time.UTC)
	authA := &coreauth.Auth{ID: "a", Provider: "codex", FileName: "a.json", Status: coreauth.StatusActive}
	authB := &coreauth.Auth{ID: "b", Provider: "codex", FileName: "b.json", Status: coreauth.StatusActive, Quota: coreauth.QuotaState{Exceeded: true, NextRecoverAt: now.Add(time.Hour)}}
	gate, errNew := New(cfg, manager, t.TempDir())
	if errNew != nil {
		t.Fatalf("New() error = %v", errNew)
	}
	defer gate.Close()
	if _, admitted := gate.Admit(authA, "gpt-5", now); !admitted {
		t.Fatal("Admit(authA) = false")
	}
	statuses := gate.ModelSnapshots([]string{"gpt-5"}, []*coreauth.Auth{authA, authB}, func(*coreauth.Auth, string) bool { return true }, now, nil)
	if len(statuses) != 1 || statuses[0].Available || statuses[0].Reason == nil || *statuses[0].Reason != "model_cooldown" {
		t.Fatalf("ModelSnapshots() = %#v, want model_cooldown", statuses)
	}
}

func TestUsagePluginCombinesPrimaryAndAdditionalModelTokens(t *testing.T) {
	requestLimit := int64(10)
	cfg := &config.Config{ProviderQuota: map[string]config.ProviderQuota{"codex": {
		QuotaWindows: config.QuotaWindows{Timezone: "UTC", Windows: []config.QuotaWindow{{
			Name: "all-day", Start: "00:00", End: "23:59", Budget: &config.QuotaBudget{Requests: &requestLimit},
		}}},
	}}}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	auth := &coreauth.Auth{ID: "codex-image-usage", Provider: "codex", FileName: "codex-image.json", Status: coreauth.StatusActive}
	gate, errNew := New(cfg, manager, t.TempDir())
	if errNew != nil {
		t.Fatalf("New() error = %v", errNew)
	}
	defer gate.Close()
	manager.SetQuotaWindowGate(gate)
	attemptCtx, errAdmit := manager.AdmitQuotaWindowAttempt(context.Background(), auth, "gpt-5")
	if errAdmit != nil {
		t.Fatalf("AdmitQuotaWindowAttempt() error = %v", errAdmit)
	}
	plugin := gate.UsagePlugin().(*UsagePlugin)
	plugin.HandleUsage(attemptCtx, coreusage.Record{Detail: coreusage.Detail{InputTokens: 3, OutputTokens: 4, TotalTokens: 7}, AdditionalUsagePending: true})
	plugin.HandleUsage(attemptCtx, coreusage.Record{Detail: coreusage.Detail{InputTokens: 5, OutputTokens: 6, TotalTokens: 11}, AdditionalUsage: true})

	resolved, configured := gate.resolve(auth, "gpt-5")
	if !configured {
		t.Fatal("quota target is not configured")
	}
	instance, active := resolved.schedule.InstanceAt(time.Now())
	if !active {
		t.Fatal("quota window is not active")
	}
	used, _ := gate.ledger.Snapshot(resolved.budgetKey, instance, instance.Budget)
	if used.InputTokens != 8 || used.OutputTokens != 10 || used.TotalTokens != 18 {
		t.Fatalf("combined usage = %+v", used)
	}
}

func TestGateNoCrossProviderFallthrough(t *testing.T) {
	zero := int64(0)
	cfg := &config.Config{ProviderQuota: map[string]config.ProviderQuota{
		"codex": {
			QuotaWindows: config.QuotaWindows{Timezone: "UTC", Windows: []config.QuotaWindow{{Name: "closed", Start: "00:00", End: "23:59", Budget: &config.QuotaBudget{Requests: &zero}}}},
		},
	}}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	gate, errNew := New(cfg, manager, t.TempDir())
	if errNew != nil {
		t.Fatalf("New() error = %v", errNew)
	}
	defer gate.Close()
	now := time.Date(2026, time.August, 21, 12, 0, 0, 0, time.UTC)
	auths := []*coreauth.Auth{{ID: "codex", Provider: "codex", FileName: "codex.json"}, {ID: "claude", Provider: "claude", FileName: "claude.json"}}
	block, blocked := gate.BlockedForModel(auths, "shared-alias", now)
	if !blocked || block.Provider != "codex" {
		t.Fatalf("BlockedForModel() = %#v, %t; want codex block", block, blocked)
	}
}

func TestGateAdmissionRechecksAllBackingProviders(t *testing.T) {
	requestLimit := int64(1)
	cfg := &config.Config{ProviderQuota: map[string]config.ProviderQuota{"codex": {
		QuotaWindows: config.QuotaWindows{Timezone: "UTC", Windows: []config.QuotaWindow{{
			Name: "all-day", Start: "00:00", End: "23:59", Budget: &config.QuotaBudget{Requests: &requestLimit},
		}}},
	}}}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	authA := &coreauth.Auth{ID: "admission-codex", Provider: "codex", FileName: "codex.json", Status: coreauth.StatusActive}
	authB := &coreauth.Auth{ID: "admission-claude", Provider: "claude", FileName: "claude.json", Status: coreauth.StatusActive}
	for _, auth := range []*coreauth.Auth{authA, authB} {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, errRegister)
		}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "shared-model"}})
		defer registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	}
	gate, errNew := New(cfg, manager, t.TempDir())
	if errNew != nil {
		t.Fatalf("New() error = %v", errNew)
	}
	defer gate.Close()
	now := time.Date(2026, time.August, 21, 12, 0, 0, 0, time.UTC)
	if _, admitted := gate.Admit(authA, "shared-model", now); !admitted {
		t.Fatal("Admit(authA) = false")
	}
	if _, admitted := gate.Admit(authB, "shared-model", now); admitted {
		t.Fatal("Admit(authB) = true after another backing provider exhausted")
	}
}

func TestGateAliasesWithSameUpstreamShareModelOverrideBudget(t *testing.T) {
	requestLimit := int64(1)
	windows := config.QuotaWindows{Timezone: "UTC", Windows: []config.QuotaWindow{{
		Name: "all-day", Start: "00:00", End: "23:59", Budget: &config.QuotaBudget{Requests: &requestLimit},
	}}}
	cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{
		Name: "deepseek",
		Models: []config.OpenAICompatibilityModel{
			{Name: "deepseek-chat", Alias: "alias-a", Quota: &windows},
			{Name: "deepseek-chat", Alias: "alias-b", Quota: &windows},
		},
	}}}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	auth := &coreauth.Auth{
		ID:       "deepseek-key",
		Provider: "openai-compatible-deepseek",
		Attributes: map[string]string{
			"api_key":      "test-key",
			"compat_name":  "deepseek",
			"provider_key": "openai-compatible-deepseek",
		},
	}
	gate, errNew := New(cfg, manager, t.TempDir())
	if errNew != nil {
		t.Fatalf("New() error = %v", errNew)
	}
	defer gate.Close()
	now := time.Date(2026, time.August, 21, 12, 0, 0, 0, time.UTC)
	if _, admitted := gate.Admit(auth, "alias-a", now); !admitted {
		t.Fatal("Admit(alias-a) = false")
	}
	records := gate.ledger.Records(false)
	if len(records) != 1 || len(records[0].ClientModels) != 2 {
		t.Fatalf("shared counter models = %#v, want both aliases", records)
	}
	if _, blocked := gate.BlockedForModel([]*coreauth.Auth{auth}, "alias-b", now); !blocked {
		t.Fatal("alias-b did not share alias-a's exhausted upstream budget")
	}
	statuses := gate.ModelSnapshots([]string{"alias-a", "alias-b"}, []*coreauth.Auth{auth}, func(*coreauth.Auth, string) bool { return true }, now, nil)
	if len(statuses) != 2 || len(statuses[0].SharesBudgetWith) != 1 || len(statuses[1].SharesBudgetWith) != 1 {
		t.Fatalf("shared-budget statuses = %#v", statuses)
	}
}

func TestGateRejectsConflictingSchedulesForSharedUpstreamAliases(t *testing.T) {
	zero := int64(0)
	one := int64(1)
	cfg := &config.Config{
		ProviderQuota: map[string]config.ProviderQuota{"deepseek": {Models: map[string]config.QuotaWindows{
			"alias-a": {Windows: []config.QuotaWindow{{Name: "peak", Start: "00:00", End: "12:00", Budget: &config.QuotaBudget{Requests: &zero}}}},
			"alias-b": {Windows: []config.QuotaWindow{{Name: "peak", Start: "00:00", End: "12:00", Budget: &config.QuotaBudget{Requests: &one}}}},
		}}},
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name: "deepseek",
			Models: []config.OpenAICompatibilityModel{
				{Name: "deepseek-chat", Alias: "alias-a"},
				{Name: "deepseek-chat", Alias: "alias-b"},
			},
		}},
	}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	if _, errNew := New(cfg, manager, t.TempDir()); errNew == nil {
		t.Fatal("New() error = nil, want conflicting shared-upstream schedules rejected")
	}
}

func TestGatePropagatesSingleOverrideAcrossSharedUpstreamAliases(t *testing.T) {
	zero := int64(0)
	windows := config.QuotaWindows{Timezone: "UTC", Windows: []config.QuotaWindow{{
		Name: "closed", Start: "00:00", End: "23:59", Budget: &config.QuotaBudget{Requests: &zero},
	}}}
	cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{
		Name: "deepseek",
		Models: []config.OpenAICompatibilityModel{
			{Name: "deepseek-chat", Alias: "alias-a", Quota: &windows},
			{Name: "deepseek-chat", Alias: "alias-b"},
		},
	}}}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	auth := &coreauth.Auth{ID: "deepseek-shared-single", Provider: "openai-compatible-deepseek", Attributes: map[string]string{
		"api_key": "test-key", "compat_name": "deepseek", "provider_key": "openai-compatible-deepseek",
	}}
	gate, errNew := New(cfg, manager, t.TempDir())
	if errNew != nil {
		t.Fatalf("New() error = %v", errNew)
	}
	defer gate.Close()
	now := time.Date(2026, time.August, 21, 12, 0, 0, 0, time.UTC)
	if _, blocked := gate.BlockedForModel([]*coreauth.Auth{auth}, "alias-b", now); !blocked {
		t.Fatal("alias without its own override bypassed the shared upstream override")
	}
}

func TestGateTopLevelModelOverrideUsesPrefixedClientModel(t *testing.T) {
	zero := int64(0)
	cfg := &config.Config{ProviderQuota: map[string]config.ProviderQuota{"codex": {
		Models: map[string]config.QuotaWindows{"team-a/gpt-5": {Timezone: "UTC", Windows: []config.QuotaWindow{{
			Name: "closed", Start: "00:00", End: "23:59", Budget: &config.QuotaBudget{Requests: &zero},
		}}}},
	}}}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	auth := &coreauth.Auth{ID: "codex-prefixed", Provider: "codex", Prefix: "team-a", FileName: "codex.json"}
	target := manager.ResolveQuotaWindowTarget(auth, "team-a/gpt-5")
	if target.ClientModel != "team-a/gpt-5" || target.UpstreamModel != "gpt-5" {
		t.Fatalf("ResolveQuotaWindowTarget() = %#v", target)
	}
	gate, errNew := New(cfg, manager, t.TempDir())
	if errNew != nil {
		t.Fatalf("New() error = %v", errNew)
	}
	defer gate.Close()
	now := time.Date(2026, time.August, 21, 12, 0, 0, 0, time.UTC)
	if _, blocked := gate.BlockedForModel([]*coreauth.Auth{auth}, "team-a/gpt-5", now); !blocked {
		t.Fatal("prefixed client-model override was not applied")
	}
}

func TestGateInlineModelOverrideIncludesProviderPrefix(t *testing.T) {
	zero := int64(0)
	windows := config.QuotaWindows{Timezone: "UTC", Windows: []config.QuotaWindow{{
		Name: "closed", Start: "00:00", End: "23:59", Budget: &config.QuotaBudget{Requests: &zero},
	}}}
	cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{
		Name: "deepseek", Prefix: "team-a", Models: []config.OpenAICompatibilityModel{{Name: "deepseek-chat", Alias: "chat", Quota: &windows}},
	}}}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	auth := &coreauth.Auth{ID: "deepseek-prefixed", Provider: "openai-compatible-deepseek", Prefix: "team-a", Attributes: map[string]string{
		"compat_name": "deepseek", "provider_key": "openai-compatible-deepseek",
	}}
	gate, errNew := New(cfg, manager, t.TempDir())
	if errNew != nil {
		t.Fatalf("New() error = %v", errNew)
	}
	defer gate.Close()
	now := time.Date(2026, time.August, 21, 12, 0, 0, 0, time.UTC)
	if _, blocked := gate.BlockedForModel([]*coreauth.Auth{auth}, "team-a/chat", now); !blocked {
		t.Fatal("prefixed inline client-model override was not applied")
	}
}

func TestGateMovesPersistenceToReloadedAuthDir(t *testing.T) {
	oldDir := t.TempDir()
	newDir := t.TempDir()
	cfg := &config.Config{AuthDir: oldDir}
	gate, errNew := New(cfg, nil, oldDir)
	if errNew != nil {
		t.Fatalf("New() error = %v", errNew)
	}
	cfgReloaded := &config.Config{AuthDir: newDir}
	if errUpdate := gate.Update(cfgReloaded); errUpdate != nil {
		t.Fatalf("Update() error = %v", errUpdate)
	}
	if _, errStat := os.Stat(filepath.Join(newDir, snapshotFileName)); errStat != nil {
		t.Fatalf("new auth-dir snapshot missing: %v", errStat)
	}
	if errClose := gate.Close(); errClose != nil {
		t.Fatalf("Close() error = %v", errClose)
	}
}

func TestModelSnapshotsProviderFilterOmitsUnbackedModels(t *testing.T) {
	cfg := &config.Config{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	gate, errNew := New(cfg, manager, t.TempDir())
	if errNew != nil {
		t.Fatalf("New() error = %v", errNew)
	}
	defer gate.Close()
	auth := &coreauth.Auth{ID: "codex", Provider: "codex"}
	statuses := gate.ModelSnapshots(
		[]string{"gpt-5", "claude-sonnet"},
		[]*coreauth.Auth{auth},
		func(_ *coreauth.Auth, model string) bool { return model == "gpt-5" },
		time.Now(),
		[]string{"codex"},
	)
	if len(statuses) != 1 || statuses[0].Model != "gpt-5" {
		t.Fatalf("filtered statuses = %#v, want only gpt-5", statuses)
	}
}
