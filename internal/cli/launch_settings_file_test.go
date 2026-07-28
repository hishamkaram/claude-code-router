package cli

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWritePrivateLaunchSettingsUsesRestrictedTemporaryFile(t *testing.T) {
	t.Parallel()

	const settings = `{"statusLine":{"type":"command","command":"private-command"}}`
	path, cleanup, err := writePrivateLaunchSettings(settings)
	if err != nil {
		t.Fatalf("writePrivateLaunchSettings() error = %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(settings) error = %v", err)
	}
	directoryInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("Stat(settings directory) error = %v", err)
	}
	if runtime.GOOS != "windows" {
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("settings mode = %o, want 600", got)
		}
		if got := directoryInfo.Mode().Perm(); got != 0o700 {
			t.Fatalf("settings directory mode = %o, want 700", got)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(settings) error = %v", err)
	}
	if string(data) != settings {
		t.Fatalf("settings = %q, want %q", data, settings)
	}
	if err := cleanup(); err != nil {
		t.Fatalf("cleanup() error = %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("settings still exists after cleanup: %v", err)
	}
}

func TestWritePrivateLaunchSettingsEmptyInputNeedsNoFile(t *testing.T) {
	t.Parallel()

	path, cleanup, err := writePrivateLaunchSettings("")
	if err != nil {
		t.Fatalf("writePrivateLaunchSettings() error = %v", err)
	}
	if path != "" {
		t.Fatalf("empty settings path = %q", path)
	}
	if err := cleanup(); err != nil {
		t.Fatalf("cleanup() error = %v", err)
	}
}
