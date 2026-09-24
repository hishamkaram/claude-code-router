package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/store"
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
	legacyStore, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	if err := legacyStore.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if err := legacyStore.AddModel(context.Background(), store.Model{
		Alias: "legacy-control", ProviderName: "litellm", ProviderModel: "all-proxy-models", Status: "degraded",
	}); err != nil {
		t.Fatalf("AddModel(legacy-control) error = %v", err)
	}
	if err := legacyStore.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	launcher := &fakeLauncher{pid: os.Getpid()}
	if _, _, err := runCommandWithDeps(t, Dependencies{
		Launcher: launcher,
	}, "--db", dbPath, "launch", "--model", "gpt"); err != nil {
		t.Fatalf("launch error = %v", err)
	}
	payload := launchSettingsPayload(t, launcher)
	if !slices.Contains(payload.AvailableModels, "anthropic.ccr.gpt") {
		t.Fatalf("availableModels = %#v, want anthropic.ccr.gpt", payload.AvailableModels)
	}
	if !slices.Contains(payload.AvailableModels, "sonnet") {
		t.Fatalf("availableModels = %#v, want existing sonnet entry preserved", payload.AvailableModels)
	}
	if slices.Contains(payload.AvailableModels, "anthropic.ccr.blocked") {
		t.Fatalf("availableModels includes blocked alias: %#v", payload.AvailableModels)
	}
	if slices.Contains(payload.AvailableModels, "anthropic.ccr.legacy-control") {
		t.Fatalf("availableModels includes legacy LiteLLM control alias: %#v", payload.AvailableModels)
	}
}

func TestLaunchModelPickerMapsCCRAliasWithoutOverwritingNativeOptions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	settings := `{"modelPicker":{"options":[{"model":"native-model","label":"Native"},{"model":"anthropic.ccr.gpt","label":"stale"}],"replaceBuiltInOptions":true}}`
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	got, err := launchModelPickerOptions([]store.Model{{Alias: "gpt", ProviderModel: "gpt-5"}})
	if err != nil {
		t.Fatalf("launchModelPickerOptions() error = %v", err)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var picker struct {
		Options               []claudeModelPickerOption `json:"options"`
		ReplaceBuiltInOptions bool                      `json:"replaceBuiltInOptions"`
	}
	if err := json.Unmarshal(encoded, &picker); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if !picker.ReplaceBuiltInOptions {
		t.Fatal("replaceBuiltInOptions = false, want native setting preserved")
	}
	if len(picker.Options) != 2 {
		t.Fatalf("model picker options = %#v, want native row plus one CCR row", picker.Options)
	}
	if picker.Options[0].Model != "native-model" || picker.Options[0].Label != "Native" {
		t.Fatalf("native picker option = %#v, want preserved native option", picker.Options[0])
	}
	gotCCR := picker.Options[1]
	if gotCCR.Model != "anthropic.ccr.gpt" || gotCCR.Label != "CCR gpt" || gotCCR.Description != "CCR-routed provider model gpt-5" || gotCCR.BehavesAs != claudeCCRModelBehavesAs {
		t.Fatalf("CCR picker option = %#v, want explicit CCR mapping", gotCCR)
	}
}

