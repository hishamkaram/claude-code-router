package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLaunchInvalidPermissionModeFailsBeforeDatabaseOpen(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	launcher := &fakeLauncher{pid: os.Getpid()}
	_, _, err := runCommandWithDeps(t, Dependencies{
		Launcher: launcher,
	}, "--db", dbPath, "launch", "--permission-mode", "bad")
	if err == nil {
		t.Fatalf("launch unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "invalid Claude Code permission mode") {
		t.Fatalf("launch error = %v", err)
	}
	if launcher.starts != 0 {
		t.Fatalf("launcher starts = %d, want 0", launcher.starts)
	}
	if _, statErr := os.Stat(dbPath); !os.IsNotExist(statErr) {
		t.Fatalf("database exists after invalid permission mode: stat err=%v", statErr)
	}
}

func TestLaunchPassesPermissionModeToClaude(t *testing.T) {
	t.Parallel()

	server := newModelsServer(t, []string{"gpt-5"})
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", server.URL, "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "gpt", "--provider", "litellm", "--model", "gpt-5"); err != nil {
		t.Fatalf("model add error = %v", err)
	}

	launcher := &fakeLauncher{pid: os.Getpid()}
	_, _, err := runCommandWithDeps(t, Dependencies{
		Launcher: launcher,
	}, "--db", dbPath, "launch", "--model", "gpt", "--permission-mode", "bypassPermissions")
	if err != nil {
		t.Fatalf("launch error = %v", err)
	}
	index := slices.Index(launcher.args, "--permission-mode")
	if index < 0 || index+1 >= len(launcher.args) || launcher.args[index+1] != "bypassPermissions" {
		t.Fatalf("launch args = %#v", launcher.args)
	}
}
