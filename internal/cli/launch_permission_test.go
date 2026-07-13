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

func TestLaunchAutoPermissionModeDoesNotForceLegacyAutoEnv(t *testing.T) {
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
	}, "--db", dbPath, "launch", "--model", "gpt", "--permission-mode", "auto")
	if err != nil {
		t.Fatalf("launch error = %v", err)
	}
	index := slices.Index(launcher.args, "--permission-mode")
	if index < 0 || index+1 >= len(launcher.args) || launcher.args[index+1] != "auto" {
		t.Fatalf("launch args = %#v", launcher.args)
	}
	if launcher.hasEnvPrefix("CLAUDE_CODE_ENABLE_AUTO_MODE=") {
		t.Fatalf("launch env should not force legacy auto mode opt-in: %#v", launcher.env)
	}
}

func TestLaunchForwardsClaudeCodeArguments(t *testing.T) {
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
	}, "--db", dbPath, "launch", "--model", "gpt", "--chrome", "--add-dir", "/tmp/extra", "review this change")
	if err != nil {
		t.Fatalf("launch error = %v", err)
	}
	modelIndex := slices.Index(launcher.args, "--model")
	if modelIndex < 0 || modelIndex+1 >= len(launcher.args) || launcher.args[modelIndex+1] != "claude-ccr-gpt" {
		t.Fatalf("launch args missing CCR model selection: %#v", launcher.args)
	}
	wantTail := []string{"--chrome", "--add-dir", "/tmp/extra", "review this change"}
	if len(launcher.args) < len(wantTail) || !slices.Equal(launcher.args[len(launcher.args)-len(wantTail):], wantTail) {
		t.Fatalf("launch args = %#v, want passthrough tail %#v", launcher.args, wantTail)
	}
}

func TestLaunchPreservesClaudeOptionTerminator(t *testing.T) {
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
	}, "--db", dbPath, "launch", "--model", "gpt", "--add-dir", "/tmp/extra", "--", "review this change")
	if err != nil {
		t.Fatalf("launch error = %v", err)
	}
	wantTail := []string{"--add-dir", "/tmp/extra", "--", "review this change"}
	if len(launcher.args) < len(wantTail) || !slices.Equal(launcher.args[len(launcher.args)-len(wantTail):], wantTail) {
		t.Fatalf("launch args = %#v, want passthrough tail %#v", launcher.args, wantTail)
	}
}

func TestParseLaunchInvocationForwardsLeadingTerminator(t *testing.T) {
	t.Parallel()

	invocation, err := parseLaunchInvocation([]string{"--", "--help"})
	if err != nil {
		t.Fatalf("parseLaunchInvocation() error = %v", err)
	}
	if !slices.Equal(invocation.claudeArgs, []string{"--", "--help"}) {
		t.Fatalf("claude args = %#v, want [-- --help]", invocation.claudeArgs)
	}
}

func TestLaunchMetadataCommandSkipsRouter(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	launcher := &fakeLauncher{pid: os.Getpid()}
	_, _, err := runCommandWithDeps(t, Dependencies{
		Launcher: launcher,
	}, "--db", dbPath, "launch", "--", "--version")
	if err != nil {
		t.Fatalf("launch error = %v", err)
	}
	if launcher.starts != 1 {
		t.Fatalf("launcher starts = %d, want 1", launcher.starts)
	}
	if !slices.Equal(launcher.args, []string{"--version"}) {
		t.Fatalf("launcher args = %#v, want [--version]", launcher.args)
	}
	if len(launcher.env.Set) != 0 || len(launcher.env.Unset) != 0 {
		t.Fatalf("launcher env = %#v, want none", launcher.env)
	}
	if _, statErr := os.Stat(dbPath); !os.IsNotExist(statErr) {
		t.Fatalf("database exists after Claude metadata command: stat err=%v", statErr)
	}
}

