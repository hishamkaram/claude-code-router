//go:build live

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/liveclaude"
)

func TestLiveLaunchOpenAIProviderAutoModeToolSearchAgent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}

	state := &liveToolSearchAgentState{}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state.handle(t, w, r)
	}))
	defer provider.Close()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	addLiveOpenAIModel(t, ctx, dbPath, provider.URL)

	prompt := `Use a subagent now. The child prompt must be: "Return exactly CCR_LIVE_TOOLSEARCH_CHILD_OK and nothing else." After the child returns, reply exactly CCR_LIVE_TOOLSEARCH_PARENT_OK.`
	out, errOut, err := runLiveCommand(ctx, Dependencies{In: strings.NewReader(prompt + "\n")}, "--db", dbPath, "launch", "--model", "gpt", "--print", "--auth-mode", "gateway-token", "--permission-mode", "auto")
	if err != nil {
		t.Fatalf("launch error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if !strings.Contains(out, "CCR_LIVE_TOOLSEARCH_PARENT_OK") {
		t.Fatalf("launch output missing parent response:\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	state.assertComplete(t, out, errOut)
}

type liveToolSearchAgentState struct {
	mu                        sync.Mutex
	chatCalls                 int
	firstRequestHadToolSearch bool
	toolReferenceResultSeen   bool
	childPromptSeen           bool
	parentAgentToolResultSeen bool
}

func (s *liveToolSearchAgentState) handle(t *testing.T, w http.ResponseWriter, r *http.Request) {
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

func (s *liveToolSearchAgentState) handleChat(t *testing.T, w http.ResponseWriter, r *http.Request) {
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
	case s.chatCalls == 1:
		s.firstRequestHadToolSearch = liveToolsContain(payload.Tools, "ToolSearch")
		writeLiveToolSearchCall(w)
	case !s.toolReferenceResultSeen && openAIMessagesContainToolRole(payload.Messages, "[Loaded tool: Agent]"):
		s.toolReferenceResultSeen = true
		writeLiveAgentCall(w)
	case !s.childPromptSeen && openAIMessagesContain(payload.Messages, "Return exactly CCR_LIVE_TOOLSEARCH_CHILD_OK"):
		s.childPromptSeen = true
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-toolsearch-child","choices":[{"message":{"content":"CCR_LIVE_TOOLSEARCH_CHILD_OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`)
	case openAIMessagesContainToolRole(payload.Messages, "CCR_LIVE_TOOLSEARCH_CHILD_OK"):
		s.parentAgentToolResultSeen = true
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-toolsearch-parent","choices":[{"message":{"content":"CCR_LIVE_TOOLSEARCH_PARENT_OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`)
	default:
		t.Errorf("unexpected provider request in ToolSearch Agent live route: %#v", payload.Messages)
		http.Error(w, "unexpected request", http.StatusBadRequest)
	}
}

func writeLiveToolSearchCall(w http.ResponseWriter) {
	writeLiveToolCall(w, "chatcmpl-toolsearch", "toolu_toolsearch_live", "ToolSearch", map[string]string{"query": "Agent"})
}

func writeLiveAgentCall(w http.ResponseWriter) {
	writeLiveToolCall(w, "chatcmpl-toolsearch-agent", "toolu_agent_after_toolsearch", "Agent", map[string]any{
		"description":       "return child sentinel",
		"prompt":            "Return exactly CCR_LIVE_TOOLSEARCH_CHILD_OK and nothing else.",
		"subagent_type":     "general-purpose",
		"run_in_background": false,
	})
}

func writeLiveToolCall(w http.ResponseWriter, id, toolID, name string, args any) {
	arguments, _ := json.Marshal(args)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": id,
		"choices": []map[string]any{{
			"message": map[string]any{
				"content": "",
				"tool_calls": []map[string]any{{
					"id":   toolID,
					"type": "function",
					"function": map[string]string{
						"name":      name,
						"arguments": string(arguments),
					},
				}},
			},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]int{"prompt_tokens": 4, "completion_tokens": 3},
	})
}

func (s *liveToolSearchAgentState) assertComplete(t *testing.T, out, errOut string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.firstRequestHadToolSearch || !s.toolReferenceResultSeen || !s.childPromptSeen || !s.parentAgentToolResultSeen {
		t.Fatalf("ToolSearch Agent live route incomplete: firstRequestHadToolSearch=%v toolReferenceResultSeen=%v childPromptSeen=%v parentAgentToolResultSeen=%v chatCalls=%d\nstdout:\n%s\nstderr:\n%s", s.firstRequestHadToolSearch, s.toolReferenceResultSeen, s.childPromptSeen, s.parentAgentToolResultSeen, s.chatCalls, out, errOut)
	}
}