func TestLaunchModelPickerPreservesReplacementAcrossLayeredSettings(t *testing.T) {
	configDir := t.TempDir()
	workspace := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Chdir(workspace)
	if err := os.MkdirAll(filepath.Join(workspace, ".claude"), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(`{"modelPicker":{"replaceBuiltInOptions":true}}`), 0o600); err != nil {
		t.Fatalf("WriteFile(global) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".claude", "settings.json"), []byte(`{"modelPicker":{"options":[{"model":"workspace-native","label":"Workspace"}]}}`), 0o600); err != nil {
		t.Fatalf("WriteFile(workspace) error = %v", err)
	}

	got, err := launchModelPickerOptions([]store.Model{{Alias: "gpt", ProviderModel: "gpt-5"}})
	if err != nil {
		t.Fatalf("launchModelPickerOptions() error = %v", err)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var picker struct {
		ReplaceBuiltInOptions bool `json:"replaceBuiltInOptions"`
	}
	if err := json.Unmarshal(encoded, &picker); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if !picker.ReplaceBuiltInOptions {
		t.Fatalf("replaceBuiltInOptions = false, want earlier layered setting preserved; payload=%s", encoded)
	}
}

func TestClaudeSettingsPathsDeduplicateConfiguredWorkspaceDirectory(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(workspace, ".claude"))
	t.Chdir(workspace)

	paths := claudeSettingsPaths()
	want := []string{
		filepath.Join(workspace, ".claude", "settings.json"),
		filepath.Join(workspace, ".claude", "settings.local.json"),
	}
	if !slices.Equal(paths, want) {
		t.Fatalf("claudeSettingsPaths() = %#v, want %#v", paths, want)
	}
}

func TestLaunchUsesCapabilityAwareOneMillionClaudeModelID(t *testing.T) {
	t.Parallel()
	server := newModelsServer(t, []string{"glm-5.2[1m]"})
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", server.URL, "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "glm", "--provider", "litellm", "--model", "glm-5.2[1m]"); err != nil {
		t.Fatalf("model add error = %v", err)
	}
	launcher := &fakeLauncher{pid: os.Getpid()}
	if _, _, err := runCommandWithDeps(t, Dependencies{Launcher: launcher}, "--db", dbPath, "launch", "--model", "glm"); err != nil {
		t.Fatalf("launch error = %v", err)
	}
	if got, ok := launcher.envValue("ANTHROPIC_CUSTOM_MODEL_OPTION"); !ok || got != "anthropic.ccr.glm[1m]" {
		t.Fatalf("ANTHROPIC_CUSTOM_MODEL_OPTION = %q, present=%v", got, ok)
	}
	payload := launchSettingsPayload(t, launcher)
	if !slices.Contains(payload.AvailableModels, "anthropic.ccr.glm[1m]") ||
		slices.Contains(payload.AvailableModels, "anthropic.ccr.glm") {
		t.Fatalf("availableModels = %#v", payload.AvailableModels)
	}
}

func TestLaunchExcludesModelsWithoutStreamingSupport(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	server := newModelsServer(t, []string{"stream-model", "non-stream-model"})
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", server.URL, "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	for _, model := range []struct {
		alias     string
		provider  string
		streaming string
	}{
		{alias: "stream", provider: "stream-model", streaming: "true"},
		{alias: "non-stream", provider: "non-stream-model", streaming: "false"},
	} {
		if _, _, err := runCommand(t, "--db", dbPath, "model", "add", model.alias, "--provider", "litellm", "--model", model.provider); err != nil {
			t.Fatalf("model add %q error = %v", model.alias, err)
		}
		if _, _, err := runCommand(t, "--db", dbPath, "model", "update", model.alias, "--streaming", model.streaming); err != nil {
			t.Fatalf("model update %q error = %v", model.alias, err)
		}
	}

	launcher := &fakeLauncher{pid: os.Getpid()}
	if _, _, err := runCommandWithDeps(t, Dependencies{Launcher: launcher}, "--db", dbPath, "launch"); err != nil {
		t.Fatalf("launch error = %v", err)
	}
	payload := launchSettingsPayload(t, launcher)
	if !slices.Contains(payload.AvailableModels, "anthropic.ccr.stream") {
		t.Fatalf("availableModels = %#v, want streaming alias", payload.AvailableModels)
	}
	if slices.Contains(payload.AvailableModels, "anthropic.ccr.non-stream") {
		t.Fatalf("availableModels includes non-streaming alias: %#v", payload.AvailableModels)
	}
}

func TestLaunchRejectsExplicitNonStreamingStartupModel(t *testing.T) {
	server := newModelsServer(t, []string{"non-stream-model"})
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", server.URL, "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "non-stream", "--provider", "litellm", "--model", "non-stream-model"); err != nil {
		t.Fatalf("model add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "update", "non-stream", "--streaming", "false"); err != nil {
		t.Fatalf("model update error = %v", err)
	}

	launcher := &fakeLauncher{pid: os.Getpid()}
	_, _, err := runCommandWithDeps(t, Dependencies{Launcher: launcher}, "--db", dbPath, "launch", "--model", "non-stream")
	if err == nil || !strings.Contains(err.Error(), "do not support streaming") || !strings.Contains(err.Error(), "excluded from /model") {
		t.Fatalf("launch error = %v", err)
	}
	if launcher.starts != 0 {
		t.Fatalf("launcher starts = %d, want 0", launcher.starts)
	}
}

