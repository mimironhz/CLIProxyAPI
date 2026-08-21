package quotawindow

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestScheduleWrappingWindowAndNextOpen(t *testing.T) {
	zero := int64(0)
	schedule, errCompile := CompileSchedule("deepseek|provider", config.QuotaWindows{
		Timezone: "UTC",
		Windows: []config.QuotaWindow{
			{Name: "peak", Start: "00:30", End: "16:30", Budget: &config.QuotaBudget{Requests: &zero}},
			{Name: "off-peak", Start: "16:30", End: "00:30"},
		},
	})
	if errCompile != nil {
		t.Fatalf("CompileSchedule() error = %v", errCompile)
	}
	now := time.Date(2026, time.August, 21, 14, 5, 0, 0, time.UTC)
	instance, active := schedule.InstanceAt(now)
	if !active || instance.Name != "peak" {
		t.Fatalf("InstanceAt() = %#v, %t; want peak", instance, active)
	}
	want := time.Date(2026, time.August, 21, 16, 30, 0, 0, time.UTC)
	if got := schedule.NextOpen(now, []string{"requests"}); !got.Equal(want) {
		t.Fatalf("NextOpen() = %s, want %s", got, want)
	}
	wrappedAt := time.Date(2026, time.August, 22, 0, 5, 0, 0, time.UTC)
	wrapped, active := schedule.InstanceAt(wrappedAt)
	if !active || wrapped.Name != "off-peak" {
		t.Fatalf("wrapped InstanceAt() = %#v, %t; want off-peak", wrapped, active)
	}
}

func TestScheduleNextOpenSkipsWindowClosedByDifferentDimension(t *testing.T) {
	zero := int64(0)
	one := int64(1)
	schedule, errCompile := CompileSchedule("provider|model", config.QuotaWindows{Timezone: "UTC", Windows: []config.QuotaWindow{
		{Name: "request-closed", Start: "00:00", End: "08:00", Budget: &config.QuotaBudget{Requests: &zero}},
		{Name: "input-closed", Start: "08:00", End: "16:00", Budget: &config.QuotaBudget{Requests: &one, InputTokens: &zero}},
		{Name: "open", Start: "16:00", End: "23:59", Budget: &config.QuotaBudget{Requests: &one, InputTokens: &one}},
	}})
	if errCompile != nil {
		t.Fatalf("CompileSchedule() error = %v", errCompile)
	}
	now := time.Date(2026, time.August, 21, 7, 0, 0, 0, time.UTC)
	want := time.Date(2026, time.August, 21, 16, 0, 0, 0, time.UTC)
	if got := schedule.NextOpen(now, []string{"requests"}); !got.Equal(want) {
		t.Fatalf("NextOpen() = %s, want %s", got, want)
	}
}

func TestScheduleDaysFilter(t *testing.T) {
	schedule, errCompile := CompileSchedule("codex|provider", config.QuotaWindows{
		Timezone: "UTC",
		Windows:  []config.QuotaWindow{{Name: "workday", Start: "09:00", End: "18:00", Days: []string{"mon"}}},
	})
	if errCompile != nil {
		t.Fatalf("CompileSchedule() error = %v", errCompile)
	}
	monday := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	if _, active := schedule.InstanceAt(monday); !active {
		t.Fatal("InstanceAt(Monday) inactive, want active")
	}
	if _, active := schedule.InstanceAt(monday.AddDate(0, 0, 1)); active {
		t.Fatal("InstanceAt(Tuesday) active, want inactive")
	}
}

func TestResolveLocalBoundaryDSTGapAndFold(t *testing.T) {
	location, errLocation := time.LoadLocation("America/Los_Angeles")
	if errLocation != nil {
		t.Fatalf("LoadLocation() error = %v", errLocation)
	}

	gap := resolveLocalBoundary(2026, time.March, 8, 2, 30, location)
	gapLocal := gap.In(location)
	if gapLocal.Hour() != 3 || gapLocal.Minute() != 0 {
		t.Fatalf("gap boundary = %s, want first valid minute 03:00", gapLocal)
	}

	fold := resolveLocalBoundary(2026, time.November, 1, 1, 30, location)
	_, offset := fold.Zone()
	if got, want := fold.UTC(), time.Date(2026, time.November, 1, 8, 30, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("fold boundary = %s, want first occurrence %s", got, want)
	}
	if offset != -7*60*60 {
		t.Fatalf("fold offset = %d, want PDT offset", offset)
	}
}
