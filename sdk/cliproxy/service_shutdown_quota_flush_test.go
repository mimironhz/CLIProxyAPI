package cliproxy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/quotawindow"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestShutdownFlushesQuotaWindowsWhenUsageDrainFails(t *testing.T) {
	dir := t.TempDir()
	gate, errNew := quotawindow.New(&config.Config{AuthDir: dir}, nil, dir)
	if errNew != nil {
		t.Fatalf("New() error = %v", errNew)
	}
	snapshotPath := filepath.Join(dir, "provider-quota-windows.qws")
	drainErr := errors.New("usage drain failed")
	s := &Service{
		quotaWindows: gate,
		stopUsageContextFn: func(context.Context) error {
			if _, errStat := os.Stat(snapshotPath); !os.IsNotExist(errStat) {
				t.Fatalf("snapshot exists before usage drain: %v", errStat)
			}
			return drainErr
		},
	}

	errShutdown := s.Shutdown(context.Background())
	if !errors.Is(errShutdown, drainErr) {
		t.Fatalf("Shutdown() error = %v, want %v", errShutdown, drainErr)
	}
	if _, errStat := os.Stat(snapshotPath); errStat != nil {
		t.Fatalf("snapshot missing after failed usage drain: %v", errStat)
	}
}
