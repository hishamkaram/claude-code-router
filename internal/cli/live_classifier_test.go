//go:build live

package cli

import (
	"context"
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

func TestLiveLaunchOpenAIProviderAutoModeClassifierRequest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}

	workspace := t.TempDir()
	t.Chdir(workspace)
	if err := os.MkdirAll(filepath.Join(workspace, ".claude"), 0o755); err != nil {
		t.Fatalf("creating test settings directory: %v", err)
	}
	writePath := filepath.Join(workspace, ".claude", "settings.json")
	debugPath := filepath.Join(os.TempDir(), "ccr-live-classifier-debug.log")
	_ = os.Remove(debugPath)
	state := &liveAutoClassifierState{writePath: writePath}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state.handle(t, w, r)
	}))
	defer provider.Close()
	classifier := newLiveFirstPartyClassifierFixture(t)
	defer classifier.Close()
	t.Setenv("ANTHROPIC_API_KEY", liveFixtureAPIKey)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	addLiveOpenAIModel(t, ctx, dbPath, provider.URL)

	prompt := `Inspect the current project state, then reply exactly CCR_LIVE_CLASSIFIER_OK.`
	deps := Dependencies{
		In: strings.NewReader(prompt + "\n"), Launcher: liveDebugClaudeLauncher{debugPath: debugPath},
		StartGateway: classifier.StartGateway,
	}
	out, errOut, err := runLiveCommand(ctx, deps, "--db", dbPath, "launch", "--model", "gpt", "--print", "--auth-mode", "preserve", "--permission-mode", "auto")
	if err != nil {
		t.Fatalf("launch error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if !strings.Contains(out, "CCR_LIVE_CLASSIFIER_OK") {
		t.Fatalf("launch output missing classifier sentinel:\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	state.assertComplete(t, out, errOut, classifier.Seen())
	if _, err := os.Stat(writePath); err != nil {
		t.Fatalf("classified Write did not create test file: %v", err)
	}
}

type liveAutoClassifierState struct {
	mu                    sync.Mutex
	chatCalls             int
	writePath             string
	firstRequestHadWrite  bool
	classifierRequestSeen bool
	writeToolResultSeen   bool
}

func (s *liveAutoClassifierState) handle(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	switch r.URL.Path {
	case "/v1/models":
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":[{"id":"gpt-5"}]}`)
	case "/v1/messages/count_tokens":
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"input_tokens":3}`)
	case "/v1/chat/completions":
		s.handleChat(t, w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *liveAutoClassifierState) handleChat(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	payload, ok := decodeLiveOpenAIChatPayload(t, w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chatCalls++
	w.Header().Set("Content-Type", "application/json")
	switch {
	case isLiveAutoClassifierRequest(payload):
		s.classifierRequestSeen = true
		writeLiveOpenAIClassifierResponse(w, payload)
	case s.chatCalls == 1:
		s.firstRequestHadWrite = liveToolsContain(payload.Tools, "Write")
		writeLiveToolCall(w, "chatcmpl-classifier-write", "toolu_classifier_write", "Write", map[string]string{
			"file_path": s.writePath,
			"content":   "{\"ccrLiveTest\":true}\n",
		})
	case openAIMessagesContainToolRole(payload.Messages, ""):
		s.writeToolResultSeen = true
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-classifier-parent","choices":[{"message":{"content":"CCR_LIVE_CLASSIFIER_OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`)
	default:
		t.Errorf("unexpected provider request in classifier live route: %#v", payload.Messages)
		http.Error(w, "unexpected request", http.StatusBadRequest)
	}
}

func (s *liveAutoClassifierState) assertComplete(t *testing.T, out, errOut string, firstPartyClassifierSeen bool) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.firstRequestHadWrite || (!s.classifierRequestSeen && !firstPartyClassifierSeen) || !s.writeToolResultSeen {
		t.Fatalf("Classifier live route incomplete: firstRequestHadWrite=%v selectedClassifierSeen=%v firstPartyClassifierSeen=%v writeToolResultSeen=%v chatCalls=%d\nstdout:\n%s\nstderr:\n%s", s.firstRequestHadWrite, s.classifierRequestSeen, firstPartyClassifierSeen, s.writeToolResultSeen, s.chatCalls, out, errOut)
	}
}

type liveDebugClaudeLauncher struct {
	debugPath string
}

func (l liveDebugClaudeLauncher) Start(ctx context.Context, args []string, env ClaudeEnvironment, in io.Reader, out, errOut io.Writer) (ClaudeProcess, error) {
	debugArgs := append([]string{}, args...)
	debugArgs = append(debugArgs, "--debug-file", l.debugPath)
	return (ExecClaudeLauncher{}).Start(ctx, debugArgs, env, in, out, errOut)
}
