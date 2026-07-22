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

func TestLiveLaunchOpenAIProviderStreamsAgentToolInput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}

	provider, state := newLiveAgentToolProvider(t)
	defer provider.Close()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	addLiveOpenAIModel(t, ctx, dbPath, provider.URL)

	prompt := `Spawn a research subagent now. The subagent prompt must be: "Return exactly CCR_LIVE_CHILD_OK and nothing else." After the subagent finishes, reply exactly CCR_LIVE_PARENT_OK if it succeeded. Do not use web or shell.`
	out, errOut, err := runLiveCommand(ctx, Dependencies{In: strings.NewReader(prompt + "\n")}, "--db", dbPath, "launch", "--model", "gpt", "--print", "--auth-mode", "gateway-token")
	if err != nil {
		t.Fatalf("launch error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if !strings.Contains(out, "CCR_LIVE_PARENT_OK") {
		t.Fatalf("launch output missing parent response:\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	state.assertComplete(t, out, errOut)
	assertLiveAgentVisibility(t, ctx, dbPath)
}

func TestLiveLaunchOpenAIProviderRunsDynamicWorkflow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}

	provider, state := newLiveWorkflowProvider(t)
	defer provider.Close()
	classifier := newLiveFirstPartyClassifierFixture(t)
	defer classifier.Close()
	t.Setenv("ANTHROPIC_API_KEY", liveFixtureAPIKey)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	addLiveOpenAIModel(t, ctx, dbPath, provider.URL)

	prompt := `Use a workflow now. The workflow should run one worker that returns exactly CCR_LIVE_WORKFLOW_CHILD_OK. After the workflow starts, reply exactly CCR_LIVE_WORKFLOW_LAUNCHED_OK. Do not use shell or web.`
	deps := Dependencies{In: strings.NewReader(prompt + "\n"), StartGateway: classifier.StartGateway}
	out, errOut, err := runLiveCommand(ctx, deps, "--db", dbPath, "launch", "--model", "gpt", "--print", "--auth-mode", "preserve", "--permission-mode", "auto")
	if err != nil {
		t.Fatalf("launch error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if !strings.Contains(out, "CCR_LIVE_WORKFLOW_LAUNCHED_OK") {
		t.Fatalf("launch output missing workflow launch response:\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	state.assertComplete(t, out, errOut, classifier.Seen())
	assertLiveAgentVisibility(t, ctx, dbPath)
}

type liveAgentToolProviderState struct {
	mu                       sync.Mutex
	chatCalls                int
	firstRequestHadAgentTool bool
	childPromptSeen          bool
	parentToolResultSeen     bool
}

type liveWorkflowProviderState struct {
	mu                          sync.Mutex
	chatCalls                   int
	firstRequestHadWorkflowTool bool
	workflowClassifierSeen      bool
	workflowChildPromptSeen     bool
	workflowLaunchResultSeen    bool
}

func newLiveAgentToolProvider(t *testing.T) (*httptest.Server, *liveAgentToolProviderState) {
	t.Helper()
	state := &liveAgentToolProviderState{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state.handle(t, w, r)
	}))
	return server, state
}

func newLiveWorkflowProvider(t *testing.T) (*httptest.Server, *liveWorkflowProviderState) {
	t.Helper()
	state := &liveWorkflowProviderState{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state.handle(t, w, r)
	}))
	return server, state
}

func (s *liveAgentToolProviderState) handle(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	switch r.URL.Path {
	case "/v1/models":
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":[{"id":"gpt-5"}]}`)
	case "/v1/chat/completions":
		s.handleChat(t, w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *liveWorkflowProviderState) handle(t *testing.T, w http.ResponseWriter, r *http.Request) {
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

func (s *liveAgentToolProviderState) handleChat(t *testing.T, w http.ResponseWriter, r *http.Request) {
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
		s.firstRequestHadAgentTool = liveToolsContainAgent(payload.Tools)
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-agent-tool","choices":[{"message":{"content":"","tool_calls":[{"id":"toolu_agent_live","type":"function","function":{"name":"Agent","arguments":"{\"description\":\"return child sentinel\",\"prompt\":\"Return exactly CCR_LIVE_CHILD_OK and nothing else.\",\"subagent_type\":\"general-purpose\",\"run_in_background\":false}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":4,"completion_tokens":3}}`)
	case s.chatCalls == 2:
		s.handleChildRequest(t, w, payload.Messages)
	case openAIMessagesContainToolRole(payload.Messages, "CCR_LIVE_CHILD_OK"):
		s.parentToolResultSeen = true
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-agent-parent","choices":[{"message":{"content":"CCR_LIVE_PARENT_OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`)
	default:
		t.Errorf("unexpected provider request after Agent tool call: %#v", payload.Messages)
		http.Error(w, "unexpected request", http.StatusBadRequest)
	}
}

