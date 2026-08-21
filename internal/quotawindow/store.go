package quotawindow

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	log "github.com/sirupsen/logrus"
)

// Use a non-.json suffix because auth-dir watchers treat every direct .json file as a credential.
const snapshotFileName = "provider-quota-windows.qws"

// Store persists ledger snapshots with debounced atomic replacement.
type Store struct {
	path   string
	ledger *Ledger
	delay  time.Duration

	mu      sync.Mutex
	flushMu sync.Mutex
	timer   *time.Timer
	closed  bool
}

func NewStore(authDir string, ledger *Ledger) *Store {
	authDir = filepath.Clean(authDir)
	if authDir == "." || authDir == "" {
		return nil
	}
	return &Store{
		path:   filepath.Join(authDir, snapshotFileName),
		ledger: ledger,
		delay:  250 * time.Millisecond,
	}
}

func (s *Store) Load() ([]CounterRecord, error) {
	if s == nil || s.path == "" {
		return nil, nil
	}
	data, errRead := os.ReadFile(s.path)
	if errRead != nil {
		if os.IsNotExist(errRead) {
			return nil, nil
		}
		return nil, fmt.Errorf("read quota-window snapshot: %w", errRead)
	}
	var snapshot struct {
		Version  int             `json:"version"`
		Counters []CounterRecord `json:"counters"`
	}
	if errDecode := json.Unmarshal(data, &snapshot); errDecode != nil {
		return nil, fmt.Errorf("decode quota-window snapshot: %w", errDecode)
	}
	if snapshot.Version != 1 {
		return nil, fmt.Errorf("decode quota-window snapshot: unsupported version %d", snapshot.Version)
	}
	if errValidate := validateSnapshotCounters(snapshot.Counters); errValidate != nil {
		return nil, fmt.Errorf("validate quota-window snapshot: %w", errValidate)
	}
	return snapshot.Counters, nil
}

func validateSnapshotCounters(records []CounterRecord) error {
	seen := make(map[string]struct{}, len(records))
	for index := range records {
		record := records[index]
		if strings.TrimSpace(record.BudgetKey) == "" || strings.TrimSpace(record.Provider) == "" || strings.TrimSpace(record.UpstreamModel) == "" {
			return fmt.Errorf("counter %d has incomplete budget identity", index)
		}
		if record.Scope != "provider" && record.Scope != "credential" {
			return fmt.Errorf("counter %d has invalid scope %q", index, record.Scope)
		}
		if strings.TrimSpace(record.Instance.ID) == "" || strings.TrimSpace(record.Instance.Schedule) == "" || strings.TrimSpace(record.Instance.Name) == "" || record.Instance.StartsAt.IsZero() || !record.Instance.EndsAt.After(record.Instance.StartsAt) {
			return fmt.Errorf("counter %d has invalid window instance", index)
		}
		canonicalKey := record.BudgetKey + "|" + record.Instance.ID
		if record.Key != canonicalKey {
			return fmt.Errorf("counter %d key %q does not match %q", index, record.Key, canonicalKey)
		}
		if _, duplicate := seen[canonicalKey]; duplicate {
			return fmt.Errorf("counter %d duplicates key %q", index, canonicalKey)
		}
		seen[canonicalKey] = struct{}{}
		if record.Budget == nil || record.Instance.Budget == nil || !reflect.DeepEqual(record.Budget, record.Instance.Budget) {
			return fmt.Errorf("counter %d has inconsistent budget", index)
		}
		if quotaBudgetHasNegative(record.Budget) {
			return fmt.Errorf("counter %d has negative budget", index)
		}
		if record.Used.Requests < 0 || record.Used.InputTokens < 0 || record.Used.OutputTokens < 0 || record.Used.TotalTokens < 0 {
			return fmt.Errorf("counter %d has negative usage", index)
		}
	}
	return nil
}

func quotaBudgetHasNegative(budget *config.QuotaBudget) bool {
	if budget == nil {
		return false
	}
	for _, value := range []*int64{budget.Requests, budget.InputTokens, budget.OutputTokens, budget.TotalTokens} {
		if value != nil && *value < 0 {
			return true
		}
	}
	return false
}

func (s *Store) Schedule() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if s.timer != nil {
		s.timer.Stop()
	}
	s.timer = time.AfterFunc(s.delay, func() {
		if errFlush := s.Flush(); errFlush != nil {
			log.WithError(errFlush).Warn("failed to persist provider quota-window ledger")
		}
	})
	s.mu.Unlock()
}

func (s *Store) Flush() error {
	if s == nil || s.path == "" || s.ledger == nil {
		return nil
	}
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	records := s.ledger.Records(true)
	payload, errMarshal := json.MarshalIndent(struct {
		Version  int             `json:"version"`
		Counters []CounterRecord `json:"counters"`
	}{Version: 1, Counters: records}, "", "  ")
	if errMarshal != nil {
		return fmt.Errorf("marshal quota-window snapshot: %w", errMarshal)
	}
	if errMkdir := os.MkdirAll(filepath.Dir(s.path), 0o755); errMkdir != nil {
		return fmt.Errorf("create quota-window snapshot directory: %w", errMkdir)
	}
	temp, errTemp := os.CreateTemp(filepath.Dir(s.path), ".provider-quota-windows-*.tmp")
	if errTemp != nil {
		return fmt.Errorf("create quota-window snapshot temp file: %w", errTemp)
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if errChmod := temp.Chmod(0o600); errChmod != nil {
		_ = temp.Close()
		return fmt.Errorf("set quota-window snapshot permissions: %w", errChmod)
	}
	if _, errWrite := temp.Write(append(payload, '\n')); errWrite != nil {
		_ = temp.Close()
		return fmt.Errorf("write quota-window snapshot: %w", errWrite)
	}
	if errSync := temp.Sync(); errSync != nil {
		_ = temp.Close()
		return fmt.Errorf("sync quota-window snapshot: %w", errSync)
	}
	if errClose := temp.Close(); errClose != nil {
		return fmt.Errorf("close quota-window snapshot: %w", errClose)
	}
	if errRename := os.Rename(tempPath, s.path); errRename != nil {
		return fmt.Errorf("replace quota-window snapshot: %w", errRename)
	}
	return nil
}

func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	s.closed = true
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.mu.Unlock()
	return s.Flush()
}
