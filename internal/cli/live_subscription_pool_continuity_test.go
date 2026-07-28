//go:build live

package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/liveclaude"
	"github.com/hishamkaram/claude-code-router/internal/secret"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestLiveFixtureSubscriptionPoolIsolatesExistingStatuslineCredentials(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}
	ccrExecutable := buildLiveCCRExecutable(t, ctx)
	configDir := isolateLiveSubscriptionClaudeHome(t)
	probePath := filepath.Join(t.TempDir(), "statusline-environment.txt")
	statuslineCommand := fmt.Sprintf(
		`printf '%%s|%%s|%%s|%%s' "${CLAUDE_CODE_OAUTH_TOKEN-unset}" "${ANTHROPIC_CUSTOM_HEADERS-unset}" "${CCR_OBSERVER_TOKEN-unset}" "$CCR_CLAUDE_ACCOUNT" > %s`,
		quotePOSIXShellArg(probePath),
	)
	settingsPath := filepath.Join(configDir, "settings.json")
	writeLiveSubscriptionJSON(t, settingsPath, map[string]any{
		"statusLine": map[string]any{"type": "command", "command": statuslineCommand},
	})
	originalSettings, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("reading original Claude settings: %v", err)
	}

	dbPath := seedSubscriptionAccounts(t, []subscriptionAccountFixture{{
		name: "personal", token: liveSubscriptionPersonalToken,
	}})
	secrets := &accountTestSecrets{values: map[string]string{
		secret.ClaudeAccountAccessTokenRef("personal"): liveSubscriptionPersonalToken,
	}}
	fixture := newLiveSubscriptionFixture(t, nil)
	launcher := newLiveSubscriptionPTYLauncher("")
	commandOut, commandErr := &bytes.Buffer{}, &bytes.Buffer{}
	commandDone := make(chan error, 1)
	t.Cleanup(func() {
		launcher.Close()
		fixture.Close()
	})
	go func() {
		cmd := NewRootCommand(ctx, Dependencies{
			In: strings.NewReader(""), Out: commandOut, Err: commandErr,
			Secrets: secrets, Launcher: launcher, StartGateway: fixture.StartGateway,
			ExecutablePath: ccrExecutable,
		})
		cmd.SetArgs([]string{
			"--db", dbPath, "launch", "--auth-mode", "subscription-pool",
			"--claude-account", "personal", "--no-lifecycle",
		})
		commandDone <- cmd.Execute()
	}()

	start := launcher.WaitStart(t, ctx, commandDone, commandOut, commandErr)
	waitForLivePickerText(t, ctx, start.Transcript, commandDone, "Welcome back!")
	probe := waitForStatuslineProbe(t, ctx, probePath, commandDone)
	if probe != "unset|unset|unset|personal" {
		t.Fatalf("credential-isolated status-line environment = %q", probe)
	}
	if stopErr := start.Process.Stop(); stopErr != nil {
		t.Fatalf("stopping real Claude process after status-line proof: %v", stopErr)
	}
	select {
	case <-commandDone:
	case <-ctx.Done():
		t.Fatalf("waiting for test-owned Claude shutdown: %v", ctx.Err())
	}
	currentSettings, readErr := os.ReadFile(settingsPath)
	if readErr != nil || string(currentSettings) != string(originalSettings) {
		t.Fatalf("user status-line settings changed: %q, %v", currentSettings, readErr)
	}
	launches := loadSubscriptionLaunches(t, dbPath)
	if len(launches) != 1 || launches[0].StatuslineState != "isolated" {
		t.Fatalf("status-line launch metadata = %#v", launches)
	}
	combined := commandOut.String() + commandErr.String() + launcher.Transcript() + probe
	assertNoSubscriptionTokenLeak(t, combined, liveSubscriptionPersonalToken)
}

func buildLiveCCRExecutable(t *testing.T, ctx context.Context) string {
	t.Helper()
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolving repository root: %v", err)
	}
	tempFile, err := os.CreateTemp("", "ccr-live-statusline-*")
	if err != nil {
		t.Fatalf("creating live CCR executable path: %v", err)
	}
	executable := tempFile.Name()
	if err := tempFile.Close(); err != nil {
		t.Fatalf("closing live CCR executable placeholder: %v", err)
	}
	if err := os.Remove(executable); err != nil {
		t.Fatalf("removing live CCR executable placeholder: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Remove(executable); err != nil && !os.IsNotExist(err) {
			t.Errorf("removing live CCR executable: %v", err)
		}
	})
	command := exec.CommandContext(ctx, "go", "build", "-o", executable, "./cmd/ccr")
	command.Dir = repoRoot
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("building live CCR status-line helper: %v\n%s", err, output)
	}
	return executable
}

