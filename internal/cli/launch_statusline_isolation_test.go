package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/secret"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestIsolatedStatuslineShellCommandPreservesOutputAndScrubsCredentials(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Windows uses the account-aware status-line fallback")
	}

	original := `printf '%s|%s|%s|%s|it'\''s' "$CCR_CLAUDE_ACCOUNT" "$CLAUDE_CODE_OAUTH_TOKEN" "$ANTHROPIC_CUSTOM_HEADERS" "$CCR_OBSERVER_TOKEN"`
	command := isolatedStatuslineShellCommand(
		original,
		`printf '%s' "$CCR_CLAUDE_ACCOUNT"`,
	)
	cmd := exec.Command("sh", "-c", command)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		statuslineClaudeAccountEnv + "=personal",
		"CLAUDE_CODE_OAUTH_TOKEN=oauth-secret",
		"ANTHROPIC_CUSTOM_HEADERS=X-CCR-Session-Token: gateway-secret",
		statuslineTokenEnv + "=observer-secret",
	}
	output, runErr := cmd.Output()
	if runErr != nil {
		t.Fatalf("isolated status-line command error = %v", runErr)
	}
	if got, want := string(output), "personal||||it's"; got != want {
		t.Fatalf("isolated status-line output = %q, want %q", got, want)
	}
	for _, secret := range []string{"oauth-secret", "gateway-secret", "observer-secret"} {
		if strings.Contains(string(output), secret) {
			t.Fatalf("isolated status-line output leaked %s", secret)
		}
	}
}

func TestIsolateClaudeStatuslineCredentialsRejectsUnsupportedShape(t *testing.T) {
	t.Parallel()

	for _, setting := range []map[string]any{
		{"type": "other", "command": "echo unsafe"},
		{"type": "command", "command": " "},
	} {
		if _, err := isolateClaudeStatuslineCredentials(setting, ""); err == nil {
			t.Fatalf("isolateClaudeStatuslineCredentials(%#v) error = nil", setting)
		}
	}
}

func TestStatuslineCredentialIsolationSupported(t *testing.T) {
	t.Parallel()

	if statuslineCredentialIsolationSupported("windows") {
		t.Fatal("Windows must use the credential-safe CCR status-line fallback")
	}
	for _, goos := range []string{"darwin", "linux"} {
		if !statuslineCredentialIsolationSupported(goos) {
			t.Fatalf("%s should support credential-isolated existing status lines", goos)
		}
	}
}

func TestClaudeStatuslineSettingHonorsExplicitNullOverride(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Chdir(t.TempDir())
	if err := os.WriteFile(
		filepath.Join(configDir, "settings.json"),
		[]byte(`{"statusLine":{"type":"command","command":"must-not-run"}}`),
		0o600,
	); err != nil {
		t.Fatalf("WriteFile(settings.json) error = %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(configDir, "settings.local.json"),
		[]byte(`{"statusLine":null}`),
		0o600,
	); err != nil {
		t.Fatalf("WriteFile(settings.local.json) error = %v", err)
	}

	statusline, state, err := claudeStatuslineSetting()
	if err != nil {
		t.Fatalf("claudeStatuslineSetting() error = %v", err)
	}
	if statusline != nil || state != claudeStatuslineDisabled {
		t.Fatalf("claudeStatuslineSetting() = %#v, %v; want nil, disabled", statusline, state)
	}
	settings := make(map[string]any)
	resultState, err := addLaunchStatusline(settings, launchSettingsOptions{
		StatuslineEnabled:            true,
		IsolateStatuslineCredentials: true,
	})
	if err != nil {
		t.Fatalf("addLaunchStatusline() error = %v", err)
	}
	if resultState != "disabled" || len(settings) != 0 {
		t.Fatalf("addLaunchStatusline() state=%q settings=%#v; want disabled and empty", resultState, settings)
	}
	var notice strings.Builder
	writeSubscriptionStatuslineNotice(
		&notice,
		&selectedClaudeAccount{Account: store.ClaudeAccount{Name: "personal"}},
		false,
		resultState,
	)
	if got := notice.String(); !strings.Contains(got, "statusLine is explicitly disabled") ||
		!strings.Contains(got, "Active account: personal") {
		t.Fatalf("status-line disabled notice = %q", got)
	}
}

func TestSubscriptionPoolExplicitNullRejectsPassthroughSettings(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Chdir(t.TempDir())
	if err := os.WriteFile(
		filepath.Join(configDir, "settings.json"),
		[]byte(`{"statusLine":null}`),
		0o600,
	); err != nil {
		t.Fatalf("WriteFile(settings.json) error = %v", err)
	}

	dbPath := seedSubscriptionAccounts(t, []subscriptionAccountFixture{
		{name: "personal", token: "personal-oauth-token"},
	})
	secrets := &accountTestSecrets{values: map[string]string{
		secret.ClaudeAccountAccessTokenRef("personal"): "personal-oauth-token",
	}}
	launcher := &fakeLauncher{pid: 4321}
	_, _, err := runCommandWithDeps(t, Dependencies{
		Secrets: secrets, Launcher: launcher,
	}, "--db", dbPath, "launch", "--auth-mode", "subscription-pool",
		"--no-lifecycle", "--settings", `{"statusLine":{"type":"command","command":"unsafe"}}`)
	if err == nil || !strings.Contains(err.Error(), "--settings cannot override") {
		t.Fatalf("subscription-pool launch error = %v, want settings rejection", err)
	}
	if launcher.starts != 0 {
		t.Fatalf("rejected launch started Claude %d time(s)", launcher.starts)
	}
	account := getAccountForCLI(t, dbPath, "personal")
	if account.LastUsedAt != "" {
		t.Fatalf("rejected launch stamped account usage at %s", account.LastUsedAt)
	}
}
