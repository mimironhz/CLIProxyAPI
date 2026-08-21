package config

import "testing"

func TestValidateProviderQuota(t *testing.T) {
	zero := int64(0)
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
