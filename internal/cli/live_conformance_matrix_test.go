//go:build live

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/gateway"
	"github.com/hishamkaram/claude-code-router/internal/liveclaude"
)

type liveClaudeConformanceFixture struct {
	protocol string
	server   *httptest.Server

	mu            sync.Mutex
	aliasModels   map[string]int
	firstParty    int
	agentToolSeen bool
	workflowSeen  bool
	requestSteps  []string
}

func TestLiveClaudeConformanceMatrix(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}
	t.Setenv("ANTHROPIC_API_KEY", liveFixtureAPIKey)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1")
	t.Setenv("CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL", "1")
	t.Setenv("CLAUDE_CODE_DISABLE_AUTO_MEMORY", "1")
	protocols, err := selectedLiveFixtureProtocols(os.Getenv("CCR_LIVE_FIXTURE_PROTOCOL"))
	if err != nil {
		t.Fatal(err)
	}
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
			runLiveClaudeConformanceProtocol(t, ctx, protocol)
		})
	}
}

func TestLiveFixtureMalformedResponses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	protocols, err := selectedLiveFixtureProtocols(os.Getenv("CCR_LIVE_FIXTURE_PROTOCOL"))
	if err != nil {
		t.Fatal(err)
	}
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			runLiveMalformedResponseProtocol(t, ctx, protocol)
		})
	}
}

