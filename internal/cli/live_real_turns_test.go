//go:build live

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Only submit the next turn after Claude Code completes the current one.
// Diagnostics deliberately omit real-provider output and error bodies.
func liveRealTurn(ctx context.Context, run *liveCompactionRun, input string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("starting turn: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { _ = run.input.Close() })
	defer stop()
	offset := len(run.out.String())
	if _, err := io.WriteString(run.input, input); err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return "", fmt.Errorf("writing turn: %w", err)
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if result, failed, complete := liveRealTurnResult(run.out.String()[offset:]); complete {
			if failed {
				return "", fmt.Errorf("Claude Code returned an error result (body withheld)")
			}
			return result, nil
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("waiting for turn: %w", ctx.Err())
		case <-run.done:
			// The final result can arrive between the snapshot and process exit.
			if result, failed, complete := liveRealTurnResult(run.out.String()[offset:]); complete && !failed {
				return result, nil
			}
			return "", fmt.Errorf("Claude Code exited before a successful turn result (output withheld)")
		case <-ticker.C:
		}
	}
}

func liveRealTurnResult(output string) (result string, failed, complete bool) {
	for _, line := range strings.Split(output, "\n") {
		var event struct {
			Type    string `json:"type"`
			Result  string `json:"result"`
			IsError bool   `json:"is_error"`
		}
		if json.Unmarshal([]byte(line), &event) == nil && event.Type == "result" {
			return event.Result, event.IsError, true
		}
	}
	return "", false, false
}

func TestLiveRealTurnResult(t *testing.T) {
	for _, test := range []struct {
		name, output, result string
		failed, complete     bool
	}{
		{"partial", `{"type":"result","result":"OK"`, "", false, false},
		{"assistant is not completion", `{"type":"assistant","result":"OK"}`, "", false, false},
		{"success", "{\"type\":\"system\"}\n{\"type\":\"result\",\"result\":\"OK\"}\n", "OK", false, true},
		{"empty reply", `{"type":"result","result":""}`, "", false, true},
		{"error", `{"type":"result","result":"private error","is_error":true}`, "private error", true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, failed, complete := liveRealTurnResult(test.output)
			if result != test.result || failed != test.failed || complete != test.complete {
				t.Fatalf("result = %q, %v, %v", result, failed, complete)
			}
		})
	}
}

func TestLiveRealTurnStartupFailureClosesInput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	run := startLiveCompactionCommand(t, ctx, Dependencies{}, []string{"--ccr-invalid-test-flag"})
	if _, err := liveRealTurn(ctx, run, "test\n"); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("startup failure should close input promptly: %v", err)
	}
}

func TestLiveRealTurnCancellationUnblocksInput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	run := &liveCompactionRun{input: writer, out: &synchronizedBuffer{}}
	cancel()
	if _, err := liveRealTurn(ctx, run, "test\n"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled input error = %v", err)
	}
}

func TestLiveRealTurnChildExitDoesNotWaitForInputEOF(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-backed subprocess fixture requires Unix")
	}
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, ".claude"))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	script := "#!/bin/sh\nIFS= read -r line\nprintf 'CCR_EARLY_EXIT\\n'\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	fixture := newLiveCompactionFixture(t)
	dbPath := configureLiveCompactionModel(t, ctx, fixture.URL())
	run := startLiveCompactionRun(t, ctx, dbPath, "a9d9014c-621c-4d16-9222-4304c5d94277")
	turnCtx, turnCancel := context.WithTimeout(ctx, 5*time.Second)
	defer turnCancel()
	_, err := liveRealTurn(turnCtx, run, liveStreamInput(t, "hello"))
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("early child exit was not detected promptly: %v", err)
	}
	if !strings.Contains(run.out.String(), "CCR_EARLY_EXIT") {
		t.Fatalf("subprocess did not read its input before exiting: %v", err)
	}
}
