package quotawindow

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

type compiledWindow struct {
	name        string
	startMinute int
	endMinute   int
	days        [7]bool
	budget      *config.QuotaBudget
}

// Schedule is one compiled recurring quota-window schedule.
type Schedule struct {
	id              string
	budgetNamespace string
	location        *time.Location
	persist         bool
	windows         []compiledWindow
	raw             config.QuotaWindows
}

// Instance is one concrete occurrence of a recurring window.
type Instance struct {
	ID       string
	Schedule string
	Name     string
	StartsAt time.Time
	EndsAt   time.Time
	Budget   *config.QuotaBudget
	Persist  bool
}

// CompileSchedule validates and compiles a quota schedule.
func CompileSchedule(id string, raw config.QuotaWindows) (*Schedule, error) {
	timezone := strings.TrimSpace(raw.Timezone)
	if timezone == "" {
		timezone = "UTC"
	}
	location, errLocation := time.LoadLocation(timezone)
	if errLocation != nil {
		return nil, fmt.Errorf("load timezone %q: %w", timezone, errLocation)
	}
	persist := true
	if raw.Persist != nil {
		persist = *raw.Persist
	}
	schedule := &Schedule{id: id, budgetNamespace: id, location: location, persist: persist, raw: raw}
	for index := range raw.Windows {
		window := raw.Windows[index]
		start, errStart := parseClock(window.Start)
		if errStart != nil {
			return nil, fmt.Errorf("window %q start: %w", window.Name, errStart)
		}
		end, errEnd := parseClock(window.End)
		if errEnd != nil {
			return nil, fmt.Errorf("window %q end: %w", window.Name, errEnd)
		}
		if start == end {
			return nil, fmt.Errorf("window %q start and end must differ", window.Name)
		}
		days, errDays := compileDays(window.Days)
		if errDays != nil {
			return nil, fmt.Errorf("window %q days: %w", window.Name, errDays)
		}
		schedule.windows = append(schedule.windows, compiledWindow{
			name:        strings.TrimSpace(window.Name),
			startMinute: start,
			endMinute:   end,
			days:        days,
			budget:      cloneBudget(window.Budget),
		})
	}
	return schedule, nil
}

func compileProviderSchedule(provider, id string, raw config.QuotaWindows) (*Schedule, error) {
	schedule, errCompile := CompileSchedule(id, raw)
	if errCompile != nil {
		return nil, errCompile
	}
	schedule.budgetNamespace = provider
	return schedule, nil
}

func parseClock(value string) (int, error) {
	parsed, err := time.Parse("15:04", strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("must use HH:MM: %w", err)
	}
	return parsed.Hour()*60 + parsed.Minute(), nil
}

func compileDays(values []string) ([7]bool, error) {
	var days [7]bool
	if len(values) == 0 {
		for i := range days {
			days[i] = true
		}
		return days, nil
	}
	for _, raw := range values {
		var index int
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "mon":
			index = 0
		case "tue":
			index = 1
		case "wed":
			index = 2
		case "thu":
			index = 3
		case "fri":
			index = 4
		case "sat":
			index = 5
		case "sun":
			index = 6
		default:
			return days, fmt.Errorf("unknown day %q", raw)
		}
		days[index] = true
	}
	return days, nil
}

func weekdayIndex(day time.Weekday) int {
	if day == time.Sunday {
		return 6
	}
	return int(day) - 1
}

func (s *Schedule) instanceForStartDate(startDate time.Time, window compiledWindow) Instance {
	year, month, day := startDate.Date()
	startHour, startMinute := window.startMinute/60, window.startMinute%60
	endHour, endMinute := window.endMinute/60, window.endMinute%60
	startsAt := resolveLocalBoundary(year, month, day, startHour, startMinute, s.location)
	endDate := startDate
	if window.endMinute <= window.startMinute {
		endDate = endDate.AddDate(0, 0, 1)
	}
	endYear, endMonth, endDay := endDate.Date()
	endsAt := resolveLocalBoundary(endYear, endMonth, endDay, endHour, endMinute, s.location)
	return Instance{
		ID:       s.budgetNamespace + "|" + strings.ToLower(window.name) + "|" + startsAt.UTC().Format(time.RFC3339Nano),
		Schedule: s.id,
		Name:     window.name,
		StartsAt: startsAt,
		EndsAt:   endsAt,
		Budget:   cloneBudget(window.budget),
		Persist:  s.persist,
	}
}

// resolveLocalBoundary normalizes nonexistent wall-clock minutes forward and
// chooses the first absolute occurrence when a minute repeats at a DST fold.
func resolveLocalBoundary(year int, month time.Month, day, hour, minute int, location *time.Location) time.Time {
	wall := time.Date(year, month, day, hour, minute, 0, 0, time.UTC)
	approximate := time.Date(year, month, day, hour, minute, 0, 0, location)
	offsets := make(map[int]struct{})
	for sample := approximate.Add(-48 * time.Hour); !sample.After(approximate.Add(48 * time.Hour)); sample = sample.Add(6 * time.Hour) {
		_, offset := sample.In(location).Zone()
		offsets[offset] = struct{}{}
	}
	for forward := 0; forward <= 24*60; forward++ {
		candidateWall := wall.Add(time.Duration(forward) * time.Minute)
		var first time.Time
		for offset := range offsets {
			candidate := candidateWall.Add(-time.Duration(offset) * time.Second)
			local := candidate.In(location)
			if local.Year() != candidateWall.Year() || local.Month() != candidateWall.Month() || local.Day() != candidateWall.Day() || local.Hour() != candidateWall.Hour() || local.Minute() != candidateWall.Minute() {
				continue
			}
			if first.IsZero() || candidate.Before(first) {
				first = candidate
			}
		}
		if !first.IsZero() {
			return first.In(location)
		}
	}
	return approximate
}

