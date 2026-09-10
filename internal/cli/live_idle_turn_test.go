//go:build live

package cli

import (
	"cmp"
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/liveclaude"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestLiveConfiguredProviderIdleTurn(t *testing.T) {
	if os.Getenv("CCR_LIVE_CONFIGURED_PROVIDER") != "1" {
		t.Skip("set CCR_LIVE_CONFIGURED_PROVIDER=1 to run the real-provider idle-turn test")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Fatalf("real-provider idle-turn test requires Claude Code: %v", err)
	}
	dbPath := configuredLiveDBPath(t)
	alias := strings.TrimSpace(os.Getenv("CCR_LIVE_CONFIGURED_MODEL_ALIAS"))
	if alias == "" {
		alias = "glm-5-2"
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var launchID int64
	run := startLiveCompactionCommand(t, ctx, Dependencies{StartGateway: liveRealGatewayStarter(&launchID)}, []string{
		"--db", dbPath, "launch", "--model", alias, "--auth-mode", "provider-only", "--print",
		"--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--bare", "--setting-sources", "", "--strict-mcp-config", "--tools", "",
	})
	for index, marker := range []string{"CCR_LIVE_IDLE_FIRST", "CCR_LIVE_IDLE_SECOND"} {
		if index > 0 {
			// Enter the former 20s + 15s health-probe window on an idle pool.
			timer := time.NewTimer(34500 * time.Millisecond)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				t.Fatal(ctx.Err())
			}
		}
		result, err := liveRealTurn(ctx, run, liveStreamInput(t, "Reply with only this exact marker:\n"+marker))
		if err != nil || strings.TrimSpace(result) != marker {
			t.Fatalf("idle turn %d: result_bytes=%d error=%v", index, len(result), err)
		}
	}
	out, errOut, err := run.finish(ctx)
	if err != nil {
		failLiveRealCommand(t, "configured idle-turn launch", err, out, errOut)
	}
	assertIdleTurnRoutes(t, ctx, dbPath, launchID, alias)
}

func assertIdleTurnRoutes(t *testing.T, ctx context.Context, dbPath string, launchID int64, alias string) {
	t.Helper()
	s, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	events, err := s.ListTraceEvents(ctx, store.TraceFilter{LaunchID: launchID, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	turns := 0
	var latestEnd time.Time
	idleObserved := false
	slices.SortFunc(events, func(a, b store.TraceEvent) int { return cmp.Compare(a.ID, b.ID) })
	for _, event := range events {
		if event.Kind != "route" {
			continue
		}
		if event.Status != "succeeded" || event.Route.ModelAlias != alias || event.Route.RouteKind != "registered" {
			t.Fatalf("unexpected idle-turn route: status=%s alias=%s kind=%s", event.Status, event.Route.ModelAlias, event.Route.RouteKind)
		}
		if event.Route.Streaming {
			turns++
		}
		start, err := time.Parse(time.RFC3339Nano, event.OccurredAt)
		if err != nil {
			t.Fatal(err)
		}
		end, err := time.Parse(time.RFC3339Nano, event.CompletedAt)
		if err != nil {
			t.Fatal(err)
		}
		idleObserved = idleObserved || (!latestEnd.IsZero() && start.Sub(latestEnd) >= 30*time.Second)
		if end.After(latestEnd) {
			latestEnd = end
		}
	}
	if turns < 2 || !idleObserved {
		t.Fatalf("streamed turns=%d idle_observed=%t, want two turns separated by a verified idle interval", turns, idleObserved)
	}
}
