package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/gateway"
	"github.com/hishamkaram/claude-code-router/internal/secret"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestSubscriptionPoolLaunchSelectsAccountAndRecordsAuth(t *testing.T) {
	t.Parallel()

	dbPath := seedSubscriptionAccounts(t, []subscriptionAccountFixture{
		{name: "personal", token: "personal-oauth-token"},
	})
	secrets := &accountTestSecrets{values: map[string]string{
		secret.ClaudeAccountAccessTokenRef("personal"): "personal-oauth-token",
	}}
	launcher := &fakeLauncher{pid: 4321}
	out, errOut, err := runCommandWithDeps(t, Dependencies{
		Secrets: secrets, Launcher: launcher,
	}, "--db", dbPath, "launch", "--auth-mode", "subscription-pool", "--no-lifecycle", "--no-statusline")
	if err != nil {
		t.Fatalf("subscription-pool launch error = %v, stderr=%q", err, errOut)
	}
	if launcher.starts != 1 || !launcher.hasEnv("CLAUDE_CODE_OAUTH_TOKEN=personal-oauth-token") {
		t.Fatalf("launcher starts=%d env=%s", launcher.starts, launcher.environmentSummary())
	}
	if !launcher.hasEnv(statuslineClaudeAccountEnv + "=personal") {
		t.Fatalf("launcher did not expose selected status-line account: %s", launcher.environmentSummary())
	}
	if !launcher.unsetsEnv("ANTHROPIC_API_KEY") || !launcher.unsetsEnv("ANTHROPIC_AUTH_TOKEN") {
		t.Fatalf("launcher did not remove higher-precedence auth: %s", launcher.environmentSummary())
	}
	if !strings.Contains(errOut, "Claude model-request account selected: personal") {
		t.Fatalf("selection was not visible: stdout=%q stderr=%q", out, errOut)
	}
	if !strings.Contains(errOut, "account-aware status line is disabled") {
		t.Fatalf("disabled status-line warning was not visible: %q", errOut)
	}
	if strings.Contains(out+errOut, "personal-oauth-token") {
		t.Fatal("launch output leaked the selected OAuth token")
	}

	launches := loadSubscriptionLaunches(t, dbPath)
	if len(launches) != 1 || launches[0].AuthMode != launchAuthModeSubscriptionPool ||
		launches[0].ClaudeAccountName != "personal" {
		t.Fatalf("launch metadata = %#v", launches)
	}
	statusOut, _, err := runCommandWithDeps(t, Dependencies{Secrets: secrets}, "--db", dbPath, "status")
	if err != nil {
		t.Fatalf("status error = %v", err)
	}
	if !strings.Contains(statusOut, "Launch auth: mode=subscription-pool account=personal") {
		t.Fatalf("status did not expose selected account metadata: %q", statusOut)
	}
}

