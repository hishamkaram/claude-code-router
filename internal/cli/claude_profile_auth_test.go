package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPreservedClaudeOAuthEnvironmentUsesExistingCredentialsWithoutCopyingThem(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		t.Skipf("file-based Claude OAuth credentials are unsupported on %s", runtime.GOOS)
	}
	source := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", source)
	credentials := `{"claudeAiOauth":{"accessToken":"oauth-access","refreshToken":"oauth-refresh","expiresAt":1790048531992,"scopes":["user:inference"]}}`
	if err := os.WriteFile(filepath.Join(source, ".credentials.json"), []byte(credentials), 0o600); err != nil {
		t.Fatalf("WriteFile(credentials) error = %v", err)
	}

	oauth, err := preservedClaudeOAuthEnvironment(launchAuthModePreserve, source)
	if err != nil {
		t.Fatalf("preservedClaudeOAuthEnvironment() error = %v", err)
	}
	if oauth.AccessToken != "oauth-access" || oauth.RefreshToken != "oauth-refresh" || oauth.ScopesJSON != `["user:inference"]` {
		t.Fatalf("preserved OAuth fields mismatch: access_present=%t refresh_present=%t scopes_present=%t", oauth.AccessToken != "", oauth.RefreshToken != "", oauth.ScopesJSON != "")
	}
	profile, err := prepareClaudeLaunchProfile()
	if err != nil {
		t.Fatalf("prepareClaudeLaunchProfile() error = %v", err)
	}
	t.Cleanup(func() { _ = profile.cleanup() })
	if _, err := os.Stat(filepath.Join(profile.configDir, ".credentials.json")); !os.IsNotExist(err) {
		t.Fatalf("private profile copied OAuth credentials: %v", err)
	}
}

func TestPreservedClaudeOAuthEnvironmentUsesSelectedProfileOverProcessEnvironment(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		t.Skipf("file-based Claude OAuth credentials are unsupported on %s", runtime.GOOS)
	}
	processProfile := t.TempDir()
	selectedProfile := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", processProfile)
	writeCredentials := func(directory, accessToken string) {
		t.Helper()
		credentials := `{"claudeAiOauth":{"accessToken":"` + accessToken + `","refreshToken":"refresh-` + accessToken + `","scopes":["user:inference"]}}`
		if err := os.WriteFile(filepath.Join(directory, ".credentials.json"), []byte(credentials), 0o600); err != nil {
			t.Fatalf("WriteFile(%s credentials) error = %v", directory, err)
		}
	}
	writeCredentials(processProfile, "process-profile-token")
	writeCredentials(selectedProfile, "selected-profile-token")

	oauth, err := preservedClaudeOAuthEnvironment(launchAuthModePreserve, selectedProfile)
	if err != nil {
		t.Fatalf("preservedClaudeOAuthEnvironment() error = %v", err)
	}
	if oauth.AccessToken != "selected-profile-token" || oauth.RefreshToken != "refresh-selected-profile-token" {
		t.Fatalf("preserved OAuth did not use selected profile: access_present=%t refresh_present=%t", oauth.AccessToken != "", oauth.RefreshToken != "")
	}
}

func TestPreservedClaudeOAuthEnvironmentRejectsRefreshTokenWithoutScopes(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		t.Skipf("file-based Claude OAuth credentials are unsupported on %s", runtime.GOOS)
	}
	source := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", source)
	credentials := `{"claudeAiOauth":{"accessToken":"oauth-access","refreshToken":"oauth-refresh","scopes":[]}}`
	if err := os.WriteFile(filepath.Join(source, ".credentials.json"), []byte(credentials), 0o600); err != nil {
		t.Fatalf("WriteFile(credentials) error = %v", err)
	}
	if _, err := preservedClaudeOAuthEnvironment(launchAuthModePreserve, source); err == nil || !strings.Contains(err.Error(), "without OAuth scopes") {
		t.Fatalf("preservedClaudeOAuthEnvironment() error = %v, want missing-scope rejection", err)
	}
}

