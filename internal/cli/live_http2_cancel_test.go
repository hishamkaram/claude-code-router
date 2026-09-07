//go:build live

package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/store"
)

// Cancel only the Claude child recorded in this test's fresh database. The
// parent CCR remains alive to observe its exit and shut down its own gateway.
func stopHTTP2TestClaude(ctx context.Context, dbPath string) error {
	db, err := store.OpenReadOnly(ctx, dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	launches, err := db.ListLaunches(ctx)
	if err != nil {
		return err
	}
	if len(launches) != 1 || launches[0].PID <= 0 {
		return fmt.Errorf("owned Claude PID unavailable")
	}
	process, err := os.FindProcess(launches[0].PID)
	if err != nil {
		return err
	}
	defer process.Release()
	return process.Kill()
}

func waitHTTP2ActiveRoute(ctx context.Context, dbPath string) (store.Launch, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		db, err := store.OpenReadOnly(ctx, dbPath)
		if err == nil {
			launches, launchErr := db.ListLaunches(ctx)
			events, eventErr := db.ListTraceEvents(ctx, store.TraceFilter{Kind: "route", Limit: 1})
			_ = db.Close()
			if launchErr == nil && eventErr == nil && len(launches) == 1 && launches[0].PID > 0 && len(events) > 0 {
				return launches[0], nil
			}
		}
		select {
		case <-ctx.Done():
			return store.Launch{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func TestLiveHTTP2BuiltBinaryCancellation(t *testing.T) {
	binary := os.Getenv("CCR_LIVE_HTTP2_BINARY")
	if binary == "" {
		t.Skip("set CCR_LIVE_HTTP2_BINARY to the candidate CCR executable")
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	source, err := store.OpenReadOnly(context.Background(), configuredLiveDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
	defer cancel()
	type running struct {
		observer       *http2StartedWriter
		cmd            *exec.Cmd
		db             string
		stdout, stderr bytes.Buffer
		launch         store.Launch
		evidence       http2BinaryCase
	}
	sessions := make([]*running, 0, 2)
	// All started processes are waited, including early assertion failures.
	waited := make(map[*exec.Cmd]bool)
	defer func() {
		cancel()
		for _, session := range sessions {
			if !waited[session.cmd] {
				_ = session.cmd.Wait()
			}
		}
	}()
	for _, alias := range []string{"litellm-grok-4-6", "litellm-gpt-5-6-terra"} {
		session := &running{db: isolateHTTP2BinaryDB(t, source, alias), evidence: http2BinaryCase{Alias: alias, Scenario: "concurrent_cancellation", BinarySHA256: hex.EncodeToString(sum[:]), StartedAt: time.Now().UTC().Format(time.RFC3339Nano)}}
		session.evidence.ExpectedDeployment = http2ExpectedDeployment(t, source, alias)
		session.cmd = exec.CommandContext(ctx, binary, "--db", session.db, "launch", "--model", alias, "--print", "--auth-mode", "provider-only", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--include-partial-messages", "--effort", "high", "--tools", "")
		session.cmd.Dir = t.TempDir()
		session.cmd.Env = http2BinaryEnvironment(t)
		session.cmd.Stdin = strings.NewReader(http2BinaryInput(t, "reasoning"))
		session.observer = &http2StartedWriter{output: &session.stdout, started: make(chan struct{})}
		session.cmd.Stdout = session.observer
		session.cmd.Stderr = &session.stderr
		if protectErr := protectHTTP2TestProcess(session.cmd); protectErr != nil {
			t.Fatal(protectErr)
		}
		if startErr := session.cmd.Start(); startErr != nil {
			t.Fatal(startErr)
		}
		session.evidence.CCRPID = session.cmd.Process.Pid
		sessions = append(sessions, session)
	}

	for _, session := range sessions {
		session.launch, err = waitHTTP2ActiveRoute(ctx, session.db)
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, session := range sessions {
		select {
		case <-session.observer.started:
		case <-ctx.Done():
			t.Fatal("provider content did not arrive")
		}
	}
	for _, session := range sessions {
		if !session.observer.active.Load() {
			t.Fatal("sessions did not overlap with active streams")
		}
	}
	cancellationAt := time.Now()
	if err := stopHTTP2TestClaude(ctx, sessions[0].db); err != nil {
		t.Fatal(err)
	}
	for index, session := range sessions {
		runErr := session.cmd.Wait()
		waited[session.cmd] = true
		sentinel, tools, results, resultError := inspectHTTP2BinaryOutput(session.stdout.Bytes())
		session.evidence.CancellationAt = cancellationAt.UTC().Format(time.RFC3339Nano)
		session.evidence.ToolNames = tools
		session.evidence.ResultCount = results
		session.evidence.RequestIDs, session.evidence.TransportReasons, session.evidence.ClaudePIDs, session.evidence.Timings = readHTTP2BinaryTrace(t, session.db)
		if index == 0 {
			session.evidence.Succeeded = runErr != nil && len(session.evidence.RequestIDs) > 0 && strings.Contains(strings.Join(session.evidence.TransportReasons, " "), "protocol=h2; idle_ping=enabled")
		} else {
			session.evidence.Succeeded = runErr == nil && sentinel && results == 1 && !resultError && http2GenerationReceiptsMatch(session.evidence.Timings, session.evidence.ExpectedDeployment)
		}
		session.evidence.Succeeded = session.evidence.Succeeded && http2CancellationRouteMatches(session.evidence, cancellationAt, index == 0)
		address := strings.TrimPrefix(session.launch.GatewayURL, "http://")
		connection, dialErr := net.DialTimeout("tcp", address, time.Second)
		if connection != nil {
			_ = connection.Close()
			session.evidence.Succeeded = false
		}
		if dialErr == nil {
			session.evidence.Succeeded = false
		}
		writeHTTP2BinaryEvidence(t, session.evidence)
		if !session.evidence.Succeeded {
			t.Errorf("concurrent cancellation acceptance failed: alias=%s exit=%v results=%d", session.evidence.Alias, runErr, results)
		}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatal("cancellation acceptance exceeded deadline")
	}
}

func http2CancellationRouteMatches(evidence http2BinaryCase, cancellationAt time.Time, canceled bool) bool {
	status := "succeeded"
	if canceled {
		status = "canceled"
	}
	for _, route := range evidence.Timings {
		completed, err := time.Parse(time.RFC3339Nano, route.CompletedAt)
		if err == nil && completed.After(cancellationAt) && route.Status == status && http2ReceiptMatches(route.TransportReason, evidence.ExpectedDeployment) {
			return true
		}
	}
	return false
}

func TestHTTP2CancellationRequiresProtectedMatchingDeployment(t *testing.T) {
	now := time.Now()
	valid := "owner=ccr; protocol=h2; idle_ping=enabled; provider_model_id=expected; provider_fallbacks=0"
	for _, canceled := range []bool{false, true} {
		status := "succeeded"
		if canceled {
			status = "canceled"
		}
		for _, reason := range []string{valid, "", strings.ReplaceAll(valid, "h2", "http/1.1"), strings.ReplaceAll(valid, "provider_fallbacks=0", "provider_fallbacks=1"), strings.ReplaceAll(valid, "expected", "other")} {
			evidence := http2BinaryCase{ExpectedDeployment: "expected", Timings: []http2RouteTiming{{Status: status, CompletedAt: now.Add(time.Second).Format(time.RFC3339Nano), TransportReason: reason}}}
			if got := http2CancellationRouteMatches(evidence, now, canceled); got != (reason == valid) {
				t.Fatalf("canceled=%v accepted=%v receipt=%s", canceled, got, reason)
			}
			if http2CancellationRouteMatches(evidence, now.Add(2*time.Second), canceled) {
				t.Fatal("completion before cancellation accepted")
			}
		}
	}
}

// Wait for provider content, not synthetic startup events emitted while dialing.
// The active flag also prevents canceling a stream that has already completed.
type http2StartedWriter struct {
	output  *bytes.Buffer
	pending []byte
	started chan struct{}
	once    sync.Once
	active  atomic.Bool
}

func (w *http2StartedWriter) Write(p []byte) (int, error) {
	n, err := w.output.Write(p)
	if err != nil {
		return n, err
	}
	w.pending = append(w.pending, p...)
	for {
		index := bytes.IndexByte(w.pending, '\n')
		if index < 0 {
			break
		}
		var event struct {
			Type  string `json:"type"`
			Event struct {
				Type  string `json:"type"`
				Delta struct {
					Type     string `json:"type"`
					Text     string `json:"text"`
					Thinking string `json:"thinking"`
				} `json:"delta"`
			} `json:"event"`
		}
		if json.Unmarshal(w.pending[:index], &event) == nil && event.Type == "stream_event" {
			if event.Event.Type == "message_start" {
				w.active.Store(true)
			}
			if event.Event.Type == "content_block_delta" &&
				((event.Event.Delta.Type == "text_delta" && event.Event.Delta.Text != "") ||
					(event.Event.Delta.Type == "thinking_delta" && event.Event.Delta.Thinking != "")) {
				w.once.Do(func() { close(w.started) })
			}
		}
		if event.Type == "result" || (event.Type == "stream_event" && event.Event.Type == "message_stop") {
			w.active.Store(false)
		}
		w.pending = w.pending[index+1:]
	}
	return n, nil
}

func TestHTTP2CancellationWaitsForProviderContent(t *testing.T) {
	for _, delta := range []string{
		`{"type":"text_delta","text":"provider text"}`,
		`{"type":"thinking_delta","thinking":"provider reasoning"}`,
	} {
		var output bytes.Buffer
		observer := &http2StartedWriter{output: &output, started: make(chan struct{})}
		write := func(value string) {
			t.Helper()
			if _, err := observer.Write([]byte(value + "\n")); err != nil {
				t.Fatal(err)
			}
		}
		write(`{"type":"stream_event","event":{"type":"message_start"}}`)
		write(`{"type":"stream_event","event":{"type":"ping"}}`)
		write(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":""}}}`)
		select {
		case <-observer.started:
			t.Fatal("synthetic startup or empty content signaled provider readiness")
		default:
		}
		write(`{"type":"stream_event","event":{"type":"content_block_delta","delta":` + delta + `}}`)
		select {
		case <-observer.started:
		default:
			t.Fatal("provider content did not signal readiness")
		}
		if !observer.active.Load() {
			t.Fatal("provider stream is not active")
		}
		write(`{"type":"stream_event","event":{"type":"message_stop"}}`)
		if observer.active.Load() {
			t.Fatal("completed stream remained active")
		}
	}
}
