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
)

const (
	liveSubscriptionResumeSessionID = "fa605631-8a49-4afa-9888-ea4f7f26f26b"
	liveSubscriptionWorktreeName    = "ccr-resume-rotation"
)

func TestLiveFixtureSubscriptionPoolRelaunchesResumedWorktreeRealClaude(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}

	projectDir := newLiveSubscriptionGitProject(t, ctx)
	configDir := isolateLiveSubscriptionClaudeHome(t)
	trustLiveSubscriptionProject(t, configDir, projectDir)
	run := newLiveSubscriptionResumeRun(t, ctx, projectDir)
	first, second, launchErr := run.rotate(t, ctx)
	run.assert(t, ctx, first, second, launchErr)
}

type liveSubscriptionResumeRun struct {
	dbPath      string
	secrets     *accountTestSecrets
	fixture     *liveSubscriptionFixture
	launcher    *liveSubscriptionPTYLauncher
	commandOut  *bytes.Buffer
	commandErr  *bytes.Buffer
	commandDone chan error
}

func newLiveSubscriptionResumeRun(
	t *testing.T,
	ctx context.Context,
	projectDir string,
) *liveSubscriptionResumeRun {
	t.Helper()
	first429Released := make(chan struct{})
	fixture := newLiveSubscriptionFixture(t, []liveSubscriptionResponse{
		{
			account: "personal", token: liveSubscriptionPersonalToken,
			text: "CCR_LIVE_RESUME_SEEDED",
		},
		{
			account: "work", token: liveSubscriptionWorkToken,
			status: http.StatusTooManyRequests, unifiedStatus: "rejected",
			hold429Until: first429Released,
		},
	})
	dbPath := seedSubscriptionAccounts(t, []subscriptionAccountFixture{
		{name: "personal", token: liveSubscriptionPersonalToken},
		{name: "work", token: liveSubscriptionWorkToken},
	})
	secrets := &accountTestSecrets{values: map[string]string{
		secret.ClaudeAccountAccessTokenRef("personal"): liveSubscriptionPersonalToken,
		secret.ClaudeAccountAccessTokenRef("work"):     liveSubscriptionWorkToken,
	}}
	seedLiveSubscriptionResumeSession(t, ctx, projectDir, dbPath, secrets, fixture)

	launcher := newLiveSubscriptionPTYLauncher(projectDir)
	run := &liveSubscriptionResumeRun{
		dbPath: dbPath, secrets: secrets, fixture: fixture, launcher: launcher,
		commandOut: &bytes.Buffer{}, commandErr: &bytes.Buffer{}, commandDone: make(chan error, 1),
	}
	t.Cleanup(func() {
		close(first429Released)
		launcher.Close()
		fixture.Close()
	})
	go run.execute(ctx)
	return run
}

func (r *liveSubscriptionResumeRun) execute(ctx context.Context) {
	cmd := NewRootCommand(ctx, Dependencies{
		In: strings.NewReader(""), Out: r.commandOut, Err: r.commandErr,
		Secrets: r.secrets, Launcher: r.launcher, StartGateway: r.fixture.StartGateway,
	})
	cmd.SetArgs([]string{
		"--db", r.dbPath, "launch", "--auth-mode", "subscription-pool",
		"--no-lifecycle", "--no-statusline",
		"--worktree", liveSubscriptionWorktreeName,
		"--resume", liveSubscriptionResumeSessionID,
	})
	r.commandDone <- cmd.Execute()
}

func seedLiveSubscriptionResumeSession(
	t *testing.T,
	ctx context.Context,
	projectDir, dbPath string,
	secrets *accountTestSecrets,
	fixture *liveSubscriptionFixture,
) {
	t.Helper()
	launcher := newLiveSubscriptionPTYLauncher(projectDir)
	t.Cleanup(launcher.Close)
	out, errOut, err := runLiveCommand(ctx, Dependencies{
		Secrets: secrets, Launcher: launcher, StartGateway: fixture.StartGateway,
	}, "--db", dbPath, "launch", "--auth-mode", "subscription-pool",
		"--claude-account", "personal", "--print", "--no-lifecycle", "--no-statusline",
		"--session-id", liveSubscriptionResumeSessionID, "Seed the resumable fixture session.")
	if err != nil {
		t.Fatalf("seeding real Claude resume session: %s\nstdout:\n%s\nstderr:\n%s\ntranscript:\n%s",
			redactLiveSubscriptionOutput(err.Error()),
			redactLiveSubscriptionOutput(out),
			redactLiveSubscriptionOutput(errOut),
			redactLiveSubscriptionOutput(launcher.Transcript()))
	}
	if err := fixture.WaitCallCount(ctx, 1); err != nil {
		t.Fatalf("waiting for seeded real Claude session: %v", err)
	}
}

