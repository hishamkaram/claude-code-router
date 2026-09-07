//go:build live

package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/modelcap"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

// This deliberately uses the real 20s/15s policy and a 60s read-idle cutoff.
// The fast tests cover the negative control; this gate proves the Claude CLI
// remains alive across a 90s upstream silence using the production constructor.
func TestLiveHTTP2IdleClaudeCLI(t *testing.T) {
	if os.Getenv("CCR_LIVE_HTTP2_IDLE") != "1" {
		t.Skip("set CCR_LIVE_HTTP2_IDLE=1 for the 90-second HTTP/2 Claude CLI regressions")
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Fatal("required Claude CLI unavailable")
	}
	for _, phase := range []string{"before_headers", "between_events", "partial_tool_failure"} {
		t.Run(phase, func(t *testing.T) { runIdleClaudeCLI(t, claude, phase) })
	}
}

func runIdleClaudeCLI(t *testing.T, claude, phase string) {
	t.Helper()
	imageData := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
	imagePath := filepath.Join(t.TempDir(), "fixture.png")
	imageBytes, err := base64.StdEncoding.DecodeString(imageData)
	if err != nil {
		t.Fatal(err)
	}
	if writeErr := os.WriteFile(imagePath, imageBytes, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	fixture := &idleClaudeFixture{phase: phase, imagePath: imagePath}
	upstream := httptest.NewUnstartedServer(fixture)
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	defer upstream.Close()
	transport, err := newUpstreamTransport(upstream.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	transport.base.ForceAttemptHTTP2 = true
	transport.base.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return idleReadConn{Conn: conn, idle: 60 * time.Second}, nil
	}
	defer transport.CloseIdleConnections()
	db := newGatewayStore(t, liveIdleProvider(upstream.URL), liveIdleModel())
	gateway := startGatewayWithConfig(t, context.Background(), Config{Store: db, Token: "fixture-token", DefaultModelAlias: "fixture", HTTPClient: &http.Client{Transport: transport}})
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = gateway.Shutdown(ctx)
	}()
	tap, probe := newIdleDownstreamProbe(t, gateway.URL())
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, claude, "--print", "--model", "anthropic.ccr.fixture", "--output-format", "stream-json", "--verbose", "--tools", "Read", "--permission-mode", "bypassPermissions", "--input-format", "stream-json")
	input, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "image", "source": map[string]string{"type": "base64", "media_type": "image/png", "data": imageData}}, map[string]string{"type": "text", "text": "Read the fixture image and reply CCR_HTTP2_IDLE_OK."}}}})
	cmd.Stdin = bytes.NewReader(append(input, '\n'))
	cmd.Dir = t.TempDir()
	cmd.Env = idleClaudeEnvironment(t, tap.URL)
	var output, diagnostic bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &diagnostic
	started := time.Now()
	runErr := cmd.Run()
	if phase == "partial_tool_failure" {
		visibleFailure := strings.Contains(output.String(), "API Error") || strings.Contains(diagnostic.String(), "API Error") || strings.Contains(output.String(), `"is_error":true`)
		if ctx.Err() != nil || !visibleFailure || fixture.calls.Load() < 1 || fixture.toolResult.Load() || strings.Contains(output.String(), "CCR_HTTP2_IDLE_OK") {
			t.Fatalf("committed failure not visible: exit=%v calls=%d tool_result=%v diagnostic=%s", runErr, fixture.calls.Load(), fixture.toolResult.Load(), idleFixtureError(output.Bytes()))
		}
		t.Logf("visible terminal failure; provider_calls=%d non_streaming_calls=%d (outer Claude requests)", fixture.calls.Load(), fixture.nonStreamingCalls.Load())
		return
	}
	if runErr != nil {
		t.Fatalf("Claude idle fixture failed: %v (stdout=%d bytes stderr=%d bytes)", runErr, output.Len(), diagnostic.Len())
	}
	if !strings.Contains(output.String(), "CCR_HTTP2_IDLE_OK") {
		t.Fatalf("Claude did not complete sentinel (stdout=%d bytes stderr=%d bytes)", output.Len(), diagnostic.Len())
	}
	if elapsed := time.Since(started); elapsed < 90*time.Second {
		t.Fatalf("idle fixture was not exercised: %s", elapsed)
	}
	if !fixture.imageHistory.Load() || !fixture.toolResult.Load() || fixture.calls.Load() != 2 {
		t.Fatalf("tool/image history not preserved: calls=%d image=%v tool=%v", fixture.calls.Load(), fixture.imageHistory.Load(), fixture.toolResult.Load())
	}
	probe.assertLiveness(t)
	t.Logf("phase=%s elapsed=%s provider_calls=%d; outer CLI calls are distinct from transport retries", phase, time.Since(started).Round(time.Millisecond), fixture.calls.Load())
}

