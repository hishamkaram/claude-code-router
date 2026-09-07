//go:build live

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/liveclaude"
)

const (
	liveCompactionMarker       = "CCR_COMPACT_MARKER"
	liveCompactionBefore       = "CCR_COMPACT_BEFORE"
	liveCompactionAfter        = "CCR_COMPACT_AFTER"
	liveCompactionPromptMarker = "Your task is to create a detailed summary"
)

func TestLiveClaudeManualCompactThroughOpenAIProvider(t *testing.T) {
	ctx := liveCompactionContext(t)
	fixture := newLiveCompactionFixture(t)
	dbPath := configureLiveCompactionModel(t, ctx, fixture.URL())
	run := startLiveCompactionRun(t, ctx, dbPath, "4ad59989-45ea-4e90-a133-78f15b1ce47f")
	for _, message := range liveCompactionHistoryMessages(16, "manual-compaction") {
		run.sendAndWaitNext(t, ctx, fixture, message)
	}
	run.sendAndWaitForCompaction(t, ctx, fixture, "/compact")
	run.sendAndWaitNext(t, ctx, fixture, "Reply with the configured response after compacting.")
	out, errOut, err := run.finish(ctx)
	if err != nil {
		t.Fatalf("manual compact launch error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if strings.Contains(out, `"compact_result":"failed"`) {
		t.Fatalf("manual compact failed\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	if !strings.Contains(out, liveCompactionAfter) {
		t.Fatalf("output missing post-compact response\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	assertLiveCompactionTraffic(t, fixture.snapshot())
}

func TestLiveClaudeAutoCompactThroughOpenAIProvider(t *testing.T) {
	ctx := liveCompactionContext(t)
	fixture := newLiveCompactionFixture(t)
	dbPath := configureLiveCompactionModel(t, ctx, fixture.URL())
	run := startLiveCompactionRun(t, ctx, dbPath, "8b814572-96c7-45bf-a9fb-da2345fc4d25", "--autocompact", "100k")
	for _, message := range liveCompactionHistoryMessages(4, "automatic-compaction") {
		run.sendAndWaitNext(t, ctx, fixture, message)
	}
	run.sendAndWaitNext(t, ctx, fixture, "Prepare automatic compaction using the retained session marker.")
	run.waitForCompletedCompaction(t, ctx, fixture, 1)
	run.sendAndWaitNext(t, ctx, fixture, "Continue after automatic compaction using the retained session marker.")
	out, errOut, err := run.finish(ctx)
	if err != nil {
		t.Fatalf("automatic compaction launch error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if strings.Contains(out, "automatic compaction failed") || strings.Contains(out, `"compact_result":"failed"`) {
		t.Fatalf("automatic compaction failed\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	if fixture.compactionCount() == 0 {
		t.Fatalf("Claude Code did not issue an automatic compaction request\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	if !strings.Contains(out, liveCompactionAfter) {
		t.Fatalf("auto-compact continuation missing configured response\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	assertLiveCompactionTraffic(t, fixture.snapshot())
}

func liveCompactionContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}
	isolatedHome := t.TempDir()
	t.Setenv("HOME", isolatedHome)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(isolatedHome, ".claude"))
	t.Setenv("ANTHROPIC_API_KEY", liveFixtureAPIKey)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1")
	t.Setenv("CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL", "1")
	t.Setenv("CLAUDE_CODE_DISABLE_AUTO_MEMORY", "1")
	return ctx
}

func configureLiveCompactionModel(t *testing.T, ctx context.Context, baseURL string) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	addLiveCompactionModel(t, ctx, dbPath, baseURL)
	return dbPath
}

func addLiveCompactionModel(t *testing.T, ctx context.Context, dbPath, baseURL string) {
	t.Helper()
	for _, args := range [][]string{
		{"--db", dbPath, "provider", "add", "compact-fixture", "--type", "litellm", "--base-url", baseURL, "--no-api-key", "--mode", "full"},
		{"--db", dbPath, "model", "add", "compact-fixture", "--provider", "compact-fixture", "--model", "fixture-compact-model", "--compat", "full"},
		{"--db", dbPath, "model", "update", "compact-fixture", "--context-window", "1000000"},
		{"--db", dbPath, "model", "update", "compact-fixture", "--prompt-caching", "false"},
	} {
		if out, errOut, err := runLiveCommand(ctx, Dependencies{}, args...); err != nil {
			t.Fatalf("run %v error = %v\nstdout:\n%s\nstderr:\n%s", args, err, out, errOut)
		}
	}
}

type liveCompactionRun struct {
	input  *os.File
	out    *synchronizedBuffer
	errOut *synchronizedBuffer
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

func startLiveCompactionRun(t *testing.T, ctx context.Context, dbPath, sessionID string, claudeArgs ...string) *liveCompactionRun {
	t.Helper()
	args := make([]string, 0, 15+len(claudeArgs))
	args = append(args,
		"--db", dbPath, "launch", "--model", "compact-fixture", "--print",
		"--auth-mode", "gateway-token",
		"--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--session-id", sessionID,
	)
	return startLiveCompactionCommand(t, ctx, Dependencies{}, append(args, claudeArgs...))
}

func startLiveCompactionCommand(t *testing.T, ctx context.Context, deps Dependencies, args []string) *liveCompactionRun {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating live command input: %v", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	run := &liveCompactionRun{
		input:  writer,
		out:    &synchronizedBuffer{},
		errOut: &synchronizedBuffer{},
		cancel: cancel,
		done:   make(chan struct{}),
	}
	deps.In, deps.Out, deps.Err = reader, run.out, run.errOut
	go func() {
		cmd := NewRootCommand(runCtx, deps)
		cmd.SetArgs(args)
		run.err = cmd.Execute()
		_ = reader.Close()
		close(run.done)
	}()
	t.Cleanup(run.abort)
	return run
}

func (r *liveCompactionRun) sendAndWait(t *testing.T, ctx context.Context, fixture *liveCompactionFixture, message string, wantResponses int) {
	t.Helper()
	wantResults := r.resultCount() + 1
	r.send(t, message)
	r.waitForResponses(t, ctx, fixture, wantResponses)
	r.waitForResults(t, ctx, wantResults)
}

func (r *liveCompactionRun) sendAndWaitNext(t *testing.T, ctx context.Context, fixture *liveCompactionFixture, message string) {
	t.Helper()
	r.sendAndWait(t, ctx, fixture, message, fixture.responseCount()+1)
}

func (r *liveCompactionRun) sendAndWaitForCompaction(t *testing.T, ctx context.Context, fixture *liveCompactionFixture, message string) {
	t.Helper()
	wantCompactions := fixture.completedCompactionCount() + 1
	r.send(t, message)
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if fixture.completedCompactionCount() >= wantCompactions {
			return
		}
		out := r.out.String()
		if strings.Contains(out, `"compact_result":"failed"`) {
			t.Fatalf("Claude Code rejected compaction before contacting the provider\nstdout:\n%s\nstderr:\n%s", out, r.errOut.String())
		}
		select {
		case <-r.done:
			t.Fatalf("Claude Code exited before completing compaction: %v\nstdout:\n%s\nstderr:\n%s", r.err, out, r.errOut.String())
		case <-ctx.Done():
			t.Fatalf("waiting for completed compaction response: %v\nstdout:\n%s\nstderr:\n%s", ctx.Err(), out, r.errOut.String())
		case <-ticker.C:
		}
	}
}

func (r *liveCompactionRun) send(t *testing.T, message string) {
	t.Helper()
	if _, err := io.WriteString(r.input, liveStreamInput(t, message)); err != nil {
		t.Fatalf("writing live compaction input: %v\nstdout:\n%s\nstderr:\n%s", err, r.out.String(), r.errOut.String())
	}
}

func (r *liveCompactionRun) waitForResponses(t *testing.T, ctx context.Context, fixture *liveCompactionFixture, want int) {
	t.Helper()
	r.waitFor(t, ctx, fmt.Sprintf("%d provider responses", want), func() bool {
		return fixture.responseCount() >= want
	})
}

func (r *liveCompactionRun) waitForCompletedCompaction(t *testing.T, ctx context.Context, fixture *liveCompactionFixture, want int) {
	t.Helper()
	r.waitFor(t, ctx, fmt.Sprintf("%d completed compaction responses", want), func() bool {
		return fixture.completedCompactionCount() >= want
	})
}

func (r *liveCompactionRun) waitForResults(t *testing.T, ctx context.Context, want int) {
	t.Helper()
	r.waitFor(t, ctx, fmt.Sprintf("%d Claude Code results", want), func() bool {
		return r.resultCount() >= want
	})
}

func (r *liveCompactionRun) waitFor(t *testing.T, ctx context.Context, want string, ready func() bool) {
	t.Helper()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if ready() {
			return
		}
		select {
		case <-r.done:
			t.Fatalf("Claude Code exited before %s: %v\nstdout:\n%s\nstderr:\n%s", want, r.err, r.out.String(), r.errOut.String())
		case <-ctx.Done():
			t.Fatalf("waiting for %s: %v\nstdout:\n%s\nstderr:\n%s", want, ctx.Err(), r.out.String(), r.errOut.String())
		case <-ticker.C:
		}
	}
}

func (r *liveCompactionRun) resultCount() int {
	out := r.out.String()
	return strings.Count(out, `"type":"result"`) + strings.Count(out, `"type": "result"`)
}

func (r *liveCompactionRun) finish(ctx context.Context) (string, string, error) {
	_ = r.input.Close()
	select {
	case <-r.done:
		r.cancel()
		return r.out.String(), r.errOut.String(), r.err
	case <-ctx.Done():
		r.abort()
		return r.out.String(), r.errOut.String(), ctx.Err()
	}
}

func (r *liveCompactionRun) abort() {
	_ = r.input.Close()
	r.cancel()
	select {
	case <-r.done:
	case <-time.After(10 * time.Second):
	}
}

func liveCompactionHistoryMessages(turns int, mode string) []string {
	historyBlock := strings.Repeat("bounded synthetic "+mode+" history detail ", 1500)
	messages := make([]string, 0, turns)
	for index := 0; index < turns; index++ {
		messages = append(messages, fmt.Sprintf(
			"Retain the session marker %s and %s decision %d. %s",
			liveCompactionMarker, mode, index+1, historyBlock,
		))
	}
	return messages
}

type liveCompactionFixture struct {
	server               *httptest.Server
	mu                   sync.Mutex
	reqs                 []liveOpenAIChatPayload
	count                int
	responses            int
	completedCompactions int
}

func newLiveCompactionFixture(t *testing.T) *liveCompactionFixture {
	t.Helper()
	fixture := &liveCompactionFixture{}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.handle(t, w, r)
	}))
	t.Cleanup(fixture.server.Close)
	return fixture
}