func TestSubscriptionPoolPreservesExistingStatusline(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	settingsPath := filepath.Join(claudeDir, "settings.json")
	existing := `{"statusLine":{"type":"command","command":"shared-profile-limits"}}`
	if err := os.WriteFile(settingsPath, []byte(existing), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	dbPath := seedSubscriptionAccounts(t, []subscriptionAccountFixture{
		{name: "personal", token: "personal-oauth-token"},
	})
	secrets := &accountTestSecrets{values: map[string]string{
		secret.ClaudeAccountAccessTokenRef("personal"): "personal-oauth-token",
	}}
	launcher := &fakeLauncher{pid: 4321}
	_, errOut, err := runCommandWithDeps(t, Dependencies{
		Secrets: secrets, Launcher: launcher,
	}, "--db", dbPath, "launch", "--auth-mode", "subscription-pool", "--no-lifecycle")
	if err != nil {
		t.Fatalf("subscription-pool launch error = %v, stderr=%q", err, errOut)
	}

	settingsJSON, ok := launcher.settingsArgValue()
	if !ok {
		t.Fatalf("launch settings missing credential-isolated status line: %#v", launcher.args)
	}
	var generated struct {
		StatusLine map[string]any `json:"statusLine"`
	}
	if decodeErr := json.Unmarshal([]byte(settingsJSON), &generated); decodeErr != nil {
		t.Fatalf("launch settings decode error = %v", decodeErr)
	}
	command, _ := generated.StatusLine["command"].(string)
	if !strings.Contains(command, "shared-profile-limits") ||
		!strings.Contains(command, "env -u CLAUDE_CODE_OAUTH_TOKEN") ||
		strings.Contains(command, "__statusline") {
		t.Fatalf("launch status line was not safely preserved: %q", command)
	}
	launchSettingsPath, ok := launcher.rawSettingsArgValue()
	if !ok || strings.Contains(launchSettingsPath, "shared-profile-limits") {
		t.Fatalf("launch argv exposed the existing status-line command: %#v", launcher.args)
	}
	if _, statErr := os.Stat(launchSettingsPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("private launch settings were not removed: %v", statErr)
	}
	data, readErr := os.ReadFile(settingsPath)
	if readErr != nil || string(data) != existing {
		t.Fatalf("existing settings changed: %q, %v", data, readErr)
	}
	for _, want := range []string{
		"Subscription limits for account personal: unknown",
		"Existing Claude statusLine preserved through a launch-only credential-isolation wrapper",
		"CCR_CLAUDE_ACCOUNT=personal",
		"OAuth and gateway tokens are removed",
	} {
		if !strings.Contains(errOut, want) {
			t.Fatalf("status-line notice missing %q: %s", want, errOut)
		}
	}
	launches := loadSubscriptionLaunches(t, dbPath)
	if len(launches) != 1 || launches[0].StatuslineState != "isolated" {
		t.Fatalf("launch status-line state = %#v", launches)
	}
}

func TestSubscriptionPoolRotatesByRelaunchingAndContinues(t *testing.T) {
	t.Parallel()

	dbPath := seedSubscriptionAccounts(t, []subscriptionAccountFixture{
		{name: "personal", token: "personal-oauth-token"},
		{name: "work", token: "work-oauth-token"},
	})
	secrets := &accountTestSecrets{values: map[string]string{
		secret.ClaudeAccountAccessTokenRef("personal"): "personal-oauth-token",
		secret.ClaudeAccountAccessTokenRef("work"):     "work-oauth-token",
	}}
	launcher := &recordingLauncher{}
	var gatewayStarts int
	startGateway := func(ctx context.Context, config gateway.Config) (*gateway.Server, error) {
		server, err := gateway.Start(ctx, config)
		if err != nil {
			return nil, err
		}
		gatewayStarts++
		if gatewayStarts == 1 {
			config.AnthropicSubscriptionExhaustion <- gateway.AnthropicSubscriptionExhaustionEvent{
				StatusCode: 429, RetryAfterDuration: time.Hour,
			}
		}
		return server, nil
	}

	out, errOut, err := runCommandWithDeps(t, Dependencies{
		Secrets: secrets, Launcher: launcher, StartGateway: startGateway,
	}, "--db", dbPath, "launch", "--auth-mode", "subscription-pool", "--no-lifecycle", "--no-statusline")
	if err != nil {
		t.Fatalf("subscription-pool rotation error = %v, stderr=%q", err, errOut)
	}
	if launcher.StartCount() != 2 {
		t.Fatalf("Claude starts = %d, want 2", launcher.StartCount())
	}
	first, second := launcher.StartAt(0), launcher.StartAt(1)
	if first.oauthToken != "personal-oauth-token" || second.oauthToken != "work-oauth-token" {
		t.Fatalf("selected token sequence was incorrect")
	}
	if !containsString(second.args, "--continue") {
		t.Fatalf("second Claude args = %v, want --continue", second.args)
	}
	if !strings.Contains(errOut, "relaunching with the next available account") {
		t.Fatalf("rotation was not visible: %q", errOut)
	}
	if strings.Contains(out+errOut, "personal-oauth-token") ||
		strings.Contains(out+errOut, "work-oauth-token") {
		t.Fatal("rotation output leaked an OAuth token")
	}
	launches := loadSubscriptionLaunches(t, dbPath)
	if len(launches) != 2 ||
		launches[0].ClaudeAccountName != "work" ||
		launches[1].ClaudeAccountName != "personal" ||
		launches[1].EndReason != "subscription_exhausted" {
		t.Fatalf("rotation launch metadata = %#v", launches)
	}
}

func TestSubscriptionPoolRotatesResumedWorktreeWithOriginalArguments(t *testing.T) {
	t.Parallel()

	dbPath := seedSubscriptionAccounts(t, []subscriptionAccountFixture{
		{name: "personal", token: "personal-oauth-token"},
		{name: "work", token: "work-oauth-token"},
	})
	secrets := &accountTestSecrets{values: map[string]string{
		secret.ClaudeAccountAccessTokenRef("personal"): "personal-oauth-token",
		secret.ClaudeAccountAccessTokenRef("work"):     "work-oauth-token",
	}}
	launcher := &recordingLauncher{}
	var gatewayStarts int
	startGateway := func(ctx context.Context, config gateway.Config) (*gateway.Server, error) {
		server, err := gateway.Start(ctx, config)
		if err != nil {
			return nil, err
		}
		gatewayStarts++
		if gatewayStarts == 1 {
			config.AnthropicSubscriptionExhaustion <- gateway.AnthropicSubscriptionExhaustionEvent{
				StatusCode: 429, RetryAfterDuration: time.Hour,
			}
		}
		return server, nil
	}
	resumeArgs := []string{
		"--worktree", "mcp-stateless-ha",
		"--resume", "fa605631-8a49-4afa-9888-ea4f7f26f26b",
	}
	commandArgs := append([]string{
		"--db", dbPath, "launch", "--auth-mode", "subscription-pool",
	}, resumeArgs...)
	commandArgs = append(commandArgs, "--no-lifecycle", "--no-statusline")

	_, errOut, err := runCommandWithDeps(t, Dependencies{
		Secrets: secrets, Launcher: launcher, StartGateway: startGateway,
	}, commandArgs...)
	if err != nil {
		t.Fatalf("resumed worktree rotation error = %v, stderr=%q", err, errOut)
	}
	if launcher.StartCount() != 2 {
		t.Fatalf("Claude starts = %d, want 2", launcher.StartCount())
	}
	for index := 0; index < 2; index++ {
		start := launcher.StartAt(index)
		for argIndex := 0; argIndex < len(resumeArgs); argIndex += 2 {
			if !containsArgPair(start.args, resumeArgs[argIndex], resumeArgs[argIndex+1]) {
				t.Fatalf("Claude start %d args = %v, missing %s %s",
					index, start.args, resumeArgs[argIndex], resumeArgs[argIndex+1])
			}
		}
		if containsString(start.args, "--continue") {
			t.Fatalf("Claude start %d args = %v, resume must not be replaced by --continue", index, start.args)
		}
	}
	for _, want := range []string{
		"Automatic account rotation: enabled",
		"relaunching with the next available account",
	} {
		if !strings.Contains(errOut, want) {
			t.Fatalf("resumed worktree rotation output missing %q: %s", want, errOut)
		}
	}
}

func TestSubscriptionPoolExplicitAccountDoesNotRotate(t *testing.T) {
	t.Parallel()

	invocation := launchInvocation{
		authMode: launchAuthModeSubscriptionPool, claudeAccount: "personal",
	}
	if subscriptionPoolCanRelaunch(invocation) {
		t.Fatal("an explicitly selected account must not rotate automatically")
	}
	invocation.claudeAccount = ""
	invocation.printMode = true
	if subscriptionPoolCanRelaunch(invocation) {
		t.Fatal("print mode must not rotate automatically")
	}
	invocation.printMode = false
	invocation.claudeArgs = []string{"prompt"}
	if subscriptionPoolCanRelaunch(invocation) {
		t.Fatal("launches with passthrough arguments must not rotate automatically")
	}
	invocation.claudeArgs = []string{
		"--worktree", "mcp-stateless-ha",
		"--resume", "fa605631-8a49-4afa-9888-ea4f7f26f26b",
	}
	plan := planSubscriptionPoolRelaunch(invocation)
	if !plan.enabled || strings.Join(plan.args, " ") != strings.Join(invocation.claudeArgs, " ") {
		t.Fatalf("resumable worktree relaunch plan = %#v", plan)
	}
	invocation.claudeArgs = []string{"--worktree=mcp-stateless-ha"}
	plan = planSubscriptionPoolRelaunch(invocation)
	if !plan.enabled || !containsString(plan.args, "--continue") {
		t.Fatalf("worktree relaunch plan = %#v, want appended --continue", plan)
	}
	for _, args := range [][]string{
		{"--resume"},
		{"--resume", ""},
		{"--resume", "  "},
		{"--worktree"},
		{"--worktree", ""},
		{"--worktree", "  "},
		{"--resume", "session-id", "--chrome"},
		{"--resume", "session-id", "--continue"},
		{"--worktree", "one", "--worktree", "two"},
	} {
		invocation.claudeArgs = args
		if subscriptionPoolCanRelaunch(invocation) {
			t.Fatalf("unsafe Claude args %v must not rotate automatically", args)
		}
	}
	invocation.claudeArgs = nil
	invocation.cuaModeSet = true
	if subscriptionPoolCanRelaunch(invocation) {
		t.Fatal("managed CUA launches must not rotate automatically")
	}
}

func containsArgPair(args []string, option, value string) bool {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == option && args[index+1] == value {
			return true
		}
	}
	return false
}

