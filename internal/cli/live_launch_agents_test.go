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

	prompt := `Spawn a research subagent now. The subagent prompt must be: "Return exactly CCR_LIVE_CHILD_OK and nothing else." After the subagent finishes, confirm that it succeeded. Do not use web or shell.`
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

func TestLiveLaunchSelectedCCRModelRoutesAgentChildThroughSelectedAlias(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}

	provider, state := newLiveAgentToolProviderForModel(t, "gpt-5-switched")
	defer provider.Close()
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	addLiveOpenAIModel(t, ctx, dbPath, provider.URL)
	if out, errOut, err := runLiveCommand(ctx, Dependencies{}, "--db", dbPath, "model", "add", "switched", "--provider", "litellm", "--model", "gpt-5-switched"); err != nil {
		t.Fatalf("adding switched model error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	configureIsolatedLivePickerClaude(t)
	run := startLivePickerRunWithArgs(t, ctx, dbPath, "--model", "gpt", "--auth-mode", "gateway-token")
	defer run.close()
	run.waitForText(t, ctx, "Detected a custom API key")
	run.write(t, "\x1b[A\r", "accepting isolated placeholder API key")
	run.waitForText(t, ctx, "Claude Code v")
	run.write(t, "Hi\r", "sending pre-switch greeting")
	run.waitForText(t, ctx, "Ready.")
	run.write(t, "/model anthropic.ccr.switched\r", "switching the active model in-session")
	run.waitForText(t, ctx, "Switch model?")
	run.write(t, "\r", "confirming the in-session model switch")
	run.waitForText(t, ctx, "Set model to CCR switched")
	prompt := `Spawn a research subagent now. The subagent prompt must be: "Return exactly CCR_LIVE_CHILD_OK and nothing else." After the subagent finishes, confirm that it succeeded. Do not use web or shell.`
	run.write(t, prompt, "typing post-switch Agent prompt")
	time.Sleep(500 * time.Millisecond)
	run.write(t, "\r", "submitting post-switch Agent prompt")
	run.waitForText(t, ctx, "CCR_LIVE_PARENT_OK")
	state.assertComplete(t, run.commandOut.String(), run.commandErr.String())
	run.write(t, "/exit\r", "exiting Claude Code")
	run.waitForExit(t, ctx)
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
	state.assertComplete(t, out, errOut)
	classifier.AssertUnused(t)
	assertLiveAgentVisibility(t, ctx, dbPath)
}

type liveAgentToolProviderState struct {
	mu                       sync.Mutex
	expectedProviderModel    string
	chatCalls                int
	firstRequestHadAgentTool bool
	childPromptSeen          bool
	childUsedCCRModel        bool
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
	return newLiveAgentToolProviderForModel(t, "gpt-5")
}

func newLiveAgentToolProviderForModel(t *testing.T, expectedProviderModel string) (*httptest.Server, *liveAgentToolProviderState) {
	t.Helper()
	state := &liveAgentToolProviderState{expectedProviderModel: expectedProviderModel}
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
		if s.expectedProviderModel == "gpt-5" {
			_, _ = fmt.Fprint(w, `{"data":[{"id":"gpt-5"}]}`)
		} else {
			_, _ = fmt.Fprintf(w, `{"data":[{"id":"gpt-5"},{"id":%q}]}`, s.expectedProviderModel)
		}
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
	payload, ok := decodeLiveOpenAIChatPayloadForModel(t, w, r, s.expectedProviderModel)
	if !ok {
		return
	}
	if isLiveAutoClassifierRequest(payload) {
		writeLiveOpenAIClassifierResponse(w, payload)
		return
	}
	if openAIMessagesContain(payload.Messages, "You are naming a coding session") {
		writeLiveOpenAITextFixture(w, payload, "chatcmpl-agent-title", "Agent tool test", 4, 2)
		return
	}
	if liveLastOpenAIUserMessageIs(payload.Messages, "Hi") {
		writeLiveOpenAITextFixture(w, payload, "chatcmpl-agent-greeting", "Ready.", 4, 2)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.chatCalls++
	switch {
	case s.chatCalls == 1:
		s.firstRequestHadAgentTool = liveToolsContainAgent(payload.Tools)
		writeLiveOpenAIToolFixture(w, payload, "chatcmpl-agent-tool", "toolu_agent_live", "Agent", map[string]any{
			"description":       "return child sentinel",
			"prompt":            "Return exactly CCR_LIVE_CHILD_OK and nothing else.",
			"subagent_type":     "general-purpose",
			"model":             "sonnet",
			"run_in_background": false,
		})
	case s.chatCalls == 2:
		s.handleChildRequest(t, w, payload)
	case openAIMessagesContainToolRole(payload.Messages, ""):
		s.parentToolResultSeen = true
		writeLiveOpenAITextFixture(w, payload, "chatcmpl-agent-parent", "CCR_LIVE_PARENT_OK", 4, 2)
	case openAIMessagesContainToolRole(payload.Messages, "CCR_LIVE_CHILD_OK"):
		s.parentToolResultSeen = true
		writeLiveOpenAITextFixture(w, payload, "chatcmpl-agent-parent", "CCR_LIVE_PARENT_OK", 4, 2)
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
	if openAIMessagesContain(payload.Messages, "You are naming a coding session") {
		writeLiveOpenAITextFixture(w, payload, "chatcmpl-workflow-title", "Workflow test", 4, 2)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chatCalls++
	switch {
	case isLiveAutoClassifierRequest(payload):
		s.workflowClassifierSeen = true
		writeLiveOpenAIClassifierResponse(w, payload)
	case !s.firstRequestHadWorkflowTool:
		s.firstRequestHadWorkflowTool = liveToolsContain(payload.Tools, "Workflow")
		writeOpenAIWorkflowToolCall(w, payload)
	case isWorkflowSubagentRequest(payload.Messages):
		s.workflowChildPromptSeen = true
		writeLiveOpenAITextFixture(w, payload, "chatcmpl-workflow-child", "CCR_LIVE_WORKFLOW_CHILD_OK", 4, 2)
	case openAIMessagesContainToolRole(payload.Messages, "Workflow launched in background"):
		s.workflowLaunchResultSeen = true
		writeLiveOpenAITextFixture(w, payload, "chatcmpl-workflow-started", "CCR_LIVE_WORKFLOW_LAUNCHED_OK", 4, 2)
	case openAIMessagesContain(payload.Messages, "<task-notification>"):
		writeLiveOpenAITextFixture(w, payload, "chatcmpl-workflow-parent", "CCR_LIVE_WORKFLOW_PARENT_OK", 4, 2)
	default:
		t.Errorf("unexpected provider request in Workflow live route: %#v", payload.Messages)
		http.Error(w, "unexpected request", http.StatusBadRequest)
	}
}

func decodeLiveOpenAIChatPayload(t *testing.T, w http.ResponseWriter, r *http.Request) (liveOpenAIChatPayload, bool) {
	return decodeLiveOpenAIChatPayloadForModel(t, w, r, "gpt-5")
}

func decodeLiveOpenAIChatPayloadForModel(t *testing.T, w http.ResponseWriter, r *http.Request, expectedModel string) (liveOpenAIChatPayload, bool) {
	t.Helper()
	var payload liveOpenAIChatPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		t.Errorf("provider decode error = %v", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return payload, false
	}
	if payload.Model != expectedModel && !(expectedModel != "gpt-5" && payload.Model == "gpt-5") {
		t.Errorf("provider model = %q, want %s", payload.Model, expectedModel)
		http.Error(w, "bad model", http.StatusBadRequest)
		return payload, false
	}
	return payload, true
}

func writeOpenAIWorkflowToolCall(w http.ResponseWriter, payload liveOpenAIChatPayload) {
	arguments, _ := json.Marshal(map[string]string{"script": liveWorkflowScript()})
	writeLiveOpenAIToolFixture(w, payload, "chatcmpl-workflow-tool", "toolu_workflow_live", "Workflow", json.RawMessage(arguments))
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

func (s *liveAgentToolProviderState) handleChildRequest(t *testing.T, w http.ResponseWriter, payload liveOpenAIChatPayload) {
	t.Helper()
	if openAIMessagesContainToolRole(payload.Messages, "") || !openAIMessagesContain(payload.Messages, "Return exactly CCR_LIVE_CHILD_OK") {
		t.Errorf("second provider request is not the child request: %#v", payload.Messages)
		http.Error(w, "bad child request", http.StatusBadRequest)
		return
	}
	s.childPromptSeen = true
	s.childUsedCCRModel = payload.Model == s.expectedProviderModel
	if liveToolsContain(payload.Tools, "SubagentHandback") {
		writeLiveOpenAIToolFixture(w, payload, "chatcmpl-agent-handback", "toolu_agent_handback", "SubagentHandback", map[string]any{
			"message": "CCR_LIVE_CHILD_OK",
		})
		return
	}
	writeLiveOpenAITextFixture(w, payload, "chatcmpl-agent-child", "CCR_LIVE_CHILD_OK", 4, 2)
}

func (s *liveWorkflowProviderState) assertComplete(t *testing.T, out, errOut string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.firstRequestHadWorkflowTool || !s.workflowClassifierSeen || !s.workflowChildPromptSeen || !s.workflowLaunchResultSeen {
		t.Fatalf("Workflow live route incomplete: firstRequestHadWorkflowTool=%v selectedClassifierSeen=%v workflowChildPromptSeen=%v workflowLaunchResultSeen=%v chatCalls=%d\nstdout:\n%s\nstderr:\n%s", s.firstRequestHadWorkflowTool, s.workflowClassifierSeen, s.workflowChildPromptSeen, s.workflowLaunchResultSeen, s.chatCalls, out, errOut)
	}
}

func (s *liveAgentToolProviderState) assertComplete(t *testing.T, out, errOut string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.firstRequestHadAgentTool || !s.childPromptSeen || !s.childUsedCCRModel || !s.parentToolResultSeen {
		t.Fatalf("Agent live route incomplete: firstRequestHadAgentTool=%v childPromptSeen=%v childUsedCCRModel=%v parentToolResultSeen=%v chatCalls=%d\nstdout:\n%s\nstderr:\n%s", s.firstRequestHadAgentTool, s.childPromptSeen, s.childUsedCCRModel, s.parentToolResultSeen, s.chatCalls, out, errOut)
	}
}

func liveToolsContainAgent(tools []struct {
	Function struct {
		Name string `json:"name"`
	} `json:"function"`
},
) bool {
	return liveToolsContain(tools, "Agent") || liveToolsContain(tools, "Task")
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
