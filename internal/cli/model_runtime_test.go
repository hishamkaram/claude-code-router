package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestModelUpdateTestAndRemove(t *testing.T) {
	t.Parallel()

	server := newModelsServer(t, []string{"gpt-5", "gpt-5-mini"})
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", server.URL, "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "litellm-gpt", "--provider", "litellm", "--model", "gpt-5"); err != nil {
		t.Fatalf("model add error = %v", err)
	}

	out, _, err := runCommand(t, "--db", dbPath, "model", "update", "litellm-gpt", "--model", "gpt-5-mini", "--compat", "full")
	if err != nil {
		t.Fatalf("model update error = %v", err)
	}
	if !strings.Contains(out, "gpt-5-mini") || !strings.Contains(out, "compat=full") {
		t.Fatalf("model update output = %q", out)
	}

	out, _, err = runCommand(t, "--db", dbPath, "model", "test", "litellm-gpt")
	if err != nil {
		t.Fatalf("model test error = %v", err)
	}
	if !strings.Contains(out, "Exact provider model verified") {
		t.Fatalf("model test output = %q", out)
	}

	out, _, err = runCommand(t, "--db", dbPath, "model", "remove", "litellm-gpt", "--yes")
	if err != nil {
		t.Fatalf("model remove error = %v", err)
	}
	if !strings.Contains(out, `Model alias "litellm-gpt" removed`) {
		t.Fatalf("model remove output = %q", out)
	}
}

func TestModelUpdateInteractiveAndRemoveInteractive(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", "http://localhost:4000", "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "litellm-gpt", "--provider", "litellm", "--model", "gpt-5"); err != nil {
		t.Fatalf("model add error = %v", err)
	}

	updateInput := strings.Join([]string{
		"litellm",
		"gpt-5-mini",
		"1", // full
	}, "\n") + "\n"
	out, errOut, err := runCommandWithDeps(t, Dependencies{
		In: newPromptReader(updateInput),
	}, "--db", dbPath, "model", "update", "litellm-gpt", "--interactive")
	if err != nil {
		t.Fatalf("interactive model update error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if !strings.Contains(out, "gpt-5-mini") || !strings.Contains(out, "compat=full") {
		t.Fatalf("interactive model update output = %q", out)
	}

	out, errOut, err = runCommandWithDeps(t, Dependencies{
		In: newPromptReader("y\n"),
	}, "--db", dbPath, "model", "remove", "litellm-gpt", "--interactive")
	if err != nil {
		t.Fatalf("interactive model remove error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if !strings.Contains(out, `Model alias "litellm-gpt" removed`) {
		t.Fatalf("interactive model remove output = %q", out)
	}
}

func TestModelTestFailsWhenProviderModelMissing(t *testing.T) {
	t.Parallel()

	server := newModelsServer(t, []string{"gpt-5"})
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", server.URL, "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "missing-model", "--provider", "litellm", "--model", "missing"); err != nil {
		t.Fatalf("model add error = %v", err)
	}

	_, _, err := runCommand(t, "--db", dbPath, "model", "test", "missing-model")
	if err == nil {
		t.Fatalf("model test unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "did not return that exact model") {
		t.Fatalf("model test error = %v", err)
	}
}

func TestModelUpdateInvalidStaticFlagsFailBeforeDatabaseOpen(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	_, _, err := runCommand(t, "--db", dbPath, "model", "update", "missing", "--compat", "nope")
	if err == nil {
		t.Fatalf("model update unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "invalid compatibility status") {
		t.Fatalf("model update error = %v", err)
	}
	if _, statErr := os.Stat(dbPath); !os.IsNotExist(statErr) {
		t.Fatalf("database exists after invalid model update: stat err=%v", statErr)
	}
}

func TestModelUpdateInteractiveInvalidStaticFlagsFailBeforeDatabaseOpen(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	_, _, err := runCommandWithDeps(t, Dependencies{
		In: newPromptReader("\n\n\n"),
	}, "--db", dbPath, "model", "update", "missing", "--interactive", "--compat", "nope")
	if err == nil {
		t.Fatalf("interactive model update unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "invalid compatibility status") {
		t.Fatalf("interactive model update error = %v", err)
	}
	if _, statErr := os.Stat(dbPath); !os.IsNotExist(statErr) {
		t.Fatalf("database exists after invalid interactive model update: stat err=%v", statErr)
	}
}

func TestConformanceRunRecordsProviderVerifiedStatus(t *testing.T) {
	t.Parallel()

	server := newCLIConformanceOpenAIServer(t, "gpt-5")
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", server.URL, "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "litellm-gpt", "--provider", "litellm", "--model", "gpt-5"); err != nil {
		t.Fatalf("model add error = %v", err)
	}

	out, _, err := runCommand(t, "--db", dbPath, "conformance", "run", "litellm-gpt")
	if err != nil {
		t.Fatalf("conformance run error = %v", err)
	}
	for _, want := range []string{"Conformance run", "passed", "forced_tool", "count_tokens"} {
		if !strings.Contains(out, want) {
			t.Fatalf("conformance output missing %q:\n%s", want, out)
		}
	}
}

func TestConformanceRunAcceptsAnthropicCompatibleProvider(t *testing.T) {
	t.Parallel()

	server := newCLIConformanceAnthropicServer(t, "claude-opus")
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "anthropic", "--base-url", server.URL, "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "claude", "--provider", "anthropic", "--model", "claude-opus"); err != nil {
		t.Fatalf("model add error = %v", err)
	}

	out, _, err := runCommand(t, "--db", dbPath, "conformance", "run", "claude")
	if err != nil {
		t.Fatalf("conformance run error = %v", err)
	}
	for _, want := range []string{"Conformance run", "passed", "stream", "count_tokens"} {
		if !strings.Contains(out, want) {
			t.Fatalf("conformance output missing %q:\n%s", want, out)
		}
	}
}

func TestConformanceRunValidatesAnthropicCompatibleSecret(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "anthropic", "--api-key-env", "ANTHROPIC_API_KEY"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "claude", "--provider", "anthropic", "--model", "claude-opus"); err != nil {
		t.Fatalf("model add error = %v", err)
	}

	fake := &fakeSecrets{failResolve: true}
	_, _, err := runCommandWithDeps(t, Dependencies{
		Secrets: fake,
	}, "--db", dbPath, "conformance", "run", "claude")
	if err == nil {
		t.Fatalf("conformance run unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "resolving API key") {
		t.Fatalf("conformance run error = %v", err)
	}
	if fake.resolveCount != 1 {
		t.Fatalf("secret Resolve called %d times, want 1", fake.resolveCount)
	}
}

func TestModelTestRejectsBlockedAliasBeforeSecretLookup(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "openrouter", "--api-key-env", "OPENROUTER_API_KEY"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "blocked", "--provider", "openrouter", "--model", "gpt-5", "--compat", "blocked"); err != nil {
		t.Fatalf("model add error = %v", err)
	}

	fake := &fakeSecrets{failResolve: true}
	_, _, err := runCommandWithDeps(t, Dependencies{
		Secrets: fake,
	}, "--db", dbPath, "model", "test", "blocked")
	if err == nil {
		t.Fatalf("model test unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "blocked and cannot be routed") {
		t.Fatalf("model test error = %v", err)
	}
	if fake.resolveCount != 0 {
		t.Fatalf("secret Resolve called %d times, want 0", fake.resolveCount)
	}
}