func (f *liveCompactionFixture) URL() string {
	return f.server.URL
}

func (f *liveCompactionFixture) handle(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	switch r.URL.Path {
	case "/v1/models":
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":[{"id":"fixture-compact-model"}]}`)
	case "/v1/chat/completions":
		f.handleChat(t, w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *liveCompactionFixture) handleChat(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	var payload liveOpenAIChatPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		t.Errorf("decoding compact fixture request: %v", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !payload.Stream {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-validation","object":"chat.completion","model":"fixture-compact-model","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		return
	}
	isCompaction := openAIMessagesContain(payload.Messages, liveCompactionPromptMarker)
	f.mu.Lock()
	f.reqs = append(f.reqs, payload)
	call := len(f.reqs)
	if isCompaction {
		f.count++
	}
	compacted := f.count > 0
	f.mu.Unlock()

	response := liveCompactionBefore
	if isCompaction {
		response = "<summary>The session marker is " + liveCompactionMarker + ".</summary>"
	} else if compacted {
		response = liveCompactionAfter
	}
	promptTokens := len(openAIMessagesText(payload.Messages)) / 4
	if promptTokens == 0 {
		promptTokens = 1
	}
	writeLiveOpenAIChatStream(w, call, response, promptTokens)
	f.mu.Lock()
	f.responses++
	if isCompaction {
		f.completedCompactions++
	}
	f.mu.Unlock()
}

func writeLiveOpenAIChatStream(w http.ResponseWriter, call int, response string, promptTokens int) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	middle := len(response) / 2
	for _, fragment := range []string{response[:middle], response[middle:]} {
		_, _ = fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-compact-%d\",\"choices\":[{\"index\":0,\"delta\":{\"content\":%q}}]}\n\n", call, fragment)
		if flusher != nil {
			flusher.Flush()
		}
	}
	_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
	_, _ = fmt.Fprintf(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":8}}\n\n", promptTokens)
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func (f *liveCompactionFixture) snapshot() []liveOpenAIChatPayload {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]liveOpenAIChatPayload(nil), f.reqs...)
}

