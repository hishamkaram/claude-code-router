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

func TestLaunchPreserveAuthModeDoesNotValidateConfiguredAnthropicSecret(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "anthropic", "--api-key-env", "ANTHROPIC_API_KEY"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}

	launcher := &fakeLauncher{pid: os.Getpid()}
	fake := &fakeSecrets{failResolve: true}
	out, _, err := runCommandWithDeps(t, Dependencies{
		Launcher: launcher,
		Secrets:  fake,
	}, "--db", dbPath, "launch")
	if err != nil {
		t.Fatalf("launch error = %v", err)
	}
	if fake.resolveCount != 0 {
		t.Fatalf("secret Resolve called %d times, want 0", fake.resolveCount)
	}
	if launcher.starts != 1 {
		t.Fatalf("launcher starts = %d, want 1", launcher.starts)
	}
	assertPreserveAuthEnv(t, launcher)
	if launcher.unsetsEnv("ANTHROPIC_API_KEY") {
		t.Fatalf("preserve auth launch should retain first-party ANTHROPIC_API_KEY: %s", launcher.environmentSummary())
	}
	if !launcher.hasEnvPrefix("ANTHROPIC_CUSTOM_HEADERS=X-CCR-Session-Token: ") {
		t.Fatalf("preserve launch env missing CCR custom header: %s", launcher.environmentSummary())
	}
	if !strings.Contains(out, "Anthropic subscription login and Anthropic API-key auth are preserved") {
		t.Fatalf("launch output missing auth preservation summary:\n%s", out)
	}
}

func TestLaunchUnsetsRegisteredProviderSecretEnvironment(t *testing.T) {
	t.Parallel()

	server := newModelsServer(t, []string{"gpt-5"})
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", server.URL, "--api-key-env", "CCR_TEST_PROVIDER_TOKEN"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "gpt", "--provider", "litellm", "--model", "gpt-5"); err != nil {
		t.Fatalf("model add error = %v", err)
	}

	launcher := &fakeLauncher{pid: os.Getpid()}
	if _, _, err := runCommandWithDeps(t, Dependencies{
		Launcher: launcher,
		Secrets:  &fakeSecrets{values: map[string]string{"env:CCR_TEST_PROVIDER_TOKEN": "test-provider-token"}},
	}, "--db", dbPath, "launch", "--model", "gpt"); err != nil {
		t.Fatalf("launch error = %v", err)
	}
	if !launcher.unsetsEnv("CCR_TEST_PROVIDER_TOKEN") || launcher.hasEnvPrefix("CCR_TEST_PROVIDER_TOKEN=") {
		t.Fatalf("Claude Code child inherited registered provider credential: %s", launcher.environmentSummary())
	}
}

func TestLaunchWithoutModelIgnoresConfiguredAnthropicChatOnlyMode(t *testing.T) {
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
	if launcher.hasArg("--tools") || launcher.hasEnv("CLAUDE_CODE_SIMPLE=1") {
		t.Fatalf("first-party default launch disabled tools from configured provider args=%#v env=%s", launcher.args, launcher.environmentSummary())
	}
	if !strings.Contains(out, "No ccr startup model selected; Claude Code will use its configured default model.") {
		t.Fatalf("launch output missing default model summary:\n%s", out)
	}
}