func TestSubscriptionPoolRotationNoticeExplainsEligibility(t *testing.T) {
	t.Parallel()

	var output strings.Builder
	writeSubscriptionRotationNotice(&output, planSubscriptionPoolRelaunch(launchInvocation{}))
	if !strings.Contains(output.String(), "Automatic account rotation: enabled") {
		t.Fatalf("enabled rotation notice = %q", output.String())
	}

	output.Reset()
	writeSubscriptionRotationNotice(&output, planSubscriptionPoolRelaunch(launchInvocation{printMode: true}))
	if !strings.Contains(output.String(), "disabled because --print is a one-shot") {
		t.Fatalf("disabled rotation notice = %q", output.String())
	}
}

func TestSubscriptionPoolPreflightDoesNotClaimAccount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "invalid model",
			args: []string{"launch", "--auth-mode", "subscription-pool", "--model", "missing"},
			want: "missing",
		},
		{
			name: "settings override",
			args: []string{"launch", "--auth-mode", "subscription-pool", "--settings", "{}"},
			want: "--settings cannot override",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			dbPath := seedSubscriptionAccounts(t, []subscriptionAccountFixture{
				{name: "preflight", token: "preflight-oauth-token"},
			})
			secrets := &accountTestSecrets{values: map[string]string{
				secret.ClaudeAccountAccessTokenRef("preflight"): "preflight-oauth-token",
			}}
			launcher := &fakeLauncher{pid: 4321}
			args := append([]string{"--db", dbPath}, test.args...)
			_, _, err := runCommandWithDeps(t, Dependencies{
				Secrets: secrets, Launcher: launcher,
			}, args...)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("subscription-pool preflight error = %v, want containing %q", err, test.want)
			}
			account := getAccountForCLI(t, dbPath, "preflight")
			if account.LastUsedAt != "" {
				t.Fatalf("rejected launch stamped account usage at %s", account.LastUsedAt)
			}
			if launcher.starts != 0 {
				t.Fatalf("rejected launch started Claude %d time(s)", launcher.starts)
			}
			if launches := loadSubscriptionLaunches(t, dbPath); len(launches) != 0 {
				t.Fatalf("rejected launch persisted launch records: %#v", launches)
			}
		})
	}
}