func TestLaunchOwnedOptionWithoutValueFailsBeforeDatabaseOpen(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	launcher := &fakeLauncher{pid: os.Getpid()}
	_, _, err := runCommandWithDeps(t, Dependencies{
		Launcher: launcher,
	}, "--db", dbPath, "launch", "--model")
	if err == nil {
		t.Fatalf("launch unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "--model requires a value") {
		t.Fatalf("launch error = %v", err)
	}
	if launcher.starts != 0 {
		t.Fatalf("launcher starts = %d, want 0", launcher.starts)
	}
	if _, statErr := os.Stat(dbPath); !os.IsNotExist(statErr) {
		t.Fatalf("database exists after missing model value: stat err=%v", statErr)
	}
}

func TestLaunchPreservesReservedLookingPromptAfterLeadingTerminator(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	launcher := &fakeLauncher{pid: os.Getpid()}
	_, _, err := runCommandWithDeps(t, Dependencies{
		Launcher: launcher,
	}, "--db", dbPath, "launch", "--", "--dangerously-skip-permissions", "explain this option")
	if err != nil {
		t.Fatalf("launch error = %v", err)
	}
	wantTail := []string{"--", "--dangerously-skip-permissions", "explain this option"}
	if len(launcher.args) < len(wantTail) || !slices.Equal(launcher.args[len(launcher.args)-len(wantTail):], wantTail) {
		t.Fatalf("launch args = %#v, want passthrough tail %#v", launcher.args, wantTail)
	}
	if launcher.starts != 1 {
		t.Fatalf("launcher starts = %d, want 1", launcher.starts)
	}
}

func TestLaunchParsesShortPrintValue(t *testing.T) {
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
	}, "--db", dbPath, "launch", "--model", "gpt", "-p=true")
	if err != nil {
		t.Fatalf("launch error = %v", err)
	}
	if !launcher.hasArg("--print") {
		t.Fatalf("launch args = %#v, want --print", launcher.args)
	}
	if strings.Contains(out, "Claude Code launched through") {
		t.Fatalf("print-mode stdout contains launch summary: %q", out)
	}
	if !strings.Contains(errOut, "Claude Code launched through") {
		t.Fatalf("print-mode stderr missing launch summary: %q", errOut)
	}
}

func TestLaunchRejectsToolsOverrideForChatOnlyRoute(t *testing.T) {
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
	_, _, err := runCommandWithDeps(t, Dependencies{
		Launcher: launcher,
	}, "--db", dbPath, "launch", "--model", "gpt", "--tools", "Read")
	if err == nil {
		t.Fatalf("launch unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "does not support tools") {
		t.Fatalf("launch error = %v", err)
	}
	if launcher.starts != 0 {
		t.Fatalf("launcher starts = %d, want 0", launcher.starts)
	}
}

func TestLaunchRejectsMCPConfigOverrideForChatOnlyRoute(t *testing.T) {
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
	_, _, err := runCommandWithDeps(t, Dependencies{
		Launcher: launcher,
	}, "--db", dbPath, "launch", "--model", "gpt", "--mcp-config", "server.json")
	if err == nil {
		t.Fatalf("launch unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "does not support tools") {
		t.Fatalf("launch error = %v", err)
	}
	if launcher.starts != 0 {
		t.Fatalf("launcher starts = %d, want 0", launcher.starts)
	}
}

func TestValidateLaunchPassthroughArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "allows Claude Code options", args: []string{"--chrome", "--verbose"}},
		{name: "allows reserved-looking prompt after terminator", args: []string{"--chrome", "--", "--model=sonnet"}},
		{name: "rejects model override", args: []string{"--model=sonnet"}, wantErr: "--model is managed"},
		{name: "rejects print override", args: []string{"-p=true"}, wantErr: "-p is managed"},
		{name: "rejects fallback model", args: []string{"--fallback-model", "sonnet"}, wantErr: "bypass the selected model route"},
		{name: "rejects background mode", args: []string{"--bg"}, wantErr: "detached agents"},
		{name: "allows disabled background mode", args: []string{"--background=false"}},
		{name: "rejects enabled background mode", args: []string{"--background=true"}, wantErr: "detached agents"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateLaunchPassthroughArgs(tt.args)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateLaunchPassthroughArgs() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateLaunchPassthroughArgs() error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateDynamicLaunchPassthroughArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		args         []string
		disableTools bool
		hasSettings  bool
		wantErr      string
	}{
		{name: "allows Claude Code options", args: []string{"--chrome", "--verbose"}},
		{name: "allows reserved-looking prompt after terminator", args: []string{"--", "--tools=Read"}, disableTools: true, hasSettings: true},
		{name: "rejects tool override", args: []string{"--tools", "Read"}, disableTools: true, wantErr: "does not support tools"},
		{name: "rejects MCP tool override", args: []string{"--mcp-config", "server.json"}, disableTools: true, wantErr: "does not support tools"},
		{name: "rejects settings override", args: []string{"--settings", "{}"}, hasSettings: true, wantErr: "model allowlist"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateDynamicLaunchPassthroughArgs(tt.args, tt.disableTools, tt.hasSettings)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateDynamicLaunchPassthroughArgs() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateDynamicLaunchPassthroughArgs() error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}