func TestLaunchWithoutConfiguredAliasesUsesClaudeDefault(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "zai", "--api-key-env", "ZAI_API_KEY"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}

	launcher := &fakeLauncher{pid: os.Getpid()}
	out, _, err := runCommandWithDeps(t, Dependencies{
		Launcher: launcher,
	}, "--db", dbPath, "launch")
	if err != nil {
		t.Fatalf("launch error = %v", err)
	}
	if launcher.starts != 1 {
		t.Fatalf("launcher starts = %d, want 1", launcher.starts)
	}
	if launcher.hasArg("--model") {
		t.Fatalf("launch args = %#v, want Claude Code default model", launcher.args)
	}
	if !strings.Contains(out, "No ccr startup model selected") {
		t.Fatalf("launch output missing default summary:\n%s", out)
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
	if !strings.Contains(out, "No ccr startup model selected") {
		t.Fatalf("launch output missing default model summary: %q", out)
	}
	if !strings.Contains(out, "/model anthropic.ccr.gpt") {
		t.Fatalf("launch output missing picker model guidance: %q", out)
	}
	if !launcher.hasEnvPrefix("ANTHROPIC_BASE_URL=http://127.0.0.1:") ||
		launcher.hasEnvPrefix("ANTHROPIC_API_KEY=") ||
		!launcher.hasEnvPrefix("ANTHROPIC_CUSTOM_HEADERS=X-CCR-Session-Token: ") ||
		launcher.hasEnv("CLAUDE_CODE_USE_GATEWAY=1") || !launcher.hasEnv("CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1") ||
		launcher.hasEnvPrefix("ANTHROPIC_CUSTOM_MODEL_OPTION=") ||
		!launcher.hasEnv("ENABLE_TOOL_SEARCH=true") || launcher.hasEnvPrefix("CLAUDE_CODE_ENABLE_AUTO_MODE=") {
		t.Fatalf("launch env = %s", launcher.environmentSummary())
	}
	assertPreserveAuthEnv(t, launcher)
	if launcher.hasEnv("CLAUDE_CODE_SIMPLE=1") {
		t.Fatalf("launch env still enables simple mode: %s", launcher.environmentSummary())
	}
	if launcher.hasArg("--tools") || launcher.hasArg("--model") {
		t.Fatalf("launch args = %#v", launcher.args)
	}
	settings, ok := launcher.settingsArgValue()
	if !ok || !strings.Contains(settings, "anthropic.ccr.gpt") {
		t.Fatalf("launch settings = %q ok=%v args=%#v", settings, ok, launcher.args)
	}

	out, _, err = runCommand(t, "--db", dbPath, "sessions")
	if err != nil {
		t.Fatalf("sessions after launch error = %v", err)
	}
	if !strings.Contains(out, "status=exited") || !strings.Contains(out, "model=(request-selected)") {
		t.Fatalf("sessions after launch output = %q", out)
	}
}

func TestLaunchHelpDescribesPreserveAuthModelSelection(t *testing.T) {
	out, _, err := runCommandWithDeps(t, Dependencies{}, "launch", "--help")
	if err != nil {
		t.Fatalf("launch help error = %v", err)
	}
	for _, want := range []string{
		"registered, compatible aliases to the visual /model picker",
		"permitted Anthropic models while preserving subscription or API-key",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("launch help = %q, missing %q", out, want)
		}
	}
}

func TestLaunchAppendsCCRSessionTokenToExistingCustomHeaders(t *testing.T) {
	t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "X-Existing: one")

	server := newModelsServer(t, []string{"gpt-5"})
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", server.URL, "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "gpt", "--provider", "litellm", "--model", "gpt-5"); err != nil {
		t.Fatalf("model add error = %v", err)
	}

	launcher := &fakeLauncher{pid: os.Getpid()}
	if _, _, err := runCommandWithDeps(t, Dependencies{
		Launcher: launcher,
	}, "--db", dbPath, "launch", "--model", "gpt"); err != nil {
		t.Fatalf("launch error = %v", err)
	}
	headers, ok := launcher.envValue("ANTHROPIC_CUSTOM_HEADERS")
	if !ok {
		t.Fatalf("launch env missing ANTHROPIC_CUSTOM_HEADERS: %s", launcher.environmentSummary())
	}
	if !strings.HasPrefix(headers, "X-Existing: one\nX-CCR-Session-Token: ") {
		t.Fatalf("custom headers = %q", headers)
	}
	assertPreserveAuthEnv(t, launcher)
}

