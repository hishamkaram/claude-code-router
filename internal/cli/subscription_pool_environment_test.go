package cli

import (
	"strings"
	"testing"
)

func TestSubscriptionPoolEnvironmentUsesOnlySelectedOAuthToken(t *testing.T) {
	const selectedToken = "selected-oauth-token"
	t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "Authorization: Bearer other-account\nX-Api-Key: stale-key")
	env := launchClaudeEnv(launchEnvironmentOptions{
		GatewayURL:        "http://127.0.0.1:43123",
		Token:             "gateway-session-token",
		ObserverToken:     "observer-token",
		LaunchID:          42,
		AuthMode:          launchAuthModeSubscriptionPool,
		ClaudeOAuthToken:  selectedToken,
		ClaudeAccountName: "personal",
		ProviderSecretEnvNames: []string{
			"ANTHROPIC_API_KEY",
			"OPENROUTER_API_KEY",
		},
	})

	set := environmentEntries(env.Set)
	if set["CLAUDE_CODE_OAUTH_TOKEN"] != selectedToken {
		t.Fatal("selected OAuth token was not set for Claude Code")
	}
	if set[statuslineClaudeAccountEnv] != "personal" {
		t.Fatalf("status-line account = %q, want personal", set[statuslineClaudeAccountEnv])
	}
	if !strings.Contains(set["ANTHROPIC_CUSTOM_HEADERS"], "X-CCR-Session-Token: gateway-session-token") {
		t.Fatal("gateway session header was not configured")
	}
	if strings.Contains(set["ANTHROPIC_CUSTOM_HEADERS"], "other-account") ||
		strings.Contains(set["ANTHROPIC_CUSTOM_HEADERS"], "stale-key") {
		t.Fatal("subscription-pool inherited auth-bearing custom headers")
	}
	for _, name := range []string{
		"ANTHROPIC_API_KEY",
		"ANTHROPIC_AUTH_TOKEN",
		"CLAUDE_CODE_OAUTH_REFRESH_TOKEN",
		"CLAUDE_CODE_OAUTH_SCOPES",
		"OPENROUTER_API_KEY",
	} {
		if !containsString(env.Unset, name) {
			t.Fatalf("%s was not removed from the inherited environment", name)
		}
	}
	for _, entry := range env.Set {
		if strings.HasPrefix(entry, "ANTHROPIC_AUTH_TOKEN=") {
			t.Fatal("subscription OAuth token was also exposed as gateway bearer auth")
		}
	}
}

func TestGatewayTokenEnvironmentRemovesInheritedClaudeAuth(t *testing.T) {
	t.Parallel()

	env := launchClaudeEnv(launchEnvironmentOptions{
		GatewayURL: "http://127.0.0.1:43123",
		Token:      "gateway-session-token",
		AuthMode:   launchAuthModeGatewayToken,
	})
	base := []string{
		"ANTHROPIC_CUSTOM_HEADERS=Authorization: Bearer stale-account",
		"CLAUDE_CODE_OAUTH_TOKEN=stale-oauth",
		"CLAUDE_CODE_OAUTH_REFRESH_TOKEN=stale-refresh",
		"CLAUDE_CODE_OAUTH_SCOPES=user:inference",
		statuslineClaudeAccountEnv + "=stale-account",
	}
	applied := environmentEntries(applyClaudeEnvironment(base, env))
	for _, name := range []string{
		"ANTHROPIC_CUSTOM_HEADERS",
		"CLAUDE_CODE_OAUTH_TOKEN",
		"CLAUDE_CODE_OAUTH_REFRESH_TOKEN",
		"CLAUDE_CODE_OAUTH_SCOPES",
		statuslineClaudeAccountEnv,
	} {
		if _, exists := applied[name]; exists {
			t.Fatalf("gateway-token preserved inherited %s", name)
		}
	}
	if applied["ANTHROPIC_AUTH_TOKEN"] != "gateway-session-token" {
		t.Fatal("gateway-token did not configure the generated local token")
	}
}

func TestExplicitClaudeAccountRequiresSubscriptionPool(t *testing.T) {
	t.Parallel()

	err := validateLaunchInputs("", launchAuthModePreserve, "personal", "")
	if err == nil || !strings.Contains(err.Error(), "--auth-mode subscription-pool") {
		t.Fatalf("validateLaunchInputs error = %v", err)
	}
	if err := validateLaunchInputs("", launchAuthModeSubscriptionPool, "personal", ""); err != nil {
		t.Fatalf("validateLaunchInputs subscription-pool error = %v", err)
	}
}

func TestSubscriptionPoolLaunchSummaryDescribesProcessBoundAuth(t *testing.T) {
	t.Parallel()

	var output strings.Builder
	writeLaunchAuthSummary(&output, launchAuthModeSubscriptionPool)
	summary := output.String()
	for _, want := range []string{
		"Model requests use the selected account's stored OAuth token",
		"account name is a local CCR label",
		"Claude UI profile and cached usage may still show the shared local login",
	} {
		if !strings.Contains(summary, want) {
			t.Fatalf("subscription-pool launch summary missing %q: %s", want, summary)
		}
	}
	if strings.Contains(summary, "are preserved") || strings.Contains(summary, "UI profile is selected") {
		t.Fatalf("subscription-pool launch summary claims inherited auth is preserved: %s", summary)
	}
}

func environmentEntries(entries []string) map[string]string {
	values := make(map[string]string, len(entries))
	for _, entry := range entries {
		name, value, found := strings.Cut(entry, "=")
		if found {
			values[name] = value
		}
	}
	return values
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
