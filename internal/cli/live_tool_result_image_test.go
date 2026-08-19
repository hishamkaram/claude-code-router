//go:build live

package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

	"github.com/hishamkaram/claude-code-router/internal/gateway"
	"github.com/hishamkaram/claude-code-router/internal/liveclaude"
)

const (
	liveImageToolName   = "image"
	liveImageToolResult = "CCR_LIVE_IMAGE_OK"
	liveImagePNGData    = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
)

func TestLiveClaudeOpenAIImageToolResultSurvivesModelSwitch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
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
	isolatedHome := t.TempDir()
	t.Setenv("HOME", isolatedHome)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(isolatedHome, ".claude"))

	fixture := newLiveImageToolFixture(t)
	defer fixture.Close()
	mcpConfig := writeLiveImageMCPConfig(t)
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	for _, args := range [][]string{
		{"--db", dbPath, "provider", "add", "litellm", "--base-url", fixture.URL(), "--no-api-key", "--mode", "full"},
		{"--db", dbPath, "model", "add", "image-chat", "--provider", "litellm", "--model", "image-chat-model", "--compat", "full"},
		{"--db", dbPath, "model", "update", "image-chat", "--input-modalities", "text,image", "--vision", "true"},
	} {
		if out, errOut, err := runLiveCommand(ctx, Dependencies{}, args...); err != nil {
			t.Fatalf("run %v error = %v\nstdout:\n%s\nstderr:\n%s", args, err, out, errOut)
		}
	}

	deps := Dependencies{
		In: strings.NewReader(liveStreamInput(
			t,
			"/model sonnet",
			"/model anthropic.ccr.image-chat",
			"Use the fixture image tool, then reply exactly "+liveImageToolResult+".",
		)),
		StartGateway: fixture.StartGateway,
	}
	out, errOut, err := runLiveCommand(
		ctx, deps,
		"--db", dbPath, "launch", "--print",
		"--auth-mode", "preserve", "--permission-mode", "auto",
		"--mcp-config", mcpConfig,
		"--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
	)
	if err != nil {
		t.Fatalf("image tool-result model-switch launch error = %v\nfixture=%s\nstdout:\n%s\nstderr:\n%s", err, fixture.Diagnostic(), out, errOut)
	}
	combined := out + "\n" + errOut
	if !strings.Contains(combined, liveImageToolResult) {
		t.Fatalf("live image tool-result launch output missing %q\nstdout:\n%s\nstderr:\n%s", liveImageToolResult, out, errOut)
	}
	for _, unexpected := range []string{"image tool_result content is not supported", "API Error: 501", "status 501"} {
		if strings.Contains(strings.ToLower(combined), strings.ToLower(unexpected)) {
			t.Fatalf("live image tool-result launch output contains %q\nstdout:\n%s\nstderr:\n%s", unexpected, out, errOut)
		}
	}
	fixture.AssertImageToolResult(t)
}

type liveImageToolFixture struct {
	server *httptest.Server

	mu              sync.Mutex
	chatCalls       int
	toolName        string
	toolSearchSeen  bool
	toolCallSeen    bool
	imageModelSeen  bool
	imageResultSeen bool
	fixtureError    string
}

func newLiveImageToolFixture(t *testing.T) *liveImageToolFixture {
	t.Helper()
	fixture := &liveImageToolFixture{}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.handle(t, w, r)
	}))
	return fixture
}

func (f *liveImageToolFixture) URL() string {
	return f.server.URL
}

func (f *liveImageToolFixture) Close() {
	f.server.Close()
}

func (f *liveImageToolFixture) StartGateway(ctx context.Context, cfg gateway.Config) (*gateway.Server, error) {
	cfg.AnthropicBaseURL = f.URL()
	return gateway.Start(ctx, cfg)
}

func (f *liveImageToolFixture) handle(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	switch r.URL.Path {
	case "/v1/models":
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":[{"id":"image-chat-model"}]}`)
	case "/v1/messages/count_tokens":
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"input_tokens":7}`)
	case "/v1/messages":
		f.handleFirstParty(w, r)
	case "/v1/chat/completions":
		f.handleOpenAI(t, w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *liveImageToolFixture) handleFirstParty(w http.ResponseWriter, r *http.Request) {
	var payload liveAnthropicMessagePayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "bad first-party request", http.StatusBadRequest)
		return
	}
	if isLiveAnthropicAutoClassifierRequest(payload) {
		writeLiveAnthropicClassifierResponse(w, payload)
		return
	}
	if payload.Stream {
		writeLiveAnthropicStream(w, payload.Model, "first-party-live-ok")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"id":"msg_live_image_first_party","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":"first-party-live-ok"}],"stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":2}}`, payload.Model)
}

