package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestPrepareClaudeLaunchProfileCopiesCustomizationsWithoutRuntimeState(t *testing.T) {
	source := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", source)
	if err := os.MkdirAll(filepath.Join(source, "commands"), 0o700); err != nil {
		t.Fatalf("MkdirAll(commands) error = %v", err)
	}
	for relativePath, contents := range map[string]string{
		"settings.json":        `{"model":"native-model","permissions":{"allow":["Read"]}}`,
		"commands/research.md": "research skill",
		".claude.json":         `{"projects":{"/repo":{"lastModel":"anthropic.ccr.old"}}}`,
		"history.jsonl":        `{"model":"anthropic.ccr.old"}`,
		"backups/old.json":     `{"model":"anthropic.ccr.old"}`,
		".env":                 "unrelated-secret",
		".ssh/id_rsa":          "unrelated-private-key",
		"project-data/notes":   "unrelated project data",
	} {
		path := filepath.Join(source, relativePath)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v", path, err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", path, err)
		}
	}

	profile, err := prepareClaudeLaunchProfile()
	if err != nil {
		t.Fatalf("prepareClaudeLaunchProfile() error = %v", err)
	}
	profileDir := profile.configDir
	t.Cleanup(func() { _ = profile.cleanup() })
	if profileDir == source {
		t.Fatal("private profile reused the user profile")
	}
	for _, path := range []string{"settings.json", "commands/research.md"} {
		if _, err := os.Stat(filepath.Join(profileDir, path)); err != nil {
			t.Fatalf("profile missing customization %s: %v", path, err)
		}
	}
	for _, path := range []string{".claude.json", "history.jsonl", "backups/old.json", ".env", ".ssh/id_rsa", "project-data/notes"} {
		if _, err := os.Stat(filepath.Join(profileDir, path)); !os.IsNotExist(err) {
			t.Fatalf("profile copied runtime state %s: %v", path, err)
		}
	}
	if err := profile.cleanup(); err != nil {
		t.Fatalf("profile cleanup error = %v", err)
	}
	if _, err := os.Stat(profileDir); !os.IsNotExist(err) {
		t.Fatalf("profile still exists after cleanup: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(source, "settings.json")); err != nil || !strings.Contains(string(got), "native-model") {
		t.Fatalf("source settings changed: %q, %v", got, err)
	}
}

