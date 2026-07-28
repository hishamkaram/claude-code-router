//go:build live

package cli

import (
	"context"
	"net/http"
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

func TestLiveFixtureSubscriptionPoolDoesNotRotateModelOrUnknownLimits(t *testing.T) {
	tests := []struct {
		name              string
		claim             string
		fallbackAvailable bool
	}{
		{
			name: "model fallback", claim: "seven_day_opus", fallbackAvailable: true,
		},
		{name: "unknown claim"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			assertLivePoolLimitIsNotAccountWide(t, test.claim, test.fallbackAvailable)
		})
	}
}

func assertLivePoolLimitIsNotAccountWide(
	t *testing.T,
	claim string,
	fallbackAvailable bool,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fixture := newLiveSubscriptionFixture(t, []liveSubscriptionResponse{
		{
			account: "personal", token: liveSubscriptionPersonalToken,
			status: http.StatusTooManyRequests, unifiedStatus: "rejected",
			representativeClaim: claim, fallbackAvailable: fallbackAvailable,
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
	launcher := &liveSubscriptionHTTPLauncher{}
	out, errOut, err := runLiveCommand(ctx, Dependencies{
		Secrets: secrets, Launcher: launcher, StartGateway: fixture.StartGateway,
	}, "--db", dbPath, "launch", "--auth-mode", "subscription-pool", "--no-lifecycle", "--no-statusline")
	if err != nil {
		t.Fatalf("non-account limit error = %v, stdout=%q stderr=%q", err, out, errOut)
	}
	if launcher.StartCount() != 1 {
		t.Fatalf("non-account limit starts=%d, want one", launcher.StartCount())
	}
	account := getAccountForCLI(t, dbPath, "personal")
	if account.LastError != "" || account.CooldownUntil != "" {
		t.Fatalf("non-account limit cooled personal account: %#v", account)
	}
	if work := getAccountForCLI(t, dbPath, "work"); work.LastUsedAt != "" {
		t.Fatalf("non-account limit selected work account: %#v", work)
	}
	fixture.AssertCalls(t, []string{"personal"})
	assertNoSubscriptionTokenLeak(t, out+errOut, liveSubscriptionPersonalToken, liveSubscriptionWorkToken)
}
