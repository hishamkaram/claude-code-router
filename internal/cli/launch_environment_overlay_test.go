package cli

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestLaunchEnvironmentPreservesCallerVariablesAcrossAuthModes(t *testing.T) {
	t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "X-User-Header: synthetic-header")
	base := []string{
		"HOME=/caller/home", "PATH=/caller/bin", "XDG_CONFIG_HOME=/caller/config",
		"XDG_DATA_HOME=/caller/data", "XDG_STATE_HOME=/caller/state", "CLAUDE_CONFIG_DIR=/caller/claude",
		"PLUGIN_API_KEY=synthetic-shared-key", "PLUGIN_EMPTY=", "DATABASE_DSN=synthetic-dsn",
		"CCR_EXECUTOR_TOKEN=synthetic-executor", "ANTHROPIC_API_KEY=synthetic-anthropic-key",
		"CLAUDE_CODE_OAUTH_TOKEN=synthetic-oauth", "ANTHROPIC_AUTH_TOKEN=stale-local-token",
	}
	for _, mode := range []string{launchAuthModeAuto, launchAuthModePreserve, launchAuthModeProviderOnly, launchAuthModeGatewayToken, launchAuthModeSubscriptionPool} {
		t.Run(mode, func(t *testing.T) {
			overlay := launchClaudeEnv(launchEnvironmentOptions{GatewayURL: "http://127.0.0.1:43123", Token: "synthetic-local-token", AuthMode: mode, ExternalTokenEnv: "CCR_EXECUTOR_TOKEN"})
			child := environmentEntries(applyClaudeEnvironment(base, overlay.processEnvironment()))
			for _, entry := range base[:9] {
				name, value, _ := strings.Cut(entry, "=")
				got, exists := child[name]
				if !exists || got != value {
					t.Fatalf("caller variable %s was changed", name)
				}
			}
			if _, exists := child["MISSING_PLUGIN_VARIABLE"]; exists {
				t.Fatal("absent variable was synthesized")
			}
			if _, exists := child["CCR_EXECUTOR_TOKEN"]; exists {
				t.Fatal("designated executor token inherited")
			}
			if mode == launchAuthModeAuto || mode == launchAuthModePreserve {
				if child["ANTHROPIC_API_KEY"] != "synthetic-anthropic-key" || child["CLAUDE_CODE_OAUTH_TOKEN"] != "synthetic-oauth" {
					t.Fatal("preserve mode lost caller authentication")
				}
				if _, exists := child["ANTHROPIC_AUTH_TOKEN"]; exists {
					t.Fatal("stale gateway token inherited")
				}
			} else if child["ANTHROPIC_AUTH_TOKEN"] != "synthetic-local-token" {
				t.Fatal("local authentication was not installed")
			}
		})
	}
}