func runLiveMalformedResponseProtocol(t *testing.T, ctx context.Context, protocol string) {
	t.Helper()
	const rawBodyMarker = "CCR_MALFORMED_FIXTURE_BODY"
	var malformedCalls atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"data":[{"id":"fixture-malformed-model"}]}`)
		case "/v1/messages/count_tokens":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"input_tokens":1}`)
		case "/v1/chat/completions", "/v1/messages":
			malformedCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"content":[`+rawBodyMarker)
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()
	var fallbackCalls atomic.Int64
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackCalls.Add(1)
		http.Error(w, "unexpected fallback", http.StatusBadGateway)
	}))
	defer fallback.Close()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	deps := Dependencies{StartGateway: func(ctx context.Context, cfg gateway.Config) (*gateway.Server, error) {
		cfg.AnthropicBaseURL = fallback.URL
		return gateway.Start(ctx, cfg)
	}}
	providerType := "litellm"
	if protocol == "anthropic" {
		providerType = "zai"
	}
	for _, args := range [][]string{
		{"--db", dbPath, "provider", "add", "malformed", "--type", providerType, "--base-url", provider.URL, "--no-api-key", "--mode", "full"},
		{"--db", dbPath, "model", "add", "malformed", "--provider", "malformed", "--model", "fixture-malformed-model", "--compat", "full"},
	} {
		out, errOut, commandErr := runLiveCommand(ctx, deps, args...)
		if commandErr != nil {
			t.Fatalf("run %v error = %v\nstdout:\n%s\nstderr:\n%s", args, commandErr, out, errOut)
		}
	}
	out, errOut, commandErr := runLiveCommand(ctx, deps, "--db", dbPath, "conformance", "run", "malformed", "--json")
	if commandErr == nil {
		t.Fatalf("malformed conformance unexpectedly passed\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	var document conformanceDocument
	if err := json.Unmarshal([]byte(out), &document); err != nil {
		t.Fatalf("malformed conformance JSON error = %v\n%s", err, out)
	}
	if document.Status != "failed" || !conformanceCheckFailed(document.Checks, "text") || malformedCalls.Load() == 0 {
		t.Fatalf("malformed conformance document = %#v; calls=%d", document, malformedCalls.Load())
	}
	if fallbackCalls.Load() != 0 {
		t.Fatalf("malformed provider response fell back to Anthropic %d times", fallbackCalls.Load())
	}
	if strings.Contains(out+errOut, rawBodyMarker) {
		t.Fatalf("raw malformed provider body reached CLI output\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	assertLiveDatabaseRedaction(t, dbPath, []string{rawBodyMarker})
}

func conformanceCheckFailed(checks []conformanceCheckView, name string) bool {
	for _, check := range checks {
		if check.Name == name && check.Status == "failed" {
			return true
		}
	}
	return false
}

func runLiveClaudeConformanceProtocol(t *testing.T, ctx context.Context, protocol string) {
	t.Helper()
	fixture := newLiveClaudeConformanceFixture(t, protocol)
	defer fixture.Close()
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	deps := Dependencies{
		StartGateway: func(ctx context.Context, cfg gateway.Config) (*gateway.Server, error) {
			cfg.AnthropicBaseURL = fixture.URL()
			return gateway.Start(ctx, cfg)
		},
	}
	providerType := "litellm"
	if protocol == "anthropic" {
		providerType = "zai"
	}
	commands := [][]string{
		{"--db", dbPath, "provider", "add", "fixture", "--type", providerType, "--base-url", fixture.URL(), "--no-api-key", "--mode", "full"},
		{"--db", dbPath, "model", "add", "fixture-sonnet", "--provider", "fixture", "--model", "fixture-full-model", "--compat", "full"},
		{"--db", dbPath, "model", "add", "fixture-chat", "--provider", "fixture", "--model", "fixture-chat-model", "--compat", "chat-only"},
	}
	for _, args := range commands {
		out, errOut, err := runLiveCommand(ctx, deps, args...)
		if err != nil {
			t.Fatalf("run %v error = %v\nstdout:\n%s\nstderr:\n%s", args, err, out, errOut)
		}
	}
	out, errOut, err := runLiveCommand(ctx, deps, "--db", dbPath, "conformance", "run",
		"fixture-sonnet", "--claude", "--include-anthropic", "--json")
	if err != nil {
		t.Fatalf("Claude conformance matrix error = %v; fixture=%s\nstdout:\n%s\nstderr:\n%s",
			err, fixture.summary(), out, errOut)
	}
	var document conformanceDocument
	if err := json.Unmarshal([]byte(out), &document); err != nil {
		t.Fatalf("conformance JSON error = %v\n%s", err, out)
	}
	if document.SchemaVersion != 1 || document.Status != "passed" || !document.LiveVerified || len(document.Checks) != 10 {
		t.Fatalf("conformance document = %#v", document)
	}
	fixture.assertComplete(t, out, errOut)
	assertLiveDatabaseRedaction(t, dbPath, []string{
		liveFixtureAPIKey, "CCR_CONFORMANCE_AGENT_CHILD_OK",
		"CCR_CONFORMANCE_WORKFLOW_CHILD_OK",
	})
}

func newLiveClaudeConformanceFixture(t *testing.T, protocol string) *liveClaudeConformanceFixture {
	t.Helper()
	fixture := &liveClaudeConformanceFixture{
		protocol: protocol, aliasModels: make(map[string]int),
	}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.handle(t, w, r)
	}))
	return fixture
}

func (f *liveClaudeConformanceFixture) URL() string {
	return f.server.URL
}

func (f *liveClaudeConformanceFixture) Close() {
	f.server.Close()
}

func (f *liveClaudeConformanceFixture) handle(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	switch r.URL.Path {
	case "/v1/models":
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":[{"id":"fixture-full-model"},{"id":"fixture-chat-model"}]}`)
	case "/v1/messages/count_tokens":
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"input_tokens":7}`)
	case "/v1/chat/completions":
		f.handleOpenAI(t, w, r)
	case "/v1/messages":
		f.handleAnthropic(t, w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *liveClaudeConformanceFixture) handleOpenAI(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	var payload liveOpenAIChatPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		t.Errorf("decoding OpenAI conformance request: %v", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	f.recordAlias(payload.Model)
	latest := latestOpenAIMessage(payload.Messages)
	f.recordRequestStep(payload.Model, latest)
	if strings.Contains(latest, "CCR_CONFORMANCE_CANCEL") {
		waitForFixtureCancellation(r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case liveToolsContain(payload.Tools, "ccr_probe"):
		writeOpenAIToolCall(w, "ccr_probe", "toolu_conformance", map[string]any{})
	case isLiveAutoClassifierRequest(payload):
		writeLiveOpenAIClassifierResponse(w, payload)
	case !f.workflowStarted() && strings.Contains(latest, claudeConformanceWorkflowParent):
		f.markWorkflow()
		writeOpenAIToolCall(w, "Workflow", "toolu_workflow_conformance", map[string]any{"script": conformanceWorkflowScript()})
	case f.workflowStarted() && openAIMessagesContain(payload.Messages, "<task-notification>"):
		f.writeOpenAIText(w, claudeConformanceWorkflowParent)
	case f.workflowStarted() && openAIMessagesContain(payload.Messages, "Workflow launched in background"):
		f.writeOpenAIText(w, claudeConformanceWorkflowParent)
	case f.workflowStarted() && strings.Contains(latest, "subagent spawned by a workflow orchestration script") && strings.Contains(latest, claudeConformanceWorkflowChild):
		f.writeOpenAIText(w, claudeConformanceWorkflowChild)
	case strings.Contains(latest, "CCR_CONFORMANCE_AGENT_CHILD_OK") &&
		(strings.Contains(latest, "tool_result") || strings.HasPrefix(latest, "tool ")):
		f.writeOpenAIText(w, claudeConformanceAgentParent)
	case strings.Contains(latest, "CCR_CONFORMANCE_AGENT_CHILD_OK") && !strings.Contains(latest, claudeConformanceAgentParent):
		f.writeOpenAIText(w, "CCR_CONFORMANCE_AGENT_CHILD_OK")
	case strings.Contains(latest, claudeConformanceAgentParent):
		f.markAgentTool()
		writeOpenAIToolCall(w, "Agent", "toolu_agent_conformance", map[string]any{
			"description": "return conformance sentinel", "prompt": "Return exactly CCR_CONFORMANCE_AGENT_CHILD_OK.",
			"subagent_type": "general-purpose", "run_in_background": false,
		})
	default:
		f.writeOpenAIText(w, latestConformanceSentinel(latest))
	}
}

func (f *liveClaudeConformanceFixture) handleAnthropic(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	var payload liveAnthropicMessagePayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		t.Errorf("decoding Anthropic conformance request: %v", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	aliasRoute := strings.HasPrefix(payload.Model, "fixture-")
	if aliasRoute {
		f.recordAlias(payload.Model)
	} else {
		f.recordFirstParty()
	}
	latest := latestAnthropicMessage(payload.Messages)
	f.recordRequestStep(payload.Model, latest)
	if strings.Contains(latest, "CCR_CONFORMANCE_CANCEL") {
		waitForFixtureCancellation(r)
		return
	}
	if isLiveAnthropicAutoClassifierRequest(payload) {
		writeLiveAnthropicClassifierResponse(w, payload)
		return
	}
	if !aliasRoute {
		writeAnthropicFixtureText(w, payload, latestConformanceSentinel(latest))
		return
	}
	switch {
	case liveAnthropicToolsContain(payload.Tools, "ccr_probe"):
		writeAnthropicFixtureToolCall(w, payload, "ccr_probe", "toolu_conformance", map[string]any{})
	case !f.workflowStarted() && strings.Contains(latest, claudeConformanceWorkflowParent):
		f.markWorkflow()
		writeAnthropicFixtureToolCall(w, payload, "Workflow", "toolu_workflow_conformance", map[string]any{"script": conformanceWorkflowScript()})
	case f.workflowStarted() && bytes.Contains(payload.System, []byte("subagent spawned by a workflow orchestration script")) &&
		strings.Contains(latest, claudeConformanceWorkflowChild):
		writeAnthropicFixtureText(w, payload, claudeConformanceWorkflowChild)
	case f.workflowStarted() && liveAnthropicMessagesContain(payload.Messages, "<task-notification>"):
		writeAnthropicFixtureText(w, payload, claudeConformanceWorkflowParent)
	case f.workflowStarted() && liveAnthropicMessagesContain(payload.Messages, "Workflow launched in background"):
		writeAnthropicFixtureText(w, payload, claudeConformanceWorkflowParent)
	case strings.Contains(latest, "CCR_CONFORMANCE_AGENT_CHILD_OK") && strings.Contains(latest, "tool_result"):
		writeAnthropicFixtureText(w, payload, claudeConformanceAgentParent)
	case strings.Contains(latest, "CCR_CONFORMANCE_AGENT_CHILD_OK") && !strings.Contains(latest, claudeConformanceAgentParent):
		writeAnthropicFixtureText(w, payload, "CCR_CONFORMANCE_AGENT_CHILD_OK")
	case strings.Contains(latest, claudeConformanceAgentParent):
		f.markAgentTool()
		writeAnthropicFixtureToolCall(w, payload, "Agent", "toolu_agent_conformance", map[string]any{
			"description": "return conformance sentinel", "prompt": "Return exactly CCR_CONFORMANCE_AGENT_CHILD_OK.",
			"subagent_type": "general-purpose", "run_in_background": false,
		})
	default:
		writeAnthropicFixtureText(w, payload, latestConformanceSentinel(latest))
	}
}

func latestOpenAIMessage(messages []liveOpenAIChatMessage) string {
	if len(messages) == 0 {
		return ""
	}
	for index := len(messages) - 1; index >= 0; index-- {
		if strings.Contains(messages[index].Content, "CCR_CONFORMANCE_") {
			return messages[index].Role + " " + messages[index].Content
		}
	}
	message := messages[len(messages)-1]
	return message.Role + " " + message.Content
}

func latestAnthropicMessage(messages []struct {
	Content json.RawMessage `json:"content"`
},
) string {
	if len(messages) == 0 {
		return ""
	}
	for index := len(messages) - 1; index >= 0; index-- {
		if bytes.Contains(messages[index].Content, []byte("CCR_CONFORMANCE_")) {
			return string(messages[index].Content)
		}
	}
	return string(messages[len(messages)-1].Content)
}

func latestConformanceSentinel(content string) string {
	fields := strings.FieldsFunc(content, func(r rune) bool {
		return (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_'
	})
	for index := len(fields) - 1; index >= 0; index-- {
		if strings.HasPrefix(fields[index], "CCR_CONFORMANCE_") {
			return fields[index]
		}
	}
	return "OK"
}

func waitForFixtureCancellation(r *http.Request) {
	select {
	case <-r.Context().Done():
	case <-time.After(100 * time.Millisecond):
	}
}

func writeOpenAIToolCall(w http.ResponseWriter, name, id string, input map[string]any) {
	arguments, _ := json.Marshal(input)
	_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"content":"","tool_calls":[{"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`, id, name, string(arguments))
}

