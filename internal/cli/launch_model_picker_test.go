package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/gateway"
	"github.com/hishamkaram/claude-code-router/internal/modelcap"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestLaunchCatalogUsesAdmittedModelIDsForPickerAndAllowlist(t *testing.T) {
	s := modelPickerTestStore(t)
	for _, includeToolsDisabled := range []bool{false, true} {
		result, err := launchClaudeSettingsArg(t.Context(), s, launchSettingsOptions{IncludeToolDisabled: includeToolsDisabled})
		if err != nil {
			t.Fatalf("launchClaudeSettingsArg() error = %v", err)
		}
		settings := decodeModelPickerSettings(t, result.JSON)
		want := []string{"anthropic.ccr.coder", "anthropic.ccr.wide[1m]"}
		if includeToolsDisabled {
			want = []string{"anthropic.ccr.chat", "anthropic.ccr.coder", "anthropic.ccr.wide[1m]"}
		}
		if got := pickerModels(settings.Picker.Options); !slices.Equal(got, want) {
			t.Fatalf("picker models = %v, want %v", got, want)
		}
		for _, id := range want {
			if !slices.Contains(settings.AvailableModels, id) {
				t.Fatalf("picker model %s absent from allowlist", id)
			}
		}
		for _, option := range settings.Picker.Options {
			if option.BehavesAs != "" {
				t.Fatal("generated catalog changed native model capability defaults")
			}
		}
		if settings.Picker.ReplaceBuiltInOptions != nil {
			t.Fatal("generated catalog changed built-in picker preference")
		}
	}
}

func TestLaunchCatalogPreservesUserPickerAndIgnoresProjectPicker(t *testing.T) {
	s := modelPickerTestStore(t)
	userDir := os.Getenv("CLAUDE_CONFIG_DIR")
	writeModelPickerTestSettings(t, filepath.Join(userDir, "settings.json"), `{"modelPicker":{"replaceBuiltInOptions":true,"options":[{"model":"user-model","label":"My model","description":"My description","behavesAs":"claude-sonnet-4-6"},{"model":"anthropic.ccr.coder","label":"My coder"}]}}`)
	project := t.TempDir()
	t.Chdir(project)
	writeModelPickerTestSettings(t, filepath.Join(project, ".claude", "settings.json"), `{"modelPicker":{"options":[{"model":"project-only"}]}}`)
	writeModelPickerTestSettings(t, filepath.Join(userDir, "settings.local.json"), `{"modelPicker":{"options":[{"model":"local-only"}]}}`)
	result, err := launchClaudeSettingsArg(t.Context(), s, launchSettingsOptions{})
	if err != nil {
		t.Fatalf("launchClaudeSettingsArg() error = %v", err)
	}
	settings := decodeModelPickerSettings(t, result.JSON)
	want := []string{"user-model", "anthropic.ccr.coder", "anthropic.ccr.wide[1m]"}
	if got := pickerModels(settings.Picker.Options); !slices.Equal(got, want) {
		t.Fatalf("picker models = %v, want %v", got, want)
	}
	if settings.Picker.ReplaceBuiltInOptions == nil || !*settings.Picker.ReplaceBuiltInOptions {
		t.Fatal("user replaceBuiltInOptions preference was lost")
	}
	first := settings.Picker.Options[0]
	if first.Label != "My model" || first.Description != "My description" || first.BehavesAs != "claude-sonnet-4-6" || settings.Picker.Options[1].Label != "My coder" {
		t.Fatal("user picker row metadata was changed")
	}
}

func TestLaunchCatalogPinnedRouteExcludesOtherModels(t *testing.T) {
	s := modelPickerTestStore(t)
	model, err := s.GetModel(t.Context(), "wide")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := s.GetProvider(t.Context(), "picker")
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := gateway.NewPinnedRoute(model, provider)
	if err != nil {
		t.Fatal(err)
	}
	if removeErr := s.RemoveModel(t.Context(), "wide"); removeErr != nil {
		t.Fatal(removeErr)
	}
	if addErr := s.AddModel(t.Context(), store.Model{Alias: "wide", ProviderName: "picker", ProviderModel: "changed", Status: "degraded"}); addErr != nil {
		t.Fatal(addErr)
	}
	writeModelPickerTestSettings(t, filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json"), `{"modelPicker":{"options":[{"model":"user-model"}]}}`)
	result, err := launchClaudeSettingsArg(t.Context(), s, launchSettingsOptions{PinnedRoute: &pinned})
	if err != nil {
		t.Fatal(err)
	}
	settings := decodeModelPickerSettings(t, result.JSON)
	want := []string{"anthropic.ccr.wide[1m]"}
	if !slices.Equal(settings.AvailableModels, want) || !slices.Equal(pickerModels(settings.Picker.Options), want) {
		t.Fatal("pinned catalog admitted an unrelated model")
	}
}

func TestLaunchCatalogPaddedUserRowPreservesNativeIdentityAndMetadata(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		name := "registered"
		if pinned {
			name = "pinned"
		}
		t.Run(name, func(t *testing.T) {
			modelPickerTestStore(t)
			const canonical = "anthropic.ccr.coder"
			const padded = " " + canonical + " "
			writeModelPickerTestSettings(t, filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json"),
				`{"modelPicker":{"options":[{"model":"`+padded+`","label":"Padded user row"}]}}`)
			settings := make(map[string]any)
			if err := addLaunchModelPicker(settings, []string{canonical}, nil, pinned); err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(settings)
			if err != nil {
				t.Fatal(err)
			}
			options := decodeModelPickerSettings(t, string(encoded)).Picker.Options
			want := []string{padded}
			if !slices.Equal(pickerModels(options), want) {
				t.Fatal("native trimmed model identity lost its user row or was duplicated")
			}
			if options[0].Label != "Padded user row" {
				t.Fatal("user picker metadata was changed")
			}
		})
	}
}

