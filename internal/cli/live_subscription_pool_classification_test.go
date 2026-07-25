//go:build live

package cli

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/secret"
)

func TestLiveFixtureSubscriptionPoolDoesNotRotateOnTemporary429(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fixture := newLiveSubscriptionFixture(t, []liveSubscriptionResponse{
		{
			account: "personal", token: liveSubscriptionPersonalToken,
			status: http.StatusTooManyRequests, unifiedStatus: "allowed",
		},
	})
	defer fixture.Close()
	dbPath := seedSubscriptionAccounts(t, []subscriptionAccountFixture{
		{name: "personal", token: liveSubscriptionPersonalToken},
	})
	secrets := &accountTestSecrets{values: map[string]string{
		secret.ClaudeAccountAccessTokenRef("personal"): liveSubscriptionPersonalToken,
	}}
	launcher := &liveSubscriptionHTTPLauncher{}
	out, errOut, err := runLiveCommand(ctx, Dependencies{
		Secrets: secrets, Launcher: launcher, StartGateway: fixture.StartGateway,
	}, "--db", dbPath, "launch", "--auth-mode", "subscription-pool", "--no-lifecycle", "--no-statusline")
	if err != nil {
		t.Fatalf("temporary 429 launch error = %s\nstdout:\n%s\nstderr:\n%s",
			redactLiveSubscriptionOutput(err.Error()),
			redactLiveSubscriptionOutput(out),
			redactLiveSubscriptionOutput(errOut))
	}
	if launcher.StartCount() != 1 {
		t.Fatalf("Claude process starts = %d, want 1", launcher.StartCount())
	}
	fixture.AssertCalls(t, []string{"personal"})
	account := getAccountForCLI(t, dbPath, "personal")
	if account.CooldownUntil != "" || account.LastError != "" {
		t.Fatalf("temporary 429 cooled account unexpectedly: %#v", account)
	}
	assertSubscriptionLaunchMetadata(t, dbPath, []subscriptionLaunchWant{
		{account: "personal", state: "completed"},
	})
	assertNoSubscriptionTokenLeak(t, out+errOut, liveSubscriptionPersonalToken)
}

func TestLiveFixtureSubscriptionPoolModelLimitUsesTransientCooldown(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fixture := newLiveSubscriptionFixture(t, []liveSubscriptionResponse{
		{
			account: "personal", token: liveSubscriptionPersonalToken,
			status: http.StatusTooManyRequests, unifiedStatus: "rejected",
			representativeClaim: "seven_day_opus", fallbackAvailable: true, retryAfterSeconds: 120,
		},
		{
			account: "work", token: liveSubscriptionWorkToken,
			text: "CCR_LIVE_MODEL_LIMIT_ROTATION_OK",
		},
	})
	defer fixture.Close()
	dbPath := seedSubscriptionAccounts(t, []subscriptionAccountFixture{
		{name: "personal", token: liveSubscriptionPersonalToken},
		{name: "work", token: liveSubscriptionWorkToken},
	})
	secrets := &accountTestSecrets{values: map[string]string{
		secret.ClaudeAccountAccessTokenRef("personal"): liveSubscriptionPersonalToken,
		secret.ClaudeAccountAccessTokenRef("work"):     liveSubscriptionWorkToken,
	}}

	out, errOut, err := runLiveCommand(ctx, Dependencies{
		Secrets: secrets, Launcher: &liveSubscriptionHTTPLauncher{}, StartGateway: fixture.StartGateway,
	}, "--db", dbPath, "launch", "--auth-mode", "subscription-pool", "--no-lifecycle", "--no-statusline")
	if err != nil {
		t.Fatalf("model-limit rotation error = %v, stdout=%q stderr=%q", err, out, errOut)
	}
	if !strings.Contains(errOut, "model limit seven_day_opus") ||
		!strings.Contains(errOut, "fallback available") {
		t.Fatalf("model-limit classification is not visible: %q", errOut)
	}

	account := getAccountForCLI(t, dbPath, "personal")
	if account.LastError != "model_limit_seven_day_opus" {
		t.Fatalf("personal last_error = %q", account.LastError)
	}
	cooldown, parseErr := time.Parse(time.RFC3339, account.CooldownUntil)
	if parseErr != nil {
		t.Fatalf("parsing personal cooldown: %v", parseErr)
	}
	remaining := time.Until(cooldown)
	if remaining <= 0 || remaining > maxTransientRateLimitCooldown {
		t.Fatalf("personal transient cooldown remaining = %s", remaining)
	}
	fixture.AssertCalls(t, []string{"personal", "work"})
}