// InstanceAt returns the concrete active window, if any. Uncovered time is unmetered.
func (s *Schedule) InstanceAt(now time.Time) (Instance, bool) {
	if s == nil || len(s.windows) == 0 {
		return Instance{}, false
	}
	local := now.In(s.location)
	date := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, s.location)
	for _, offset := range []int{0, -1} {
		startDate := date.AddDate(0, 0, offset)
		dayIndex := weekdayIndex(startDate.Weekday())
		for _, window := range s.windows {
			if !window.days[dayIndex] {
				continue
			}
			instance := s.instanceForStartDate(startDate, window)
			if !now.Before(instance.StartsAt) && now.Before(instance.EndsAt) {
				return instance, true
			}
		}
	}
	return Instance{}, false
}

// NextOpen returns the first future boundary whose complete budget is serviceable.
func (s *Schedule) NextOpen(now time.Time, _ []string) time.Time {
	if s == nil {
		return now
	}
	boundaries := s.boundaries(now, 9)
	for _, boundary := range boundaries {
		if !boundary.After(now) {
			continue
		}
		instance, active := s.InstanceAt(boundary.Add(time.Nanosecond))
		if !active || budgetAllows(instance.Budget) {
			return boundary
		}
	}
	return time.Time{}
}

// NextInstance returns the next configured window instance, including the current one.
func (s *Schedule) NextInstance(now time.Time) (Instance, bool) {
	if current, ok := s.InstanceAt(now); ok {
		return current, true
	}
	if s == nil {
		return Instance{}, false
	}
	local := now.In(s.location)
	date := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, s.location)
	var next Instance
	for offset := 0; offset <= 8; offset++ {
		startDate := date.AddDate(0, 0, offset)
		dayIndex := weekdayIndex(startDate.Weekday())
		for _, window := range s.windows {
			if !window.days[dayIndex] {
				continue
			}
			instance := s.instanceForStartDate(startDate, window)
			if instance.StartsAt.Before(now) {
				continue
			}
			if next.StartsAt.IsZero() || instance.StartsAt.Before(next.StartsAt) {
				next = instance
			}
		}
	}
	return next, !next.StartsAt.IsZero()
}

// NextFutureInstance returns the first configured window whose start is after now.
func (s *Schedule) NextFutureInstance(now time.Time) (Instance, bool) {
	if s == nil {
		return Instance{}, false
	}
	local := now.In(s.location)
	date := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, s.location)
	var next Instance
	for offset := -1; offset <= 8; offset++ {
		startDate := date.AddDate(0, 0, offset)
		dayIndex := weekdayIndex(startDate.Weekday())
		for _, window := range s.windows {
			if !window.days[dayIndex] {
				continue
			}
			instance := s.instanceForStartDate(startDate, window)
			if !instance.StartsAt.After(now) {
				continue
			}
			if next.StartsAt.IsZero() || instance.StartsAt.Before(next.StartsAt) {
				next = instance
			}
		}
	}
	return next, !next.StartsAt.IsZero()
}

func (s *Schedule) boundaries(now time.Time, daysAhead int) []time.Time {
	if s == nil {
		return nil
	}
	local := now.In(s.location)
	date := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, s.location)
	boundaries := make([]time.Time, 0, len(s.windows)*(daysAhead+2)*2)
	for offset := -1; offset <= daysAhead; offset++ {
		startDate := date.AddDate(0, 0, offset)
		dayIndex := weekdayIndex(startDate.Weekday())
		for _, window := range s.windows {
			if !window.days[dayIndex] {
				continue
			}
			instance := s.instanceForStartDate(startDate, window)
			boundaries = append(boundaries, instance.StartsAt, instance.EndsAt)
		}
	}
	sort.Slice(boundaries, func(i, j int) bool { return boundaries[i].Before(boundaries[j]) })
	out := boundaries[:0]
	for _, boundary := range boundaries {
		if len(out) == 0 || !boundary.Equal(out[len(out)-1]) {
			out = append(out, boundary)
		}
	}
	return out
}

func budgetAllows(budget *config.QuotaBudget) bool {
	if budget == nil {
		return true
	}
	for _, limit := range []*int64{budget.Requests, budget.InputTokens, budget.OutputTokens, budget.TotalTokens} {
		if limit != nil && *limit <= 0 {
			return false
		}
	}
	return true
}

func cloneBudget(budget *config.QuotaBudget) *config.QuotaBudget {
	if budget == nil {
		return nil
	}
	clone := *budget
	clone.Requests = cloneInt64(budget.Requests)
	clone.InputTokens = cloneInt64(budget.InputTokens)
	clone.OutputTokens = cloneInt64(budget.OutputTokens)
	clone.TotalTokens = cloneInt64(budget.TotalTokens)
	return &clone
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}