func TestLaunchSettingsPinOnlyNonSecretRoutingControls(t *testing.T) {
	t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "X-Synthetic: synthetic-long-lived-header")
	for _, mode := range []string{launchAuthModePreserve, launchAuthModeProviderOnly, launchAuthModeGatewayToken, launchAuthModeSubscriptionPool} {
		t.Run(mode, func(t *testing.T) {
			overlay := launchClaudeEnv(launchEnvironmentOptions{GatewayURL: "http://127.0.0.1:43123", Token: "synthetic-secret-gateway", ObserverToken: "synthetic-secret-observer", AuthMode: mode, ModelAlias: "coding", ModelID: "anthropic.ccr.coding", ExternalTokenEnv: "SYNTHETIC_EXECUTOR_KEY"})
			settings := overlay.settingsEnvironment()
			for _, name := range cloudProviderSelectors() {
				value, exists := settings[name]
				if !exists || value != "" {
					t.Fatalf("provider selector %s was not pinned off", name)
				}
				if !reservedLaunchExternalTokenEnvName(strings.ToLower(name)) {
					t.Fatalf("provider selector %s allowed as executor token", name)
				}
			}
			if settings["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:43123" {
				t.Fatal("gateway URL not pinned")
			}
			if settings["ANTHROPIC_CUSTOM_MODEL_OPTION"] != "anthropic.ccr.coding" {
				t.Fatal("model option not pinned")
			}
			for _, name := range []string{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_CUSTOM_HEADERS", "CLAUDE_CODE_OAUTH_TOKEN", statuslineTokenEnv, statuslineClaudeAccountEnv, "CCR_LAUNCH_ID", "SYNTHETIC_EXECUTOR_KEY", "ENABLE_TOOL_SEARCH"} {
				if _, exists := settings[name]; exists {
					t.Fatalf("process-only variable %s entered settings", name)
				}
			}
			data, err := json.Marshal(settings)
			if err != nil {
				t.Fatal(err)
			}
			for _, sentinel := range []string{"synthetic-secret-gateway", "synthetic-secret-observer", "synthetic-long-lived-header"} {
				if strings.Contains(string(data), sentinel) {
					t.Fatal("credential entered generated settings")
				}
			}
		})
	}
	toolsOff := launchClaudeEnv(launchEnvironmentOptions{DisableTools: true}).settingsEnvironment()
	if toolsOff["CLAUDE_CODE_SIMPLE"] != "1" {
		t.Fatal("chat-only mode not pinned")
	}
	if value, exists := toolsOff["ENABLE_TOOL_SEARCH"]; !exists || value != "" {
		t.Fatal("chat-only tool search not pinned off")
	}
}

func TestMergeClaudeEnvironmentUsesPlatformNameSemantics(t *testing.T) {
	t.Parallel()
	base := []string{"anthropic_api_key=synthetic-key", "Anthropic_Auth_Token=stale", "Plugin_Key=synthetic-plugin"}
	overlay := ClaudeEnvironment{Unset: []string{"ANTHROPIC_API_KEY"}, Set: []string{"ANTHROPIC_AUTH_TOKEN=fresh"}}
	windows := mergeClaudeEnvironment(base, overlay, true)
	if !reflect.DeepEqual(windows, []string{"Plugin_Key=synthetic-plugin", "ANTHROPIC_AUTH_TOKEN=fresh"}) {
		t.Fatal("Windows environment replacement was case-sensitive")
	}
	unix := mergeClaudeEnvironment(base, overlay, false)
	if !slices.Contains(unix, base[0]) || !slices.Contains(unix, base[1]) {
		t.Fatal("Unix environment names were case-folded")
	}
}

func TestLaunchEnvironmentNoticeContainsNamesOnly(t *testing.T) {
	t.Parallel()
	env := launchClaudeEnv(launchEnvironmentOptions{AuthMode: launchAuthModeProviderOnly, Token: "synthetic-new-token"})
	var out strings.Builder
	writeLaunchEnvironmentNotice(&out, env, []string{"PLUGIN_KEY=synthetic-plugin-secret", "ANTHROPIC_AUTH_TOKEN=synthetic-old-secret", "CLAUDE_CODE_USE_BEDROCK=synthetic-selector", "ANTHROPIC_API_KEY="})
	if !strings.Contains(out.String(), "ANTHROPIC_AUTH_TOKEN, CLAUDE_CODE_USE_BEDROCK") {
		t.Fatal("notice missing controlled inherited names")
	}
	for _, unwanted := range []string{"synthetic-", "PLUGIN_KEY", "ANTHROPIC_API_KEY"} {
		if strings.Contains(out.String(), unwanted) {
			t.Fatal("notice exposed values or names outside its contract")
		}
	}
}

func assertPinnedLaunchSettings(t *testing.T, launcher *fakeLauncher, actual map[string]string) {
	t.Helper()
	expected := map[string]string{}
	for _, name := range cloudProviderSelectors() {
		expected[name] = ""
	}
	for name, value := range environmentEntries(launcher.env.Set) {
		if name == "ANTHROPIC_BASE_URL" || name == "CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY" || strings.HasPrefix(name, "ANTHROPIC_CUSTOM_MODEL_OPTION") || strings.HasPrefix(name, "ANTHROPIC_DEFAULT_") {
			expected[name] = value
		}
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatal("generated settings do not match required non-secret routing controls")
	}
}
