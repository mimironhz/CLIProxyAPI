package usage

import (
	"context"
	"errors"
	"testing"
	"time"
)

type blockingUsagePlugin struct {
	started chan struct{}
	release chan struct{}
}

func (p *blockingUsagePlugin) HandleUsage(context.Context, Record) {
	close(p.started)
	<-p.release
}

func TestGenerateEnabledDefaultsNilToTrue(t *testing.T) {
	if !GenerateEnabled(nil) {
		t.Fatalf("GenerateEnabled(nil) = false, want true")
	}
}

func TestGenerateEnabledHonorsExplicitFalse(t *testing.T) {
	if GenerateEnabled(GenerateFlag(false)) {
		t.Fatalf("GenerateEnabled(false) = true, want false")
	}
}

func TestGenerateEnabledHonorsExplicitTrue(t *testing.T) {
	if !GenerateEnabled(GenerateFlag(true)) {
		t.Fatalf("GenerateEnabled(true) = false, want true")
	}
}

func TestGenerateFromContextDefaultsMissingToTrue(t *testing.T) {
	if !GenerateFromContext(context.Background()) {
		t.Fatalf("GenerateFromContext(background) = false, want true")
	}
}

func TestGenerateFromContextHonorsExplicitFalse(t *testing.T) {
	ctx := WithGenerate(context.Background(), false)
	if GenerateFromContext(ctx) {
		t.Fatalf("GenerateFromContext(false) = true, want false")
	}
}

func TestRecordOmittedGenerateIsEnabled(t *testing.T) {
	// Existing callers construct Record without setting Generate.
	// Omission must remain distinguishable from explicit false and default to true.
	record := Record{
		Provider: "openai",
		Model:    "gpt-5.4",
	}
	if record.Generate != nil {
		t.Fatalf("Record.Generate = %v, want nil for omitted field", record.Generate)
	}
	if !GenerateEnabled(record.Generate) {
		t.Fatalf("GenerateEnabled(omitted) = false, want true")
	}
}

func TestStopContextHonorsDeadlineForBlockedPlugin(t *testing.T) {
	manager := NewManager(1)
	plugin := &blockingUsagePlugin{started: make(chan struct{}), release: make(chan struct{})}
	manager.Register(plugin)
	manager.Publish(context.Background(), Record{})
	<-plugin.started

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if errStop := manager.StopContext(ctx); !errors.Is(errStop, context.DeadlineExceeded) {
		t.Fatalf("StopContext() error = %v, want deadline exceeded", errStop)
	}
	close(plugin.release)
	if errStop := manager.StopContext(context.Background()); errStop != nil {
		t.Fatalf("StopContext() after release error = %v", errStop)
	}
}