func TestLaunchCatalogRejectsMalformedUserPickerWithoutEchoingValues(t *testing.T) {
	s := modelPickerTestStore(t)
	model, err := s.GetModel(t.Context(), "coder")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := s.GetProvider(t.Context(), "picker")
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := gateway.NewPinnedRoute(model, provider)
	if err != nil {
		t.Fatal(err)
	}
	for _, contents := range []string{
		`{"modelPicker":null}`,
		`{"modelPicker":987654321}`,
		`{"modelPicker":"synthetic-private-marker"}`,
		`{"modelPicker":{"options":"synthetic-private-marker"}}`,
		`{"modelPicker":{"options":[{"model":42,"label":"synthetic-private-marker"}]}}`,
		`{"modelPicker":{"options":[{"model":""}]}}`,
		`{"modelPicker":{"options":[{"model":987654321}]}}`,
		`{"modelPicker":{"options":[{"model":"coder","label":987654321}]}}`,
		`{"modelPicker":{"options":[{"model":"coder","label":null}]}}`,
		`{"modelPicker":{"options":[{"model":"coder","description":null}]}}`,
		`{"modelPicker":{"options":[{"model":"coder","behavesAs":null}]}}`,
		`{"modelPicker":{"options":[],"replaceBuiltInOptions":"synthetic-private-marker"}}`,
		`{"modelPicker":{"options":[],"replaceBuiltInOptions":987654321}}`,
		`{"modelPicker":{"options":[],"replaceBuiltInOptions":null}}`,
	} {
		writeModelPickerTestSettings(t, filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json"), contents)
		for _, options := range []launchSettingsOptions{{}, {PinnedRoute: &pinned}} {
			_, settingsErr := launchClaudeSettingsArg(t.Context(), s, options)
			if settingsErr == nil {
				t.Fatal("malformed user picker accepted")
			}
			if strings.Contains(settingsErr.Error(), "synthetic-private-marker") || strings.Contains(settingsErr.Error(), "987654321") {
				t.Fatal("error echoed a setting value")
			}
			if !strings.Contains(settingsErr.Error(), "settings.json") || !strings.Contains(settingsErr.Error(), "modelPicker") {
				t.Fatal("malformed picker error omitted its file or setting name")
			}
		}
	}
}

func TestLaunchCatalogMalformedDocumentDoesNotEchoNumericValue(t *testing.T) {
	s := modelPickerTestStore(t)
	writeModelPickerTestSettings(t, filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json"), `987654321`)
	_, err := launchClaudeSettingsArg(t.Context(), s, launchSettingsOptions{})
	if err == nil || strings.Contains(err.Error(), "987654321") {
		t.Fatal("malformed numeric settings were accepted or exposed")
	}
}

func TestLaunchCatalogPreservesEachBuiltInPreference(t *testing.T) {
	s := modelPickerTestStore(t)
	for _, preference := range []string{"", `,"replaceBuiltInOptions":false`, `,"replaceBuiltInOptions":true`} {
		path := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json")
		original := `{"modelPicker":{"options":[{"model":"user-model","label":"Mine"}]` + preference + `},"env":{"UNRELATED_SECRET":"synthetic-private-marker"}}`
		writeModelPickerTestSettings(t, path, original)
		result, err := launchClaudeSettingsArg(t.Context(), s, launchSettingsOptions{})
		if err != nil {
			t.Fatal(err)
		}
		settings := decodeModelPickerSettings(t, result.JSON)
		got := settings.Picker.ReplaceBuiltInOptions
		if preference == "" && got != nil || preference != "" && (got == nil || *got != strings.Contains(preference, "true")) {
			t.Fatal("user built-in picker preference changed")
		}
		if strings.Contains(result.JSON, "synthetic-private-marker") || strings.Contains(result.JSON, "UNRELATED_SECRET") {
			t.Fatal("picker merge copied an unrelated setting")
		}
		data, err := os.ReadFile(path)
		if err != nil || string(data) != original {
			t.Fatal("launch modified user settings")
		}
	}
}

func TestLaunchCatalogHonorsUserSettingsSource(t *testing.T) {
	s := modelPickerTestStore(t)
	writeModelPickerTestSettings(t, filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json"), `{"modelPicker":{"options":[{"model":"user-model"}]}}`)
	for _, test := range []struct {
		args []string
		user bool
	}{
		{nil, true},
		{[]string{"--setting-sources", "user,local"}, true},
		{[]string{"--setting-sources", "project, user"}, true},
		{[]string{"--setting-sources", " user "}, true},
		{[]string{"--setting-sources", "project, local"}, false},
		{[]string{"--setting-sources=project,local"}, false},
		{[]string{"--setting-sources", ""}, false},
		{[]string{"--setting-sources=user", "--setting-sources=project"}, false},
		{[]string{"--", "--setting-sources="}, true},
	} {
		result, err := launchClaudeSettingsArg(t.Context(), s, launchSettingsOptions{ClaudeArgs: test.args})
		if err != nil {
			t.Fatal(err)
		}
		settings := decodeModelPickerSettings(t, result.JSON)
		if got := slices.Contains(pickerModels(settings.Picker.Options), "user-model"); got != test.user {
			t.Fatalf("user picker included=%t, want=%t for args %v", got, test.user, test.args)
		}
	}
}

func TestLaunchCatalogForwardsSettingsSourceToBuilderAndNative(t *testing.T) {
	modelPickerTestStore(t)
	writeModelPickerTestSettings(t, filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json"), `{"modelPicker":{"options":[{"model":"user-model"}]}}`)
	launcher := &fakeLauncher{pid: os.Getpid()}
	if _, _, err := runCommandWithDeps(t, Dependencies{Launcher: launcher},
		"--db", filepath.Join(os.Getenv("HOME"), "ccr.db"), "launch", "--auth-mode", "preserve", "--no-statusline", "--no-lifecycle",
		"--setting-sources", "project,local"); err != nil {
		t.Fatal(err)
	}
	settings, found := launcher.settingsArgValue()
	if !found {
		t.Fatal("missing generated settings")
	}
	payload := decodeModelPickerSettings(t, settings)
	if slices.Contains(pickerModels(payload.Picker.Options), "user-model") {
		t.Fatal("disabled user picker was promoted into flag settings")
	}
	if !launcher.hasArg("--setting-sources") || !launcher.hasArg("project,local") {
		t.Fatal("native settings-source option was lost")
	}
}

func TestLaunchCatalogDoesNotOverridePickerWithoutRoutableModels(t *testing.T) {
	s := modelPickerTestStore(t)
	for _, alias := range []string{"coder", "wide", "chat"} {
		if err := s.RemoveModel(t.Context(), alias); err != nil {
			t.Fatal(err)
		}
	}
	result, err := launchClaudeSettingsArg(t.Context(), s, launchSettingsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.JSON != "" {
		t.Fatalf("empty catalog generated settings: %s", result.JSON)
	}
}

type modelPickerTestOption struct {
	Model       string `json:"model"`
	Label       string `json:"label"`
	Description string `json:"description"`
	BehavesAs   string `json:"behavesAs"`
}

type modelPickerTestSettings struct {
	AvailableModels []string `json:"availableModels"`
	Picker          struct {
		Options               []modelPickerTestOption `json:"options"`
		ReplaceBuiltInOptions *bool                   `json:"replaceBuiltInOptions"`
	} `json:"modelPicker"`
}

func decodeModelPickerSettings(t *testing.T, settings string) modelPickerTestSettings {
	t.Helper()
	var payload modelPickerTestSettings
	if err := json.Unmarshal([]byte(settings), &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

func pickerModels(options []modelPickerTestOption) []string {
	models := make([]string, 0, len(options))
	for _, option := range options {
		models = append(models, option.Model)
	}
	return models
}

func modelPickerTestStore(t *testing.T) *store.Store {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "user-claude"))
	s, err := store.Open(t.Context(), filepath.Join(home, "ccr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := s.AddProvider(t.Context(), store.Provider{
		Name: "picker", Type: "litellm", BaseURL: "http://127.0.0.1:1", Mode: "full",
		SupportsTools: true, SupportsStreaming: true,
	}); err != nil {
		t.Fatal(err)
	}
	for _, model := range []store.Model{
		{Alias: "coder", ProviderName: "picker", ProviderModel: "coder", Status: "degraded"},
		{Alias: "wide", ProviderName: "picker", ProviderModel: "wide", Status: "degraded", CapabilityOverrides: modelcap.Values{ContextWindowTokens: modelcap.Int64(1000000)}},
		{Alias: "chat", ProviderName: "picker", ProviderModel: "chat", Status: "chat-only", CapabilityOverrides: modelcap.Values{SupportsTools: modelcap.Bool(false)}},
		{Alias: "blocked", ProviderName: "picker", ProviderModel: "blocked", Status: "blocked"},
		{Alias: "control", ProviderName: "picker", ProviderModel: "all-proxy-models", Status: "degraded"},
		{Alias: "no-stream", ProviderName: "picker", ProviderModel: "no-stream", Status: "degraded", CapabilityOverrides: modelcap.Values{SupportsStreaming: modelcap.Bool(false)}},
	} {
		if err := s.AddModel(t.Context(), model); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func writeModelPickerTestSettings(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