func TestLaunchExcludesResponsesModelsWithoutProviderResponsesSupport(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	server := newModelsServer(t, []string{"responses-model"})
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", server.URL, "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "responses", "--provider", "litellm", "--model", "responses-model"); err != nil {
		t.Fatalf("model add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "update", "responses", "--model-kind", "responses", "--responses", "true"); err != nil {
		t.Fatalf("model update error = %v", err)
	}

	launcher := &fakeLauncher{pid: os.Getpid()}
	if _, _, err := runCommandWithDeps(t, Dependencies{Launcher: launcher}, "--db", dbPath, "launch"); err != nil {
		t.Fatalf("launch error = %v", err)
	}
	payload := launchSettingsPayload(t, launcher)
	if slices.Contains(payload.AvailableModels, "anthropic.ccr.responses") {
		t.Fatalf("availableModels includes Responses alias without provider support: %#v", payload.AvailableModels)
	}

	startupLauncher := &fakeLauncher{pid: os.Getpid()}
	_, _, err := runCommandWithDeps(t, Dependencies{Launcher: startupLauncher}, "--db", dbPath, "launch", "--model", "responses")
	if err == nil || !strings.Contains(err.Error(), "OpenAI Responses API") || !strings.Contains(err.Error(), "excluded from /model") {
		t.Fatalf("launch error = %v", err)
	}
	if startupLauncher.starts != 0 {
		t.Fatalf("launcher starts = %d, want 0", startupLauncher.starts)
	}
}

func TestLaunchExcludesModelsWithoutSystemMessageSupport(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	server := newModelsServer(t, []string{"system-model", "no-system-model"})
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", server.URL, "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	for _, model := range []struct {
		alias          string
		provider       string
		systemMessages string
	}{
		{alias: "system", provider: "system-model", systemMessages: "true"},
		{alias: "no-system", provider: "no-system-model", systemMessages: "false"},
	} {
		if _, _, err := runCommand(t, "--db", dbPath, "model", "add", model.alias, "--provider", "litellm", "--model", model.provider); err != nil {
			t.Fatalf("model add %q error = %v", model.alias, err)
		}
		if _, _, err := runCommand(t, "--db", dbPath, "model", "update", model.alias, "--system-messages", model.systemMessages); err != nil {
			t.Fatalf("model update %q error = %v", model.alias, err)
		}
	}

	launcher := &fakeLauncher{pid: os.Getpid()}
	if _, _, err := runCommandWithDeps(t, Dependencies{Launcher: launcher}, "--db", dbPath, "launch"); err != nil {
		t.Fatalf("launch error = %v", err)
	}
	payload := launchSettingsPayload(t, launcher)
	if !slices.Contains(payload.AvailableModels, "anthropic.ccr.system") {
		t.Fatalf("availableModels = %#v, want system-message alias", payload.AvailableModels)
	}
	if slices.Contains(payload.AvailableModels, "anthropic.ccr.no-system") {
		t.Fatalf("availableModels includes alias without system-message support: %#v", payload.AvailableModels)
	}
}

func TestLaunchRejectsExplicitModelWithoutSystemMessageSupport(t *testing.T) {
	server := newModelsServer(t, []string{"no-system-model"})
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", server.URL, "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "no-system", "--provider", "litellm", "--model", "no-system-model"); err != nil {
		t.Fatalf("model add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "update", "no-system", "--system-messages", "false"); err != nil {
		t.Fatalf("model update error = %v", err)
	}

	launcher := &fakeLauncher{pid: os.Getpid()}
	_, _, err := runCommandWithDeps(t, Dependencies{Launcher: launcher}, "--db", dbPath, "launch", "--model", "no-system")
	if err == nil || !strings.Contains(err.Error(), "do not support system messages") || !strings.Contains(err.Error(), "excluded from /model") {
		t.Fatalf("launch error = %v", err)
	}
	if launcher.starts != 0 {
		t.Fatalf("launcher starts = %d, want 0", launcher.starts)
	}
}