func (r *liveSubscriptionResumeRun) rotate(
	t *testing.T,
	ctx context.Context,
) (*liveSubscriptionPTYStart, *liveSubscriptionPTYStart, error) {
	t.Helper()
	first := r.launcher.WaitStart(t, ctx, r.commandDone, r.commandOut, r.commandErr)
	waitForLivePickerText(t, ctx, first.Transcript, r.commandDone, "Welcome back!")
	first.Submit(t, "Trigger the configured rate-limit response.")
	if err := r.fixture.WaitCallCount(ctx, 2); err != nil {
		t.Fatalf("waiting for resumed real Claude rate-limit request: %v", err)
	}
	second := r.launcher.WaitStart(t, ctx, r.commandDone, r.commandOut, r.commandErr)
	if !first.Process.Stopped() || !first.Process.DoneObserved() {
		t.Fatal("first resumed Claude process was not stopped before account rotation")
	}
	waitForLivePickerText(t, ctx, second.Transcript, r.commandDone, "Welcome back!")
	if stopErr := second.Process.Stop(); stopErr != nil {
		t.Fatalf("stopping rotated resumed Claude process: %v", stopErr)
	}
	var launchErr error
	select {
	case launchErr = <-r.commandDone:
	case <-ctx.Done():
		t.Fatalf("waiting for stopped resumed Claude relaunch: %v", ctx.Err())
	}
	return first, second, launchErr
}

func (r *liveSubscriptionResumeRun) assert(
	t *testing.T,
	ctx context.Context,
	first, second *liveSubscriptionPTYStart,
	launchErr error,
) {
	t.Helper()
	for index, start := range []*liveSubscriptionPTYStart{first, second} {
		assertLiveSubscriptionContinuityArgs(t, index, start.Args)
	}
	if !first.UsesToken(liveSubscriptionWorkToken) ||
		!second.UsesToken(liveSubscriptionPersonalToken) {
		t.Fatal("resumed real Claude rotation did not use the next account token")
	}
	if first.PID == 0 || second.PID == 0 || first.PID == second.PID {
		t.Fatalf("resumed real Claude PIDs = first:%d second:%d, want distinct processes",
			first.PID, second.PID)
	}
	combined := r.commandOut.String() + r.commandErr.String() +
		r.launcher.Transcript() + fmt.Sprint(launchErr)
	if !strings.Contains(combined, "relaunching with the next available account") {
		t.Fatalf("resumed real Claude output missing rotation metadata:\n%s",
			redactLiveSubscriptionOutput(combined))
	}
	r.assertPersistence(t, ctx, combined)
}

func (r *liveSubscriptionResumeRun) assertPersistence(
	t *testing.T,
	ctx context.Context,
	combined string,
) {
	t.Helper()
	r.fixture.AssertCalls(t, []string{"personal", "work"})
	assertSubscriptionLaunchMetadata(t, r.dbPath, []subscriptionLaunchWant{
		{account: "personal", state: "failed"},
		{account: "work", state: "failed", endReason: "subscription_exhausted"},
		{account: "personal", state: "completed"},
	})
	statusOut, _, err := runLiveCommand(
		ctx, Dependencies{Secrets: r.secrets}, "--db", r.dbPath, "status",
	)
	if err != nil {
		t.Fatalf("resumed subscription-pool status error = %v", err)
	}
	assertSubscriptionDatabaseRedaction(
		t, r.dbPath, liveSubscriptionPersonalToken, liveSubscriptionWorkToken,
	)
	assertNoSubscriptionTokenLeak(
		t, combined+statusOut, liveSubscriptionPersonalToken, liveSubscriptionWorkToken,
	)
}

func assertLiveSubscriptionContinuityArgs(t *testing.T, index int, args []string) {
	t.Helper()
	for _, pair := range [][2]string{
		{"--worktree", liveSubscriptionWorktreeName},
		{"--resume", liveSubscriptionResumeSessionID},
	} {
		if !containsArgPair(args, pair[0], pair[1]) {
			t.Fatalf("resumed Claude start %d args = %v, missing %s %s",
				index, args, pair[0], pair[1])
		}
	}
	if containsString(args, "--continue") {
		t.Fatalf("resumed Claude start %d args = %v, must preserve explicit resume", index, args)
	}
}

func newLiveSubscriptionGitProject(t *testing.T, ctx context.Context) string {
	t.Helper()
	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, "README.md"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatalf("writing live subscription fixture project: %v", err)
	}
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "ccr-fixture@example.invalid"},
		{"config", "user.name", "CCR Fixture"},
		{"config", "commit.gpgsign", "false"},
		{"add", "README.md"},
		{"commit", "-m", "fixture"},
	} {
		command := exec.CommandContext(ctx, "git", append([]string{"-C", projectDir}, args...)...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("preparing live subscription Git project with git %s: %v\n%s",
				strings.Join(args, " "), err, output)
		}
	}
	return projectDir
}

func trustLiveSubscriptionProject(t *testing.T, configDir, projectDir string) {
	t.Helper()
	writeLiveSubscriptionJSON(t, filepath.Join(configDir, ".claude.json"), map[string]any{
		"hasCompletedOnboarding": true,
		"projects": map[string]any{
			projectDir: map[string]any{
				"hasTrustDialogAccepted":     true,
				"projectOnboardingSeenCount": 1,
			},
		},
	})
}
