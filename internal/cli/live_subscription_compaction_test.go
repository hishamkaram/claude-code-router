//go:build live

package cli

import (
	"strings"
	"testing"
)

func TestLiveFixtureSubscriptionPoolCompactAfterProviderSwitch(t *testing.T) {
	for _, mode := range []string{"manual", "automatic"} {
		t.Run(mode, func(t *testing.T) {
			ctx := liveCompactionContext(t)
			fixture := newLiveCompactionFixture(t)
			subscription := newLiveSubscriptionFixture(t, []liveSubscriptionResponse{
				{account: "personal", token: liveSubscriptionPersonalToken, text: "CCR_POOL_BEFORE_SWITCH"},
			})
			t.Cleanup(subscription.Close)
			dbPath, secrets := seedLiveSubscriptionCredentials(t, []subscriptionAccountFixture{
				{name: "personal", token: liveSubscriptionPersonalToken},
			})
			addLiveCompactionModel(t, ctx, dbPath, fixture.URL())
			args := []string{
				"--db", dbPath, "launch", "--auth-mode", "subscription-pool", "--print", "--no-lifecycle",
				"--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
			}
			turns := 16
			if mode == "automatic" {
				args = append(args, "--autocompact", "100k")
				turns = 4
			}
			run := startLiveCompactionCommand(t, ctx, Dependencies{
				Secrets: secrets, StartGateway: subscription.StartGateway,
			}, args)
			run.send(t, "/model sonnet")
			run.send(t, "Reply with the configured subscription response.")
			run.waitFor(t, ctx, "subscription response", func() bool {
				return strings.Contains(run.out.String(), "CCR_POOL_BEFORE_SWITCH") && run.resultCount() >= 2
			})
			run.send(t, "/model anthropic.ccr.compact-fixture[1m]")
			run.waitForResults(t, ctx, 3)
			for _, message := range liveCompactionHistoryMessages(turns, mode) {
				run.sendAndWaitNext(t, ctx, fixture, message)
			}
			if mode == "manual" {
				run.sendAndWaitForCompaction(t, ctx, fixture, "/compact")
			} else {
				run.sendAndWaitNext(t, ctx, fixture, "Prepare automatic compaction using the retained session marker.")
				run.waitForCompletedCompaction(t, ctx, fixture, 1)
			}
			run.sendAndWaitNext(t, ctx, fixture, "Continue after compaction using the retained session marker.")
			out, errOut, err := run.finish(ctx)
			if err != nil || !strings.Contains(out, liveCompactionAfter) || strings.Contains(out, `"compact_result":"failed"`) {
				t.Fatalf("compaction after subscription-pool switch: %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
			}
			if got := subscription.CallCount(); got != 1 {
				t.Fatalf("subscription calls = %d, want only the initial turn", got)
			}
			assertLiveCompactionTraffic(t, fixture.snapshot())
		})
	}
}
