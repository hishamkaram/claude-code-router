package cli

import (
	"context"
	"database/sql"
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

func TestConformanceRunRecordsLocalUnverifiedStatus(t *testing.T) {
	t.Parallel()

	server := newModelsServer(t, []string{"gpt-5"})
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
	for _, want := range []string{"local-verified", "Live runtime status: unverified"} {
		if !strings.Contains(out, want) {
			t.Fatalf("conformance output missing %q:\n%s", want, out)
		}
	}
}

func TestConformanceRunAcceptsAnthropicCompatibleProvider(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "anthropic", "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "claude", "--provider", "anthropic", "--model", "claude-opus"); err != nil {
		t.Fatalf("model add error = %v", err)
	}

	out, _, err := runCommand(t, "--db", dbPath, "conformance", "run", "claude")
	if err != nil {
		t.Fatalf("conformance run error = %v", err)
	}
	for _, want := range []string{"local-verified", "protocol=anthropic-compatible", "Live runtime status: unverified"} {
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

func TestLaunchAcceptsAnthropicModelAliasForPassThrough(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "anthropic", "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "claude", "--provider", "anthropic", "--model", "claude-opus"); err != nil {
		t.Fatalf("model add error = %v", err)
	}

	launcher := &fakeLauncher{pid: os.Getpid()}
	out, _, err := runCommandWithDeps(t, Dependencies{
		Launcher: launcher,
	}, "--db", dbPath, "launch", "--model", "claude")
	if err != nil {
		t.Fatalf("launch error = %v", err)
	}
	if !strings.Contains(out, `Selected ccr model alias "claude"`) {
		t.Fatalf("launch output = %q", out)
	}
	if launcher.starts != 1 {
		t.Fatalf("launcher starts = %d, want 1", launcher.starts)
	}
}

func TestLaunchRejectsInvalidAnthropicPassThroughBeforeStartingClaude(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "anthropic", "--api-key-env", "ANTHROPIC_API_KEY"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}

	launcher := &fakeLauncher{pid: os.Getpid()}
	_, _, err := runCommandWithDeps(t, Dependencies{
		Launcher: launcher,
		Secrets:  &fakeSecrets{failResolve: true},
	}, "--db", dbPath, "launch")
	if err == nil {
		t.Fatalf("launch unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "validating Anthropic pass-through provider") {
		t.Fatalf("launch error = %v", err)
	}
	if launcher.starts != 0 {
		t.Fatalf("launcher starts = %d, want 0", launcher.starts)
	}
}

func TestLaunchChatOnlyAnthropicPassThroughDisablesClaudeTools(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "anthropic", "--mode", "chat-only", "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}

	launcher := &fakeLauncher{pid: os.Getpid()}
	out, _, err := runCommandWithDeps(t, Dependencies{
		Launcher: launcher,
	}, "--db", dbPath, "launch")
	if err != nil {
		t.Fatalf("launch error = %v", err)
	}
	if !launcher.hasArg("--tools") || !launcher.hasEnv("CLAUDE_CODE_SIMPLE=1") {
		t.Fatalf("chat-only pass-through launch args=%#v env=%#v", launcher.args, launcher.env)
	}
	if !strings.Contains(out, `Anthropic pass-through provider "anthropic" protocol=anthropic-compatible mode=chat-only`) ||
		!strings.Contains(out, "Anthropic pass-through route does not support tools") {
		t.Fatalf("launch output missing pass-through degradation:\n%s", out)
	}
}

func TestLaunchRequiresAliasForZAIWithoutAnthropicPassThrough(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "zai", "--api-key-env", "ZAI_API_KEY"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}

	launcher := &fakeLauncher{pid: os.Getpid()}
	_, _, err := runCommandWithDeps(t, Dependencies{
		Launcher: launcher,
	}, "--db", dbPath, "launch")
	if err == nil {
		t.Fatalf("launch unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "requires a configured Anthropic provider for Claude pass-through or one routable model alias") {
		t.Fatalf("launch error = %v", err)
	}
	if launcher.starts != 0 {
		t.Fatalf("launcher starts = %d, want 0", launcher.starts)
	}
}

func TestLaunchExplicitModelRejectsUnsupportedProtocolBeforeStartingClaude(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", "http://localhost:4000", "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "native", "--provider", "litellm", "--model", "native-model"); err != nil {
		t.Fatalf("model add error = %v", err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer db.Close()
	if _, execErr := db.Exec(`UPDATE providers SET protocol = 'native-unknown' WHERE name = 'litellm'`); execErr != nil {
		t.Fatalf("update provider protocol error = %v", execErr)
	}

	launcher := &fakeLauncher{pid: os.Getpid()}
	_, _, err = runCommandWithDeps(t, Dependencies{
		Launcher: launcher,
	}, "--db", dbPath, "launch", "--model", "native")
	if err == nil {
		t.Fatalf("launch unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), `protocol "native-unknown"`) || !strings.Contains(err.Error(), "not supported by the gateway path") {
		t.Fatalf("launch error = %v", err)
	}
	if launcher.starts != 0 {
		t.Fatalf("launcher starts = %d, want 0", launcher.starts)
	}
}

func TestLaunchRejectsMissingProviderModelBeforeStartingClaude(t *testing.T) {
	t.Parallel()

	server := newModelsServer(t, []string{"gpt-5"})
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", server.URL, "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "missing-model", "--provider", "litellm", "--model", "missing"); err != nil {
		t.Fatalf("model add error = %v", err)
	}

	launcher := &fakeLauncher{pid: os.Getpid()}
	_, _, err := runCommandWithDeps(t, Dependencies{
		Launcher: launcher,
	}, "--db", dbPath, "launch", "--model", "missing-model")
	if err == nil {
		t.Fatalf("launch unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "did not return that exact model") {
		t.Fatalf("launch error = %v", err)
	}
	if launcher.starts != 0 {
		t.Fatalf("launcher starts = %d, want 0", launcher.starts)
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

func TestSessionsAgentsAndLaunch(t *testing.T) {
	t.Parallel()

	server := newModelsServer(t, []string{"gpt-5"})
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	out, _, err := runCommand(t, "--db", dbPath, "sessions")
	if err != nil {
		t.Fatalf("sessions error = %v", err)
	}
	if !strings.Contains(out, "No launch sessions tracked.") {
		t.Fatalf("sessions output = %q", out)
	}
	out, _, err = runCommand(t, "--db", dbPath, "agents")
	if err != nil {
		t.Fatalf("agents error = %v", err)
	}
	if !strings.Contains(out, "No agents observed.") {
		t.Fatalf("agents output = %q", out)
	}
	if _, _, addErr := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", server.URL, "--no-api-key"); addErr != nil {
		t.Fatalf("provider add error = %v", addErr)
	}
	if _, _, addErr := runCommand(t, "--db", dbPath, "model", "add", "gpt", "--provider", "litellm", "--model", "gpt-5"); addErr != nil {
		t.Fatalf("model add error = %v", addErr)
	}

	launcher := &fakeLauncher{pid: os.Getpid()}
	out, _, err = runCommandWithDeps(t, Dependencies{
		Launcher: launcher,
	}, "--db", dbPath, "launch")
	if err != nil {
		t.Fatalf("launch error = %v", err)
	}
	if !strings.Contains(out, "Claude Code launched through http://127.0.0.1:") {
		t.Fatalf("launch output = %q", out)
	}
	if !strings.Contains(out, `Selected ccr model alias "gpt" is exposed to Claude Code and used as the startup model.`) {
		t.Fatalf("launch output missing selected model: %q", out)
	}
	if !launcher.hasEnvPrefix("ANTHROPIC_BASE_URL=http://127.0.0.1:") || !launcher.hasEnvPrefix("ANTHROPIC_AUTH_TOKEN=") ||
		!launcher.hasEnv("CLAUDE_CODE_USE_GATEWAY=1") || !launcher.hasEnv("CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1") ||
		!launcher.hasEnv("ANTHROPIC_CUSTOM_MODEL_OPTION=gpt") {
		t.Fatalf("launch env = %#v", launcher.env)
	}
	if launcher.hasEnv("CLAUDE_CODE_SIMPLE=1") {
		t.Fatalf("launch env still enables simple mode: %#v", launcher.env)
	}
	if launcher.hasArg("--tools") || !launcher.hasArg("--model") || !launcher.hasArg("gpt") {
		t.Fatalf("launch args = %#v", launcher.args)
	}

	out, _, err = runCommand(t, "--db", dbPath, "sessions")
	if err != nil {
		t.Fatalf("sessions after launch error = %v", err)
	}
	if !strings.Contains(out, "status=running") || !strings.Contains(out, "model=gpt") {
		t.Fatalf("sessions after launch output = %q", out)
	}
}

func TestLaunchPrintModeWritesSummaryToStderr(t *testing.T) {
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
	out, errOut, err := runCommandWithDeps(t, Dependencies{
		Launcher: launcher,
	}, "--db", dbPath, "launch", "--model", "gpt", "--print")
	if err != nil {
		t.Fatalf("launch --print error = %v", err)
	}
	if strings.Contains(out, "Claude Code launched through") {
		t.Fatalf("print-mode stdout contains launch summary: %q", out)
	}
	if !strings.Contains(errOut, "Claude Code launched through") {
		t.Fatalf("print-mode stderr missing launch summary: %q", errOut)
	}
	if !launcher.hasArg("--print") {
		t.Fatalf("launch args = %#v", launcher.args)
	}
}

func TestLaunchChatOnlyAliasDisablesClaudeTools(t *testing.T) {
	t.Parallel()

	server := newModelsServer(t, []string{"gpt-5"})
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", server.URL, "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "gpt", "--provider", "litellm", "--model", "gpt-5", "--compat", "chat-only"); err != nil {
		t.Fatalf("model add error = %v", err)
	}

	launcher := &fakeLauncher{pid: os.Getpid()}
	out, _, err := runCommandWithDeps(t, Dependencies{
		Launcher: launcher,
	}, "--db", dbPath, "launch", "--model", "gpt")
	if err != nil {
		t.Fatalf("launch error = %v", err)
	}
	if !launcher.hasArg("--tools") || !launcher.hasArg("") || !launcher.hasEnv("CLAUDE_CODE_SIMPLE=1") {
		t.Fatalf("chat-only launch args=%#v env=%#v", launcher.args, launcher.env)
	}
	if !strings.Contains(out, "Selected route does not support tools; Claude Code tools are disabled for this launch.") {
		t.Fatalf("launch output missing chat-only degradation: %q", out)
	}
}

func TestLaunchChatOnlyProviderDisablesClaudeTools(t *testing.T) {
	t.Parallel()

	server := newModelsServer(t, []string{"gpt-5"})
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", server.URL, "--mode", "chat-only", "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "gpt", "--provider", "litellm", "--model", "gpt-5"); err != nil {
		t.Fatalf("model add error = %v", err)
	}

	launcher := &fakeLauncher{pid: os.Getpid()}
	out, _, err := runCommandWithDeps(t, Dependencies{
		Launcher: launcher,
	}, "--db", dbPath, "launch", "--model", "gpt")
	if err != nil {
		t.Fatalf("launch error = %v", err)
	}
	if !launcher.hasArg("--tools") || !launcher.hasEnv("CLAUDE_CODE_SIMPLE=1") {
		t.Fatalf("chat-only provider launch args=%#v env=%#v", launcher.args, launcher.env)
	}
	if !strings.Contains(out, "Provider protocol=openai-compatible mode=chat-only") || !strings.Contains(out, "tools are disabled") {
		t.Fatalf("launch output missing provider degradation:\n%s", out)
	}
}

func TestLaunchPreservesFileWritersForClaudeProcess(t *testing.T) {
	t.Parallel()

	server := newModelsServer(t, []string{"gpt-5"})
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", server.URL, "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "gpt", "--provider", "litellm", "--model", "gpt-5"); err != nil {
		t.Fatalf("model add error = %v", err)
	}
	stdoutFile, err := os.CreateTemp(t.TempDir(), "stdout-*")
	if err != nil {
		t.Fatalf("CreateTemp(stdout) error = %v", err)
	}
	defer stdoutFile.Close()
	stderrFile, err := os.CreateTemp(t.TempDir(), "stderr-*")
	if err != nil {
		t.Fatalf("CreateTemp(stderr) error = %v", err)
	}
	defer stderrFile.Close()

	launcher := &fakeLauncher{pid: os.Getpid()}
	cmd := NewRootCommand(context.Background(), Dependencies{
		In:       strings.NewReader(""),
		Out:      stdoutFile,
		Err:      stderrFile,
		Secrets:  &fakeSecrets{},
		Launcher: launcher,
	})
	cmd.SetArgs([]string{"--db", dbPath, "launch"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("launch error = %v", err)
	}
	if launcher.out != stdoutFile || launcher.errOut != stderrFile {
		t.Fatalf("launcher writers = (%T,%T), want raw files", launcher.out, launcher.errOut)
	}
}

func TestLaunchInvalidModelFlagFailsBeforeDatabaseOpen(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	launcher := &fakeLauncher{pid: os.Getpid()}
	_, _, err := runCommandWithDeps(t, Dependencies{
		Launcher: launcher,
	}, "--db", dbPath, "launch", "--model", "BadName")
	if err == nil {
		t.Fatalf("launch unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "invalid model alias") {
		t.Fatalf("launch error = %v", err)
	}
	if launcher.starts != 0 {
		t.Fatalf("launcher starts = %d, want 0", launcher.starts)
	}
	if _, statErr := os.Stat(dbPath); !os.IsNotExist(statErr) {
		t.Fatalf("database exists after invalid launch model: stat err=%v", statErr)
	}
}

func TestLaunchCleansUpClaudeWhenSessionPersistenceFails(t *testing.T) {
	t.Parallel()

	server := newModelsServer(t, []string{"gpt-5"})
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", server.URL, "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "gpt", "--provider", "litellm", "--model", "gpt-5"); err != nil {
		t.Fatalf("model add error = %v", err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer db.Close()
	if _, execErr := db.Exec(`CREATE TRIGGER fail_sessions BEFORE INSERT ON sessions BEGIN SELECT RAISE(FAIL, 'session insert denied'); END;`); execErr != nil {
		t.Fatalf("create trigger error = %v", execErr)
	}

	launcher := &fakeLauncher{pid: os.Getpid()}
	_, _, err = runCommandWithDeps(t, Dependencies{
		Launcher: launcher,
	}, "--db", dbPath, "launch")
	if err == nil {
		t.Fatalf("launch unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "recording launch session") {
		t.Fatalf("launch error = %v", err)
	}
	if launcher.process == nil {
		t.Fatalf("launcher did not return a process")
	}
	if !launcher.process.stopped {
		t.Fatalf("process was not stopped after session persistence failure")
	}
	if !launcher.process.waited {
		t.Fatalf("process was not waited after session persistence failure")
	}
}