func TestClaudeProfileDoesNotCopyOAuthThroughSymlinkAlias(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows symlink creation requires elevated privileges")
	}
	source := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", source)
	credentialsPath := filepath.Join(source, ".credentials.json")
	if err := os.WriteFile(credentialsPath, []byte(`{"claudeAiOauth":{"accessToken":"secret"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(credentialsPath, filepath.Join(source, "custom-credentials")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	profile, err := prepareClaudeLaunchProfile()
	if err != nil {
		t.Fatalf("prepareClaudeLaunchProfile() error = %v", err)
	}
	t.Cleanup(func() { _ = profile.cleanup() })
	if _, err := os.Stat(filepath.Join(profile.configDir, "custom-credentials")); !os.IsNotExist(err) {
		t.Fatalf("private profile copied OAuth through symlink alias: %v", err)
	}
}

func TestLaunchClaudeEnvSetsPrivateConfigAndPreservedOAuth(t *testing.T) {
	env := launchClaudeEnv(launchEnvironmentOptions{
		GatewayURL:        "http://127.0.0.1:1",
		ClaudeConfigDir:   "/tmp/ccr-profile",
		AuthMode:          launchAuthModePreserve,
		OAuthToken:        "oauth-access",
		OAuthRefreshToken: "oauth-refresh",
		OAuthScopes:       `["user:inference"]`,
	})
	set := environmentEntries(env.Set)
	if set["CLAUDE_CONFIG_DIR"] != "/tmp/ccr-profile" || set["CLAUDE_CODE_OAUTH_TOKEN"] != "oauth-access" ||
		set["CLAUDE_CODE_OAUTH_REFRESH_TOKEN"] != "oauth-refresh" || set["CLAUDE_CODE_OAUTH_SCOPES"] != "user:inference" {
		t.Fatalf("launch environment preserved private config and OAuth fields incorrectly: config_present=%t access_present=%t refresh_present=%t scopes_present=%t",
			set["CLAUDE_CONFIG_DIR"] != "", set["CLAUDE_CODE_OAUTH_TOKEN"] != "", set["CLAUDE_CODE_OAUTH_REFRESH_TOKEN"] != "", set["CLAUDE_CODE_OAUTH_SCOPES"] != "")
	}
	if !containsString(env.Unset, "CLAUDE_CONFIG_DIR") {
		t.Fatal("launch environment does not replace inherited CLAUDE_CONFIG_DIR")
	}
}

func TestLaunchClaudeEnvDoesNotOverrideNativeModelWindowEnforcement(t *testing.T) {
	env := launchClaudeEnv(launchEnvironmentOptions{
		GatewayURL: "http://127.0.0.1:1", ModelAlias: "worker", ModelID: "anthropic.ccr.worker",
	})
	set := environmentEntries(env.Set)
	if _, ok := set["CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT"]; ok {
		t.Fatalf("launch environment overrides native unknown-model compatibility: %#v", set)
	}
	if containsString(env.Unset, "CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT") {
		t.Fatal("launch environment clears native unknown-model compatibility")
	}
}

func TestLaunchClaudeEnvClearsInheritedSubagentConfigurationBeforeRuntimeModelSelection(t *testing.T) {
	env := launchClaudeEnv(launchEnvironmentOptions{
		GatewayURL: "http://127.0.0.1:1",
	})
	if !containsString(env.Unset, "CLAUDE_CODE_SUBAGENT_MODEL") || !containsString(env.Unset, "CLAUDE_CODE_SUBAGENT_MODEL_FORCE") {
		t.Fatalf("launch environment did not clear inherited child-model configuration before runtime alias selection: %#v", env.Unset)
	}
}

func TestLaunchClaudeEnvClearsInheritedSubagentConfigurationForCCRModel(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SUBAGENT_MODEL", "stale-native-or-ccr-alias")
	t.Setenv("CLAUDE_CODE_SUBAGENT_MODEL_FORCE", "1")
	env := launchClaudeEnv(launchEnvironmentOptions{
		GatewayURL: "http://127.0.0.1:1", ModelAlias: "worker", ModelID: "anthropic.ccr.worker",
	})
	if !containsString(env.Unset, "CLAUDE_CODE_SUBAGENT_MODEL") || !containsString(env.Unset, "CLAUDE_CODE_SUBAGENT_MODEL_FORCE") {
		t.Fatalf("CCR launch did not clear inherited child-model overrides: %#v", env.Unset)
	}
	applied := environmentEntries(applyClaudeEnvironment(os.Environ(), env))
	if _, ok := applied["CLAUDE_CODE_SUBAGENT_MODEL"]; ok {
		t.Fatal("CCR launch preserved inherited CLAUDE_CODE_SUBAGENT_MODEL")
	}
	if _, ok := applied["CLAUDE_CODE_SUBAGENT_MODEL_FORCE"]; ok {
		t.Fatal("CCR launch preserved inherited CLAUDE_CODE_SUBAGENT_MODEL_FORCE")
	}
}
