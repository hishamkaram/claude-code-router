package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/secret"
)

type fakeSecrets struct {
	values    map[string]string
	failStore bool
}

func (f *fakeSecrets) Available(ctx context.Context) error {
	return ctx.Err()
}

func (f *fakeSecrets) Store(ctx context.Context, ref string, value string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.failStore {
		return fmt.Errorf("fake keyring unavailable")
	}
	if f.values == nil {
		f.values = make(map[string]string)
	}
	f.values[ref] = value
	return nil
}

func (f *fakeSecrets) Resolve(ctx context.Context, ref string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return f.values[ref], nil
}

func TestRootHelpExplainsRouterConcepts(t *testing.T) {
	t.Parallel()

	out, _, err := runCommand(t, "help")
	if err != nil {
		t.Fatalf("help error = %v", err)
	}
	for _, want := range []string{"fixed local gateway", "/model <alias>", "SQLite", "never silently fall back"} {
		if !strings.Contains(out, want) {
			t.Fatalf("help output missing %q:\n%s", want, out)
		}
	}
}

func TestVersionCommand(t *testing.T) {
	t.Parallel()

	out, _, err := runCommand(t, "version")
	if err != nil {
		t.Fatalf("version error = %v", err)
	}
	if !strings.Contains(out, "ccr dev") {
		t.Fatalf("version output = %q", out)
	}
}

func TestUnknownCommandReturnsSuggestion(t *testing.T) {
	t.Parallel()

	_, _, err := runCommand(t, "provder")
	if err == nil {
		t.Fatalf("expected unknown command error")
	}
	if !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("error = %v", err)
	}
}

func TestNoArgCommandsRejectStrayArgs(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	tests := [][]string{
		{"--db", dbPath, "init", "unexpected"},
		{"version", "unexpected"},
		{"--db", dbPath, "status", "unexpected"},
		{"--db", dbPath, "doctor", "unexpected"},
		{"sessions", "unexpected"},
		{"agents", "unexpected"},
	}
	for _, args := range tests {
		args := args
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			_, _, err := runCommand(t, args...)
			if err == nil {
				t.Fatalf("runCommand(%v) unexpectedly succeeded", args)
			}
		})
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("database path exists after invalid init: stat err=%v", err)
	}
}

func TestPlaceholderCommandsPreserveExpectedOperands(t *testing.T) {
	t.Parallel()

	tests := [][]string{
		{"provider", "remove", "anthropic"},
		{"model", "test", "qwen"},
		{"model", "remove", "qwen"},
	}
	for _, args := range tests {
		args := args
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			_, _, err := runCommand(t, args...)
			if err == nil {
				t.Fatalf("runCommand(%v) unexpectedly succeeded", args)
			}
			if !strings.Contains(err.Error(), "not implemented yet") {
				t.Fatalf("error = %v, want not implemented", err)
			}
			if strings.Contains(err.Error(), "unknown command") {
				t.Fatalf("error = %v, want operand accepted by placeholder", err)
			}
		})
	}
}

func TestProviderAddRequiresAPIKeyForOpenRouter(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	_, _, err := runCommand(t, "--db", dbPath, "provider", "add", "openrouter")
	if err == nil {
		t.Fatalf("expected missing API key error")
	}
	if !strings.Contains(err.Error(), "API key required") {
		t.Fatalf("error = %v", err)
	}
}

func TestProviderAndModelAddRoundTrip(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	out, _, err := runCommand(t, "--db", dbPath, "provider", "add", "openrouter", "--api-key-env", "OPENROUTER_API_KEY")
	if err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if strings.Contains(out, "sk-") || !strings.Contains(out, secret.EnvRef("OPENROUTER_API_KEY")) {
		t.Fatalf("provider add output did not redact/store env ref as expected: %q", out)
	}

	out, _, err = runCommand(t, "--db", dbPath, "model", "add", "qwen", "--provider", "openrouter", "--model", "qwen/qwen3-coder")
	if err != nil {
		t.Fatalf("model add error = %v", err)
	}
	if !strings.Contains(out, `Model alias "qwen" added`) {
		t.Fatalf("model add output = %q", out)
	}

	out, _, err = runCommand(t, "--db", dbPath, "model", "list")
	if err != nil {
		t.Fatalf("model list error = %v", err)
	}
	if !strings.Contains(out, "qwen") || !strings.Contains(out, "openrouter") {
		t.Fatalf("model list output = %q", out)
	}
}

