package api

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/quotawindow"
)

func TestFilterQuotaModelStatusesNarrowsToRequestedModels(t *testing.T) {
	statuses := []quotawindow.ModelStatus{
		{Model: "gpt-5", SharesBudgetWith: []string{"gpt-5-codex"}},
		{Model: "deepseek-v4"},
	}
	if filtered := filterQuotaModelStatuses(statuses, nil); len(filtered) != 2 {
		t.Fatalf("unfiltered statuses = %#v", filtered)
	}
	filtered := filterQuotaModelStatuses(statuses, []string{" GPT-5 "})
	if len(filtered) != 1 || filtered[0].Model != "gpt-5" {
		t.Fatalf("filtered statuses = %#v, want gpt-5", filtered)
	}
	if len(filtered[0].SharesBudgetWith) != 1 || filtered[0].SharesBudgetWith[0] != "gpt-5-codex" {
		t.Fatalf("shares_budget_with = %#v, want sibling preserved", filtered[0].SharesBudgetWith)
	}
	if filtered := filterQuotaModelStatuses(statuses, []string{"unknown"}); len(filtered) != 0 {
		t.Fatalf("unknown filtered statuses = %#v, want empty", filtered)
	}
}
