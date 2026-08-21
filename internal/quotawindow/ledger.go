package quotawindow

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

// Usage stores accumulated budget consumption.
type Usage struct {
	Requests     int64 `json:"requests"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}

// CounterRecord is one persisted window-instance counter.
type CounterRecord struct {
	Key           string              `json:"key"`
	BudgetKey     string              `json:"budget_key"`
	Provider      string              `json:"provider"`
	Scope         string              `json:"scope"`
	ClientModel   string              `json:"client_model"`
	ClientModels  []string            `json:"client_models,omitempty"`
	UpstreamModel string              `json:"upstream_model"`
	Credential    string              `json:"credential,omitempty"`
	AuthID        string              `json:"auth_id,omitempty"`
	Instance      Instance            `json:"instance"`
	Budget        *config.QuotaBudget `json:"budget,omitempty"`
	Used          Usage               `json:"used"`
	Persist       bool                `json:"persist"`
}

type reservation struct {
	counterKey string
}

// Ledger owns quota consumption independently from auth registration state.
type Ledger struct {
	mu           sync.RWMutex
	counters     map[string]*CounterRecord
	reservations map[string]reservation
	sequence     atomic.Uint64
	onChange     func()
}

func NewLedger() *Ledger {
	return &Ledger{
		counters:     make(map[string]*CounterRecord),
		reservations: make(map[string]reservation),
	}
}

func (l *Ledger) SetOnChange(onChange func()) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.onChange = onChange
	l.mu.Unlock()
}

func (l *Ledger) Admit(record CounterRecord, now time.Time) (string, bool) {
	if l == nil || record.Budget == nil {
		return "", true
	}
	counterKey := record.BudgetKey + "|" + record.Instance.ID
	l.mu.Lock()
	counter := l.counters[counterKey]
	if counter == nil {
		copyRecord := record
		copyRecord.Key = counterKey
		copyRecord.Budget = cloneBudget(record.Budget)
		copyRecord.Instance.Budget = cloneBudget(record.Instance.Budget)
		counter = &copyRecord
		l.counters[counterKey] = counter
	} else {
		counter.Budget = cloneBudget(record.Budget)
		counter.Instance.Budget = cloneBudget(record.Instance.Budget)
		counter.Persist = record.Persist
	}
	addCounterClientModel(counter, record.ClientModel)
	if len(exhaustedDimensions(counter.Budget, counter.Used)) > 0 {
		l.mu.Unlock()
		return "", false
	}
	counter.Used.Requests++
	reservationID := fmt.Sprintf("%x-%x", now.UnixNano(), l.sequence.Add(1))
	l.reservations[reservationID] = reservation{counterKey: counterKey}
	onChange := l.onChange
	l.mu.Unlock()
	if onChange != nil {
		onChange()
	}
	return reservationID, true
}

func (l *Ledger) Settle(reservationID string, detail coreusage.Detail) bool {
	if l == nil || reservationID == "" {
		return false
	}
	l.mu.Lock()
	reserved, ok := l.reservations[reservationID]
	if !ok {
		l.mu.Unlock()
		return false
	}
	delete(l.reservations, reservationID)
	counter := l.counters[reserved.counterKey]
	if counter != nil {
		counter.Used.InputTokens = saturatingUsageAdd(counter.Used.InputTokens, maxZero(detail.InputTokens))
		counter.Used.OutputTokens = saturatingUsageAdd(counter.Used.OutputTokens, maxZero(detail.OutputTokens))
		counter.Used.TotalTokens = saturatingUsageAdd(counter.Used.TotalTokens, maxZero(detail.TotalTokens))
	}
	onChange := l.onChange
	l.mu.Unlock()
	if onChange != nil && counter != nil {
		onChange()
	}
	return counter != nil
}

func saturatingUsageAdd(current, increment int64) int64 {
	const maxInt64 = int64(^uint64(0) >> 1)
	if increment <= 0 {
		return current
	}
	if current >= maxInt64-increment {
		return maxInt64
	}
	return current + increment
}

func maxZero(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

func (l *Ledger) Snapshot(budgetKey string, instance Instance, budget *config.QuotaBudget) (Usage, []string) {
	if l == nil {
		return Usage{}, exhaustedDimensions(budget, Usage{})
	}
	counterKey := budgetKey + "|" + instance.ID
	l.mu.RLock()
	counter := l.counters[counterKey]
	var used Usage
	if counter != nil {
		used = counter.Used
	}
	l.mu.RUnlock()
	return used, exhaustedDimensions(budget, used)
}

func exhaustedDimensions(budget *config.QuotaBudget, used Usage) []string {
	if budget == nil {
		return nil
	}
	exhausted := make([]string, 0, 4)
	if budget.Requests != nil && used.Requests >= *budget.Requests {
		exhausted = append(exhausted, "requests")
	}
	if budget.InputTokens != nil && used.InputTokens >= *budget.InputTokens {
		exhausted = append(exhausted, "input-tokens")
	}
	if budget.OutputTokens != nil && used.OutputTokens >= *budget.OutputTokens {
		exhausted = append(exhausted, "output-tokens")
	}
	if budget.TotalTokens != nil && used.TotalTokens >= *budget.TotalTokens {
		exhausted = append(exhausted, "total-tokens")
	}
	return exhausted
}

func (l *Ledger) Reconcile(activeInstances map[string][]Instance, knownSchedules map[string]struct{}) {
	if l == nil {
		return
	}
	l.mu.Lock()
	changed := false
	reservedCounters := make(map[string]struct{}, len(l.reservations))
	for _, reserved := range l.reservations {
		reservedCounters[reserved.counterKey] = struct{}{}
	}
	for key, counter := range l.counters {
		instance, keep := reconcileInstance(activeInstances[counter.Instance.ID], counter)
		if !keep {
			_, scheduleStillExists := knownSchedules[counter.Instance.Schedule]
			_, hasInFlightSettlement := reservedCounters[key]
			if scheduleStillExists && hasInFlightSettlement {
				continue
			}
			delete(l.counters, key)
			changed = true
			continue
		}
		if counter.Persist != instance.Persist || counter.Instance.Schedule != instance.Schedule || !reflect.DeepEqual(counter.Budget, instance.Budget) {
			counter.Persist = instance.Persist
			counter.Budget = cloneBudget(instance.Budget)
			counter.Instance = instance
			counter.Instance.Budget = cloneBudget(instance.Budget)
			changed = true
		}
	}
	for id, reserved := range l.reservations {
		if counter := l.counters[reserved.counterKey]; counter == nil {
			delete(l.reservations, id)
		}
	}
	onChange := l.onChange
	l.mu.Unlock()
	if changed && onChange != nil {
		onChange()
	}
}

func reconcileInstance(instances []Instance, counter *CounterRecord) (Instance, bool) {
	if len(instances) == 0 {
		return Instance{}, false
	}
	allowed := make(map[string]struct{})
	if counter != nil {
		allowed[counter.Instance.Schedule] = struct{}{}
		allowed[strings.ToLower(counter.Provider)+"|provider"] = struct{}{}
		for _, model := range counter.ClientModels {
			allowed[strings.ToLower(counter.Provider)+"|model:"+strings.ToLower(strings.TrimSpace(model))] = struct{}{}
		}
		if counter.ClientModel != "" {
			allowed[strings.ToLower(counter.Provider)+"|model:"+strings.ToLower(strings.TrimSpace(counter.ClientModel))] = struct{}{}
		}
	}
	candidates := make([]Instance, 0, len(instances))
	for _, instance := range instances {
		if _, ok := allowed[instance.Schedule]; ok {
			candidates = append(candidates, instance)
		}
	}
	if len(candidates) == 0 {
		return Instance{}, false
	}
	sort.Slice(candidates, func(i, j int) bool {
		if counter != nil {
			leftExact := candidates[i].Schedule == counter.Instance.Schedule
			rightExact := candidates[j].Schedule == counter.Instance.Schedule
			if leftExact != rightExact {
				return leftExact
			}
		}
		return candidates[i].Schedule < candidates[j].Schedule
	})
	return candidates[0], true
}

func (l *Ledger) Replace(records []CounterRecord) error {
	if l == nil {
		return nil
	}
	if errValidate := validateSnapshotCounters(records); errValidate != nil {
		return errValidate
	}
	l.mu.Lock()
	l.counters = make(map[string]*CounterRecord, len(records))
	for i := range records {
		record := records[i]
		if record.Key == "" {
			record.Key = record.BudgetKey + "|" + record.Instance.ID
		}
		copyRecord := record
		copyRecord.ClientModels = append([]string(nil), record.ClientModels...)
		copyRecord.Budget = cloneBudget(record.Budget)
		copyRecord.Instance.Budget = cloneBudget(record.Instance.Budget)
		l.counters[record.Key] = &copyRecord
	}
	l.reservations = make(map[string]reservation)
	l.mu.Unlock()
	return nil
}

func (l *Ledger) Records(persistedOnly bool) []CounterRecord {
	if l == nil {
		return nil
	}
	l.mu.RLock()
	records := make([]CounterRecord, 0, len(l.counters))
	for _, counter := range l.counters {
		if counter == nil || (persistedOnly && !counter.Persist) {
			continue
		}
		copyRecord := *counter
		copyRecord.ClientModels = append([]string(nil), counter.ClientModels...)
		copyRecord.Budget = cloneBudget(counter.Budget)
		copyRecord.Instance.Budget = cloneBudget(counter.Instance.Budget)
		records = append(records, copyRecord)
	}
	l.mu.RUnlock()
	sort.Slice(records, func(i, j int) bool { return records[i].Key < records[j].Key })
	return records
}

func addCounterClientModel(counter *CounterRecord, model string) {
	if counter == nil || model == "" {
		return
	}
	for _, existing := range counter.ClientModels {
		if strings.EqualFold(existing, model) {
			return
		}
	}
	counter.ClientModels = append(counter.ClientModels, model)
	sort.Strings(counter.ClientModels)
}

func (l *Ledger) Reset(match func(CounterRecord) bool) int {
	if l == nil || match == nil {
		return 0
	}
	l.mu.Lock()
	reset := 0
	for _, counter := range l.counters {
		if counter != nil && match(*counter) {
			counter.Used = Usage{}
			reset++
		}
	}
	onChange := l.onChange
	l.mu.Unlock()
	if reset > 0 && onChange != nil {
		onChange()
	}
	return reset
}