func TestPrepareClaudeLaunchProfileSharesImmutableCustomizationTrees(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows directory links require platform-specific privileges")
	}
	source := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", source)
	for _, relative := range []string{"skills/research/SKILL.md", "commands/research.md", "rules/project.md"} {
		path := filepath.Join(source, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(relative), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	nestedTarget := t.TempDir()
	if err := os.WriteFile(filepath.Join(nestedTarget, "outside.md"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(nestedTarget, filepath.Join(source, "skills", "external")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	profile, err := prepareClaudeLaunchProfile()
	if err != nil {
		t.Fatalf("prepareClaudeLaunchProfile() error = %v", err)
	}
	t.Cleanup(func() { _ = profile.cleanup() })
	for _, relative := range []string{"commands", "rules"} {
		info, err := os.Lstat(filepath.Join(profile.configDir, relative))
		if err != nil {
			t.Fatalf("shared customization tree %s missing: %v", relative, err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("customization tree %s was copied instead of shared", relative)
		}
	}
	if info, err := os.Lstat(filepath.Join(profile.configDir, "skills")); err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("customization tree with nested link was shared: %v, %v", info, err)
	}
	if containsString(profile.degradations, "skills/external") == false {
		t.Fatalf("profile degradations = %v, want nested symlink omission", profile.degradations)
	}
	if _, err := os.Lstat(filepath.Join(profile.configDir, "skills", "external")); !os.IsNotExist(err) {
		t.Fatalf("nested symlink unexpectedly appeared in private profile: %v", err)
	}
}

func TestPrepareClaudeLaunchProfileReportsSkippedSymlinkedCustomizations(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows symlink creation requires elevated privileges")
	}
	source := t.TempDir()
	target := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", source)
	if err := os.MkdirAll(filepath.Join(target, "research"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{"CLAUDE.md", "settings.json", "settings.local.json"} {
		if err := os.WriteFile(filepath.Join(target, relative), []byte(relative), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(target, relative), filepath.Join(source, relative)); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
	}
	if err := os.Symlink(target, filepath.Join(source, "skills")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	pluginTarget := filepath.Join(target, "plugin-definitions")
	if err := os.MkdirAll(pluginTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(pluginTarget, filepath.Join(source, "plugins")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	profile, err := prepareClaudeLaunchProfile()
	if err != nil {
		t.Fatalf("prepareClaudeLaunchProfile() error = %v", err)
	}
	t.Cleanup(func() { _ = profile.cleanup() })
	if !containsString(profile.degradations, "skills") {
		t.Fatalf("profile degradations = %v, want skipped skills customization", profile.degradations)
	}
	for _, relative := range []string{"CLAUDE.md", "settings.json", "settings.local.json"} {
		if !containsString(profile.degradations, relative) {
			t.Fatalf("profile degradations = %v, want skipped %s customization", profile.degradations, relative)
		}
		if _, err := os.Lstat(filepath.Join(profile.configDir, relative)); !os.IsNotExist(err) {
			t.Fatalf("skipped symlinked %s unexpectedly appeared in private profile: %v", relative, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(profile.configDir, "skills")); !os.IsNotExist(err) {
		t.Fatalf("skipped symlinked skills unexpectedly appeared in private profile: %v", err)
	}
	if !containsString(profile.degradations, "plugins") {
		t.Fatalf("profile degradations = %v, want skipped plugins customization", profile.degradations)
	}
	if _, err := os.Lstat(filepath.Join(profile.configDir, "plugins")); !os.IsNotExist(err) {
		t.Fatalf("skipped symlinked plugins unexpectedly appeared in private profile: %v", err)
	}
}

func TestCopyClaudeProfileStateDirectoryPreservesManagedAssetLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows directory links require platform-specific privileges")
	}
	native := t.TempDir()
	profile := t.TempDir()
	destination := filepath.Join(t.TempDir(), "migrated")
	asset := filepath.Join(native, "skills")
	if err := os.MkdirAll(asset, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(asset, "research.md"), []byte("research"), 0o600); err != nil {
		t.Fatal(err)
	}
	marker, markerErr := claudeProfileMarkerContents(native)
	if markerErr != nil {
		t.Fatal(markerErr)
	}
	if writeErr := os.WriteFile(filepath.Join(profile, ".ccr-profile-ready"), marker, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	if symlinkErr := os.Symlink(asset, filepath.Join(profile, "skills")); symlinkErr != nil {
		t.Skipf("symlink unavailable: %v", symlinkErr)
	}
	if copyErr := copyClaudeProfileStateDirectory(profile, destination); copyErr != nil {
		t.Fatalf("copyClaudeProfileStateDirectory() error = %v", copyErr)
	}
	info, err := os.Lstat(filepath.Join(destination, "skills"))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("migrated managed asset link = %v, %v; want symlink", info, err)
	}
	got, err := os.ReadFile(filepath.Join(destination, "skills", "research.md"))
	if err != nil || string(got) != "research" {
		t.Fatalf("migrated managed asset contents = %q, %v", got, err)
	}
}

func TestPrepareClaudeLaunchProfileUsesDefaultSiblingBootstrapState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	bootstrap := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(bootstrap, []byte(`{"hasCompletedOnboarding":true,"projects":{"/repo":{"hasTrustDialogAccepted":true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	profile, err := prepareClaudeLaunchProfile()
	if err != nil {
		t.Fatalf("prepareClaudeLaunchProfile() error = %v", err)
	}
	t.Cleanup(func() { _ = profile.cleanup() })
	privateState, err := os.ReadFile(filepath.Join(profile.configDir, ".claude.json"))
	if err != nil {
		t.Fatalf("ReadFile(private bootstrap) error = %v", err)
	}
	if !strings.Contains(string(privateState), "hasCompletedOnboarding") || !strings.Contains(string(privateState), "hasTrustDialogAccepted") {
		t.Fatalf("private profile lost default bootstrap state: %s", privateState)
	}
}

func TestPrepareClaudeLaunchProfileUsesClaudeAlternateBootstrapState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".config.json"), []byte(`{"hasCompletedOnboarding":true,"projects":{"/repo":{"hasTrustDialogAccepted":true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"hasCompletedOnboarding":false}`), 0o600); err != nil {
		t.Fatal(err)
	}

	profile, err := prepareClaudeLaunchProfile()
	if err != nil {
		t.Fatalf("prepareClaudeLaunchProfile() error = %v", err)
	}
	t.Cleanup(func() { _ = profile.cleanup() })
	privateState, err := os.ReadFile(filepath.Join(profile.configDir, ".claude.json"))
	if err != nil {
		t.Fatalf("ReadFile(private bootstrap) error = %v", err)
	}
	if !strings.Contains(string(privateState), "hasCompletedOnboarding") || !strings.Contains(string(privateState), "hasTrustDialogAccepted") {
		t.Fatalf("private profile did not mirror Claude alternate bootstrap state: %s", privateState)
	}
}

func TestPrepareClaudeLaunchProfileSanitizesSettingsEnvironment(t *testing.T) {
	source := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", source)
	settings := `{"env":{"ANTHROPIC_BASE_URL":"https://native.example","ANTHROPIC_API_KEY":"native-secret","CLAUDE_CODE_OAUTH_TOKEN":"oauth-secret","CLAUDE_CONFIG_DIR":"/native","CLAUDE_CODE_SUBAGENT_MODEL":"sonnet","CLAUDE_CODE_USE_GATEWAY":"1","CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY":"1","CCR_LAUNCH_ID":"stale","CLAUDE_CODE_SIMPLE":"1","ENABLE_TOOL_SEARCH":"true","SAFE_SETTING":"kept"},"permissions":{"allow":["Read"]}}`
	if err := os.WriteFile(filepath.Join(source, "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	profile, err := prepareClaudeLaunchProfile()
	if err != nil {
		t.Fatalf("prepareClaudeLaunchProfile() error = %v", err)
	}
	t.Cleanup(func() { _ = profile.cleanup() })
	data, err := os.ReadFile(filepath.Join(profile.configDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	env, ok := got["env"].(map[string]any)
	if !ok || env["SAFE_SETTING"] != "kept" {
		t.Fatalf("safe settings environment was not preserved: %#v", got["env"])
	}
	for _, key := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CONFIG_DIR", "CLAUDE_CODE_SUBAGENT_MODEL", "CLAUDE_CODE_USE_GATEWAY", "CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY", "CCR_LAUNCH_ID", "CLAUDE_CODE_SIMPLE", "ENABLE_TOOL_SEARCH"} {
		if _, present := env[key]; present {
			t.Fatalf("private settings retained isolated launch variable %q", key)
		}
	}
}

func TestPrepareClaudeLaunchProfilePreservesNativeAuthButRemovesChildModelOverride(t *testing.T) {
	source := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", source)
	settings := `{"env":{"ANTHROPIC_API_KEY":"native-secret","ANTHROPIC_BASE_URL":"https://native.example","CLAUDE_CODE_SUBAGENT_MODEL":"sonnet","CLAUDE_CODE_SUBAGENT_MODEL_FORCE":"1","CLAUDE_CONFIG_DIR":"/native","CLAUDE_CODE_USE_GATEWAY":"1","SAFE_SETTING":"kept"}}`
	if err := os.WriteFile(filepath.Join(source, "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	profile, err := prepareClaudeLaunchProfileAtForSessionWithOptions("", "", claudeProfileCopyOptions{
		preserveAnthropicAPIKey: true,
	})
	if err != nil {
		t.Fatalf("prepareClaudeLaunchProfileAtForSessionWithOptions() error = %v", err)
	}
	t.Cleanup(func() { _ = profile.cleanup() })
	data, err := os.ReadFile(filepath.Join(profile.configDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	env, ok := got["env"].(map[string]any)
	if !ok || env["ANTHROPIC_API_KEY"] != "native-secret" || env["SAFE_SETTING"] != "kept" {
		t.Fatalf("allowed native settings were not preserved: %#v", got["env"])
	}
	for _, key := range []string{"ANTHROPIC_BASE_URL", "CLAUDE_CONFIG_DIR", "CLAUDE_CODE_USE_GATEWAY", "CLAUDE_CODE_SUBAGENT_MODEL", "CLAUDE_CODE_SUBAGENT_MODEL_FORCE"} {
		if _, present := env[key]; present {
			t.Fatalf("private settings retained isolated launch variable %q", key)
		}
	}
}

func TestPersistentClaudeLaunchProfileResanitizesChangedLaunchMode(t *testing.T) {
	source := t.TempDir()
	destination := filepath.Join(t.TempDir(), "session-profile")
	t.Setenv("CLAUDE_CONFIG_DIR", source)
	settings := `{"env":{"ANTHROPIC_API_KEY":"native-secret","CLAUDE_CODE_SUBAGENT_MODEL":"sonnet","CLAUDE_CODE_SUBAGENT_MODEL_FORCE":"1","SAFE_SETTING":"kept"}}`
	if err := os.WriteFile(filepath.Join(source, "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}

	preserve, err := prepareClaudeLaunchProfileAtForSessionWithOptions(destination, "", claudeProfileCopyOptions{
		preserveAnthropicAPIKey: true,
	})
	if err != nil {
		t.Fatalf("preserve launch profile error = %v", err)
	}
	if cleanupErr := preserve.cleanup(); cleanupErr != nil {
		t.Fatal(cleanupErr)
	}

	providerOnly, err := prepareClaudeLaunchProfileAtForSessionWithOptions(destination, "", claudeProfileCopyOptions{})
	if err != nil {
		t.Fatalf("provider-only profile reuse error = %v", err)
	}
	if cleanupErr := providerOnly.cleanup(); cleanupErr != nil {
		t.Fatal(cleanupErr)
	}
	assertClaudeProfileEnvironment(t, destination, map[string]any{
		"SAFE_SETTING": "kept",
	}, "ANTHROPIC_API_KEY", "CLAUDE_CODE_SUBAGENT_MODEL", "CLAUDE_CODE_SUBAGENT_MODEL_FORCE")

	restored, err := prepareClaudeLaunchProfileAtForSessionWithOptions(destination, "", claudeProfileCopyOptions{
		preserveAnthropicAPIKey: true,
	})
	if err != nil {
		t.Fatalf("restored preserve profile error = %v", err)
	}
	if err := restored.cleanup(); err != nil {
		t.Fatal(err)
	}
	assertClaudeProfileEnvironment(t, destination, map[string]any{
		"ANTHROPIC_API_KEY": "native-secret",
		"SAFE_SETTING":      "kept",
	}, "CLAUDE_CODE_SUBAGENT_MODEL", "CLAUDE_CODE_SUBAGENT_MODEL_FORCE")
}

func TestPrepareClaudeLaunchProfileRemovesConfiguredProviderSecretFromSettings(t *testing.T) {
	source := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", source)
	settings := `{"env":{"CCR_TEST_PROVIDER_TOKEN":"provider-secret","SAFE_SETTING":"kept"}}`
	if err := os.WriteFile(filepath.Join(source, "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	profile, err := prepareClaudeLaunchProfileAtForSessionWithOptions("", "", claudeProfileCopyOptions{
		providerSecretEnvNames: []string{"CCR_TEST_PROVIDER_TOKEN"},
	})
	if err != nil {
		t.Fatalf("prepareClaudeLaunchProfileAtForSessionWithOptions() error = %v", err)
	}
	t.Cleanup(func() { _ = profile.cleanup() })
	assertClaudeProfileEnvironment(t, profile.configDir, map[string]any{
		"SAFE_SETTING": "kept",
	}, "CCR_TEST_PROVIDER_TOKEN")
}

func assertClaudeProfileEnvironment(t *testing.T, directory string, want map[string]any, absent ...string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(directory, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	env, ok := document["env"].(map[string]any)
	if !ok {
		env = map[string]any{}
	}
	for key, value := range want {
		if env[key] != value {
			t.Fatalf("settings env[%q] = %#v, want %#v", key, env[key], value)
		}
	}
	for _, key := range absent {
		if _, present := env[key]; present {
			t.Fatalf("settings retained isolated environment %q: %#v", key, env)
		}
	}
}

func TestPrepareClaudeLaunchProfileCopiesNestedSettingsAsCustomization(t *testing.T) {
	source := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", source)
	nested := filepath.Join(source, "plugins", "example", "settings.json")
	if err := os.MkdirAll(filepath.Dir(nested), 0o700); err != nil {
		t.Fatal(err)
	}
	const contents = "plugin-specific non-JSON settings"
	if err := os.WriteFile(nested, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	profile, err := prepareClaudeLaunchProfile()
	if err != nil {
		t.Fatalf("prepareClaudeLaunchProfile() error = %v", err)
	}
	t.Cleanup(func() { _ = profile.cleanup() })
	got, err := os.ReadFile(filepath.Join(profile.configDir, "plugins", "example", "settings.json"))
	if err != nil {
		t.Fatalf("nested plugin settings were not copied: %v", err)
	}
	if string(got) != contents {
		t.Fatalf("nested plugin settings = %q, want %q", got, contents)
	}
}

func TestPrepareClaudeLaunchProfileSharesInstalledPluginPayloads(t *testing.T) {
	source := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", source)

	cachePath := filepath.Join(source, "plugins", "cache", "example-marketplace", "example-plugin", "1.0.0")
	marketplacePath := filepath.Join(source, "plugins", "marketplaces", "example-marketplace")
	entries := map[string]string{
		"plugins/cache/example-marketplace/example-plugin/1.0.0/skills/example/SKILL.md": "cached skill",
		"plugins/marketplaces/example-marketplace/.claude-plugin/marketplace.json":       "marketplace manifest",
		"plugins/synced/example-install/manifest.json":                                   "synced manifest",
		"plugins/data/example-plugin/runtime.json":                                       "runtime state",
	}
	for relative, contents := range entries {
		path := filepath.Join(source, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	installed := `{"version":2,"plugins":{"example-plugin@example-marketplace":[{"scope":"user","installPath":"` + filepath.ToSlash(cachePath) + `","version":"1.0.0"}]}}`
	if err := os.WriteFile(filepath.Join(source, "plugins", "installed_plugins.json"), []byte(installed), 0o600); err != nil {
		t.Fatal(err)
	}
	knownMarketplaces := `{"example-marketplace":{"source":{"source":"directory"},"installLocation":"` + filepath.ToSlash(marketplacePath) + `"}}`
	if err := os.WriteFile(filepath.Join(source, "plugins", "known_marketplaces.json"), []byte(knownMarketplaces), 0o600); err != nil {
		t.Fatal(err)
	}

	profile, err := prepareClaudeLaunchProfile()
	if err != nil {
		t.Fatalf("prepareClaudeLaunchProfile() error = %v", err)
	}
	t.Cleanup(func() { _ = profile.cleanup() })

	for _, relative := range []string{"plugins/cache", "plugins/marketplaces", "plugins/synced", "plugins/data"} {
		if _, err := os.Lstat(filepath.Join(profile.configDir, relative)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("isolated profile retained shared or runtime plugin path %s: %v", relative, err)
		}
	}
	for _, relative := range []string{"plugins/installed_plugins.json", "plugins/known_marketplaces.json"} {
		got, err := os.ReadFile(filepath.Join(profile.configDir, relative))
		if err != nil {
			t.Fatalf("plugin registry metadata %s was not copied: %v", relative, err)
		}
		var want string
		switch relative {
		case "plugins/installed_plugins.json":
			want = installed
		case "plugins/known_marketplaces.json":
			want = knownMarketplaces
		}
		if string(got) != want {
			t.Fatalf("plugin registry metadata %s = %q, want %q", relative, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(source, "plugins", "cache")); err != nil {
		t.Fatalf("native installed plugin payload disappeared: %v", err)
	}
}

func TestClaudeBootstrapStateCopiesOnlyOnboardingState(t *testing.T) {
	source := t.TempDir()
	destination := filepath.Join(t.TempDir(), "profile", ".claude.json")
	if err := os.WriteFile(filepath.Join(source, ".claude.json"), []byte(`{"hasCompletedOnboarding":true,"mcpServers":{"local":{"command":"node","args":["server.js"],"env":{"MCP_MODE":"readonly"}}},"projects":{"/repo":{"hasTrustDialogAccepted":true,"projectOnboardingSeenCount":2,"lastModel":"anthropic.ccr.old","sessionId":"secret-state"}},"oauthAccount":"do-not-copy"}`), 0o600); err != nil {
		t.Fatalf("WriteFile(source bootstrap) error = %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		t.Fatalf("MkdirAll(destination) error = %v", err)
	}
	if err := copyClaudeBootstrapState(filepath.Join(source, ".claude.json"), destination); err != nil {
		t.Fatalf("copyClaudeBootstrapState() error = %v", err)
	}
	data, err := os.ReadFile(destination)
	if err != nil {
		t.Fatalf("ReadFile(destination bootstrap) error = %v", err)
	}
	got := string(data)
	for _, forbidden := range []string{"lastModel", "sessionId", "oauthAccount", "anthropic.ccr.old"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("sanitized bootstrap state contains %q: %s", forbidden, got)
		}
	}
	if !strings.Contains(got, "hasTrustDialogAccepted") || !strings.Contains(got, "projectOnboardingSeenCount") {
		t.Fatalf("sanitized bootstrap state lost onboarding fields: %s", got)
	}
	if !strings.Contains(got, `"mcpServers"`) || !strings.Contains(got, `"server.js"`) || !strings.Contains(got, `"MCP_MODE"`) {
		t.Fatalf("sanitized bootstrap state lost MCP configuration: %s", got)
	}
}

func TestSeedClaudeGatewayModelCacheWritesOnlyCCRModels(t *testing.T) {
	directory := t.TempDir()
	settings := `{"availableModels":["claude-sonnet-5","anthropic.ccr.worker","anthropic.ccr.reviewer"]}`
	if err := seedClaudeGatewayModelCache(directory, "http://127.0.0.1:43123/", settings); err != nil {
		t.Fatalf("seedClaudeGatewayModelCache() error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(directory, "cache", "gateway-models.json"))
	if err != nil {
		t.Fatalf("ReadFile(gateway-models.json) error = %v", err)
	}
	var cache claudeGatewayModelCache
	if err := json.Unmarshal(data, &cache); err != nil {
		t.Fatalf("Unmarshal(gateway-models.json) error = %v", err)
	}
	if cache.BaseURL != "http://127.0.0.1:43123" || len(cache.Models) != 2 || cache.Models[0].ID != "anthropic.ccr.worker" {
		t.Fatalf("private Claude model cache = %#v", cache)
	}
	if err := seedClaudeGatewayModelCache(directory, "http://127.0.0.1:43123/", `{"availableModels":["claude-sonnet-5"]}`); err != nil {
		t.Fatalf("seedClaudeGatewayModelCache() stale-cache cleanup error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, "cache", "gateway-models.json")); !os.IsNotExist(err) {
		t.Fatalf("stale private Claude model cache remains: %v", err)
	}
}

func TestRemoveClaudeGatewayModelCacheRejectsSymlinkedParent(t *testing.T) {
	root := t.TempDir()
	profile := filepath.Join(root, "profile")
	external := filepath.Join(root, "external")
	for _, directory := range []string{profile, external} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatalf("Mkdir(%s) error = %v", directory, err)
		}
	}
	cachePath := filepath.Join(external, "gateway-models.json")
	const contents = "external cache must remain untouched"
	if err := os.WriteFile(cachePath, []byte(contents), 0o600); err != nil {
		t.Fatalf("WriteFile(external cache) error = %v", err)
	}
	if err := os.Symlink(external, filepath.Join(profile, "cache")); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}

	if err := removeClaudeGatewayModelCache(filepath.Join(profile, "cache", "gateway-models.json")); err == nil {
		t.Fatal("removeClaudeGatewayModelCache() succeeded through a symlinked parent")
	}
	got, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("ReadFile(external cache) error = %v", err)
	}
	if string(got) != contents {
		t.Fatalf("external cache contents = %q, want %q", got, contents)
	}
}

func TestPersistentClaudeLaunchProfileSurvivesContinuationPreparation(t *testing.T) {
	source := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", source)
	if err := os.WriteFile(filepath.Join(source, "settings.json"), []byte(`{"model":"native-model"}`), 0o600); err != nil {
		t.Fatalf("WriteFile(settings.json) error = %v", err)
	}
	destination := filepath.Join(t.TempDir(), "session-profile")
	first, err := prepareClaudeLaunchProfileAt(destination)
	if err != nil {
		t.Fatalf("first prepareClaudeLaunchProfileAt() error = %v", err)
	}
	if writeErr := os.WriteFile(filepath.Join(destination, ".claude.json"), []byte(`{"session":"kept"}`), 0o600); writeErr != nil {
		t.Fatalf("WriteFile(.claude.json) error = %v", writeErr)
	}
	if cleanupErr := first.cleanup(); cleanupErr != nil {
		t.Fatalf("persistent profile cleanup error = %v", cleanupErr)
	}
	second, err := prepareClaudeLaunchProfileAt(destination)
	if err != nil {
		t.Fatalf("second prepareClaudeLaunchProfileAt() error = %v", err)
	}
	if secondCleanupErr := second.cleanup(); secondCleanupErr != nil {
		t.Fatalf("second persistent profile cleanup error = %v", secondCleanupErr)
	}
	data, err := os.ReadFile(filepath.Join(destination, ".claude.json"))
	if err != nil || string(data) != `{"session":"kept"}` {
		t.Fatalf("persistent Claude session state = %q, %v", data, err)
	}
}

func TestPersistentClaudeLaunchProfileRefreshesAndRemovesNativeSettings(t *testing.T) {
	source := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", source)
	settingsPath := filepath.Join(source, "settings.json")
	if err := os.WriteFile(settingsPath, []byte(`{"model":"native-model","permissions":{"allow":["Read"]},"env":{"SAFE_SETTING":"kept"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "session-profile")
	first, err := prepareClaudeLaunchProfileAt(destination)
	if err != nil {
		t.Fatalf("initial prepareClaudeLaunchProfileAt() error = %v", err)
	}
	if cleanupErr := first.cleanup(); cleanupErr != nil {
		t.Fatal(cleanupErr)
	}

	if writeErr := os.WriteFile(settingsPath, []byte(`{"model":"changed-model","permissions":{"deny":["Write"]}}`), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	second, err := prepareClaudeLaunchProfileAt(destination)
	if err != nil {
		t.Fatalf("refresh prepareClaudeLaunchProfileAt() error = %v", err)
	}
	privateSettings, err := os.ReadFile(filepath.Join(destination, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got, want map[string]any
	if unmarshalErr := json.Unmarshal(privateSettings, &got); unmarshalErr != nil {
		t.Fatal(unmarshalErr)
	}
	if unmarshalErr := json.Unmarshal([]byte(`{"model":"changed-model","permissions":{"deny":["Write"]}}`), &want); unmarshalErr != nil {
		t.Fatal(unmarshalErr)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("private settings retained stale native values: got %#v, want %#v", got, want)
	}
	if cleanupErr := second.cleanup(); cleanupErr != nil {
		t.Fatal(cleanupErr)
	}

	if removeErr := os.Remove(settingsPath); removeErr != nil {
		t.Fatal(removeErr)
	}
	third, err := prepareClaudeLaunchProfileAt(destination)
	if err != nil {
		t.Fatalf("refresh after native settings removal error = %v", err)
	}
	if err := third.cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(destination, "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("stale private settings survived native removal: %v", err)
	}
}

func TestPersistentClaudeLaunchProfileReusesSymlinkedNativeSettingsAsVisibleDegradation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows symlink creation requires elevated privileges")
	}
	source := t.TempDir()
	target := filepath.Join(t.TempDir(), "native-settings.json")
	if err := os.WriteFile(target, []byte(`{"model":"native-model"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(source, "settings.json")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", source)
	destination := filepath.Join(t.TempDir(), "session-profile")
	first, err := prepareClaudeLaunchProfileAt(destination)
	if err != nil {
		t.Fatalf("initial symlinked settings preparation error = %v", err)
	}
	if !containsString(first.degradations, "settings.json") {
		t.Fatalf("initial profile degradations = %v, want settings.json", first.degradations)
	}
	if cleanupErr := first.cleanup(); cleanupErr != nil {
		t.Fatal(cleanupErr)
	}

	second, err := prepareClaudeLaunchProfileAt(destination)
	if err != nil {
		t.Fatalf("reusing symlinked settings profile error = %v", err)
	}
	if !containsString(second.degradations, "settings.json") {
		t.Fatalf("reused profile degradations = %v, want settings.json", second.degradations)
	}
	if err := second.cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(destination, "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("symlinked native settings unexpectedly appeared in private profile: %v", err)
	}
}

func TestPersistentClaudeLaunchProfilePreservesExistingStateAfterRefreshFailure(t *testing.T) {
	source := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", source)
	if err := os.WriteFile(filepath.Join(source, "settings.json"), []byte(`{"model":"native-model"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "session-profile")
	profile, err := prepareClaudeLaunchProfileAt(destination)
	if err != nil {
		t.Fatalf("initial prepareClaudeLaunchProfileAt() error = %v", err)
	}
	if cleanupErr := profile.cleanup(); cleanupErr != nil {
		t.Fatal(cleanupErr)
	}
	const history = `{"session":"must-survive"}`
	if writeErr := os.WriteFile(filepath.Join(destination, ".claude.json"), []byte(history), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	if writeErr := os.WriteFile(filepath.Join(destination, "settings.json"), []byte("not-json"), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	if _, refreshErr := prepareClaudeLaunchProfileAt(destination); refreshErr == nil {
		t.Fatal("refresh unexpectedly succeeded with malformed persistent settings")
	}
	got, err := os.ReadFile(filepath.Join(destination, ".claude.json"))
	if err != nil || string(got) != history {
		t.Fatalf("persistent profile state after refresh failure = %q, %v; want preserved history", got, err)
	}
	if _, err := os.Stat(filepath.Join(destination, ".ccr-profile-ready")); err != nil {
		t.Fatalf("persistent profile readiness marker was deleted after refresh failure: %v", err)
	}
}

func TestPersistentClaudeLaunchProfileRebuildsOwnedInterruptedProfile(t *testing.T) {
	source := t.TempDir()
	destination := filepath.Join(t.TempDir(), "session-profile")
	t.Setenv("CLAUDE_CONFIG_DIR", source)
	if err := os.WriteFile(filepath.Join(source, "settings.json"), []byte(`{"model":"native-model"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(destination, "hooks"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, "hooks", "removed-hook"), []byte("stale executable configuration"), 0o700); err != nil {
		t.Fatal(err)
	}
	initializing, err := claudeProfileInitializationMarkerContents(source)
	if err != nil {
		t.Fatal(err)
	}
	if writeErr := os.WriteFile(filepath.Join(destination, claudeProfileInitializationMarkerName), initializing, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}

	profile, err := prepareClaudeLaunchProfileAt(destination)
	if err != nil {
		t.Fatalf("rebuilding interrupted persistent profile: %v", err)
	}
	if err := profile.cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(destination, "hooks", "removed-hook")); !os.IsNotExist(err) {
		t.Fatalf("stale hook survived interrupted profile rebuild: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, ".ccr-profile-ready")); err != nil {
		t.Fatalf("rebuilt profile is not ready: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, claudeProfileInitializationMarkerName)); !os.IsNotExist(err) {
		t.Fatalf("initialization marker survived ready publication: %v", err)
	}
}

func TestPersistentClaudeLaunchProfileRejectsUnmarkedNonEmptyProfile(t *testing.T) {
	source := t.TempDir()
	destination := filepath.Join(t.TempDir(), "session-profile")
	t.Setenv("CLAUDE_CONFIG_DIR", source)
	if err := os.MkdirAll(filepath.Join(destination, "hooks"), 0o700); err != nil {
		t.Fatal(err)
	}
	const foreignContents = "foreign profile state"
	if err := os.WriteFile(filepath.Join(destination, "hooks", "foreign-hook"), []byte(foreignContents), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareClaudeLaunchProfileAt(destination); err == nil || !strings.Contains(err.Error(), "has no CCR readiness marker") {
		t.Fatalf("unmarked non-empty profile was reused: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(destination, "hooks", "foreign-hook"))
	if err != nil || string(got) != foreignContents {
		t.Fatalf("unmarked profile changed after refusal: %q, %v", got, err)
	}
}

func TestPersistentClaudeLaunchProfileRejectsSymlinkDestination(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows symlink creation requires elevated privileges")
	}
	source := t.TempDir()
	target := t.TempDir()
	destination := filepath.Join(t.TempDir(), "session-profile")
	if err := os.Symlink(target, destination); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", source)
	if _, err := prepareClaudeLaunchProfileAt(destination); err == nil || !strings.Contains(err.Error(), "must not be a symlink") {
		t.Fatalf("prepareClaudeLaunchProfileAt() error = %v, want symlink rejection", err)
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("symlink target received private profile state: %v", entries)
	}
}

func TestPersistentClaudeProfileRejectsNestedSymlinkWrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows symlink creation requires elevated privileges")
	}
	source := t.TempDir()
	destination := filepath.Join(t.TempDir(), "session-profile")
	t.Setenv("CLAUDE_CONFIG_DIR", source)
	if err := os.WriteFile(filepath.Join(source, "settings.json"), []byte(`{"env":{"SAFE_SETTING":"kept"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	first, firstErr := prepareClaudeLaunchProfileAt(destination)
	if firstErr != nil {
		t.Fatalf("initial persistent profile error = %v", firstErr)
	}
	if cleanupErr := first.cleanup(); cleanupErr != nil {
		t.Fatal(cleanupErr)
	}

	external := filepath.Join(t.TempDir(), "outside-settings.json")
	const outsideContents = "must remain unchanged"
	if writeErr := os.WriteFile(external, []byte(outsideContents), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	if removeErr := os.Remove(filepath.Join(destination, "settings.json")); removeErr != nil {
		t.Fatal(removeErr)
	}
	if err := os.Symlink(external, filepath.Join(destination, "settings.json")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := prepareClaudeLaunchProfileAt(destination); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("nested settings symlink was not rejected: %v", err)
	}
	got, err := os.ReadFile(external)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != outsideContents {
		t.Fatalf("nested symlink target was modified: %q", got)
	}
}
