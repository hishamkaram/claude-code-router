//go:build live

package cli

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/liveclaude"
)

func TestLiveFixtureModelPickerStartupAliasAppearsOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Fatalf("required native Claude unavailable: %v", err)
	}
	executable := buildLiveCCRExecutable(t, ctx)
	project, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve synthetic picker project: %v", err)
	}
	t.Chdir(project)
	configureIsolatedLivePickerClaude(t)
	_, provider := newLivePickerProvider(t)
	t.Cleanup(provider.Close)
	database := filepath.Join(t.TempDir(), "ccr.db")
	addLivePickerModels(t, ctx, database, provider.URL)
	run := startLivePickerRun(t, ctx, database, executable, "--model", "gpt")
	defer run.close()
	run.waitForText(t, ctx, "Detected a custom API key")
	run.write(t, "\x1b[A\r", "accepting isolated placeholder API key")
	run.waitForText(t, ctx, "Claude Code v")
	run.submit(t, ctx, "/model", "opening startup-model picker")
	required := []string{"Opus", "Sonnet", "Haiku", "anthropic.ccr.s%6fnnet", "anthropic.ccr.%6fpus", "anthropic.ccr.h%61iku"}
	ready := func(view livePickerSnapshot) bool {
		if !strings.Contains(normalizeLiveClaudeText(view.screen), "Selectmodel") {
			return false
		}
		options := livePickerVisibleOptions(view.screen)
		for _, want := range required {
			if !livePickerOptionsContain(options, want) {
				return false
			}
		}
		return livePickerStartupRows(options) > 0
	}
	if err := waitForPickerState(ctx, run.transcript.Snapshot, ready); err != nil {
		t.Fatalf("startup-model picker did not render visible options: %v", err)
	}
	options := livePickerVisibleOptions(run.transcript.Snapshot().screen)
	startupRows := livePickerStartupRows(options)
	if startupRows != 1 {
		t.Fatalf("visible startup-model rows=%d, want=1", startupRows)
	}
	for _, want := range required {
		if !livePickerOptionsContain(options, want) {
			t.Fatalf("visible native picker omitted required synthetic/allowed option %q", want)
		}
	}
	run.write(t, "\x1b", "closing startup-model picker")
	run.waitForText(t, ctx, "Kept model as")
	run.submit(t, ctx, "/exit", "exiting startup-model picker fixture")
	run.waitForExit(t, ctx)
}

func livePickerStartupRows(options []string) int {
	count := 0
	for _, option := range options {
		compact := normalizeLiveClaudeText(option)
		if strings.HasPrefix(compact, "anthropic.ccr.gpt") || strings.HasPrefix(compact, "CCRgpt") {
			count++
		}
	}
	return count
}

func livePickerVisibleOptions(screen string) []string {
	var options []string
	for line := range strings.SplitSeq(screen, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "❯"))
		number, option, ok := strings.Cut(line, ".")
		if !ok {
			continue
		}
		if _, err := strconv.Atoi(number); err == nil && strings.TrimSpace(option) != "" {
			options = append(options, strings.TrimSpace(option))
		}
	}
	return options
}

func livePickerOptionsContain(options []string, want string) bool {
	for _, option := range options {
		if strings.Contains(normalizeLiveClaudeText(option), normalizeLiveClaudeText(want)) {
			return true
		}
	}
	return false
}

func TestLiveModelPickerVisibleOptionsExcludeRedrawsAndHeadings(t *testing.T) {
	terminal := newLivePickerTerminal()
	if _, err := terminal.Write([]byte("Current model: CCR gpt\r\n1. CCR gpt\r\n2. duplicate startup\x1b[3;1H2. Sonnet\x1b[K\r\n3. Default (description mentions CCR gpt)")); err != nil {
		t.Fatal(err)
	}
	options := livePickerVisibleOptions(terminal.Snapshot().screen)
	if len(options) != 3 || options[0] != "CCR gpt" || options[1] != "Sonnet" || livePickerStartupRows(options) != 1 {
		t.Fatal("visible option projection counted a heading or stale redraw")
	}
}
