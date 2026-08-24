package config

import "testing"

func TestValidateProviderQuota(t *testing.T) {
	zero := int64(0)
	one := int64(1)
	negative := int64(-1)
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{
			name: "valid wrapping window and explicit zero",
			cfg: Config{ProviderQuota: map[string]ProviderQuota{"codex": {
				QuotaWindows: QuotaWindows{Timezone: "UTC", Windows: []QuotaWindow{{Name: "peak", Start: "16:30", End: "00:30", Budget: &QuotaBudget{Requests: &zero}}}},
			}}},
		},
		{
			name:    "unknown provider",
			cfg:     Config{ProviderQuota: map[string]ProviderQuota{"typo-provider": {}}},
			wantErr: true,
		},
		{
			name: "empty schedule still validates timezone",
			cfg: Config{ProviderQuota: map[string]ProviderQuota{"codex": {
				QuotaWindows: QuotaWindows{Timezone: "Not/A_Timezone"},
			}}},
			wantErr: true,
		},
		{
			name: "overlap",
			cfg: Config{ProviderQuota: map[string]ProviderQuota{"codex": {QuotaWindows: QuotaWindows{Windows: []QuotaWindow{
				{Name: "one", Start: "09:00", End: "12:00"},
				{Name: "two", Start: "11:00", End: "13:00"},
			}}}}},
			wantErr: true,
		},
		{
			name: "negative budget",
			cfg: Config{ProviderQuota: map[string]ProviderQuota{"codex": {QuotaWindows: QuotaWindows{Windows: []QuotaWindow{
				{Name: "workday", Start: "09:00", End: "17:00", Budget: &QuotaBudget{InputTokens: &negative}},
			}}}}},
			wantErr: true,
		},
		{
			name: "inline openai compatibility",
			cfg: Config{OpenAICompatibility: []OpenAICompatibility{{Name: "deepseek", Quota: &ProviderQuota{
				QuotaWindows: QuotaWindows{Windows: []QuotaWindow{{Name: "peak", Start: "00:30", End: "16:30", Budget: &QuotaBudget{Requests: &zero}}}},
			}}}},
		},
		{
			name: "normalized duplicate provider",
			cfg: Config{ProviderQuota: map[string]ProviderQuota{
				"codex":   {},
				" CODEX ": {},
			}},
			wantErr: true,
		},
		{
			name: "normalized duplicate model override",
			cfg: Config{ProviderQuota: map[string]ProviderQuota{"codex": {Models: map[string]QuotaWindows{
				"gpt-5":   {},
				" GPT-5 ": {},
			}}}},
			wantErr: true,
		},
		{
			name: "home rejects quota windows without authoritative credential inventory",
			cfg: Config{
				Home: HomeConfig{Enabled: true},
				ProviderQuota: map[string]ProviderQuota{"codex": {QuotaWindows: QuotaWindows{Windows: []QuotaWindow{{
					Name: "workday", Start: "09:00", End: "17:00", Budget: &QuotaBudget{Requests: &zero},
				}}}}},
			},
			wantErr: true,
		},
		{
			name: "shared upstream aliases reject conflicting schedules before runtime commit",
			cfg: Config{
				ProviderQuota: map[string]ProviderQuota{"deepseek": {Models: map[string]QuotaWindows{
					"alias-a": {Windows: []QuotaWindow{{Name: "peak", Start: "00:00", End: "12:00", Budget: &QuotaBudget{Requests: &zero}}}},
					"alias-b": {Windows: []QuotaWindow{{Name: "peak", Start: "00:00", End: "12:00"}}},
				}}},
				OpenAICompatibility: []OpenAICompatibility{{Name: "deepseek", Models: []OpenAICompatibilityModel{
					{Name: "deepseek-chat", Alias: "alias-a"},
					{Name: "deepseek-chat", Alias: "alias-b"},
				}}},
			},
			wantErr: true,
		},
		{
			name: "provider base conflicts with prefixed override on shared upstream",
			cfg: Config{
				ProviderQuota: map[string]ProviderQuota{"codex": {
					QuotaWindows: QuotaWindows{Timezone: "UTC", Windows: []QuotaWindow{{Name: "base", Start: "00:00", End: "23:59", Budget: &QuotaBudget{Requests: &one}}}},
					Models:       map[string]QuotaWindows{"team-a/gpt-5": {Timezone: "UTC", Windows: []QuotaWindow{{Name: "override", Start: "00:00", End: "23:59", Budget: &QuotaBudget{Requests: &zero}}}}},
				}},
				CodexKey: []CodexKey{
					{APIKey: "base", Models: []CodexModel{{Name: "gpt-5", Alias: "gpt-5"}}},
					{APIKey: "prefixed", Prefix: "team-a", Models: []CodexModel{{Name: "gpt-5", Alias: "gpt-5"}}},
				},
			},
			wantErr: true,
		},
		{
			name: "provider base and prefixed override may share identical schedule",
			cfg: Config{
				ProviderQuota: map[string]ProviderQuota{"codex": {
					QuotaWindows: QuotaWindows{Timezone: "UTC", Windows: []QuotaWindow{{Name: "shared", Start: "00:00", End: "23:59", Budget: &QuotaBudget{Requests: &one}}}},
					Models:       map[string]QuotaWindows{"team-a/gpt-5": {Timezone: "UTC", Windows: []QuotaWindow{{Name: "shared", Start: "00:00", End: "23:59", Budget: &QuotaBudget{Requests: &one}}}}},
				}},
				CodexKey: []CodexKey{
					{APIKey: "base", Models: []CodexModel{{Name: "gpt-5", Alias: "gpt-5"}}},
					{APIKey: "prefixed", Prefix: "team-a", Models: []CodexModel{{Name: "gpt-5", Alias: "gpt-5"}}},
				},
			},
		},
		{
			name: "compat quota rejects built in provider identity collision",
			cfg: Config{
				ProviderQuota:       map[string]ProviderQuota{"codex": {QuotaWindows: QuotaWindows{Windows: []QuotaWindow{{Name: "workday", Start: "09:00", End: "17:00"}}}}},
				OpenAICompatibility: []OpenAICompatibility{{Name: "codex"}},
			},
			wantErr: true,
		},
		{
			name: "quota relevant duplicate compat names",
			cfg: Config{
				ProviderQuota: map[string]ProviderQuota{"deepseek": {QuotaWindows: QuotaWindows{Windows: []QuotaWindow{{Name: "peak", Start: "00:00", End: "12:00"}}}}},
				OpenAICompatibility: []OpenAICompatibility{
					{Name: "deepseek"},
					{Name: " DEEPSEEK "},
				},
			},
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.cfg.ValidateProviderQuota()
			if (err != nil) != test.wantErr {
				t.Fatalf("ValidateProviderQuota() error = %v, wantErr = %t", err, test.wantErr)
			}
		})
	}
}

func TestCanonicalQuotaModelsStripsThinkingSuffixes(t *testing.T) {
	if got := CanonicalQuotaModels([]string{"gpt-5(high)", " GPT-5 "}, ""); got != "gpt-5" {
		t.Fatalf("CanonicalQuotaModels() = %q, want gpt-5", got)
	}
}

func TestQuotaWindowsPolicyKeyNormalizesDefaultsAndDayOrder(t *testing.T) {
	persist := true
	left := QuotaWindows{Windows: []QuotaWindow{{Name: "peak", Start: "09:00", End: "17:00", Days: []string{"fri", "mon"}}}}
	right := QuotaWindows{Timezone: "UTC", Persist: &persist, Windows: []QuotaWindow{{Name: "peak", Start: "09:00", End: "17:00", Days: []string{"mon", "fri"}}}}
	if leftKey, rightKey := QuotaWindowsPolicyKey(left), QuotaWindowsPolicyKey(right); leftKey != rightKey {
		t.Fatalf("policy keys differ: %q != %q", leftKey, rightKey)
	}
}