func (f *liveImageToolFixture) handleOpenAI(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	var payload liveImageOpenAIRequest
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		f.setError(fmt.Sprintf("decode OpenAI request: %v", err))
		http.Error(w, "bad OpenAI request", http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	f.chatCalls++
	if payload.Model == "image-chat-model" {
		f.imageModelSeen = true
	}
	toolName := f.toolName
	if toolName == "" {
		toolName = findLiveImageTool(payload.Tools)
	}
	toolSearchName := findLiveToolByName(payload.Tools, "ToolSearch")
	toolSearchCall := toolName == "" && toolSearchName != "" && !f.toolSearchSeen
	if toolSearchCall {
		toolName = "mcp__fixture__" + liveImageToolName
		f.toolName = toolName
		f.toolSearchSeen = true
	}
	toolCallSeen := f.toolCallSeen
	f.mu.Unlock()

	if toolSearchCall {
		writeLiveImageToolSearchCall(w, payload.Model, toolSearchName)
		return
	}

	if !toolCallSeen {
		if toolName == "" {
			if len(payload.Tools) == 0 {
				writeLiveImageTextResponse(w, payload.Model)
				return
			}
			f.setError(fmt.Sprintf("OpenAI request did not expose the fixture image MCP tool: %#v", payload.Tools))
			http.Error(w, "fixture image tool missing", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.toolCallSeen = true
		f.mu.Unlock()
		writeLiveImageToolCall(w, payload.Model, toolName)
		return
	}

	if !liveOpenAIImageToolResultConverted(payload.Messages) {
		f.setError("OpenAI request after MCP image tool call did not contain a trailing image user message")
		http.Error(w, "converted image tool result missing", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.imageResultSeen = true
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"id":"chatcmpl_live_image","choices":[{"message":{"content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":3}}`, liveImageToolResult)
}

func (f *liveImageToolFixture) setError(message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fixtureError == "" {
		f.fixtureError = message
	}
}

func (f *liveImageToolFixture) AssertImageToolResult(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fixtureError != "" {
		t.Fatalf("image tool-result fixture error: %s", f.fixtureError)
	}
	if f.chatCalls < 2 || !f.imageModelSeen || !f.toolCallSeen || !f.imageResultSeen {
		t.Fatalf("image tool-result fixture calls=%d imageModelSeen=%v tool=%q toolCallSeen=%v imageResultSeen=%v", f.chatCalls, f.imageModelSeen, f.toolName, f.toolCallSeen, f.imageResultSeen)
	}
}

func (f *liveImageToolFixture) Diagnostic() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fmt.Sprintf("calls=%d imageModelSeen=%v tool=%q toolCallSeen=%v imageResultSeen=%v error=%q", f.chatCalls, f.imageModelSeen, f.toolName, f.toolCallSeen, f.imageResultSeen, f.fixtureError)
}

type liveImageOpenAITool struct {
	Name     string `json:"name"`
	Function struct {
		Name string `json:"name"`
	} `json:"function"`
}

type liveImageOpenAIMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type liveImageOpenAIRequest struct {
	Model    string                   `json:"model"`
	Tools    []liveImageOpenAITool    `json:"tools"`
	Messages []liveImageOpenAIMessage `json:"messages"`
}

func findLiveImageTool(tools []liveImageOpenAITool) string {
	for _, tool := range tools {
		name := tool.Name
		if name == "" {
			name = tool.Function.Name
		}
		if strings.Contains(strings.ToLower(name), "mcp__") && strings.Contains(strings.ToLower(name), "fixture") {
			return name
		}
	}
	return ""
}

func findLiveToolByName(tools []liveImageOpenAITool, want string) string {
	for _, tool := range tools {
		name := tool.Name
		if name == "" {
			name = tool.Function.Name
		}
		if name == want {
			return name
		}
	}
	return ""
}

func liveOpenAIImageToolResultConverted(messages []liveImageOpenAIMessage) bool {
	toolIndex := -1
	imageIndex := -1
	for index, message := range messages {
		if message.Role == "tool" && bytes.Contains(message.Content, []byte("[image output]")) {
			toolIndex = index
		}
		if message.Role == "user" &&
			bytes.Contains(message.Content, []byte(`"type":"image_url"`)) &&
			bytes.Contains(message.Content, []byte("data:image/png;base64,"+liveImagePNGData)) {
			imageIndex = index
		}
	}
	return toolIndex >= 0 && imageIndex > toolIndex
}

func writeLiveImageToolCall(w http.ResponseWriter, model, name string) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": "chatcmpl_live_image_tool",
		"choices": []map[string]any{{
			"message": map[string]any{
				"content": "",
				"tool_calls": []map[string]any{{
					"id":   "toolu_live_image",
					"type": "function",
					"function": map[string]string{
						"name":      name,
						"arguments": "{}",
					},
				}},
			},
			"finish_reason": "tool_calls",
		}},
		"model": model,
	})
}

func writeLiveImageTextResponse(w http.ResponseWriter, model string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"id":"chatcmpl_live_image_text","choices":[{"message":{"content":"model-switch-live-ok"},"finish_reason":"stop"}],"model":%q}`, model)
}

func writeLiveImageToolSearchCall(w http.ResponseWriter, model, name string) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": "chatcmpl_live_tool_search",
		"choices": []map[string]any{{
			"message": map[string]any{
				"content": "",
				"tool_calls": []map[string]any{{
					"id":   "toolu_live_tool_search",
					"type": "function",
					"function": map[string]string{
						"name":      name,
						"arguments": `{"query":"fixture image"}`,
					},
				}},
			},
			"finish_reason": "tool_calls",
		}},
		"model": model,
	})
}

func writeLiveImageMCPConfig(t *testing.T) string {
	t.Helper()
	serverBinary, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatalf("resolve live test binary: %v", err)
	}
	serverScript := filepath.Join(t.TempDir(), "mcp-image-server")
	script := "#!/bin/sh\nCCR_LIVE_IMAGE_MCP_SERVER=1 exec " + quoteLiveShellArg(serverBinary) + " -test.run='^TestLiveImageMCPServerProcess$'\n"
	if err := os.WriteFile(serverScript, []byte(script), 0o700); err != nil {
		t.Fatalf("write MCP server launcher: %v", err)
	}
	configPath := filepath.Join(t.TempDir(), "mcp.json")
	config := map[string]any{
		"mcpServers": map[string]any{
			"fixture": map[string]any{
				"command": serverScript,
			},
		},
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("encode MCP config: %v", err)
	}
	if err := os.WriteFile(configPath, encoded, 0o600); err != nil {
		t.Fatalf("write MCP config: %v", err)
	}
	return configPath
}

func TestLiveImageMCPServerProcess(t *testing.T) {
	if os.Getenv("CCR_LIVE_IMAGE_MCP_SERVER") != "1" {
		return
	}

	decoder := json.NewDecoder(bufio.NewReader(os.Stdin))
	encoder := json.NewEncoder(os.Stdout)
	for {
		var request liveMCPRequest
		if err := decoder.Decode(&request); err != nil {
			if errors.Is(err, io.EOF) {
				// The child process owns stdout as an MCP JSON-RPC stream. Exit
				// before the Go test harness can print its PASS line there.
				os.Exit(0)
			}
			t.Fatalf("decode MCP request: %v", err)
		}
		switch request.Method {
		case "initialize":
			var params struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			_ = json.Unmarshal(request.Params, &params)
			version := params.ProtocolVersion
			if version == "" {
				version = "2024-11-05"
			}
			if err := writeLiveMCPResponse(encoder, request.ID, map[string]any{
				"protocolVersion": version,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]string{"name": "ccr-image-fixture", "version": "1.0.0"},
			}); err != nil {
				t.Fatalf("write MCP initialize response: %v", err)
			}
		case "notifications/initialized", "notifications/cancelled":
			continue
		case "ping":
			if err := writeLiveMCPResponse(encoder, request.ID, map[string]any{}); err != nil {
				t.Fatalf("write MCP ping response: %v", err)
			}
		case "tools/list":
			if err := writeLiveMCPResponse(encoder, request.ID, map[string]any{
				"tools": []map[string]any{{
					"name":        liveImageToolName,
					"description": "Return a deterministic screenshot image for CCR live tests.",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
				}},
			}); err != nil {
				t.Fatalf("write MCP tools/list response: %v", err)
			}
		case "tools/call":
			if err := writeLiveMCPResponse(encoder, request.ID, map[string]any{
				"content": []map[string]any{{
					"type":     "image",
					"data":     liveImagePNGData,
					"mimeType": "image/png",
				}},
				"isError": false,
			}); err != nil {
				t.Fatalf("write MCP tools/call response: %v", err)
			}
		default:
			if len(request.ID) == 0 {
				continue
			}
			if err := writeLiveMCPError(encoder, request.ID, -32601, "method not found"); err != nil {
				t.Fatalf("write MCP error response: %v", err)
			}
		}
	}
}

type liveMCPRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func writeLiveMCPResponse(encoder *json.Encoder, id json.RawMessage, result any) error {
	return encoder.Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	})
}

func writeLiveMCPError(encoder *json.Encoder, id json.RawMessage, code int, message string) error {
	return encoder.Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	})
}
