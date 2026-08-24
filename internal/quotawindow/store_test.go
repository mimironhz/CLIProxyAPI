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

func TestStorePersistsWhenAuthDirIsCurrentDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	store := NewStore(".", NewLedger())
	if store == nil {
		t.Fatal("NewStore(.) = nil")
	}
	if errFlush := store.Flush(); errFlush != nil {
		t.Fatalf("Flush() error = %v", errFlush)
	}
	if _, errStat := os.Stat(filepath.Join(dir, snapshotFileName)); errStat != nil {
		t.Fatalf("snapshot missing from current directory: %v", errStat)
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
	store.delay = 50 * time.Millisecond
	store.maxDelay = 200 * time.Millisecond
	defer func() {
		if errClose := store.Close(); errClose != nil {
			t.Errorf("Close() error = %v", errClose)
		}
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		store.Schedule()
		if _, errStat := os.Stat(store.path); errStat == nil {
			break
		} else if !os.IsNotExist(errStat) {
			t.Fatalf("Stat() error = %v", errStat)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, errStat := os.Stat(store.path); errStat != nil {
		t.Fatalf("snapshot was not flushed during sustained changes: %v", errStat)
	}

	debounced := NewStore(t.TempDir(), NewLedger())
	debounced.delay = time.Hour
	debounced.maxDelay = 2 * time.Hour
	defer func() {
		if errClose := debounced.Close(); errClose != nil {
			t.Errorf("Close() error = %v", errClose)
		}
	}()
	debounced.Schedule()
	if _, errStat := os.Stat(debounced.path); !os.IsNotExist(errStat) {
		t.Fatalf("snapshot exists before debounce elapsed: %v", errStat)
	}
	debounced.mu.Lock()
	generation := debounced.generation
	if debounced.timer != nil {
		debounced.timer.Stop()
	}
	debounced.mu.Unlock()
	debounced.flushPending(generation)
	if _, errStat := os.Stat(debounced.path); errStat != nil {
		t.Fatalf("snapshot missing after pending flush: %v", errStat)
	}
}

func TestStoreCloseWaitsForPendingFlush(t *testing.T) {
	ledger := NewLedger()
	store := NewStore(t.TempDir(), ledger)
	store.delay = 0
	store.maxDelay = time.Second
	ledger.mu.Lock()
	ledgerLocked := true
	defer func() {
		if ledgerLocked {
			ledger.mu.Unlock()
		}
	}()
	store.Schedule()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if store.flushMu.TryLock() {
			store.flushMu.Unlock()
			if time.Now().After(deadline) {
				t.Fatal("pending flush did not start")
			}
			time.Sleep(time.Millisecond)
			continue
		}
		break
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- store.Close() }()
	for {
		store.mu.Lock()
		closed := store.closed
		store.mu.Unlock()
		if closed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Close() did not mark store closed")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case errClose := <-closeDone:
		t.Fatalf("Close() returned before pending flush completed: %v", errClose)
	default:
	}
	ledger.mu.Unlock()
	ledgerLocked = false
	if errClose := <-closeDone; errClose != nil {
		t.Fatalf("Close() error = %v", errClose)
	}
}