func TestSubscriptionPoolDoesNotRotateAfterCleanupFailure(t *testing.T) {
	t.Parallel()

	dbPath := seedSubscriptionAccounts(t, []subscriptionAccountFixture{
		{name: "personal", token: "personal-oauth-token"},
		{name: "work", token: "work-oauth-token"},
	})
	secrets := &accountTestSecrets{values: map[string]string{
		secret.ClaudeAccountAccessTokenRef("personal"): "personal-oauth-token",
		secret.ClaudeAccountAccessTokenRef("work"):     "work-oauth-token",
	}}
	launcher := &failingStopLauncher{}
	startGateway := func(ctx context.Context, config gateway.Config) (*gateway.Server, error) {
		server, err := gateway.Start(ctx, config)
		if err == nil {
			config.AnthropicSubscriptionExhaustion <- gateway.AnthropicSubscriptionExhaustionEvent{StatusCode: 429}
		}
		return server, err
	}
	_, _, err := runCommandWithDeps(t, Dependencies{
		Secrets: secrets, Launcher: launcher, StartGateway: startGateway,
	}, "--db", dbPath, "launch", "--auth-mode", "subscription-pool", "--no-lifecycle", "--no-statusline")
	if err == nil || !strings.Contains(err.Error(), "stopping test process") {
		t.Fatalf("subscription-pool cleanup error = %v", err)
	}
	if launcher.starts != 1 {
		t.Fatalf("Claude starts = %d, want 1 after cleanup failure", launcher.starts)
	}
}

