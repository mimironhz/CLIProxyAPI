package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLoadPrivateDotEnvRequiresPrivatePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not enforced on Windows")
	}
	path := filepath.Join(t.TempDir(), ".env")
	if errWrite := os.WriteFile(path, []byte("ROOT_PROXY_TEST_SECRET=value\n"), 0o644); errWrite != nil {
		t.Fatalf("write .env: %v", errWrite)
	}
	if errLoad := loadPrivateDotEnv(path); errLoad == nil || !strings.Contains(errLoad.Error(), "0600") {
		t.Fatalf("loadPrivateDotEnv() error = %v", errLoad)
	}
}

func TestLoadPrivateDotEnvLoadsPrivateFile(t *testing.T) {
	const variable = "ROOT_PROXY_DOTENV_TEST_VALUE"
	oldValue, hadOldValue := os.LookupEnv(variable)
	if errUnset := os.Unsetenv(variable); errUnset != nil {
		t.Fatalf("unset test environment: %v", errUnset)
	}
	t.Cleanup(func() {
		if hadOldValue {
			_ = os.Setenv(variable, oldValue)
		} else {
			_ = os.Unsetenv(variable)
		}
	})

	path := filepath.Join(t.TempDir(), ".env")
	if errWrite := os.WriteFile(path, []byte(variable+"=loaded\n"), 0o600); errWrite != nil {
		t.Fatalf("write .env: %v", errWrite)
	}
	if errLoad := loadPrivateDotEnv(path); errLoad != nil {
		t.Fatalf("loadPrivateDotEnv() error = %v", errLoad)
	}
	if got := os.Getenv(variable); got != "loaded" {
		t.Fatalf("environment value = %q", got)
	}
}

func TestLoadPrivateDotEnvAcceptsMissingFileAndRejectsDirectory(t *testing.T) {
	temporaryDirectory := t.TempDir()
	if errLoad := loadPrivateDotEnv(filepath.Join(temporaryDirectory, "missing")); errLoad != nil {
		t.Fatalf("missing .env error = %v", errLoad)
	}
	if errLoad := loadPrivateDotEnv(temporaryDirectory); errLoad == nil {
		t.Fatal("directory .env was accepted")
	}
}

func TestRunRejectsUnexpectedArguments(t *testing.T) {
	errRun := run(context.Background(), []string{"unexpected"}, &bytes.Buffer{})
	if errRun == nil || !strings.Contains(errRun.Error(), "unexpected positional") {
		t.Fatalf("run() error = %v", errRun)
	}
}

func TestRunHelp(t *testing.T) {
	errRun := run(context.Background(), []string{"--help"}, &bytes.Buffer{})
	if !errors.Is(errRun, flag.ErrHelp) {
		t.Fatalf("run() error = %v, want flag.ErrHelp", errRun)
	}
}