func (f *liveClaudeConformanceFixture) writeOpenAIText(w http.ResponseWriter, text string) {
	_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`, text)
}

func writeAnthropicFixtureToolCall(w http.ResponseWriter, payload liveAnthropicMessagePayload, name, id string, input map[string]any) {
	if payload.Stream {
		writeLiveAnthropicToolCall(w, payload.Model, id, name, input)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	encoded, _ := json.Marshal(input)
	_, _ = fmt.Fprintf(w, `{"type":"message","model":%q,"content":[{"type":"tool_use","id":%q,"name":%q,"input":%s}],"stop_reason":"tool_use","usage":{"input_tokens":4,"output_tokens":2}}`, payload.Model, id, name, encoded)
}

func writeAnthropicFixtureText(w http.ResponseWriter, payload liveAnthropicMessagePayload, text string) {
	if payload.Stream {
		writeLiveAnthropicStream(w, payload.Model, text)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"type":"message","model":%q,"content":[{"type":"text","text":%q}],"stop_reason":"end_turn","usage":{"input_tokens":4,"output_tokens":2}}`, payload.Model, text)
}

func conformanceWorkflowScript() string {
	return `export const meta = {
  name: 'ccr-conformance-workflow',
  description: 'Return the conformance sentinel',
  phases: [{ title: 'Run' }],
}
phase('Run')
const result = await agent('Return exactly CCR_CONFORMANCE_WORKFLOW_CHILD_OK.', { label: 'worker', phase: 'Run' })
return result
`
}