func TestLaunchReadsAvailableModelsFromCustomClaudeConfigDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	customConfigDir := filepath.Join(t.TempDir(), "custom-claude")
	t.Setenv("CLAUDE_CONFIG_DIR", customConfigDir)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatalf("MkdirAll(home config) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(`{"availableModels":["ignored-home-model"]}`), 0o600); err != nil {
		t.Fatalf("WriteFile(home settings) error = %v", err)
	}
	if err := os.MkdirAll(customConfigDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(custom config) error = %v", err)
	}
	customSettings := []byte(`{"availableModels":["custom-model"],"statusLine":{"type":"command","command":"custom-status"}}`)
	customSettingsPath := filepath.Join(customConfigDir, "settings.json")
	if err := os.WriteFile(customSettingsPath, customSettings, 0o600); err != nil {
		t.Fatalf("WriteFile(custom settings) error = %v", err)
	}

	server := newModelsServer(t, []string{"gpt-5"})
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", server.URL, "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "gpt", "--provider", "litellm", "--model", "gpt-5"); err != nil {
		t.Fatalf("model add error = %v", err)
	}

	launcher := &fakeLauncher{pid: os.Getpid()}
	if _, _, err := runCommandWithDeps(t, Dependencies{Launcher: launcher}, "--db", dbPath, "launch"); err != nil {
		t.Fatalf("launch error = %v", err)
	}
	settingsJSON, ok := launcher.settingsArgValue()
	if !ok {
		t.Fatalf("launch args missing --settings: %#v", launcher.args)
	}
	var payload struct {
		AvailableModels []string        `json:"availableModels"`
		StatusLine      json.RawMessage `json:"statusLine"`
	}
	if err := json.Unmarshal([]byte(settingsJSON), &payload); err != nil {
		t.Fatalf("settings JSON %q did not parse: %v", settingsJSON, err)
	}
	for _, want := range []string{"custom-model", "anthropic.ccr.gpt"} {
		if !slices.Contains(payload.AvailableModels, want) {
			t.Fatalf("availableModels = %#v, want %s", payload.AvailableModels, want)
		}
	}
	if slices.Contains(payload.AvailableModels, "ignored-home-model") {
		t.Fatalf("availableModels used ignored home configuration: %#v", payload.AvailableModels)
	}
	if payload.StatusLine != nil {
		t.Fatalf("generated settings replaced custom status line: %s", settingsJSON)
	}
	current, err := os.ReadFile(customSettingsPath)
	if err != nil || !slices.Equal(current, customSettings) {
		t.Fatalf("custom Claude settings changed: %q, %v", current, err)
	}
}

func TestLaunchSettingsUseDependencyClaudeConfigDirOverEnvironment(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	environmentConfigDir := filepath.Join(t.TempDir(), "environment-claude")
	dependencyConfigDir := filepath.Join(t.TempDir(), "dependency-claude")
	for _, fixture := range []struct {
		dir      string
		settings string
	}{
		{dir: environmentConfigDir, settings: `{"availableModels":["environment-model"],"modelPicker":{"options":[{"model":"environment-picker"}]}}`},
		{dir: dependencyConfigDir, settings: `{"availableModels":["dependency-model"],"modelPicker":{"options":[{"model":"dependency-picker"}]}}`},
	} {
		if err := os.MkdirAll(fixture.dir, 0o700); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v", fixture.dir, err)
		}
		if err := os.WriteFile(filepath.Join(fixture.dir, "settings.json"), []byte(fixture.settings), 0o600); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", fixture.dir, err)
		}
	}
	t.Setenv("CLAUDE_CONFIG_DIR", environmentConfigDir)

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
		ClaudeConfigDir: dependencyConfigDir,
		Launcher:        launcher,
	}, "--db", dbPath, "launch"); err != nil {
		t.Fatalf("launch error = %v", err)
	}
	settingsJSON, ok := launcher.settingsArgValue()
	if !ok {
		t.Fatalf("launch args missing --settings: %#v", launcher.args)
	}
	var payload struct {
		AvailableModels []string `json:"availableModels"`
		ModelPicker     struct {
			Options []struct {
				Model string `json:"model"`
			} `json:"options"`
		} `json:"modelPicker"`
	}
	if err := json.Unmarshal([]byte(settingsJSON), &payload); err != nil {
		t.Fatalf("settings JSON %q did not parse: %v", settingsJSON, err)
	}
	if !slices.Contains(payload.AvailableModels, "dependency-model") || slices.Contains(payload.AvailableModels, "environment-model") {
		t.Fatalf("availableModels = %#v, want dependency profile only", payload.AvailableModels)
	}
	pickerModels := make([]string, 0, len(payload.ModelPicker.Options))
	for _, option := range payload.ModelPicker.Options {
		pickerModels = append(pickerModels, option.Model)
	}
	if !slices.Contains(pickerModels, "dependency-picker") || slices.Contains(pickerModels, "environment-picker") {
		t.Fatalf("model picker models = %#v, want dependency profile only", pickerModels)
	}
}