func (s *liveWorkflowProviderState) handleChat(t *testing.T, w http.ResponseWriter, r *http.Request) {
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
		s.workflowClassifierSeen = true
		writeLiveOpenAIClassifierResponse(w, payload)
	case !s.firstRequestHadWorkflowTool:
		s.firstRequestHadWorkflowTool = liveToolsContain(payload.Tools, "Workflow")
		writeOpenAIWorkflowToolCall(w)
	case isWorkflowSubagentRequest(payload.Messages):
		s.workflowChildPromptSeen = true
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-workflow-child","choices":[{"message":{"content":"CCR_LIVE_WORKFLOW_CHILD_OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`)
	case openAIMessagesContainToolRole(payload.Messages, "Workflow launched in background"):
		s.workflowLaunchResultSeen = true
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-workflow-started","choices":[{"message":{"content":"CCR_LIVE_WORKFLOW_LAUNCHED_OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`)
	case openAIMessagesContain(payload.Messages, "<task-notification>"):
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-workflow-parent","choices":[{"message":{"content":"CCR_LIVE_WORKFLOW_PARENT_OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`)
	default:
		t.Errorf("unexpected provider request in Workflow live route: %#v", payload.Messages)
		http.Error(w, "unexpected request", http.StatusBadRequest)
	}
}

func decodeLiveOpenAIChatPayload(t *testing.T, w http.ResponseWriter, r *http.Request) (liveOpenAIChatPayload, bool) {
	t.Helper()
	var payload liveOpenAIChatPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		t.Errorf("provider decode error = %v", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return payload, false
	}
	if payload.Model != "gpt-5" {
		t.Errorf("provider model = %q, want gpt-5", payload.Model)
		http.Error(w, "bad model", http.StatusBadRequest)
		return payload, false
	}
	return payload, true
}

func writeOpenAIWorkflowToolCall(w http.ResponseWriter) {
	arguments, _ := json.Marshal(map[string]string{"script": liveWorkflowScript()})
	response := map[string]any{
		"id": "chatcmpl-workflow-tool",
		"choices": []map[string]any{{
			"message": map[string]any{
				"content": "",
				"tool_calls": []map[string]any{{
					"id":   "toolu_workflow_live",
					"type": "function",
					"function": map[string]string{
						"name":      "Workflow",
						"arguments": string(arguments),
					},
				}},
			},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]int{"prompt_tokens": 4, "completion_tokens": 3},
	}
	_ = json.NewEncoder(w).Encode(response)
}

func liveWorkflowScript() string {
	return `export const meta = {
  name: 'ccr-live-workflow',
  description: 'Return workflow sentinel',
  phases: [{ title: 'Run' }],
}
phase('Run')
const result = await agent('Find latest ChatGPT news using web research, then return exactly CCR_LIVE_WORKFLOW_CHILD_OK.', {label: 'investigating-researcher', phase: 'Run'})
return result
`
}

func isWorkflowSubagentRequest(messages []liveOpenAIChatMessage) bool {
	return openAIMessagesContain(messages, "subagent spawned by a workflow orchestration script") &&
		openAIMessagesContain(messages, "Find latest ChatGPT news")
}

func (s *liveAgentToolProviderState) handleChildRequest(t *testing.T, w http.ResponseWriter, messages []liveOpenAIChatMessage) {
	t.Helper()
	if openAIMessagesContainToolRole(messages, "") || !openAIMessagesContain(messages, "Return exactly CCR_LIVE_CHILD_OK") {
		t.Errorf("second provider request is not the child request: %#v", messages)
		http.Error(w, "bad child request", http.StatusBadRequest)
		return
	}
	s.childPromptSeen = true
	_, _ = fmt.Fprint(w, `{"id":"chatcmpl-agent-child","choices":[{"message":{"content":"CCR_LIVE_CHILD_OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`)
}

func (s *liveWorkflowProviderState) assertComplete(t *testing.T, out, errOut string, firstPartyClassifierSeen bool) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.firstRequestHadWorkflowTool || (!s.workflowClassifierSeen && !firstPartyClassifierSeen) || !s.workflowChildPromptSeen || !s.workflowLaunchResultSeen {
		t.Fatalf("Workflow live route incomplete: firstRequestHadWorkflowTool=%v selectedClassifierSeen=%v firstPartyClassifierSeen=%v workflowChildPromptSeen=%v workflowLaunchResultSeen=%v chatCalls=%d\nstdout:\n%s\nstderr:\n%s", s.firstRequestHadWorkflowTool, s.workflowClassifierSeen, firstPartyClassifierSeen, s.workflowChildPromptSeen, s.workflowLaunchResultSeen, s.chatCalls, out, errOut)
	}
}

func (s *liveAgentToolProviderState) assertComplete(t *testing.T, out, errOut string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.firstRequestHadAgentTool || !s.childPromptSeen || !s.parentToolResultSeen {
		t.Fatalf("Agent live route incomplete: firstRequestHadAgentTool=%v childPromptSeen=%v parentToolResultSeen=%v chatCalls=%d\nstdout:\n%s\nstderr:\n%s", s.firstRequestHadAgentTool, s.childPromptSeen, s.parentToolResultSeen, s.chatCalls, out, errOut)
	}
}

func liveToolsContainAgent(tools []struct {
	Function struct {
		Name string `json:"name"`
	} `json:"function"`
},
) bool {
	return liveToolsContain(tools, "Agent")
}

func liveToolsContain(tools []struct {
	Function struct {
		Name string `json:"name"`
	} `json:"function"`
}, name string,
) bool {
	for _, tool := range tools {
		if tool.Function.Name == name {
			return true
		}
	}
	return false
}
