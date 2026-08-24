package quotawindow

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestStoreSnapshotDoesNotUseAuthJSONSuffix(t *testing.T) {
	store := NewStore(t.TempDir(), NewLedger())
	if store == nil {
		t.Fatal("NewStore() = nil")
	}
	if extension := filepath.Ext(store.path); extension == ".json" {
		t.Fatalf("snapshot extension = %q; auth-dir watchers treat .json as credentials", extension)
	}
}

func TestStoreLoadRejectsUnsupportedOrCorruptSnapshots(t *testing.T) {
	limit := int64(10)
	instance := Instance{
		ID:       "codex|workday|2026-08-21T16:00:00Z",
		Schedule: "codex|provider",
		Name:     "workday",
		StartsAt: time.Date(2026, time.August, 21, 16, 0, 0, 0, time.UTC),
		EndsAt:   time.Date(2026, time.August, 22, 1, 0, 0, 0, time.UTC),
		Budget:   &config.QuotaBudget{Requests: &limit},
		Persist:  true,
	}
	record := CounterRecord{
		BudgetKey:     "credential|codex|credential|gpt-5",
		Provider:      "codex",
		Scope:         "credential",
		ClientModel:   "gpt-5",
		UpstreamModel: "gpt-5",
		Credential:    "credential",
		Instance:      instance,
		Budget:        instance.Budget,
		Persist:       true,
	}
	record.Key = record.BudgetKey + "|" + record.Instance.ID

	tests := []struct {
		name    string
		version int
		records []CounterRecord
		mutate  func(*CounterRecord)
	}{
		{name: "missing version", version: 0, records: []CounterRecord{record}},
		{name: "mismatched key", version: 1, records: []CounterRecord{record}, mutate: func(record *CounterRecord) { record.Key = "wrong" }},
		{name: "negative usage", version: 1, records: []CounterRecord{record}, mutate: func(record *CounterRecord) { record.Used.Requests = -1 }},
		{name: "duplicate key", version: 1, records: []CounterRecord{record, record}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			store := NewStore(dir, NewLedger())
			records := append([]CounterRecord(nil), test.records...)
			if test.mutate != nil {
				test.mutate(&records[0])
			}
			payload, errMarshal := json.Marshal(map[string]any{"version": test.version, "counters": records})
			if errMarshal != nil {
				t.Fatalf("Marshal() error = %v", errMarshal)
			}
			if errWrite := os.WriteFile(store.path, payload, 0o600); errWrite != nil {
				t.Fatalf("WriteFile() error = %v", errWrite)
			}
			if _, errLoad := store.Load(); errLoad == nil {
				t.Fatal("Load() error = nil, want rejection")
			}
		})
	}
}

func TestStoreScheduleFlushesUnderSustainedChanges(t *testing.T) {
	store := NewStore(t.TempDir(), NewLedger())
	store.delay = 20 * time.Millisecond
	store.maxDelay = 60 * time.Millisecond
	defer func() {
		if errClose := store.Close(); errClose != nil {
			t.Errorf("Close() error = %v", errClose)
		}
	}()

	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		store.Schedule()
		if _, errStat := os.Stat(store.path); errStat == nil {
			break
		} else if !os.IsNotExist(errStat) {
			t.Fatalf("Stat() error = %v", errStat)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, errStat := os.Stat(store.path); errStat != nil {
		t.Fatalf("snapshot was not flushed during sustained changes: %v", errStat)
	}

	debounced := NewStore(t.TempDir(), NewLedger())
	debounced.delay = 20 * time.Millisecond
	debounced.maxDelay = 60 * time.Millisecond
	defer func() {
		if errClose := debounced.Close(); errClose != nil {
			t.Errorf("Close() error = %v", errClose)
		}
	}()
	debounced.Schedule()
	if _, errStat := os.Stat(debounced.path); !os.IsNotExist(errStat) {
		t.Fatalf("snapshot exists before debounce elapsed: %v", errStat)
	}
	time.Sleep(40 * time.Millisecond)
	if _, errStat := os.Stat(debounced.path); errStat != nil {
		t.Fatalf("snapshot missing after debounce elapsed: %v", errStat)
	}
}
