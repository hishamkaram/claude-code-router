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

func TestLiveLaunchAnthropicCompatibleProviderAutoModePluginResearchAgent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}
	pluginDir := configureLiveResearchPlugin(t)

	state := &liveAnthropicToolSearchAgentState{}
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
	for _, args := range [][]string{
		{"--db", dbPath, "provider", "add", "zai", "--base-url", provider.URL, "--no-api-key"},
		{"--db", dbPath, "model", "add", "glm", "--provider", "zai", "--model", "glm-4.7"},
	} {
		out, errOut, err := runLiveCommand(ctx, Dependencies{}, args...)
		if err != nil {
			t.Fatalf("run %v error = %v\nstdout:\n%s\nstderr:\n%s", args, err, out, errOut)
		}
	}

	prompt := `/ccr-live-plugin:research Find latest ChatGPT news`
	deps := Dependencies{
		In: strings.NewReader(prompt + "\n"), Launcher: livePluginClaudeLauncher{pluginDir: pluginDir},
		StartGateway: classifier.StartGateway,
	}
	out, errOut, err := runLiveCommand(ctx, deps, "--db", dbPath, "launch", "--model", "glm", "--print", "--auth-mode", "preserve", "--permission-mode", "auto")
	if err != nil {
		t.Fatalf("launch error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if !strings.Contains(out, liveToolSearchAgentResult) {
		t.Fatalf("launch output missing completed agent response:\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	state.assertComplete(t, out, errOut, classifier.Seen())
	assertLiveAgentVisibility(t, ctx, dbPath)
}

type liveAnthropicToolSearchAgentState struct {
	mu                      sync.Mutex
	messageCalls            int
	toolSearchSeen          bool
	toolReferenceResultSeen bool
	classifierRequestSeen   bool
	childPromptSeen         bool
	callerAgentResultSeen   bool
}

type liveAnthropicMessagePayload struct {
	Model         string          `json:"model"`
	MaxTokens     int             `json:"max_tokens"`
	Temperature   *float64        `json:"temperature"`
	StopSequences []string        `json:"stop_sequences"`
	Stream        bool            `json:"stream"`
	System        json.RawMessage `json:"system"`
	Tools         []struct {
		Name string `json:"name"`
	} `json:"tools"`
	Messages []struct {
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

func (s *liveAnthropicToolSearchAgentState) handle(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	switch r.URL.Path {
	case "/v1/messages/count_tokens":
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"input_tokens":3}`)
	case "/v1/messages":
		s.handleMessage(t, w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *liveAnthropicToolSearchAgentState) handleMessage(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	var payload liveAnthropicMessagePayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		t.Errorf("provider decode error: %v", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messageCalls++
	if isLiveAnthropicAutoClassifierRequest(payload) {
		s.classifierRequestSeen = true
		writeLiveAnthropicClassifierResponse(w, payload)
		return
	}
	switch {
	case !s.toolSearchSeen:
		s.toolSearchSeen = liveAnthropicToolsContain(payload.Tools, "ToolSearch")
		writeLiveAnthropicToolCall(w, payload.Model, "toolu_anthropic_toolsearch", "ToolSearch", map[string]string{"query": "Agent"})
	case !s.toolReferenceResultSeen && liveAnthropicMessagesContain(payload.Messages, `"type":"tool_reference"`) && liveAnthropicMessagesContain(payload.Messages, `"tool_name":"Agent"`):
		s.toolReferenceResultSeen = true
		writeLiveAnthropicToolCall(w, payload.Model, "toolu_anthropic_agent", "Agent", map[string]any{
			"description":       "research latest ChatGPT news",
			"prompt":            "Find latest ChatGPT news using web research and return concise findings.",
			"subagent_type":     "ccr-live-plugin:investigating-researcher",
			"run_in_background": false,
		})
	case !s.childPromptSeen && liveAnthropicMessagesContain(payload.Messages, "Find latest ChatGPT news"):
		s.childPromptSeen = true
		writeLiveAnthropicStream(w, payload.Model, liveToolSearchAgentResult)
	case liveAnthropicMessagesContain(payload.Messages, liveToolSearchAgentResult):
		s.callerAgentResultSeen = true
		writeLiveAnthropicStream(w, payload.Model, liveToolSearchAgentResult)
	default:
		t.Errorf("unexpected provider request in Anthropic research Agent route: %#v", payload.Messages)
		http.Error(w, "unexpected request", http.StatusBadRequest)
	}
}

func (s *liveAnthropicToolSearchAgentState) assertComplete(t *testing.T, out, errOut string, firstPartyClassifierSeen bool) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.toolSearchSeen || !s.toolReferenceResultSeen || (!s.classifierRequestSeen && !firstPartyClassifierSeen) || !s.childPromptSeen {
		t.Fatalf("Anthropic research Agent live route incomplete: toolSearchSeen=%v toolReferenceResultSeen=%v selectedClassifierSeen=%v firstPartyClassifierSeen=%v childPromptSeen=%v callerAgentResultSeen=%v messageCalls=%d\nstdout:\n%s\nstderr:\n%s", s.toolSearchSeen, s.toolReferenceResultSeen, s.classifierRequestSeen, firstPartyClassifierSeen, s.childPromptSeen, s.callerAgentResultSeen, s.messageCalls, out, errOut)
	}
}

func liveAnthropicToolsContain(tools []struct {
	Name string `json:"name"`
}, want string,
) bool {
	for _, tool := range tools {
		if tool.Name == want {
			return true
		}
	}
	return false
}

func liveAnthropicMessagesContain(messages []struct {
	Content json.RawMessage `json:"content"`
}, want string,
) bool {
	for _, message := range messages {
		if strings.Contains(string(message.Content), want) {
			return true
		}
	}
	return false
}

func writeLiveAnthropicToolCall(w http.ResponseWriter, model, toolID, name string, input any) {
	arguments, err := json.Marshal(input)
	if err != nil {
		http.Error(w, "encoding tool arguments", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_live\",\"type\":\"message\",\"role\":\"assistant\",\"model\":%q,\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":2,\"output_tokens\":0}}}\n\n", model)
	_, _ = fmt.Fprintf(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":%q,\"name\":%q,\"input\":{}}}\n\n", toolID, name)
	_, _ = fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":%q}}\n\n", string(arguments))
	_, _ = fmt.Fprint(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
	_, _ = fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":2}}\n\n")
	_, _ = fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}