func TestLaunchReplacesExistingCCRSessionTokenInCustomHeaders(t *testing.T) {
	t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "X-Existing: one\nX-CCR-Session-Token: stale-token\nX-Other: two")

	server := newModelsServer(t, []string{"gpt-5"})
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", server.URL, "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "gpt", "--provider", "litellm", "--model", "gpt-5"); err != nil {
		t.Fatalf("model add error = %v", err)
	}

	launcher := &fakeLauncher{pid: os.Getpid()}
	if _, _, err := runCommandWithDeps(t, Dependencies{
		Launcher: launcher,
	}, "--db", dbPath, "launch", "--model", "gpt"); err != nil {
		t.Fatalf("launch error = %v", err)
	}
	headers, ok := launcher.envValue("ANTHROPIC_CUSTOM_HEADERS")
	if !ok {
		t.Fatalf("launch env missing ANTHROPIC_CUSTOM_HEADERS: %s", launcher.environmentSummary())
	}
	if strings.Contains(headers, "stale-token") {
		t.Fatalf("custom headers kept stale CCR token: %q", headers)
	}
	if strings.Count(headers, "X-CCR-Session-Token: ") != 1 {
		t.Fatalf("custom headers = %q, want exactly one CCR token header", headers)
	}
	if !strings.Contains(headers, "X-Existing: one") || !strings.Contains(headers, "X-Other: two") {
		t.Fatalf("custom headers did not preserve existing non-CCR headers: %q", headers)
	}
}

func TestLaunchPreserveAuthModeClearsInheritedGatewayAuth(t *testing.T) {
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "stale-gateway-token")
	t.Setenv("CLAUDE_CODE_USE_GATEWAY", "1")

	server := newModelsServer(t, []string{"gpt-5"})
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", server.URL, "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "gpt", "--provider", "litellm", "--model", "gpt-5"); err != nil {
		t.Fatalf("model add error = %v", err)
	}

	launcher := &fakeLauncher{pid: os.Getpid()}
	if _, _, err := runCommandWithDeps(t, Dependencies{
		Launcher: launcher,
	}, "--db", dbPath, "launch", "--model", "gpt"); err != nil {
		t.Fatalf("launch error = %v", err)
	}
	assertPreserveAuthEnv(t, launcher)
}

func assertPreserveAuthEnv(t *testing.T, launcher *fakeLauncher) {
	t.Helper()
	if !launcher.unsetsEnv("ANTHROPIC_AUTH_TOKEN") {
		t.Fatalf("preserve launch env should unset inherited ANTHROPIC_AUTH_TOKEN: %s", launcher.environmentSummary())
	}
	if !launcher.unsetsEnv("CLAUDE_CODE_USE_GATEWAY") {
		t.Fatalf("preserve launch env should unset inherited CLAUDE_CODE_USE_GATEWAY: %s", launcher.environmentSummary())
	}
	if launcher.hasEnvPrefix("ANTHROPIC_API_KEY=") {
		t.Fatalf("preserve launch env overrides Anthropic API-key auth: %s", launcher.environmentSummary())
	}
}

func TestLaunchGatewayTokenAuthModeUsesLegacyAuthToken(t *testing.T) {
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
	out, _, err := runCommandWithDeps(t, Dependencies{
		Launcher: launcher,
	}, "--db", dbPath, "launch", "--model", "gpt", "--auth-mode", "gateway-token", "--permission-mode", "auto")
	if err != nil {
		t.Fatalf("launch error = %v", err)
	}
	if !launcher.hasEnvPrefix("ANTHROPIC_AUTH_TOKEN=") {
		t.Fatalf("gateway-token launch env missing ANTHROPIC_AUTH_TOKEN: %s", launcher.environmentSummary())
	}
	if !launcher.unsetsEnv("CLAUDE_CODE_USE_GATEWAY") || launcher.hasEnvPrefix("CLAUDE_CODE_USE_GATEWAY=") {
		t.Fatalf("gateway-token launch env should unset CLAUDE_CODE_USE_GATEWAY: %s", launcher.environmentSummary())
	}
	if !launcher.unsetsEnv("ANTHROPIC_API_KEY") {
		t.Fatalf("gateway-token launch env should unset inherited ANTHROPIC_API_KEY: %s", launcher.environmentSummary())
	}
	if !launcher.hasEnv("ENABLE_TOOL_SEARCH=true") {
		t.Fatalf("gateway-token launch env missing ENABLE_TOOL_SEARCH: %s", launcher.environmentSummary())
	}
	if launcher.hasEnvPrefix("CLAUDE_CODE_ENABLE_AUTO_MODE=") {
		t.Fatalf("gateway-token launch env should not force legacy auto mode opt-in: %s", launcher.environmentSummary())
	}
	if launcher.hasEnvPrefix("ANTHROPIC_CUSTOM_HEADERS=") {
		t.Fatalf("gateway-token launch env should not set custom headers: %s", launcher.environmentSummary())
	}
	if !strings.Contains(out, "not active in --auth-mode gateway-token") {
		t.Fatalf("launch output missing gateway-token warning:\n%s", out)
	}
	if !strings.Contains(out, "auto mode may require first-party Anthropic access") {
		t.Fatalf("launch output missing auto-mode compatibility warning:\n%s", out)
	}
}