func (f *liveClaudeConformanceFixture) recordAlias(model string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.aliasModels[model]++
}

func (f *liveClaudeConformanceFixture) recordFirstParty() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.firstParty++
}

func (f *liveClaudeConformanceFixture) markAgentTool() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.agentToolSeen = true
}

func (f *liveClaudeConformanceFixture) markWorkflow() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.workflowSeen = true
}

func (f *liveClaudeConformanceFixture) workflowStarted() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.workflowSeen
}

func (f *liveClaudeConformanceFixture) recordRequestStep(model, latest string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requestSteps = append(f.requestSteps, model+":"+latestConformanceSentinel(latest))
}

func (f *liveClaudeConformanceFixture) summary() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fmt.Sprintf("aliases=%v firstParty=%d agent=%t workflow=%t steps=%v",
		f.aliasModels, f.firstParty, f.agentToolSeen, f.workflowSeen, f.requestSteps)
}

func (f *liveClaudeConformanceFixture) assertComplete(t *testing.T, out, errOut string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.aliasModels["fixture-full-model"] == 0 || f.aliasModels["fixture-chat-model"] == 0 ||
		f.firstParty == 0 || !f.agentToolSeen || !f.workflowSeen {
		t.Fatalf("live conformance fixture incomplete: aliases=%v firstParty=%d agent=%v workflow=%v\nstdout:\n%s\nstderr:\n%s",
			f.aliasModels, f.firstParty, f.agentToolSeen, f.workflowSeen, out, errOut)
	}
}
