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
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/gateway"
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
	var firstPartyCalls atomic.Int64
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		firstPartyCalls.Add(1)
		http.Error(w, "unexpected first-party request", http.StatusBadGateway)
	}))
	defer fallback.Close()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	addLiveOpenAIModel(t, ctx, dbPath, provider.URL)

	prompt := `Spawn a research subagent now. The subagent prompt must be: "Return exactly CCR_LIVE_CHILD_OK and nothing else." After the subagent finishes, reply exactly CCR_LIVE_PARENT_OK if it succeeded. Do not use web or shell.`
	deps := Dependencies{
		In: strings.NewReader(prompt + "\n"),
		StartGateway: func(ctx context.Context, cfg gateway.Config) (*gateway.Server, error) {
			cfg.AnthropicBaseURL = fallback.URL
			return gateway.Start(ctx, cfg)
		},
	}
	out, errOut, err := runLiveCommand(ctx, deps, "--db", dbPath, "launch", "--model", "gpt", "--print", "--auth-mode", "gateway-token")
	if err != nil {
		t.Fatalf("launch error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if !strings.Contains(out, "CCR_LIVE_PARENT_OK") {
		t.Fatalf("launch output missing parent response:\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	state.assertComplete(t, out, errOut)
	if got := firstPartyCalls.Load(); got != 0 {
		t.Fatalf("model-declared Agent request reached first-party Anthropic %d times", got)
	}
	assertLiveAgentVisibility(t, ctx, dbPath)
}

func TestLiveLaunchOpenAIProviderRoutesFamilyAfterSameSessionModelSwitch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}

	provider, state := newLiveSwitchedAgentProvider(t)
	defer provider.Close()
	var firstPartyCalls atomic.Int64
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		firstPartyCalls.Add(1)
		http.Error(w, "unexpected first-party request", http.StatusBadGateway)
	}))
	defer fallback.Close()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	for _, args := range [][]string{
		{"--db", dbPath, "provider", "add", "litellm", "--base-url", provider.URL, "--no-api-key"},
		{"--db", dbPath, "model", "add", "gpt", "--provider", "litellm", "--model", "gpt-5"},
		{"--db", dbPath, "model", "add", "qwen", "--provider", "litellm", "--model", "qwen-5"},
	} {
		if out, errOut, err := runLiveCommand(ctx, Dependencies{}, args...); err != nil {
			t.Fatalf("run %v error = %v\nstdout:\n%s\nstderr:\n%s", args, err, out, errOut)
		}
	}

	prompt := `Spawn a research subagent now. The subagent prompt must be: "Return exactly CCR_LIVE_SWITCH_CHILD_OK and nothing else." After the subagent finishes, reply exactly CCR_LIVE_SWITCH_PARENT_OK if it succeeded. Do not use web or shell.`
	deps := Dependencies{
		In: strings.NewReader(liveStreamInput(t, "/model anthropic.ccr.qwen", prompt)),
		StartGateway: func(ctx context.Context, cfg gateway.Config) (*gateway.Server, error) {
			cfg.AnthropicBaseURL = fallback.URL
			return gateway.Start(ctx, cfg)
		},
	}
	out, errOut, err := runLiveCommand(ctx, deps, "--db", dbPath, "launch", "--model", "gpt", "--print", "--auth-mode", "gateway-token", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--", "--allowedTools", "Agent", "--forward-subagent-text")
	if err != nil {
		t.Fatalf("launch error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if !strings.Contains(out, "CCR_LIVE_SWITCH_CHILD_OK") {
		t.Fatalf("launch output missing switched child response:\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	state.assertFamilyRequestsUseSwitchedAlias(t, out, errOut)
	if got := firstPartyCalls.Load(); got != 0 {
		t.Fatalf("family-declared Agent request reached first-party Anthropic %d times", got)
	}
}

func TestLiveLaunchOpenAIProviderRoutesWorkflowFamilyAfterSameSessionModelSwitch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}

	provider, state := newLiveSwitchedWorkflowProvider(t)
	defer provider.Close()
	var firstPartyCalls atomic.Int64
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		firstPartyCalls.Add(1)
		http.Error(w, "unexpected first-party request", http.StatusBadGateway)
	}))
	defer fallback.Close()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	for _, args := range [][]string{
		{"--db", dbPath, "provider", "add", "litellm", "--base-url", provider.URL, "--no-api-key"},
		{"--db", dbPath, "model", "add", "gpt", "--provider", "litellm", "--model", "gpt-5"},
		{"--db", dbPath, "model", "add", "qwen", "--provider", "litellm", "--model", "qwen-5"},
	} {
		if out, errOut, err := runLiveCommand(ctx, Dependencies{}, args...); err != nil {
			t.Fatalf("run %v error = %v\nstdout:\n%s\nstderr:\n%s", args, err, out, errOut)
		}
	}

	prompt := `Use a workflow now. The workflow should run one worker that uses the sonnet model and returns exactly CCR_LIVE_SWITCH_WORKFLOW_CHILD_OK. After the workflow completes, reply exactly CCR_LIVE_SWITCH_WORKFLOW_PARENT_OK. Do not use shell or web.`
	deps := Dependencies{
		In: strings.NewReader(liveStreamInput(t, "/model anthropic.ccr.qwen", prompt)),
		StartGateway: func(ctx context.Context, cfg gateway.Config) (*gateway.Server, error) {
			cfg.AnthropicBaseURL = fallback.URL
			return gateway.Start(ctx, cfg)
		},
	}
	out, errOut, err := runLiveCommand(ctx, deps, "--db", dbPath, "launch", "--model", "gpt", "--print", "--auth-mode", "gateway-token", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--permission-mode", "auto")
	if err != nil {
		t.Fatalf("launch error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if !strings.Contains(out, "CCR_LIVE_SWITCH_WORKFLOW_PARENT_OK") {
		t.Fatalf("launch output missing switched workflow parent response:\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	state.assertFamilyRequestsUseSwitchedAlias(t, out, errOut)
	if got := firstPartyCalls.Load(); got != 0 {
		t.Fatalf("family-declared Workflow request reached first-party Anthropic %d times", got)
	}
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

type liveSwitchedAgentProviderState struct {
	mu            sync.Mutex
	regularModels []string
	childModel    string
	parentModel   string
}

func newLiveSwitchedAgentProvider(t *testing.T) (*httptest.Server, *liveSwitchedAgentProviderState) {
	t.Helper()
	state := &liveSwitchedAgentProviderState{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state.handle(t, w, r)
	}))
	return server, state
}

func (s *liveSwitchedAgentProviderState) handle(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	switch r.URL.Path {
	case "/v1/models":
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":[{"id":"gpt-5"},{"id":"qwen-5"}]}`)
	case "/v1/messages/count_tokens":
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"input_tokens":3}`)
	case "/v1/chat/completions":
		s.handleChat(t, w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *liveSwitchedAgentProviderState) handleChat(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	var payload liveOpenAIChatPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		t.Errorf("provider decode error = %v", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if payload.Model != "gpt-5" && payload.Model != "qwen-5" {
		t.Errorf("provider model = %q, want gpt-5 or qwen-5", payload.Model)
		http.Error(w, "bad model", http.StatusBadRequest)
		return
	}
	if isLiveAutoClassifierRequest(payload) {
		writeLiveOpenAIClassifierResponse(w, payload)
		return
	}
	if openAIMessagesContain(payload.Messages, "You are naming a coding session") {
		writeLiveOpenAITextFixture(w, payload, "chatcmpl-switch-title", "Switched Agent test", 4, 2)
		return
	}
	if openAIUserMessageEquals(payload.Messages, "Hi") {
		writeLiveOpenAITextFixture(w, payload, "chatcmpl-switch-probe", "ready", 4, 2)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.regularModels = append(s.regularModels, payload.Model)
	switch {
	case len(s.regularModels) == 1:
		writeLiveOpenAIToolFixture(w, payload, "chatcmpl-switch-agent", "toolu_switch_agent", "Agent", map[string]any{
			"description":       "return child sentinel",
			"prompt":            "Return exactly CCR_LIVE_SWITCH_CHILD_OK and nothing else.",
			"model":             "sonnet",
			"subagent_type":     "general-purpose",
			"run_in_background": false,
		})
	case openAIMessagesContain(payload.Messages, "Return exactly CCR_LIVE_SWITCH_CHILD_OK") &&
		(openAIMessagesContainToolRole(payload.Messages, "") ||
			openAIMessageRoleContains(payload.Messages, "assistant", "CCR_LIVE_SWITCH_CHILD_OK") ||
			strings.Contains(latestOpenAIMessage(payload.Messages), "tool_result") ||
			strings.HasPrefix(latestOpenAIMessage(payload.Messages), "tool ")):
		s.parentModel = payload.Model
		writeLiveOpenAITextFixture(w, payload, "chatcmpl-switch-parent", "CCR_LIVE_SWITCH_PARENT_OK", 4, 2)
	case openAIMessagesContain(payload.Messages, "Return exactly CCR_LIVE_SWITCH_CHILD_OK"):
		s.childModel = payload.Model
		if liveToolsContain(payload.Tools, "SubagentHandback") {
			writeLiveOpenAIToolFixture(w, payload, "chatcmpl-switch-handback", "toolu_switch_handback", "SubagentHandback", map[string]any{
				"message": "CCR_LIVE_SWITCH_CHILD_OK",
			})
		} else {
			writeLiveOpenAITextFixture(w, payload, "chatcmpl-switch-child", "CCR_LIVE_SWITCH_CHILD_OK", 4, 2)
		}
	default:
		t.Errorf("unexpected provider request after switched Agent tool call: %#v", payload.Messages)
		http.Error(w, "unexpected request", http.StatusBadRequest)
	}
}

func (s *liveSwitchedAgentProviderState) assertFamilyRequestsUseSwitchedAlias(t *testing.T, out, errOut string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.regularModels) < 2 || s.childModel != "qwen-5" || (s.parentModel != "" && s.parentModel != "qwen-5") {
		t.Fatalf("family requests did not follow same-session alias: regularModels=%v childModel=%q parentModel=%q\nstdout:\n%s\nstderr:\n%s", s.regularModels, s.childModel, s.parentModel, out, errOut)
	}
	for _, model := range s.regularModels {
		if model != "qwen-5" {
			t.Fatalf("post-switch family request used provider model %q, want qwen-5; all models=%v", model, s.regularModels)
		}
	}
}

type liveSwitchedWorkflowProviderState struct {
	mu              sync.Mutex
	regularModels   []string
	workflowStarted bool
	childModel      string
	parentModel     string
}

func newLiveSwitchedWorkflowProvider(t *testing.T) (*httptest.Server, *liveSwitchedWorkflowProviderState) {
	t.Helper()
	state := &liveSwitchedWorkflowProviderState{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state.handle(t, w, r)
	}))
	return server, state
}

func (s *liveSwitchedWorkflowProviderState) handle(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	switch r.URL.Path {
	case "/v1/models":
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":[{"id":"gpt-5"},{"id":"qwen-5"}]}`)
	case "/v1/messages/count_tokens":
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"input_tokens":3}`)
	case "/v1/chat/completions":
		s.handleChat(t, w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *liveSwitchedWorkflowProviderState) handleChat(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	var payload liveOpenAIChatPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		t.Errorf("provider decode error = %v", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if payload.Model != "gpt-5" && payload.Model != "qwen-5" {
		t.Errorf("provider model = %q, want gpt-5 or qwen-5", payload.Model)
		http.Error(w, "bad model", http.StatusBadRequest)
		return
	}
	if isLiveAutoClassifierRequest(payload) {
		writeLiveOpenAIClassifierResponse(w, payload)
		return
	}
	if openAIMessagesContain(payload.Messages, "You are naming a coding session") {
		writeLiveOpenAITextFixture(w, payload, "chatcmpl-switch-workflow-title", "Switched Workflow test", 4, 2)
		return
	}
	if openAIUserMessageEquals(payload.Messages, "Hi") {
		writeLiveOpenAITextFixture(w, payload, "chatcmpl-switch-workflow-probe", "ready", 4, 2)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.regularModels = append(s.regularModels, payload.Model)
	switch {
	case !s.workflowStarted && openAIMessagesContain(payload.Messages, "CCR_LIVE_SWITCH_WORKFLOW_PARENT_OK"):
		s.workflowStarted = true
		arguments, _ := json.Marshal(map[string]string{"script": liveSwitchedWorkflowScript()})
		writeLiveOpenAIToolFixture(w, payload, "chatcmpl-switch-workflow", "toolu_switch_workflow", "Workflow", json.RawMessage(arguments))
	case s.workflowStarted && (openAIMessagesContainToolRole(payload.Messages, "Workflow launched in background") ||
		openAIMessagesContain(payload.Messages, "<task-notification>")):
		s.parentModel = payload.Model
		writeLiveOpenAITextFixture(w, payload, "chatcmpl-switch-workflow-parent", "CCR_LIVE_SWITCH_WORKFLOW_PARENT_OK", 4, 2)
	case s.workflowStarted && isSwitchedWorkflowSubagentRequest(payload.Messages):
		s.childModel = payload.Model
		writeLiveOpenAITextFixture(w, payload, "chatcmpl-switch-workflow-child", "CCR_LIVE_SWITCH_WORKFLOW_CHILD_OK", 4, 2)
	default:
		t.Errorf("unexpected provider request in switched Workflow route: %#v", payload.Messages)
		http.Error(w, "unexpected request", http.StatusBadRequest)
	}
}

func (s *liveSwitchedWorkflowProviderState) assertFamilyRequestsUseSwitchedAlias(t *testing.T, out, errOut string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.regularModels) < 3 || s.childModel != "qwen-5" || s.parentModel != "qwen-5" {
		t.Fatalf("workflow family requests did not follow same-session alias: regularModels=%v childModel=%q parentModel=%q\nstdout:\n%s\nstderr:\n%s", s.regularModels, s.childModel, s.parentModel, out, errOut)
	}
	for _, model := range s.regularModels {
		if model != "qwen-5" {
			t.Fatalf("switched workflow request used provider model %q, want qwen-5; all models=%v", model, s.regularModels)
		}
	}
}

func liveSwitchedWorkflowScript() string {
	return `export const meta = {
  name: 'ccr-live-switched-workflow',
  description: 'Return switched workflow sentinel',
  phases: [{ title: 'Run' }],
}
phase('Run')
const result = await agent('Find latest ChatGPT news using web research, then return exactly CCR_LIVE_SWITCH_WORKFLOW_CHILD_OK.', {label: 'investigating-researcher', phase: 'Run', model: 'sonnet'})
return result
`
}

func isSwitchedWorkflowSubagentRequest(messages []liveOpenAIChatMessage) bool {
	return openAIMessagesContain(messages, "subagent spawned by a workflow orchestration script") &&
		openAIMessagesContain(messages, "Find latest ChatGPT news") &&
		openAIMessagesContain(messages, "CCR_LIVE_SWITCH_WORKFLOW_CHILD_OK")
}

func openAIUserMessageEquals(messages []liveOpenAIChatMessage, want string) bool {
	for _, message := range messages {
		if message.Role == "user" && strings.TrimSpace(message.Content) == want {
			return true
		}
	}
	return false
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
	if isLiveAutoClassifierRequest(payload) {
		writeLiveOpenAIClassifierResponse(w, payload)
		return
	}
	if openAIMessagesContain(payload.Messages, "You are naming a coding session") {
		writeLiveOpenAITextFixture(w, payload, "chatcmpl-agent-title", "Agent tool test", 4, 2)
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
			"model":             "sonnet",
			"subagent_type":     "general-purpose",
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
