//go:build linux || darwin

package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestForwardedSettingsContentsChangeExecutionFingerprint(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "profile"))
	settings := filepath.Join(root, "custom.json")
	if err := os.WriteFile(settings, []byte(`{"env":{"EXAMPLE":"before"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	inv, err := parseLaunchInvocation([]string{"--auth-mode=preserve", "--no-lifecycle", "--no-statusline", "--settings=" + settings})
	if err != nil {
		t.Fatal(err)
	}
	opts := &options{dbPath: filepath.Join(root, "ccr.db")}
	before, err := executionFingerprint(t.Context(), opts, Dependencies{}, inv)
	if err != nil {
		t.Fatal(err)
	}
	if writeErr := os.WriteFile(settings, []byte(`{"env":{"EXAMPLE":"after"}}`), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	after, err := executionFingerprint(t.Context(), opts, Dependencies{}, inv)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("forwarded settings changed but execution fingerprint stayed identical")
	}
}