func TestLaunchGatewayTokenAuthModeRequiresCCRStartupModel(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	launcher := &fakeLauncher{pid: os.Getpid()}
	_, _, err := runCommandWithDeps(t, Dependencies{
		Launcher: launcher,
	}, "--db", dbPath, "launch", "--auth-mode", "gateway-token")
	if err == nil {
		t.Fatalf("launch unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "--auth-mode gateway-token requires --model") {
		t.Fatalf("launch error = %v", err)
	}
	if launcher.starts != 0 {
		t.Fatalf("launcher starts = %d, want 0", launcher.starts)
	}
}

func TestLaunchInvalidAuthModeFailsBeforeDatabaseOpen(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	launcher := &fakeLauncher{pid: os.Getpid()}
	_, _, err := runCommandWithDeps(t, Dependencies{
		Launcher: launcher,
	}, "--db", dbPath, "launch", "--auth-mode", "bad")
	if err == nil {
		t.Fatalf("launch unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "invalid launch auth mode") {
		t.Fatalf("launch error = %v", err)
	}
	if launcher.starts != 0 {
		t.Fatalf("launcher starts = %d, want 0", launcher.starts)
	}
	if _, statErr := os.Stat(dbPath); !os.IsNotExist(statErr) {
		t.Fatalf("database exists after invalid launch auth mode: stat err=%v", statErr)
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
	if !launcher.hasArg("--tools") || !launcher.hasArg("") || !launcher.hasEnv("CLAUDE_CODE_SIMPLE=1") || !launcher.hasEnv("ENABLE_TOOL_SEARCH=") {
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
	if !launcher.hasArg("--tools") || !launcher.hasEnv("CLAUDE_CODE_SIMPLE=1") || !launcher.hasEnv("ENABLE_TOOL_SEARCH=") {
		t.Fatalf("chat-only provider launch args=%#v env=%#v", launcher.args, launcher.env)
	}
	if !strings.Contains(out, "Provider protocol=openai-compatible mode=chat-only token-count=provider") || !strings.Contains(out, "tools are disabled") {
		t.Fatalf("launch output missing provider degradation:\n%s", out)
	}
	if strings.Contains(out, "count_tokens requests will be rejected") {
		t.Fatalf("launch output contains stale count_tokens rejection warning:\n%s", out)
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

func TestLaunchCleansUpClaudeWhenLaunchActivationFails(t *testing.T) {
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
	if _, execErr := db.Exec(`
CREATE TRIGGER fail_launch_activation
BEFORE UPDATE OF gateway_url, pid, state ON launches
WHEN NEW.state = 'running'
BEGIN
  SELECT RAISE(FAIL, 'launch activation denied');
END;
`); execErr != nil {
		t.Fatalf("create trigger error = %v", execErr)
	}

	launcher := &fakeLauncher{pid: os.Getpid()}
	_, _, err = runCommandWithDeps(t, Dependencies{
		Launcher: launcher,
	}, "--db", dbPath, "launch")
	if err == nil {
		t.Fatalf("launch unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "activating launch record") {
		t.Fatalf("launch error = %v", err)
	}
	if launcher.process == nil {
		t.Fatalf("launcher did not return a process")
	}
	if !launcher.process.stopped {
		t.Fatalf("process was not stopped after launch activation failure")
	}
	if !launcher.process.waited {
		t.Fatalf("process was not waited after launch activation failure")
	}
}