func TestLaunchSelectivelyEscapesAnthropicFamilyNamesInCCRAliases(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	server := newModelsServer(t, []string{"third-party-sonnet", "third-party-opus", "third-party-haiku"})
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", server.URL, "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	models := []struct {
		alias string
		model string
	}{
		{alias: "sonnet", model: "third-party-sonnet"},
		{alias: "opus", model: "third-party-opus"},
		{alias: "haiku", model: "third-party-haiku"},
	}
	for _, model := range models {
		if _, _, err := runCommand(t, "--db", dbPath, "model", "add", model.alias, "--provider", "litellm", "--model", model.model); err != nil {
			t.Fatalf("model add %q error = %v", model.alias, err)
		}
	}

	launcher := &fakeLauncher{pid: os.Getpid()}
	out, _, err := runCommandWithDeps(t, Dependencies{Launcher: launcher}, "--db", dbPath, "launch")
	if err != nil {
		t.Fatalf("launch error = %v", err)
	}
	payload := launchSettingsPayload(t, launcher)
	for _, want := range []string{
		"sonnet",
		"opus",
		"haiku",
		"anthropic.ccr.s%6fnnet",
		"anthropic.ccr.%6fpus",
		"anthropic.ccr.h%61iku",
	} {
		if !slices.Contains(payload.AvailableModels, want) {
			t.Fatalf("availableModels = %#v, want %s", payload.AvailableModels, want)
		}
		if strings.HasPrefix(want, "anthropic.ccr.") && !strings.Contains(out, "/model "+want) {
			t.Fatalf("launch output = %q, missing picker ID %q", out, want)
		}
	}
	for _, unwanted := range []string{
		"anthropic.ccr.sonnet",
		"anthropic.ccr.opus",
		"anthropic.ccr.haiku",
		"claude-ccr-sonnet",
		"claude-ccr-opus",
		"claude-ccr-haiku",
	} {
		if slices.Contains(payload.AvailableModels, unwanted) {
			t.Fatalf("availableModels = %#v, should not include %q", payload.AvailableModels, unwanted)
		}
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
	out, _, err := runCommandWithDeps(t, Dependencies{
		Launcher: launcher,
	}, "--db", dbPath, "launch")
	if err != nil {
		t.Fatalf("launch error = %v", err)
	}
	if launcher.hasArg("--model") {
		t.Fatalf("launch args = %#v, want Claude Code default startup model", launcher.args)
	}
	payload := launchSettingsPayload(t, launcher)
	for _, want := range []string{"sonnet", "anthropic.ccr.gpt", "anthropic.ccr.qwen"} {
		if !slices.Contains(payload.AvailableModels, want) {
			t.Fatalf("availableModels = %#v, want %s", payload.AvailableModels, want)
		}
	}
	if slices.Contains(payload.AvailableModels, "anthropic.ccr.chat") {
		t.Fatalf("availableModels includes tool-disabled alias in tools-enabled launch: %#v", payload.AvailableModels)
	}
	for _, want := range []string{"/model anthropic.ccr.gpt", "/model anthropic.ccr.qwen"} {
		if !strings.Contains(out, want) {
			t.Fatalf("launch output = %q, missing %q", out, want)
		}
	}
	if strings.Contains(out, "/model anthropic.ccr.chat") {
		t.Fatalf("launch output includes tool-disabled alias guidance: %q", out)
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
	for _, want := range []string{"sonnet", "opus", "anthropic.ccr.gpt"} {
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
	for _, want := range []string{"sonnet", "opus", "anthropic.ccr.gpt", "anthropic.ccr.qwen"} {
		if !slices.Contains(payload.AvailableModels, want) {
			t.Fatalf("availableModels = %#v, want %s", payload.AvailableModels, want)
		}
	}
	if slices.Contains(payload.AvailableModels, "anthropic.ccr.chat") {
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
	if slices.Contains(payload.AvailableModels, "anthropic.ccr.chat") {
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
