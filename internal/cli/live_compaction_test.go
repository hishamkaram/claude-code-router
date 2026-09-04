//go:build live

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
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
	launchTurn := liveCompactionLauncher(t, ctx, dbPath, "4ad59989-45ea-4e90-a133-78f15b1ce47f")

	seedLiveCompactionHistory(t, launchTurn, 8)
	compactOut, compactErrOut, err := launchTurn("/compact", false)
	if err != nil {
		t.Fatalf("manual compact launch error = %v\nstdout:\n%s\nstderr:\n%s", err, compactOut, compactErrOut)
	}
	if strings.Contains(compactOut, `"compact_result":"failed"`) {
		t.Fatalf("manual compact failed\nstdout:\n%s\nstderr:\n%s", compactOut, compactErrOut)
	}

	out, errOut, err := launchTurn("Reply with the configured response after compacting.", false)
	if err != nil {
		t.Fatalf("post-compact continuation launch error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
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
	launchTurn := liveCompactionLauncher(
		t, ctx, dbPath, "8b814572-96c7-45bf-a9fb-da2345fc4d25", "--autocompact", "100k",
	)

	historyBlock := strings.Repeat("bounded synthetic automatic compact history detail ", 1500)
	var lastOut, lastErrOut string
	for index := 0; index < 12 && fixture.compactionCount() == 0; index++ {
		prompt := fmt.Sprintf("Retain the session marker %s and automatic-compaction decision %d. %s", liveCompactionMarker, index+1, historyBlock)
		var err error
		lastOut, lastErrOut, err = launchTurn(prompt, index == 0)
		if err != nil {
			t.Fatalf("auto-compact history turn %d error = %v\nstdout:\n%s\nstderr:\n%s", index+1, err, lastOut, lastErrOut)
		}
		if strings.Contains(lastOut, "automatic compaction failed") || strings.Contains(lastOut, `"compact_result":"failed"`) {
			t.Fatalf("automatic compaction failed on turn %d\nstdout:\n%s\nstderr:\n%s", index+1, lastOut, lastErrOut)
		}
	}
	if fixture.compactionCount() == 0 {
		t.Fatalf("Claude Code did not issue an automatic compaction request\nstdout:\n%s\nstderr:\n%s", lastOut, lastErrOut)
	}

	out, errOut, err := launchTurn("Continue after automatic compaction using the retained session marker.", false)
	if err != nil {
		t.Fatalf("post-auto-compact continuation error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
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
	for _, args := range [][]string{
		{"--db", dbPath, "provider", "add", "compact-fixture", "--type", "litellm", "--base-url", baseURL, "--no-api-key", "--mode", "full"},
		{"--db", dbPath, "model", "add", "compact-fixture", "--provider", "compact-fixture", "--model", "fixture-compact-model", "--compat", "full"},
		{"--db", dbPath, "model", "update", "compact-fixture", "--context-window", "1000000"},
	} {
		if out, errOut, err := runLiveCommand(ctx, Dependencies{}, args...); err != nil {
			t.Fatalf("run %v error = %v\nstdout:\n%s\nstderr:\n%s", args, err, out, errOut)
		}
	}
	return dbPath
}

type liveCompactionLaunch func(input string, first bool) (string, string, error)

func liveCompactionLauncher(t *testing.T, ctx context.Context, dbPath, sessionID string, claudeArgs ...string) liveCompactionLaunch {
	t.Helper()
	return func(input string, first bool) (string, string, error) {
		args := []string{
			"--db", dbPath, "launch", "--model", "compact-fixture", "--print",
			"--auth-mode", "gateway-token",
			"--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		}
		if first {
			args = append(args, "--session-id", sessionID)
		} else {
			args = append(args, "--resume", sessionID)
		}
		args = append(args, claudeArgs...)
		return runLiveCommand(ctx, Dependencies{In: strings.NewReader(liveStreamInput(t, input))}, args...)
	}
}

func seedLiveCompactionHistory(t *testing.T, launchTurn liveCompactionLaunch, turns int) {
	t.Helper()
	historyBlock := strings.Repeat("bounded synthetic manual compact history detail ", 1500)
	for index := 0; index < turns; index++ {
		prompt := fmt.Sprintf("Retain the session marker %s and manual-compaction decision %d. %s", liveCompactionMarker, index+1, historyBlock)
		out, errOut, err := launchTurn(prompt, index == 0)
		if err != nil {
			t.Fatalf("compact history turn %d error = %v\nstdout:\n%s\nstderr:\n%s", index+1, err, out, errOut)
		}
	}
}

type liveCompactionFixture struct {
	server *httptest.Server
	mu     sync.Mutex
	reqs   []liveOpenAIChatPayload
	count  int
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
		closeLiveProviderConnection(t, w)
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
}

func closeLiveProviderConnection(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		t.Error("compact fixture response writer cannot hijack connection")
		return
	}
	connection, _, err := hijacker.Hijack()
	if err != nil {
		t.Errorf("hijacking compact fixture connection: %v", err)
		return
	}
	if err := connection.Close(); err != nil && !isClosedNetworkError(err) {
		t.Errorf("closing compact fixture connection: %v", err)
	}
}

func isClosedNetworkError(err error) bool {
	return err == net.ErrClosed || strings.Contains(strings.ToLower(err.Error()), "closed network connection")
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
