package quotawindow

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestLedgerAdmitAndSettleReservedInstance(t *testing.T) {
	requestLimit := int64(1)
	tokenLimit := int64(10)
	now := time.Date(2026, time.August, 21, 12, 0, 0, 0, time.UTC)
	instance := Instance{ID: "instance-a", Name: "peak", StartsAt: now.Add(-time.Hour), EndsAt: now.Add(time.Hour), Persist: true}
	budget := &config.QuotaBudget{Requests: &requestLimit, TotalTokens: &tokenLimit}
	ledger := NewLedger()
	record := CounterRecord{BudgetKey: "provider|deepseek|deepseek-chat", Provider: "deepseek", Scope: "provider", Instance: instance, Budget: budget, Persist: true}
	reservation, admitted := ledger.Admit(record, now)
	if !admitted || reservation == "" {
		t.Fatalf("Admit() = %q, %t; want reservation", reservation, admitted)
	}
	if _, admittedAgain := ledger.Admit(record, now); admittedAgain {
		t.Fatal("second Admit() succeeded past request limit")
	}
	if !ledger.Settle(reservation, coreusage.Detail{TotalTokens: 12}) {
		t.Fatal("Settle() = false")
	}
	used, exhausted := ledger.Snapshot(record.BudgetKey, instance, budget)
	if used.Requests != 1 || used.TotalTokens != 12 {
		t.Fatalf("Snapshot() used = %#v", used)
	}
	if len(exhausted) != 2 || exhausted[0] != "requests" || exhausted[1] != "total-tokens" {
		t.Fatalf("Snapshot() exhausted = %v", exhausted)
	}
}

func TestLedgerRetainsReservedInstanceAcrossReloadBoundary(t *testing.T) {
	tokenLimit := int64(10)
	now := time.Date(2026, time.August, 21, 16, 29, 0, 0, time.UTC)
	instance := Instance{ID: "peak-instance", Schedule: "deepseek|provider", Name: "peak", StartsAt: now.Add(-time.Hour), EndsAt: now.Add(time.Minute), Persist: true}
	budget := &config.QuotaBudget{TotalTokens: &tokenLimit}
	ledger := NewLedger()
	record := CounterRecord{BudgetKey: "provider|deepseek|deepseek-chat", Provider: "deepseek", Scope: "provider", Instance: instance, Budget: budget, Persist: true}
	reservation, admitted := ledger.Admit(record, now)
	if !admitted {
		t.Fatal("Admit() = false")
	}
	ledger.Reconcile(map[string][]Instance{}, map[string]struct{}{instance.Schedule: {}})
	if !ledger.Settle(reservation, coreusage.Detail{TotalTokens: 7}) {
		t.Fatal("Settle() lost the reserved instance across reload boundary")
	}
	used, _ := ledger.Snapshot(record.BudgetKey, instance, budget)
	if used.TotalTokens != 7 {
		t.Fatalf("Snapshot() total tokens = %d, want 7", used.TotalTokens)
	}
}

func TestLedgerReconcileUsesExactScheduleForSharedInstanceID(t *testing.T) {
	now := time.Now().UTC()
	limitA, limitB := int64(10), int64(20)
	instanceA := Instance{ID: "provider|peak|start", Schedule: "provider|model:a", Name: "peak", StartsAt: now.Add(-time.Hour), EndsAt: now.Add(time.Hour), Budget: &config.QuotaBudget{Requests: &limitA}, Persist: true}
	instanceB := Instance{ID: instanceA.ID, Schedule: "provider|model:b", Name: "peak", StartsAt: instanceA.StartsAt, EndsAt: instanceA.EndsAt, Budget: &config.QuotaBudget{Requests: &limitB}, Persist: false}
	ledger := NewLedger()
	if _, admitted := ledger.Admit(CounterRecord{BudgetKey: "key-a", Instance: instanceA, Budget: instanceA.Budget, Persist: instanceA.Persist}, now); !admitted {
		t.Fatal("Admit(key-a) = false")
	}
	if _, admitted := ledger.Admit(CounterRecord{BudgetKey: "key-b", Instance: instanceB, Budget: instanceB.Budget, Persist: instanceB.Persist}, now); !admitted {
		t.Fatal("Admit(key-b) = false")
	}

	ledger.Reconcile(map[string][]Instance{instanceA.ID: {instanceB, instanceA}}, map[string]struct{}{instanceA.Schedule: {}, instanceB.Schedule: {}})
	records := ledger.Records(false)
	if len(records) != 2 {
		t.Fatalf("records = %d, want 2", len(records))
	}
	for _, record := range records {
		switch record.BudgetKey {
		case "key-a":
			if record.Instance.Schedule != instanceA.Schedule || record.Budget.Requests == nil || *record.Budget.Requests != limitA || !record.Persist {
				t.Fatalf("key-a reconciled from wrong schedule: %#v", record)
			}
		case "key-b":
			if record.Instance.Schedule != instanceB.Schedule || record.Budget.Requests == nil || *record.Budget.Requests != limitB || record.Persist {
				t.Fatalf("key-b reconciled from wrong schedule: %#v", record)
			}
		}
	}
}

func TestLedgerReconcileDoesNotUseUnrelatedModelSchedule(t *testing.T) {
	now := time.Now().UTC()
	limit := int64(10)
	instanceA := Instance{ID: "provider|peak|start", Schedule: "provider|model:a", Name: "peak", StartsAt: now.Add(-time.Hour), EndsAt: now.Add(time.Hour), Budget: &config.QuotaBudget{Requests: &limit}, Persist: true}
	instanceB := instanceA
	instanceB.Schedule = "provider|model:b"
	ledger := NewLedger()
	if _, admitted := ledger.Admit(CounterRecord{BudgetKey: "key-a", Provider: "provider", ClientModel: "a", ClientModels: []string{"a"}, Instance: instanceA, Budget: instanceA.Budget, Persist: true}, now); !admitted {
		t.Fatal("Admit() = false")
	}
	ledger.Reconcile(map[string][]Instance{instanceA.ID: {instanceB}}, map[string]struct{}{instanceB.Schedule: {}})
	if records := ledger.Records(false); len(records) != 0 {
		t.Fatalf("records = %#v, want removed counter", records)
	}
}
