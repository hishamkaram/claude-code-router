package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestLaunchExtendsExistingClaudeAvailableModelsForCCRAliases(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(`{"availableModels":["sonnet"]}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	server := newModelsServer(t, []string{"gpt-5", "blocked-model"})
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", server.URL, "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "gpt", "--provider", "litellm", "--model", "gpt-5"); err != nil {
		t.Fatalf("model add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "blocked", "--provider", "litellm", "--model", "blocked-model", "--compat", "blocked"); err != nil {
		t.Fatalf("blocked model add error = %v", err)
	}

	launcher := &fakeLauncher{pid: os.Getpid()}
	if _, _, err := runCommandWithDeps(t, Dependencies{
		Launcher: launcher,
	}, "--db", dbPath, "launch", "--model", "gpt"); err != nil {
		t.Fatalf("launch error = %v", err)
	}
	payload := launchSettingsPayload(t, launcher)
	if !slices.Contains(payload.AvailableModels, "claude-ccr-gpt") {
		t.Fatalf("availableModels = %#v, want claude-ccr-gpt", payload.AvailableModels)
	}
	if !slices.Contains(payload.AvailableModels, "sonnet") {
		t.Fatalf("availableModels = %#v, want existing sonnet entry preserved", payload.AvailableModels)
	}
	if slices.Contains(payload.AvailableModels, "claude-ccr-blocked") {
		t.Fatalf("availableModels includes blocked alias: %#v", payload.AvailableModels)
	}
}

func TestLaunchExtendsClaudeAvailableModelsWithoutStartupModel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(`{"availableModels":["sonnet"]}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	server := newModelsServer(t, []string{"gpt-5", "qwen3", "chat-model"})
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", server.URL, "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "gpt", "--provider", "litellm", "--model", "gpt-5"); err != nil {
		t.Fatalf("model add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "qwen", "--provider", "litellm", "--model", "qwen3"); err != nil {
		t.Fatalf("model add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "chat", "--provider", "litellm", "--model", "chat-model", "--compat", "chat-only"); err != nil {
		t.Fatalf("chat-only model add error = %v", err)
	}

	launcher := &fakeLauncher{pid: os.Getpid()}
	if _, _, err := runCommandWithDeps(t, Dependencies{
		Launcher: launcher,
	}, "--db", dbPath, "launch"); err != nil {
		t.Fatalf("launch error = %v", err)
	}
	if launcher.hasArg("--model") {
		t.Fatalf("launch args = %#v, want Claude Code default startup model", launcher.args)
	}
	payload := launchSettingsPayload(t, launcher)
	for _, want := range []string{"sonnet", "claude-ccr-gpt", "claude-ccr-qwen"} {
		if !slices.Contains(payload.AvailableModels, want) {
			t.Fatalf("availableModels = %#v, want %s", payload.AvailableModels, want)
		}
	}
	if slices.Contains(payload.AvailableModels, "claude-ccr-chat") {
		t.Fatalf("availableModels includes tool-disabled alias in tools-enabled launch: %#v", payload.AvailableModels)
	}
}

func TestLaunchRegistersStartupModelWhenClaudeAvailableModelsUnset(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

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
	payload := launchSettingsPayload(t, launcher)
	for _, want := range []string{"sonnet", "opus", "claude-ccr-gpt"} {
		if !slices.Contains(payload.AvailableModels, want) {
			t.Fatalf("availableModels = %#v, want %s", payload.AvailableModels, want)
		}
	}
}

func TestLaunchCreatesClaudeAvailableModelsAllowlistWithoutStartupModel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	server := newModelsServer(t, []string{"gpt-5", "qwen3", "chat-model"})
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", server.URL, "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "gpt", "--provider", "litellm", "--model", "gpt-5"); err != nil {
		t.Fatalf("model add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "qwen", "--provider", "litellm", "--model", "qwen3"); err != nil {
		t.Fatalf("model add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "chat", "--provider", "litellm", "--model", "chat-model", "--compat", "chat-only"); err != nil {
		t.Fatalf("chat-only model add error = %v", err)
	}

	launcher := &fakeLauncher{pid: os.Getpid()}
	if _, _, err := runCommandWithDeps(t, Dependencies{
		Launcher: launcher,
	}, "--db", dbPath, "launch"); err != nil {
		t.Fatalf("launch error = %v", err)
	}
	if launcher.hasArg("--model") {
		t.Fatalf("launch args = %#v, want Claude Code default startup model", launcher.args)
	}
	payload := launchSettingsPayload(t, launcher)
	for _, want := range []string{"sonnet", "opus", "claude-ccr-gpt", "claude-ccr-qwen"} {
		if !slices.Contains(payload.AvailableModels, want) {
			t.Fatalf("availableModels = %#v, want %s", payload.AvailableModels, want)
		}
	}
	if slices.Contains(payload.AvailableModels, "claude-ccr-chat") {
		t.Fatalf("availableModels includes tool-disabled alias in tools-enabled launch: %#v", payload.AvailableModels)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("launch wrote Claude settings file: stat err=%v", err)
	}
}

func TestLaunchCreatesFirstPartyAllowlistWhenAllAliasesNeedToolsDisabled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	server := newModelsServer(t, []string{"chat-model"})
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", server.URL, "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "chat", "--provider", "litellm", "--model", "chat-model", "--compat", "chat-only"); err != nil {
		t.Fatalf("chat-only model add error = %v", err)
	}

	launcher := &fakeLauncher{pid: os.Getpid()}
	if _, _, err := runCommandWithDeps(t, Dependencies{
		Launcher: launcher,
	}, "--db", dbPath, "launch"); err != nil {
		t.Fatalf("launch error = %v", err)
	}
	if launcher.hasArg("--model") {
		t.Fatalf("launch args = %#v, want Claude Code default startup model", launcher.args)
	}
	payload := launchSettingsPayload(t, launcher)
	for _, want := range []string{"sonnet", "opus"} {
		if !slices.Contains(payload.AvailableModels, want) {
			t.Fatalf("availableModels = %#v, want %s", payload.AvailableModels, want)
		}
	}
	if slices.Contains(payload.AvailableModels, "claude-ccr-chat") {
		t.Fatalf("availableModels includes tool-disabled alias in tools-enabled launch: %#v", payload.AvailableModels)
	}
}

type launchSettings struct {
	AvailableModels []string `json:"availableModels"`
}

func launchSettingsPayload(t *testing.T, launcher *fakeLauncher) launchSettings {
	t.Helper()
	settings, ok := launcher.settingsArgValue()
	if !ok {
		t.Fatalf("launch args missing --settings: %#v", launcher.args)
	}
	var payload launchSettings
	if err := json.Unmarshal([]byte(settings), &payload); err != nil {
		t.Fatalf("settings JSON %q did not parse: %v", settings, err)
	}
	return payload
}
