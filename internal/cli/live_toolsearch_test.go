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

func TestLiveLaunchOpenAIProviderAutoModePluginResearchAgent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}
	pluginDir := configureLiveResearchPlugin(t)

	state := &liveToolSearchAgentState{}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state.handle(t, w, r)
	}))
	defer provider.Close()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	addLiveOpenAIModel(t, ctx, dbPath, provider.URL)

	prompt := `/ccr-live-plugin:research Find latest ChatGPT news`
	deps := Dependencies{In: strings.NewReader(prompt + "\n"), Launcher: livePluginClaudeLauncher{pluginDir: pluginDir}}
	out, errOut, err := runLiveCommand(ctx, deps, "--db", dbPath, "launch", "--model", "gpt", "--print", "--auth-mode", "gateway-token", "--permission-mode", "auto")
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
	classifierRequestSeen     bool
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
	case isLiveAutoClassifierRequest(payload):
		s.classifierRequestSeen = true
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-toolsearch-classifier","choices":[{"message":{"content":"<block>no</block>"},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":3}}`)
	case s.chatCalls == 1:
		s.firstRequestHadToolSearch = liveToolsContain(payload.Tools, "ToolSearch")
		writeLiveToolSearchCall(w)
	case !s.toolReferenceResultSeen && openAIMessagesContainToolRole(payload.Messages, "[Loaded tool: Agent]"):
		s.toolReferenceResultSeen = true
		writeLiveResearchAgentCall(w)
	case !s.childPromptSeen && openAIMessagesContain(payload.Messages, "Find latest ChatGPT news"):
		s.childPromptSeen = true
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-toolsearch-child","choices":[{"message":{"content":"CCR_LIVE_TOOLSEARCH_CHILD_OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`)
	case openAIMessagesContainToolRole(payload.Messages, "CCR_LIVE_TOOLSEARCH_CHILD_OK"):
		s.parentAgentToolResultSeen = true
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-toolsearch-parent","choices":[{"message":{"content":"CCR_LIVE_TOOLSEARCH_PARENT_OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`)
	default:
		t.Errorf("unexpected provider request in research Agent live route: %#v", payload.Messages)
		http.Error(w, "unexpected request", http.StatusBadRequest)
	}
}

func writeLiveToolSearchCall(w http.ResponseWriter) {
	writeLiveToolCall(w, "chatcmpl-toolsearch", "toolu_toolsearch_live", "ToolSearch", map[string]string{"query": "Agent"})
}

func writeLiveResearchAgentCall(w http.ResponseWriter) {
	writeLiveToolCall(w, "chatcmpl-toolsearch-agent", "toolu_agent_after_toolsearch", "Agent", map[string]any{
		"description":       "research latest ChatGPT news",
		"prompt":            "Find latest ChatGPT news using web research and return concise findings.",
		"subagent_type":     "ccr-live-plugin:investigating-researcher",
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
	if !s.firstRequestHadToolSearch || !s.toolReferenceResultSeen || !s.classifierRequestSeen || !s.childPromptSeen || !s.parentAgentToolResultSeen {
		t.Fatalf("Research Agent live route incomplete: firstRequestHadToolSearch=%v toolReferenceResultSeen=%v classifierRequestSeen=%v childPromptSeen=%v parentAgentToolResultSeen=%v chatCalls=%d\nstdout:\n%s\nstderr:\n%s", s.firstRequestHadToolSearch, s.toolReferenceResultSeen, s.classifierRequestSeen, s.childPromptSeen, s.parentAgentToolResultSeen, s.chatCalls, out, errOut)
	}
}

func configureLiveResearchPlugin(t *testing.T) string {
	t.Helper()
	workspace := t.TempDir()
	pluginDir := filepath.Join(workspace, "ccr-live-plugin")
	manifestDir := filepath.Join(pluginDir, ".claude-plugin")
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		t.Fatalf("creating live plugin manifest directory: %v", err)
	}
	manifest := `{"name":"ccr-live-plugin","description":"CCR live research plugin","version":"1.0.0"}`
	if err := os.WriteFile(filepath.Join(manifestDir, "plugin.json"), []byte(manifest), 0o600); err != nil {
		t.Fatalf("writing live plugin manifest: %v", err)
	}
	agentsDir := filepath.Join(pluginDir, "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatalf("creating live plugin agent directory: %v", err)
	}
	agent := `---
name: investigating-researcher
description: Research current news using web sources
tools: WebSearch, WebFetch
model: inherit
---
Research the requested topic and report concise findings.
`
	if err := os.WriteFile(filepath.Join(agentsDir, "investigating-researcher.md"), []byte(agent), 0o600); err != nil {
		t.Fatalf("writing live plugin agent: %v", err)
	}
	skillDir := filepath.Join(pluginDir, "skills", "research")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("creating live plugin skill directory: %v", err)
	}
	skill := `---
description: Research current news with the plugin's investigating researcher
---
Use the Agent tool to launch ccr-live-plugin:investigating-researcher with this task: "$ARGUMENTS".
Wait for the agent, then reply exactly CCR_LIVE_TOOLSEARCH_PARENT_OK.
`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skill), 0o600); err != nil {
		t.Fatalf("writing live plugin skill: %v", err)
	}
	t.Chdir(workspace)
	return pluginDir
}

type livePluginClaudeLauncher struct {
	pluginDir string
}

func (l livePluginClaudeLauncher) Start(ctx context.Context, args, env []string, in io.Reader, out, errOut io.Writer) (ClaudeProcess, error) {
	pluginArgs := append([]string{}, args...)
	pluginArgs = append(pluginArgs, "--plugin-dir", l.pluginDir)
	return (ExecClaudeLauncher{}).Start(ctx, pluginArgs, env, in, out, errOut)
}