func TestLiveFixtureSubscriptionPoolKeepsRealClaudeOpenWhenAllAccountsLimited(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}
	isolateLiveSubscriptionClaudeHome(t)

	release429 := make(chan struct{})
	fixture := newLiveSubscriptionFixture(t, []liveSubscriptionResponse{{
		account: "personal", token: liveSubscriptionPersonalToken,
		status: http.StatusTooManyRequests, unifiedStatus: "rejected",
		representativeClaim: "five_hour", hold429Until: release429,
	}})
	fixture.repeatLast = true
	dbPath := seedSubscriptionAccounts(t, []subscriptionAccountFixture{{
		name: "personal", token: liveSubscriptionPersonalToken,
	}})
	secrets := &accountTestSecrets{values: map[string]string{
		secret.ClaudeAccountAccessTokenRef("personal"): liveSubscriptionPersonalToken,
	}}
	launcher := newLiveSubscriptionPTYLauncher("")
	commandOut, commandErr := &bytes.Buffer{}, &bytes.Buffer{}
	commandDone := make(chan error, 1)
	t.Cleanup(func() {
		close(release429)
		launcher.Close()
		fixture.Close()
	})

	go runLiveSubscriptionContinuityCommand(
		ctx, dbPath, secrets, launcher, fixture, commandOut, commandErr, commandDone,
	)
	start := launcher.WaitStart(t, ctx, commandDone, commandOut, commandErr)
	waitForLivePickerText(t, ctx, start.Transcript, commandDone, "Welcome back!")
	start.Submit(t, "Trigger the configured rate-limit response.")
	if err := fixture.WaitCallCount(ctx, 1); err != nil {
		t.Fatalf("waiting for real Claude rate-limit request: %v", err)
	}
	waitForClaudeAccountFailure(t, dbPath, "personal", "subscription_limit_five_hour")
	assertRealClaudeRemainsOpen(t, start, launcher, commandDone)

	if err := start.Process.Stop(); err != nil {
		t.Fatalf("stopping real Claude process after continuity proof: %v", err)
	}
	var launchErr error
	select {
	case launchErr = <-commandDone:
	case <-ctx.Done():
		t.Fatalf("waiting for test-owned Claude shutdown: %v", ctx.Err())
	}
	combined := commandOut.String() + commandErr.String() + launcher.Transcript() + fmt.Sprint(launchErr)
	for _, want := range []string{
		"no replacement account is usable",
		"kept Claude Code running",
	} {
		if !strings.Contains(combined, want) {
			t.Fatalf("real Claude continuity output missing %q:\n%s",
				want, redactLiveSubscriptionOutput(combined))
		}
	}
	assertLiveSubscriptionCallsUseOnly(t, fixture, "personal")
	assertSubscriptionDatabaseRedaction(t, dbPath, liveSubscriptionPersonalToken)
	assertNoSubscriptionTokenLeak(t, combined, liveSubscriptionPersonalToken)
}

func waitForStatuslineProbe(
	t *testing.T,
	ctx context.Context,
	path string,
	commandDone <-chan error,
) string {
	t.Helper()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		data, err := os.ReadFile(path)
		if err == nil && len(data) > 0 {
			return string(data)
		}
		select {
		case commandErr := <-commandDone:
			t.Fatalf("Claude exited before status-line probe completed: %v", commandErr)
		case <-ctx.Done():
			t.Fatalf("waiting for status-line probe: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}

func assertLiveSubscriptionCallsUseOnly(
	t *testing.T,
	fixture *liveSubscriptionFixture,
	account string,
) {
	t.Helper()
	fixture.mu.Lock()
	calls := append([]string(nil), fixture.calls...)
	fixture.mu.Unlock()
	if len(calls) == 0 {
		t.Fatal("real Claude did not send a subscription request")
	}
	for _, call := range calls {
		if call != account {
			t.Fatalf("subscription-pool upstream accounts = %v, want only %s", calls, account)
		}
	}
}

func runLiveSubscriptionContinuityCommand(
	ctx context.Context,
	dbPath string,
	secrets *accountTestSecrets,
	launcher *liveSubscriptionPTYLauncher,
	fixture *liveSubscriptionFixture,
	commandOut, commandErr *bytes.Buffer,
	commandDone chan<- error,
) {
	cmd := NewRootCommand(ctx, Dependencies{
		In: strings.NewReader(""), Out: commandOut, Err: commandErr,
		Secrets: secrets, Launcher: launcher, StartGateway: fixture.StartGateway,
	})
	cmd.SetArgs([]string{
		"--db", dbPath, "launch", "--auth-mode", "subscription-pool",
		"--no-lifecycle", "--no-statusline",
	})
	commandDone <- cmd.Execute()
}

func assertRealClaudeRemainsOpen(
	t *testing.T,
	start *liveSubscriptionPTYStart,
	launcher *liveSubscriptionPTYLauncher,
	commandDone <-chan error,
) {
	t.Helper()
	time.Sleep(500 * time.Millisecond)
	if start.Process.Stopped() || start.Process.DoneObserved() {
		t.Fatal("CCR stopped the real Claude process without a replacement account")
	}
	select {
	case err := <-commandDone:
		t.Fatalf("CCR exited while the real Claude process should remain open: %v", err)
	default:
	}
	select {
	case unexpected := <-launcher.starts:
		t.Fatalf("CCR unexpectedly relaunched real Claude with pid %d", unexpected.PID)
	default:
	}
}

func waitForClaudeAccountFailure(t *testing.T, dbPath, name, failureClass string) {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open error = %v", err)
	}
	defer closeStore(s)
	deadline := time.Now().Add(10 * time.Second)
	for {
		account, getErr := s.GetClaudeAccount(ctx, name)
		if getErr == nil && account.LastError == failureClass {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf(
				"Claude account %q failure = %q, error=%v, want %q",
				name, account.LastError, getErr, failureClass,
			)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