func TestLiveFixtureSubscriptionPoolUnclassifiedLimitsUseTransientCooldown(t *testing.T) {
	tests := []struct {
		name              string
		fallbackAvailable bool
		wantFailure       string
		wantDescription   string
	}{
		{
			name: "without fallback", wantFailure: "transient_rate_limit",
			wantDescription: "received an unclassified rate limit",
		},
		{
			name: "with fallback", fallbackAvailable: true, wantFailure: "model_rate_limit",
			wantDescription: "hit a model limit",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			assertLiveUnclassifiedPoolRotation(t, test.fallbackAvailable, test.wantFailure, test.wantDescription)
		})
	}
}

func assertLiveUnclassifiedPoolRotation(
	t *testing.T,
	fallbackAvailable bool,
	wantFailure string,
	wantDescription string,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fixture := newLiveSubscriptionFixture(t, []liveSubscriptionResponse{
		{
			account: "personal", token: liveSubscriptionPersonalToken,
			status: http.StatusTooManyRequests, unifiedStatus: "rejected",
			fallbackAvailable: fallbackAvailable, retryAfterSeconds: 120,
		},
		{account: "work", token: liveSubscriptionWorkToken, text: "CCR_LIVE_UNCLASSIFIED_ROTATION_OK"},
	})
	defer fixture.Close()
	dbPath := seedSubscriptionAccounts(t, []subscriptionAccountFixture{
		{name: "personal", token: liveSubscriptionPersonalToken},
		{name: "work", token: liveSubscriptionWorkToken},
	})
	secrets := &accountTestSecrets{values: map[string]string{
		secret.ClaudeAccountAccessTokenRef("personal"): liveSubscriptionPersonalToken,
		secret.ClaudeAccountAccessTokenRef("work"):     liveSubscriptionWorkToken,
	}}
	launcher := &liveSubscriptionHTTPLauncher{}
	out, errOut, err := runLiveCommand(ctx, Dependencies{
		Secrets: secrets, Launcher: launcher, StartGateway: fixture.StartGateway,
	}, "--db", dbPath, "launch", "--auth-mode", "subscription-pool", "--no-lifecycle", "--no-statusline")
	if err != nil {
		t.Fatalf("unclassified rotation error = %v, stdout=%q stderr=%q", err, out, errOut)
	}
	if launcher.StartCount() != 2 || !strings.Contains(errOut, wantDescription) {
		t.Fatalf("unclassified rotation starts=%d stderr=%q", launcher.StartCount(), errOut)
	}
	assertLiveTransientAccountFailure(t, dbPath, "personal", wantFailure)
	fixture.AssertCalls(t, []string{"personal", "work"})
	assertNoSubscriptionTokenLeak(t, out+errOut, liveSubscriptionPersonalToken, liveSubscriptionWorkToken)
}

func assertLiveTransientAccountFailure(t *testing.T, dbPath, name, wantFailure string) {
	t.Helper()
	account := getAccountForCLI(t, dbPath, name)
	if account.LastError != wantFailure {
		t.Fatalf("%s last_error = %q, want %q", name, account.LastError, wantFailure)
	}
	cooldown, parseErr := time.Parse(time.RFC3339, account.CooldownUntil)
	if parseErr != nil {
		t.Fatalf("parsing %s cooldown: %v", name, parseErr)
	}
	if remaining := time.Until(cooldown); remaining <= 0 || remaining > maxTransientRateLimitCooldown {
		t.Fatalf("%s transient cooldown remaining = %s", name, remaining)
	}
}
