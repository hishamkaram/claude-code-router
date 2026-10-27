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
	result, _, err := liveRealTurnWithCorrelation(ctx, run, input, "")
	return result, err
}

func liveRealTurnWithCorrelation(ctx context.Context, run *liveCompactionRun, input, inputUUID string) (string, liveRealTurnMetadata, error) {
	if err := ctx.Err(); err != nil {
		return "", liveRealTurnMetadata{}, fmt.Errorf("starting turn: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { _ = run.input.Close() })
	defer stop()
	reader := &liveRealResultReader{offset: len(run.out.String())}
	if inputUUID != "" {
		if run.correlatedResults == nil {
			run.correlatedResults = &liveRealResultReader{correlated: true}
		}
		reader = run.correlatedResults
		if reader.inputUUID != "" {
			return "", reader.metadata(), fmt.Errorf("previous correlated native input has not completed")
		}
		reader.inputUUID = inputUUID
		reader.observed = liveRealTurnMetadata{}
		if _, failed, _ := reader.next(run.out.String()); failed {
			return "", reader.metadata(), liveRealResultFailure(reader.metadata())
		}
	} else if run.correlatedResults != nil {
		return "", liveRealTurnMetadata{}, fmt.Errorf("cannot switch a correlated native session to serial completion")
	}
	if _, err := io.WriteString(run.input, input); err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return "", reader.metadata(), fmt.Errorf("writing turn: %w", err)
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if result, failed, complete := reader.next(run.out.String()); complete {
			if failed {
				return "", reader.metadata(), liveRealResultFailure(reader.metadata())
			}
			return result, reader.metadata(), nil
		}
		select {
		case <-ctx.Done():
			return "", reader.metadata(), fmt.Errorf("waiting for turn: %w", ctx.Err())
		case <-run.done:
			// The final result can arrive between the snapshot and process exit.
			if result, failed, complete := reader.read(run.out.String(), true); complete {
				if failed {
					return "", reader.metadata(), liveRealResultFailure(reader.metadata())
				}
				return result, reader.metadata(), nil
			}
			return "", reader.metadata(), fmt.Errorf("Claude Code exited before a successful turn result (output withheld)")
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

// Classify untrusted selection text without returning it or the requested ID.
func liveRealSelectionSummary(selection, requestedID string) string {
	lower := strings.ToLower(selection)
	validationError := strings.Contains(lower, "invalid model") ||
		strings.Contains(lower, "unknown model") ||
		strings.Contains(lower, "model not found") ||
		strings.Contains(lower, "model is not available") ||
		strings.Contains(lower, "model is not allowed") ||
		strings.Contains(lower, "not in the list of available models")
	return fmt.Sprintf("selection_bytes=%d ack_at_start=%t ack_anywhere=%t requested_id_present=%t known_validation_error=%t native_validation_timeout=%t",
		len(selection), strings.HasPrefix(selection, "Set model to "),
		strings.Contains(selection, "Set model to "),
		requestedID != "" && strings.Contains(selection, requestedID), validationError,
		liveRealSelectionTimedOut(selection, requestedID))
}

func liveRealSelectionTimedOut(selection, requestedID string) bool {
	return requestedID != "" && selection == "Couldn't confirm model '"+requestedID+"' is available (the check timed out) · try again"
}

func TestLiveRealSelectionSummary(t *testing.T) {
	const model = "anthropic.ccr.fixture"
	for _, test := range []struct {
		name, selection, requestedID, flags string
	}{
		{"acknowledgment", "Set model to " + model, model, "ack_at_start=true ack_anywhere=true requested_id_present=true known_validation_error=false"},
		{"warning before acknowledgment", "Warning\nSet model to " + model, model, "ack_at_start=false ack_anywhere=true requested_id_present=true known_validation_error=false"},
		{"validation error", "Invalid model: " + model, model, "ack_at_start=false ack_anywhere=false requested_id_present=true known_validation_error=true"},
		{"unknown response", "Unrecognized response", model, "ack_at_start=false ack_anywhere=false requested_id_present=false known_validation_error=false"},
		{"empty response", "", model, "ack_at_start=false ack_anywhere=false requested_id_present=false known_validation_error=false"},
		{"empty requested ID", "Set model to " + model, "", "ack_at_start=true ack_anywhere=true requested_id_present=false known_validation_error=false"},
		{"native validation timeout", "Couldn't confirm model '" + model + "' is available (the check timed out) · try again", model, "ack_at_start=false ack_anywhere=false requested_id_present=true known_validation_error=false"},
		{"timeout for another model", "Couldn't confirm model 'other' is available (the check timed out) · try again", model, "ack_at_start=false ack_anywhere=false requested_id_present=false known_validation_error=false"},
		{"untrusted text around timeout", "private\nCouldn't confirm model '" + model + "' is available (the check timed out) · try again", model, "ack_at_start=false ack_anywhere=false requested_id_present=true known_validation_error=false"},
	} {
		t.Run(test.name, func(t *testing.T) {
			want := fmt.Sprintf("selection_bytes=%d %s native_validation_timeout=%t", len(test.selection), test.flags, test.name == "native validation timeout")
			if got := liveRealSelectionSummary(test.selection, test.requestedID); got != want {
				t.Fatalf("selection summary = %q, want %q", got, want)
			}
		})
	}
}

func TestLiveRealSelectionSummaryWithholdsUntrustedText(t *testing.T) {
	const secret = "ccr-dummy-selection-secret"
	const model = "anthropic.ccr.private-fixture-alias"
	selection := "Warning: " + secret + "\nInvalid model: " + model
	summary := liveRealSelectionSummary(selection, model)
	if strings.Contains(summary, secret) || strings.Contains(summary, model) || strings.Contains(summary, "Warning") {
		t.Fatal("selection summary disclosed untrusted text")
	}
	want := fmt.Sprintf("selection_bytes=%d ack_at_start=false ack_anywhere=false requested_id_present=true known_validation_error=true native_validation_timeout=false", len(selection))
	if summary != want {
		t.Fatal("selection summary did not retain the expected safe classifications")
	}
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