func TestStopClaudeProcessAndWaitIsBounded(t *testing.T) {
	process := &blockingClaudeProcess{done: make(chan error)}
	started := time.Now()
	err := stopClaudeProcessAndWait(process, process.Done(), 10*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("stopClaudeProcessAndWait() error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("stopClaudeProcessAndWait() elapsed = %s, want bounded wait", elapsed)
	}
}

func TestSubscriptionCooldownClassifiesConfirmedAndTransientLimits(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	confirmed := func(event gateway.AnthropicSubscriptionExhaustionEvent) gateway.AnthropicSubscriptionExhaustionEvent {
		event.RepresentativeClaim = gateway.AnthropicRateLimitClaimFiveHour
		return event
	}
	tests := []struct {
		name  string
		event gateway.AnthropicSubscriptionExhaustionEvent
		want  time.Duration
	}{
		{name: "unclassified default", want: defaultTransientRateLimitCooldown},
		{
			name:  "unclassified retry after",
			event: gateway.AnthropicSubscriptionExhaustionEvent{RetryAfterDuration: 30 * time.Second},
			want:  30 * time.Second,
		},
		{
			name:  "unclassified cap",
			event: gateway.AnthropicSubscriptionExhaustionEvent{RetryAfterTime: now.Add(time.Hour)},
			want:  maxTransientRateLimitCooldown,
		},
		{
			name: "model fallback uses transient retry after",
			event: gateway.AnthropicSubscriptionExhaustionEvent{
				RepresentativeClaim: gateway.AnthropicRateLimitClaimSevenDayOpus,
				FallbackAvailable:   true,
				RetryAfterDuration:  time.Minute,
				RetryAfterTime:      now.Add(7 * 24 * time.Hour),
			},
			want: time.Minute,
		},
		{name: "confirmed default", event: confirmed(gateway.AnthropicSubscriptionExhaustionEvent{}), want: defaultSubscriptionCooldown},
		{
			name:  "confirmed duration",
			event: confirmed(gateway.AnthropicSubscriptionExhaustionEvent{RetryAfterDuration: time.Hour}),
			want:  time.Hour,
		},
		{
			name:  "confirmed date",
			event: confirmed(gateway.AnthropicSubscriptionExhaustionEvent{RetryAfterTime: now.Add(2 * time.Hour)}),
			want:  2 * time.Hour,
		},
		{
			name: "confirmed unified reset before retry after",
			event: confirmed(gateway.AnthropicSubscriptionExhaustionEvent{
				RetryAfterDuration: time.Hour,
				RetryAfterTime:     now.Add(2 * time.Hour),
			}),
			want: 2 * time.Hour,
		},
		{
			name:  "confirmed cap",
			event: confirmed(gateway.AnthropicSubscriptionExhaustionEvent{RetryAfterDuration: 72 * time.Hour}),
			want:  maxSubscriptionCooldown,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := subscriptionCooldownUntil(now, test.event)
			if got.Sub(now) != test.want {
				t.Fatalf("cooldown = %s, want %s", got.Sub(now), test.want)
			}
		})
	}
}

