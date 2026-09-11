//go:build live && (linux || darwin)

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/jobs"
)

func TestLiveDetachedClaudeJobs(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Fatal("required real Claude CLI unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	binary := buildLiveCCRExecutable(t, ctx)
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1")
	t.Setenv("CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL", "1")
	t.Setenv("CLAUDE_CODE_DISABLE_AUTO_MEMORY", "1")
	t.Run("session-conflicts", func(t *testing.T) { proveDetachedSessionValidation(t, ctx, binary) })
	t.Run("ambiguous-boundary", func(t *testing.T) { proveDetachedBoundaryValidation(t, ctx, binary) })
	for _, cancelJob := range []bool{false, true} {
		t.Run(map[bool]string{false: "completion", true: "cancellation"}[cancelJob], func(t *testing.T) { proveDetachedClaude(t, ctx, binary, cancelJob) })
	}
}

func proveDetachedBoundaryValidation(t *testing.T, ctx context.Context, binary string) {
	t.Helper()
	prompt := filepath.Join(t.TempDir(), "prompt.txt")
	if err := os.WriteFile(prompt, []byte("PONG"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, option := range []string{"--session-id", "--resume", "--no-session-persistence"} {
		command := exec.CommandContext(ctx, binary, "launch", "--detach", "-p", "--prompt-file", prompt, "--", "--system-prompt", "--", option, "fixture")
		var stderr bytes.Buffer
		command.Stderr = &stderr
		out, err := command.Output()
		if err == nil || len(out) != 0 || !strings.Contains(stderr.String(), "standalone -- is not supported") {
			t.Fatalf("boundary validation %q: err=%v stdout=%q stderr=%q", option, err, out, stderr.String())
		}
	}
}

func proveDetachedSessionValidation(t *testing.T, ctx context.Context, binary string) {
	t.Helper()
	prompt := filepath.Join(t.TempDir(), "prompt.txt")
	if err := os.WriteFile(prompt, []byte("PONG"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, arg := range []string{"-r550e8400-e29b-41d4-a716-446655440000", "-pc", "-pr550e8400-e29b-41d4-a716-446655440000"} {
		command := exec.CommandContext(ctx, binary, "launch", "--detach", "-p", "--prompt-file", prompt, "--", arg)
		var stderr bytes.Buffer
		command.Stderr = &stderr
		out, err := command.Output()
		if err == nil || len(out) != 0 || !strings.Contains(stderr.String(), "conflicts with CCR's detached session identity") {
			t.Fatalf("session conflict %q: err=%v stdout=%q stderr=%q", arg, err, out, stderr.String())
		}
	}
	s, err := jobStore()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Root); !os.IsNotExist(err) {
		t.Fatal("conflicting session arguments created job storage")
	}
}

func proveDetachedClaude(t *testing.T, ctx context.Context, binary string, cancelJob bool) {
	t.Helper()
	fixture := newLiveMatrixFixture(t, "openai-chat")
	defer fixture.Close()
	upstream, err := url.Parse(fixture.URL())
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var releaseOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			once.Do(func() { close(entered) })
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			// Cancellation never releases a provider response. The release
			// channel only unblocks teardown if a request outlives the job.
			if cancelJob {
				return
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	defer server.Close()
	defer releaseOnce.Do(func() { close(release) })
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	configureLiveMatrixModels(t, ctx, Dependencies{}, dbPath, server.URL, "openai-chat")
	dir := t.TempDir()
	prompt := filepath.Join(dir, "prompt.txt")
	if writeErr := os.WriteFile(prompt, []byte("Reply with the configured alias fixture response.\n"), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	launch := exec.CommandContext(ctx, binary, "--db", dbPath, "launch", "--model", "fixture-full", "--auth-mode", "provider-only", "--detach", "-p", "--prompt-file", prompt, "--", "--output-format", "stream-json", "--verbose", "--max-turns", "1", "--tools", "", "--strict-mcp-config")
	launch.Dir = dir
	var stderr bytes.Buffer
	launch.Stderr = &stderr
	out, err := launch.Output()
	if err != nil {
		t.Fatalf("detached launch: %v %s", err, stderr.String())
	}
	var receipt jobReceipt
	if err := json.Unmarshal(out, &receipt); err != nil {
		t.Fatalf("invalid receipt: %v %s", err, out)
	}
	if err := jobs.ValidateID(receipt.JobID); err != nil {
		t.Fatal(err)
	}
	registerJobCleanup(t, ctx, binary, receipt.JobID)
	if err := os.Remove(prompt); err != nil {
		t.Fatal(err)
	}
	assertDetachedLifecycle(t, ctx, binary, receipt, cancelJob, entered, func() { releaseOnce.Do(func() { close(release) }) })
}

func registerJobCleanup(t *testing.T, ctx context.Context, binary, id string) {
	t.Helper()
	t.Cleanup(func() {
		cleanupCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer stop()
		_ = exec.CommandContext(cleanupCtx, binary, "cancel", id, "--json").Run()
		s, storeErr := jobStore()
		if storeErr == nil {
			for cleanupCtx.Err() == nil {
				r, readErr := s.Status(id)
				if readErr != nil || r.Terminal() {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
		}
	})
}

func assertDetachedLifecycle(t *testing.T, ctx context.Context, binary string, receipt jobReceipt, cancelJob bool, entered <-chan struct{}, release func()) {
	t.Helper()
	running := readBuiltJobStatus(t, ctx, binary, receipt.JobID)
	if running.Status != "running" || running.ExitCode != nil {
		t.Fatalf("not asynchronously admitted: %+v", running)
	}
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("real Claude never reached provider")
	}
	if cancelJob {
		if data, err := exec.CommandContext(ctx, binary, "cancel", receipt.JobID, "--json").CombinedOutput(); err != nil {
			t.Fatalf("cancel: %v %s", err, data)
		}
	} else {
		release()
	}
	var final jobs.Record
	for {
		final = readBuiltJobStatus(t, ctx, binary, receipt.JobID)
		if final.Terminal() {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("job did not finish")
		case <-time.After(100 * time.Millisecond):
		}
	}
	want := "completed"
	if cancelJob {
		want = "cancelled" //nolint:misspell // Public job status contract.
	}
	backend := expectedLiveContainment(t)
	if final.Status != want || final.SessionID != receipt.SessionID || final.Containment != backend || final.Cleanup.Coverage != "partial" || len(final.Cleanup.Survivors) != 0 {
		t.Fatalf("unexpected job result: %+v", final)
	}
	if !cancelJob {
		data, err := os.ReadFile(final.Log)
		if err != nil {
			t.Fatal(err)
		}
		assertLiveLaunchJSON(t, string(data), "stream-json")
		if final.ExitCode == nil || *final.ExitCode != 0 {
			t.Fatalf("exit code: %v", final.ExitCode)
		}
	}
	if data, err := exec.CommandContext(ctx, binary, "cancel", receipt.JobID, "--json").CombinedOutput(); err != nil {
		t.Fatalf("terminal no-op cancellation: %v %s", err, data)
	}
}

func readBuiltJobStatus(t *testing.T, ctx context.Context, binary, id string) jobs.Record {
	t.Helper()
	data, err := exec.CommandContext(ctx, binary, "status", id, "--json").Output()
	if err != nil {
		t.Fatal(err)
	}
	var r jobs.Record
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func expectedLiveContainment(t *testing.T) string {
	t.Helper()
	if expected := os.Getenv("CCR_LIVE_CONTAINMENT"); expected != "" {
		if expected != "systemd-scope" && expected != "process-group" {
			t.Fatalf("invalid expected containment %q", expected)
		}
		return expected
	}
	if runtime.GOOS == "darwin" {
		return "process-group"
	}
	return "systemd-scope"
}