func (f *liveCompactionFixture) compactionCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.count
}

func (f *liveCompactionFixture) responseCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.responses
}

func (f *liveCompactionFixture) completedCompactionCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.completedCompactions
}

func assertLiveCompactionTraffic(t *testing.T, requests []liveOpenAIChatPayload) {
	t.Helper()
	compactionIndex := -1
	for index, request := range requests {
		if !request.Stream || request.StreamOptions == nil || !request.StreamOptions.IncludeUsage {
			t.Fatalf("provider call %d stream=%t stream_options=%#v", index+1, request.Stream, request.StreamOptions)
		}
		if openAIMessagesContain(request.Messages, liveCompactionPromptMarker) {
			if compactionIndex >= 0 {
				t.Fatalf("multiple compaction requests observed at calls %d and %d", compactionIndex+1, index+1)
			}
			compactionIndex = index
		}
	}
	if compactionIndex < 0 || compactionIndex+1 >= len(requests) {
		t.Fatalf("compaction/continuation sequence missing from %d provider requests", len(requests))
	}
	compactionChars := len(openAIMessagesText(requests[compactionIndex].Messages))
	continuation := requests[len(requests)-1]
	continuationChars := len(openAIMessagesText(continuation.Messages))
	if continuationChars >= compactionChars {
		t.Fatalf("continuation payload was not reduced: compact=%d chars continuation=%d chars", compactionChars, continuationChars)
	}
	if !openAIMessagesContain(continuation.Messages, liveCompactionMarker) {
		t.Fatal("post-compaction continuation lost the session marker")
	}
}