func TestSubscriptionRateLimitClassification(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		event       gateway.AnthropicSubscriptionExhaustionEvent
		confirmed   bool
		failure     string
		description string
	}{
		{
			name: "unclassified", failure: "transient_rate_limit",
			description: "received an unclassified rate limit",
		},
		{
			name:    "fallback without claim",
			event:   gateway.AnthropicSubscriptionExhaustionEvent{FallbackAvailable: true},
			failure: "model_rate_limit", description: "hit a model limit",
		},
		{
			name: "model limit",
			event: gateway.AnthropicSubscriptionExhaustionEvent{
				RepresentativeClaim: gateway.AnthropicRateLimitClaimSevenDayOpus, FallbackAvailable: true,
			},
			failure: "model_limit_seven_day_opus", description: "hit model limit seven_day_opus",
		},
		{
			name:      "account limit",
			event:     gateway.AnthropicSubscriptionExhaustionEvent{RepresentativeClaim: gateway.AnthropicRateLimitClaimFiveHour},
			confirmed: true, failure: "subscription_limit_five_hour",
			description: "reached subscription limit five_hour",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := confirmedAccountSubscriptionLimit(test.event); got != test.confirmed {
				t.Fatalf("confirmedAccountSubscriptionLimit() = %t, want %t", got, test.confirmed)
			}
			if got := subscriptionRateLimitFailureClass(test.event); got != test.failure {
				t.Fatalf("subscriptionRateLimitFailureClass() = %q, want %q", got, test.failure)
			}
			if got := subscriptionRateLimitDescription(test.event); !strings.Contains(got, test.description) {
				t.Fatalf("subscriptionRateLimitDescription() = %q, want substring %q", got, test.description)
			}
		})
	}
}

type subscriptionAccountFixture struct {
	name  string
	token string
}

func seedSubscriptionAccounts(t *testing.T, fixtures []subscriptionAccountFixture) string {
	t.Helper()
	dbPath := t.TempDir() + "/ccr.db"
	ctx := context.Background()
	s, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open error = %v", err)
	}
	defer closeStore(s)
	if migrateErr := s.Migrate(ctx); migrateErr != nil {
		t.Fatalf("store.Migrate error = %v", migrateErr)
	}
	for _, fixture := range fixtures {
		if _, err := s.AddClaudeAccount(ctx, store.ClaudeAccount{
			Name: fixture.name, AccessTokenRef: secret.ClaudeAccountAccessTokenRef(fixture.name),
			ScopesJSON: "[]", Enabled: true,
		}); err != nil {
			t.Fatalf("AddClaudeAccount(%s) error = %v", fixture.name, err)
		}
	}
	return dbPath
}

func loadSubscriptionLaunches(t *testing.T, dbPath string) []store.Launch {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open error = %v", err)
	}
	defer closeStore(s)
	if migrateErr := s.Migrate(ctx); migrateErr != nil {
		t.Fatalf("store.Migrate error = %v", migrateErr)
	}
	launches, err := s.ListLaunches(ctx)
	if err != nil {
		t.Fatalf("ListLaunches error = %v", err)
	}
	return launches
}

type recordedLaunch struct {
	args       []string
	oauthToken string
}

type recordingLauncher struct {
	mu     sync.Mutex
	starts []recordedLaunch
}

func (l *recordingLauncher) Start(
	ctx context.Context,
	args []string,
	env ClaudeEnvironment,
	_ io.Reader,
	_, _ io.Writer,
) (ClaudeProcess, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	token := environmentEntries(env.Set)["CLAUDE_CODE_OAUTH_TOKEN"]
	l.mu.Lock()
	l.starts = append(l.starts, recordedLaunch{args: append([]string(nil), args...), oauthToken: token})
	index := len(l.starts)
	l.mu.Unlock()
	return &fakeProcess{pid: 5000 + index}, nil
}

func (l *recordingLauncher) StartCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.starts)
}

func (l *recordingLauncher) StartAt(index int) recordedLaunch {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.starts[index]
}

type failingStopLauncher struct {
	starts int
}

func (l *failingStopLauncher) Start(
	_ context.Context,
	_ []string,
	_ ClaudeEnvironment,
	_ io.Reader,
	_, _ io.Writer,
) (ClaudeProcess, error) {
	l.starts++
	done := make(chan error, 1)
	return &blockingClaudeProcess{
		done: done,
		stop: func() error {
			done <- nil
			close(done)
			return errors.New("stopping test process")
		},
	}, nil
}

type blockingClaudeProcess struct {
	done chan error
	stop func() error
}

func (p *blockingClaudeProcess) PID() int {
	return 9000
}

func (p *blockingClaudeProcess) Done() <-chan error {
	return p.done
}

func (p *blockingClaudeProcess) Stop() error {
	if p.stop != nil {
		return p.stop()
	}
	return nil
}
