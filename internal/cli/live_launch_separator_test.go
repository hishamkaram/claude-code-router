//go:build live

package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/liveclaude"
)

func TestLiveLaunchSeparatorOutputFormats(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1")
	t.Setenv("CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL", "1")
	t.Setenv("CLAUDE_CODE_DISABLE_AUTO_MEMORY", "1")
	fixture := newLiveMatrixFixture(t, "openai-chat")
	defer fixture.Close()
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	deps := liveMatrixDependencies(fixture)
	configureLiveMatrixModels(t, ctx, deps, dbPath, fixture.URL(), "openai-chat")
	for _, format := range []string{"json", "stream-json"} {
		for _, separator := range []bool{false, true} {
			name := format + "/direct"
			if separator {
				name = format + "/separator"
			}
			t.Run(name, func(t *testing.T) {
				args := []string{"--db", dbPath, "launch", "--model", "fixture-full", "--auth-mode", "provider-only", "-p"}
				if separator {
					args = append(args, "--")
				}
				args = append(args, "--output-format", format, "--max-turns", "1")
				launchDeps := deps
				launchDeps.In = strings.NewReader("Reply with the configured alias fixture response.\n")
				if format == "stream-json" {
					args = append(args, "--input-format", "stream-json", "--verbose")
					launchDeps.In = strings.NewReader(liveStreamInput(t, "Reply with the configured alias fixture response."))
				}
				out, errOut, err := runLiveCommand(ctx, launchDeps, args...)
				if err != nil {
					t.Fatalf("launch: %v\nstdout: %s\nstderr: %s", err, out, errOut)
				}
				assertLiveLaunchJSON(t, out, format)
			})
		}
	}
}

func assertLiveLaunchJSON(t *testing.T, out, format string) {
	t.Helper()
	lines := []string{strings.TrimSpace(out)}
	if format == "stream-json" {
		lines = strings.Split(strings.TrimSpace(out), "\n")
	}
	var resultSeen, assistantSeen bool
	for _, line := range lines {
		var event struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
			Result  string `json:"result"`
			IsError bool   `json:"is_error"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("stdout is not %s: %v\n%s", format, err, out)
		}
		if event.Type == "" {
			t.Fatal("JSON event has no type")
		}
		assistantSeen = assistantSeen || event.Type == "assistant"
		if event.Type == "result" {
			resultSeen = true
			if event.IsError || event.Subtype != "success" || !strings.Contains(event.Result, "CCR_LIVE_ALIAS") {
				t.Fatalf("unexpected result: %s", line)
			}
		}
	}
	if !resultSeen || (format == "stream-json" && !assistantSeen) {
		t.Fatalf("missing result or assistant event: %s", out)
	}
}
