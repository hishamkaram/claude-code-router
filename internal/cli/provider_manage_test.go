package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProviderRemoveDeletesProviderAndModels(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", "http://localhost:4000", "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "litellm-gpt-5", "--provider", "litellm", "--model", "gpt-5"); err != nil {
		t.Fatalf("model add error = %v", err)
	}

	out, _, err := runCommand(t, "--db", dbPath, "provider", "remove", "litellm", "--yes")
	if err != nil {
		t.Fatalf("provider remove error = %v", err)
	}
	for _, want := range []string{`Provider "litellm" removed`, "Removed 1 model aliases", "Secret reference removed from SQLite"} {
		if !strings.Contains(out, want) {
			t.Fatalf("provider remove output missing %q:\n%s", want, out)
		}
	}

	out, _, err = runCommand(t, "--db", dbPath, "provider", "list")
	if err != nil {
		t.Fatalf("provider list error = %v", err)
	}
	if !strings.Contains(out, "No providers configured.") {
		t.Fatalf("provider list output = %q", out)
	}

	out, _, err = runCommand(t, "--db", dbPath, "model", "list")
	if err != nil {
		t.Fatalf("model list error = %v", err)
	}
	if !strings.Contains(out, "No model aliases configured.") {
		t.Fatalf("model list output = %q", out)
	}
}

func TestProviderRemoveMissingProviderFailsBeforeSideEffects(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	_, _, err := runCommand(t, "--db", dbPath, "provider", "remove", "missing", "--yes")
	if err == nil {
		t.Fatalf("provider remove unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), `provider "missing" does not exist`) {
		t.Fatalf("provider remove error = %v", err)
	}
}

func TestProviderRemoveWithoutConfirmationFailsAndPreservesRows(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", "http://localhost:4000", "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}

	_, _, err := runCommand(t, "--db", dbPath, "provider", "remove", "litellm")
	if err == nil {
		t.Fatalf("provider remove unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "requires confirmation") {
		t.Fatalf("provider remove error = %v", err)
	}

	out, _, err := runCommand(t, "--db", dbPath, "provider", "list")
	if err != nil {
		t.Fatalf("provider list error = %v", err)
	}
	if !strings.Contains(out, "litellm") {
		t.Fatalf("provider was removed without confirmation: %q", out)
	}
}

func TestProviderRemoveInteractiveCanCancel(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", "http://localhost:4000", "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	out, errOut, err := runCommandWithDeps(t, Dependencies{
		In: newPromptReader("n\n"),
	}, "--db", dbPath, "provider", "remove", "litellm", "--interactive")
	if err != nil {
		t.Fatalf("interactive provider remove error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if !strings.Contains(out, `Provider "litellm" was not removed.`) {
		t.Fatalf("interactive provider remove output = %q", out)
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

func TestProviderUpdateWithFlags(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", "http://localhost:4000", "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}

	out, _, err := runCommand(t, "--db", dbPath, "provider", "update", "litellm", "--base-url", "http://localhost:5000", "--api-key-env", "LITELLM_API_KEY")
	if err != nil {
		t.Fatalf("provider update error = %v", err)
	}
	if !strings.Contains(out, "http://localhost:5000") || !strings.Contains(out, "env:LITELLM_API_KEY") {
		t.Fatalf("provider update output = %q", out)
	}

	out, _, err = runCommand(t, "--db", dbPath, "provider", "list")
	if err != nil {
		t.Fatalf("provider list error = %v", err)
	}
	if !strings.Contains(out, "http://localhost:5000") || !strings.Contains(out, "env:LITELLM_API_KEY") {
		t.Fatalf("provider list output = %q", out)
	}
}

func TestProviderUpdateTypeRequiresExplicitAuthDecision(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", "http://localhost:4000", "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}

	_, _, err := runCommand(t, "--db", dbPath, "provider", "update", "litellm", "--type", "openrouter")
	if err == nil {
		t.Fatalf("provider update unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "requires an API key") {
		t.Fatalf("provider update error = %v", err)
	}

	out, _, err := runCommand(t, "--db", dbPath, "provider", "update", "litellm", "--type", "openrouter", "--no-api-key")
	if err != nil {
		t.Fatalf("provider update explicit no-key error = %v", err)
	}
	if !strings.Contains(out, "openrouter") {
		t.Fatalf("provider update explicit no-key output = %q", out)
	}
}

func TestProviderUpdateInvalidStaticFlagsFailBeforeDatabaseOpen(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	_, _, err := runCommand(t, "--db", dbPath, "provider", "update", "missing", "--type", "bad")
	if err == nil {
		t.Fatalf("provider update unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "invalid provider type") {
		t.Fatalf("provider update error = %v", err)
	}
	if _, statErr := os.Stat(dbPath); !os.IsNotExist(statErr) {
		t.Fatalf("database exists after invalid provider update: stat err=%v", statErr)
	}
}

func TestProviderUpdateInteractiveInvalidStaticFlagsFailBeforePrompt(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", "http://localhost:4000", "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}

	_, _, err := runCommandWithDeps(t, Dependencies{
		In: newPromptReader("\n\n\n"),
	}, "--db", dbPath, "provider", "update", "litellm", "--interactive", "--type", "bad")
	if err == nil {
		t.Fatalf("interactive provider update unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "invalid provider type") {
		t.Fatalf("interactive provider update error = %v", err)
	}
}

func TestProviderUpdateInteractive(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", "http://localhost:4000", "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	input := strings.Join([]string{
		"1",                     // LiteLLM/OpenAI-compatible
		"http://localhost:5000", // base URL
		"4",                     // no API key
	}, "\n") + "\n"

	out, errOut, err := runCommandWithDeps(t, Dependencies{
		In: newPromptReader(input),
	}, "--db", dbPath, "provider", "update", "litellm", "--interactive")
	if err != nil {
		t.Fatalf("interactive provider update error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if !strings.Contains(out, "http://localhost:5000") {
		t.Fatalf("interactive provider update output = %q", out)
	}
}

func TestProviderUpdateInteractiveRequiresAuthDecisionWhenTypeStartsRequiringKey(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", "http://localhost:4000", "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	input := strings.Join([]string{
		"2", // OpenRouter
		"",  // keep existing base URL
		"1", // keep current empty secret reference
	}, "\n") + "\n"

	_, _, err := runCommandWithDeps(t, Dependencies{
		In: newPromptReader(input),
	}, "--db", dbPath, "provider", "update", "litellm", "--interactive")
	if err == nil {
		t.Fatalf("interactive provider update unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "requires an API key") {
		t.Fatalf("interactive provider update error = %v", err)
	}
}

func TestProviderTestOpenAICompatibleAndAnthropic(t *testing.T) {
	t.Parallel()

	server := newModelsServer(t, []string{"gpt-5"})
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", server.URL, "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	out, _, err := runCommand(t, "--db", dbPath, "provider", "test", "litellm")
	if err != nil {
		t.Fatalf("provider test error = %v", err)
	}
	if !strings.Contains(out, "discovered 1 models") {
		t.Fatalf("provider test output = %q", out)
	}

	if _, _, addErr := runCommand(t, "--db", dbPath, "provider", "add", "anthropic", "--no-api-key"); addErr != nil {
		t.Fatalf("anthropic provider add error = %v", addErr)
	}
	out, _, err = runCommand(t, "--db", dbPath, "provider", "test", "anthropic")
	if err != nil {
		t.Fatalf("anthropic provider test error = %v", err)
	}
	if !strings.Contains(out, "Anthropic live routing is outside this pass") {
		t.Fatalf("anthropic provider test output = %q", out)
	}
}
