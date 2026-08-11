package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	if launcher.starts != 1 ||
		!launcher.hasEnvPrefix("ANTHROPIC_AUTH_TOKEN=") ||
		!launcher.unsetsEnv("CLAUDE_CODE_OAUTH_TOKEN") {
		t.Fatalf("launcher starts=%d env=%s", launcher.starts, launcher.environmentSummary())
	}
	if !launcher.hasEnv(statuslineClaudeAccountEnv + "=personal") {
		t.Fatalf("launcher did not expose selected status-line account: %s", launcher.environmentSummary())
	}
	if !launcher.unsetsEnv("ANTHROPIC_API_KEY") {
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

func TestNoUsableClaudeAccountErrorIncludesEachRepairPath(t *testing.T) {
	err := noUsableClaudeAccountError("work")
	message := err.Error()
	for _, want := range []string{
		"ccr claude-account show work",
		"ccr claude-account enable work",
		"ccr claude-account clear-cooldown work",
		"ccr claude-account refresh work --from current",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("noUsableClaudeAccountError() = %q, missing %q", message, want)
		}
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
		!strings.Contains(command, "__statusline-account") {
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