func TestProviderAddFromStdinStoresKeyringReferenceOnly(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	fake := &fakeSecrets{}
	out, _, err := runCommandWithDeps(t, Dependencies{
		In:      strings.NewReader("sk-test\n"),
		Secrets: fake,
	}, "--db", dbPath, "provider", "add", "anthropic", "--api-key-stdin")
	if err != nil {
		t.Fatalf("provider add stdin error = %v", err)
	}
	if strings.Contains(out, "sk-test") {
		t.Fatalf("provider add leaked secret: %q", out)
	}
	if len(fake.values) != 1 {
		t.Fatalf("stored secrets = %#v, want 1", fake.values)
	}
}

func TestDuplicateProviderDoesNotOverwriteKeyringSecret(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	fake := &fakeSecrets{}
	if _, _, err := runCommandWithDeps(t, Dependencies{
		In:      strings.NewReader("old-key\n"),
		Secrets: fake,
	}, "--db", dbPath, "provider", "add", "anthropic", "--api-key-stdin"); err != nil {
		t.Fatalf("initial provider add error = %v", err)
	}

	_, _, err := runCommandWithDeps(t, Dependencies{
		In:      strings.NewReader("new-key\n"),
		Secrets: fake,
	}, "--db", dbPath, "provider", "add", "anthropic", "--api-key-stdin")
	if err == nil {
		t.Fatalf("duplicate provider add unexpectedly succeeded")
	}

	ref := secret.KeyringRef("anthropic")
	if got := fake.values[ref]; got != "old-key" {
		t.Fatalf("keyring value for %s = %q, want old-key", ref, got)
	}
}

func TestProviderAddDoesNotPersistWhenKeyringStoreFails(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	fake := &fakeSecrets{failStore: true}
	_, _, err := runCommandWithDeps(t, Dependencies{
		In:      strings.NewReader("sk-test\n"),
		Secrets: fake,
	}, "--db", dbPath, "provider", "add", "anthropic", "--api-key-stdin")
	if err == nil {
		t.Fatalf("provider add unexpectedly succeeded")
	}

	out, _, err := runCommand(t, "--db", dbPath, "provider", "list")
	if err != nil {
		t.Fatalf("provider list error = %v", err)
	}
	if !strings.Contains(out, "No providers configured.") {
		t.Fatalf("provider list output = %q, want no persisted provider", out)
	}
}

func TestProviderAddValidation(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	_, _, err := runCommand(t, "--db", dbPath, "provider", "add", "BadName", "--no-api-key")
	if err == nil {
		t.Fatalf("expected invalid provider name error")
	}
	if !strings.Contains(err.Error(), "invalid provider name") {
		t.Fatalf("error = %v", err)
	}
}

func TestDoctorUsesDatabaseAndReportsClaudeCode(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	out, _, err := runCommand(t, "--db", dbPath, "doctor")
	if err != nil {
		t.Fatalf("doctor error = %v", err)
	}
	for _, want := range []string{"SQLite: ok", "Secrets: ok", "Claude Code:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor output missing %q:\n%s", want, out)
		}
	}
}

func runCommand(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	return runCommandWithDeps(t, Dependencies{}, args...)
}

func runCommandWithDeps(t *testing.T, deps Dependencies, args ...string) (string, string, error) {
	t.Helper()
	var out bytes.Buffer
	var errOut bytes.Buffer
	if deps.In == nil {
		deps.In = strings.NewReader("")
	}
	deps.Out = &out
	deps.Err = &errOut
	if deps.Secrets == nil {
		deps.Secrets = &fakeSecrets{}
	}
	cmd := NewRootCommand(context.Background(), deps)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}