func waitIdleFixture(ctx context.Context) bool {
	timer := time.NewTimer(90 * time.Second)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func idleClaudeEnvironment(t *testing.T, url string) []string {
	t.Helper()
	overrides := map[string]string{"ANTHROPIC_BASE_URL": url, "ANTHROPIC_API_KEY": "fixture-token", "ANTHROPIC_AUTH_TOKEN": "", "CLAUDE_CODE_OAUTH_TOKEN": "", "CLAUDE_CONFIG_DIR": t.TempDir(), "CLAUDE_CODE_MAX_RETRIES": "1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1", "CLAUDE_CODE_DISABLE_AUTO_MEMORY": "1", "CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL": "1"}
	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, ok := overrides[key]; !ok {
			env = append(env, entry)
		}
	}
	for key, value := range overrides {
		env = append(env, key+"="+value)
	}
	return env
}

func liveIdleProvider(url string) store.Provider {
	return store.Provider{Name: "fixture", Type: "litellm", BaseURL: url, SupportsTools: true, SupportsStreaming: true, SupportsThinking: true}
}

func liveIdleModel() store.Model {
	supported := true
	return store.Model{Alias: "fixture", ProviderName: "fixture", ProviderModel: "fixture-model", Status: "degraded", CapabilityOverrides: modelcap.Values{SupportsVision: &supported, InputModalities: []string{"text", "image"}}}
}

func idleFixtureError(raw []byte) string {
	for _, line := range bytes.Split(raw, []byte("\n")) {
		var event struct {
			Type    string `json:"type"`
			IsError bool   `json:"is_error"`
			Result  string `json:"result"`
		}
		if json.Unmarshal(line, &event) == nil && event.Type == "result" && event.IsError {
			if len(event.Result) > 300 {
				return event.Result[:300]
			}
			return event.Result
		}
	}
	return "no structured error"
}

type idleClaudeFixture struct {
	phase, imagePath                  string
	imageHistory, toolResult, delayed atomic.Bool
	calls, nonStreamingCalls          atomic.Int32
}

func (f *idleClaudeFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls.Add(1)
	var payload struct {
		Stream   bool            `json:"stream"`
		Messages json.RawMessage `json:"messages"`
	}
	if decodeErr := json.NewDecoder(r.Body).Decode(&payload); decodeErr != nil {
		http.Error(w, "invalid fixture request", 400)
		return
	}
	if bytes.Contains(payload.Messages, []byte(`"role":"tool"`)) {
		f.toolResult.Store(true)
	}
	if !payload.Stream {
		f.nonStreamingCalls.Add(1)
		if f.phase == "partial_tool_failure" {
			http.Error(w, "fixture remains failed", http.StatusBadGateway)
			return
		}
	}
	wait := !f.delayed.Swap(true)
	if f.phase == "partial_tool_failure" {
		wait = true
	}
	if !wait {
		f.imageHistory.Store(bytes.Contains(payload.Messages, []byte("data:image/png;base64,")))
	}
	if wait && f.phase == "before_headers" && !waitIdleFixture(r.Context()) {
		return
	}
	if wait && payload.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n")
		w.(http.Flusher).Flush()
		if f.phase == "between_events" && !waitIdleFixture(r.Context()) {
			return
		}
		if f.phase == "partial_tool_failure" {
			_, _ = fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_image","type":"function","function":{"name":"Read","arguments":"{\"file_path\":"}}]}}]}`+"\n\n")
			w.(http.Flusher).Flush()
			return
		}
		arguments, _ := json.Marshal(map[string]string{"file_path": f.imagePath})
		event := map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call_image", "type": "function", "function": map[string]any{"name": "Read", "arguments": string(arguments)}}}}, "finish_reason": "tool_calls"}}}
		encoded, _ := json.Marshal(event)
		_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", encoded)
		return
	}
	if payload.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"idle-fixture\",\"model\":\"fixture-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"CCR_HTTP2_\"}}]}\n\n")
		w.(http.Flusher).Flush()
		if wait && f.phase == "between_events" && !waitIdleFixture(r.Context()) {
			return
		}
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"IDLE_OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		return
	}
	_, _ = fmt.Fprint(w, `{"id":"idle-fixture","model":"fixture-model","choices":[{"message":{"role":"assistant","content":"CCR_HTTP2_IDLE_OK"},"finish_reason":"stop"}]}`)
}
