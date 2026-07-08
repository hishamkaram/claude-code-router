package config

import (
	"path/filepath"
	"testing"
)

func TestDefaultDataDirUsesXDGDataHome(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_DATA_HOME", root)

	got, err := DefaultDataDir()
	if err != nil {
		t.Fatalf("DefaultDataDir() error = %v", err)
	}
	want := filepath.Join(root, AppName)
	if got != want {
		t.Fatalf("DefaultDataDir() = %q, want %q", got, want)
	}
}
