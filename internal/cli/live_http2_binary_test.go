//go:build live

package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/store"
)

type http2RouteTiming struct {
	Operation       string `json:"operation"`
	TransportReason string `json:"transport_reason"`
	CompletedAt     string `json:"completed_at"`
	RequestID       string `json:"request_id"`
	Status          string `json:"status"`
	FirstResponseMS int64  `json:"first_response_ms"`
	CompletionMS    int64  `json:"completion_ms"`
}

type http2BinaryCase struct {
	CancellationAt     string             `json:"cancellation_at,omitempty"`
	Timings            []http2RouteTiming `json:"route_timings"`
	Alias              string             `json:"alias"`
	Scenario           string             `json:"scenario"`
	BinarySHA256       string             `json:"binary_sha256"`
	ClaudePIDs         []int              `json:"claude_pids"`
	ExpectedDeployment string             `json:"expected_deployment"`
	CCRPID             int                `json:"ccr_pid"`
	StartedAt          string             `json:"started_at"`
	ElapsedMS          int64              `json:"elapsed_ms"`
	ResultCount        int                `json:"result_count"`
	ToolNames          []string           `json:"tool_names"`
	RequestIDs         []string           `json:"request_ids"`
	TransportReasons   []string           `json:"transport_reasons"`
	Succeeded          bool               `json:"succeeded"`
}

func TestLiveHTTP2BuiltBinary(t *testing.T) {
	if os.Getenv("CCR_LIVE_HTTP2_BINARY") == "" {
		t.Skip("set CCR_LIVE_HTTP2_BINARY to the candidate CCR executable")
	}
	binary, err := filepath.Abs(os.Getenv("CCR_LIVE_HTTP2_BINARY"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if _, lookupErr := exec.LookPath("claude"); lookupErr != nil {
		t.Fatal("required Claude CLI unavailable")
	}
	source, err := store.OpenReadOnly(context.Background(), configuredLiveDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	aliases := []string{"litellm-grok-4-6", "litellm-gpt-5-6-terra"}
	for _, scenario := range []string{"reasoning", "image_tool", "multi_turn_agent"} {
		t.Run(scenario, func(t *testing.T) {
			for _, alias := range aliases {
				dbPath := isolateHTTP2BinaryDB(t, source, alias)
				deployment := http2ExpectedDeployment(t, source, alias)
				t.Run(alias, func(t *testing.T) {
					t.Parallel()
					runHTTP2BinaryCase(t, binary, dbPath, http2BinaryCase{Alias: alias, Scenario: scenario, BinarySHA256: hex.EncodeToString(sum[:]), ExpectedDeployment: deployment})
				})
			}
		})
	}
}

func http2ExpectedDeployment(t *testing.T, source *store.Store, alias string) string {
	t.Helper()
	raw, err := os.ReadFile(os.Getenv("CCR_LIVE_HTTP2_EXPECTED_DEPLOYMENTS"))
	if err != nil {
		t.Fatal("required expected-deployment evidence unavailable: set CCR_LIVE_HTTP2_EXPECTED_DEPLOYMENTS")
	}
	var expected map[string]struct {
		DeploymentID string `json:"deployment_id"`
	}
	if err := json.Unmarshal(raw, &expected); err != nil {
		t.Fatal(err)
	}
	model, err := source.GetModel(context.Background(), alias)
	if err != nil {
		t.Fatal(err)
	}
	deployment := expected[model.ProviderModel].DeploymentID
	if deployment == "" {
		t.Fatal("missing expected deployment for", alias)
	}
	return deployment
}

func isolateHTTP2BinaryDB(t *testing.T, source *store.Store, alias string) string {
	t.Helper()
	ctx := context.Background()
	model, err := source.GetModel(ctx, alias)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := source.GetProvider(ctx, model.ProviderName)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ccr.db")
	destination, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()
	if err := destination.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := destination.AddProvider(ctx, provider); err != nil {
		t.Fatal(err)
	}
	if err := destination.AddModel(ctx, model); err != nil {
		t.Fatal(err)
	}
	return path
}

func runHTTP2BinaryCase(t *testing.T, binary, dbPath string, evidence http2BinaryCase) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
	defer cancel()
	args := []string{"--db", dbPath, "launch", "--model", evidence.Alias, "--print", "--auth-mode", "provider-only", "--permission-mode", "default", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--effort", "high"}
	tools := ""
	if evidence.Scenario == "multi_turn_agent" {
		tools = "Agent"
		args = append(args, "--allowedTools", "Agent", "--forward-subagent-text")
	}
	if evidence.Scenario == "image_tool" {
		args = append(args, "--mcp-config", writeLiveImageMCPConfigWithData(t, http2ImageData(t)), "--strict-mcp-config", "--allowedTools", "mcp__fixture__image")
	}
	args = append(args, "--tools", tools)
	cmd := exec.CommandContext(ctx, binary, args...)
	if protectErr := protectHTTP2TestProcess(cmd); protectErr != nil {
		t.Fatal(protectErr)
	}
	cmd.Dir = t.TempDir()
	if evidence.Scenario == "multi_turn_agent" {
		prepareHTTP2AgentRepository(t, cmd.Dir)
	}
	cmd.Env = http2BinaryEnvironment(t)
	if evidence.Scenario == "multi_turn_agent" {
		cmd.Env = applyClaudeEnvironment(cmd.Env, ClaudeEnvironment{Set: []string{
			"CLAUDE_CODE_SUBAGENT_MODEL=anthropic.ccr." + evidence.Alias,
			"CLAUDE_CODE_SUBAGENT_MODEL_FORCE=1",
		}})
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	var input io.WriteCloser
	if evidence.Scenario == "multi_turn_agent" {
		var pipeErr error
		input, pipeErr = cmd.StdinPipe()
		if pipeErr != nil {
			t.Fatal(pipeErr)
		}
		defer input.Close()
		cmd.Stdout = &http2TurnWriter{output: &stdout, input: input, next: liveStreamInput(t, "Using the previous result, give one cancellation regression test. Do not call another tool. End with CCR_HTTP2_BINARY_OK.")}
	} else {
		cmd.Stdin = strings.NewReader(http2BinaryInput(t, evidence.Scenario))
	}
	cmd.Stderr = &stderr
	started := time.Now()
	evidence.StartedAt = started.UTC().Format(time.RFC3339Nano)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	evidence.CCRPID = cmd.Process.Pid
	if input != nil {
		if _, writeErr := io.WriteString(input, http2BinaryInput(t, evidence.Scenario)); writeErr != nil {
			cancel()
			_ = cmd.Wait()
			t.Fatal(writeErr)
		}
	}
	runErr := cmd.Wait()
	evidence.ElapsedMS = time.Since(started).Milliseconds()
	sentinel, toolNames, resultCount, resultError := inspectHTTP2BinaryOutput(stdout.Bytes())
	evidence.ToolNames = toolNames
	evidence.ResultCount = resultCount
	evidence.RequestIDs, evidence.TransportReasons, evidence.ClaudePIDs, evidence.Timings = readHTTP2BinaryTrace(t, dbPath)
	assertHTTP2BinaryHumanTrace(t, ctx, binary, dbPath)
	evidence.Succeeded = runErr == nil && !resultError && sentinel && resultCount > 0
	if evidence.Scenario == "image_tool" && !hasHTTP2ImageToolResult(stdout.Bytes()) {
		evidence.Succeeded = false
	}
	if evidence.Scenario == "multi_turn_agent" && (!hasHTTP2Tool(toolNames, "Agent") || resultCount != 2 || !hasHTTP2ChildResult(stdout.Bytes())) {
		evidence.Succeeded = false
	}
	protected := false
	identity := http2GenerationReceiptsMatch(evidence.Timings, evidence.ExpectedDeployment)
	for _, reason := range evidence.TransportReasons {
		if strings.Contains(reason, "protocol=h2; idle_ping=enabled") {
			protected = true
		}
	}
	evidence.Succeeded = evidence.Succeeded && protected && identity && len(evidence.ClaudePIDs) > 0
	writeHTTP2BinaryEvidence(t, evidence)
	if !evidence.Succeeded {
		t.Logf("output event metadata: %s", http2OutputMetadata(stdout.Bytes()))
		t.Fatalf("binary acceptance failed: alias=%s scenario=%s exit=%v results=%d result_error=%v sentinel=%v tools=%v protected=%v stdout_bytes=%d stderr_bytes=%d", evidence.Alias, evidence.Scenario, runErr, resultCount, resultError, sentinel, toolNames, protected, stdout.Len(), stderr.Len())
	}
	t.Logf("alias=%s scenario=%s elapsed_ms=%d ccr_pid=%d results=%d tools=%v", evidence.Alias, evidence.Scenario, evidence.ElapsedMS, evidence.CCRPID, resultCount, toolNames)
}

func assertHTTP2BinaryHumanTrace(t *testing.T, ctx context.Context, binary, dbPath string) {
	t.Helper()
	command := exec.CommandContext(ctx, binary, "--db", dbPath, "trace", "--limit", "1000")
	output, err := command.Output()
	if err != nil {
		t.Fatalf("candidate human trace failed: %v", err)
	}
	for _, required := range []string{"upstream_transport_policy", "http2_ping_after_ms=20000", "upstream_transport", "protocol=h2; idle_ping=enabled", "external="} {
		if !strings.Contains(string(output), required) {
			t.Fatalf("candidate human trace lacks %q", required)
		}
	}
}

func http2BinaryInput(t *testing.T, scenario string) string {
	t.Helper()
	switch scenario {
	case "reasoning":
		return liveStreamInput(t, "Develop a rigorous algorithm and correctness argument for an LRU cache with TTL expiry and concurrent readers. Explain the races and give pseudocode and at least five adversarial tests. Do not use tools. Finish with CCR_HTTP2_BINARY_OK.")
	case "multi_turn_agent":
		return liveStreamInput(t, "Use Agent exactly once with ONLY these arguments: description=return sentinel, subagent_type=general-purpose, prompt=Return exactly CCR_HTTP2_CHILD_OK without using any tools, run_in_background=false. Omit model and isolation so the child inherits the active model and current temporary directory. Do not request a worktree. After the child returns, reply exactly CCR_HTTP2_BINARY_OK. Do not run shell commands, edit files, or call Agent again.")
	case "image_tool":
		payload := map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []map[string]any{{"type": "image", "source": map[string]string{"type": "base64", "media_type": "image/png", "data": http2ImageData(t)}}, {"type": "text", "text": "Inspect this test image, then call the fixture image MCP tool exactly once to get another test image. Acknowledge both images and finish with CCR_HTTP2_BINARY_OK. Do not use shell or edit files."}}}, "parent_tool_use_id": nil}
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw) + "\n"
	default:
		t.Fatal("unknown binary acceptance scenario")
		return ""
	}
}

func http2BinaryEnvironment(t *testing.T) []string {
	t.Helper()
	overrides := map[string]string{"CLAUDE_CONFIG_DIR": t.TempDir(), "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1", "CLAUDE_CODE_DISABLE_AUTO_MEMORY": "1", "CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL": "1"}
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

func inspectHTTP2BinaryOutput(raw []byte) (bool, []string, int, bool) {
	sentinel := true
	var tools []string
	results := 0
	failed := false
	for _, line := range bytes.Split(raw, []byte("\n")) {
		var event struct {
			Type    string `json:"type"`
			IsError bool   `json:"is_error"`
			Message struct {
				Content []struct {
					Type    string `json:"type"`
					Name    string `json:"name"`
					Text    string `json:"text"`
					IsError bool   `json:"is_error"`
				} `json:"content"`
			} `json:"message"`
			Result string `json:"result"`
		}
		if json.Unmarshal(line, &event) != nil {
			continue
		}
		if event.Type == "result" {
			results++
			failed = failed || event.IsError
			sentinel = sentinel && strings.Contains(event.Result, "CCR_HTTP2_BINARY_OK")
		}
		if event.Type == "assistant" {
			for _, block := range event.Message.Content {
				if block.Type == "tool_use" {
					tools = append(tools, block.Name)
				}
			}
		}
		if event.Type == "user" {
			for _, block := range event.Message.Content {
				failed = failed || (block.Type == "tool_result" && block.IsError)
			}
		}
	}
	return sentinel, tools, results, failed
}

func hasHTTP2Tool(names []string, want string) bool {
	for _, name := range names {
		if name == want || strings.HasSuffix(name, "__"+want) {
			return true
		}
	}
	return false
}

func readHTTP2BinaryTrace(t *testing.T, dbPath string) ([]string, []string, []int, []http2RouteTiming) {
	t.Helper()
	db, err := store.OpenReadOnly(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	events, err := db.ListTraceEvents(context.Background(), store.TraceFilter{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	transportByRequest := make(map[string]string)
	for _, event := range events {
		if event.Kind == "lifecycle" && event.Lifecycle.Name == "upstream_transport" {
			transportByRequest[event.Lifecycle.ExternalID] = event.Lifecycle.Reason
		}
	}
	var requests, reasons []string
	var timings []http2RouteTiming
	for _, event := range events {
		if event.Kind == "route" {
			requests = append(requests, event.Route.RequestID)
			timings = append(timings, http2RouteTiming{Operation: event.Route.Operation, TransportReason: transportByRequest[event.Route.RequestID], RequestID: event.Route.RequestID, Status: event.Route.Status, FirstResponseMS: event.Route.Stream.FirstDownstreamEventMS, CompletionMS: event.Route.LatencyMS, CompletedAt: event.Route.CompletedAt})
		}
		if event.Kind == "lifecycle" && event.Lifecycle.Name == "upstream_transport" {
			reasons = append(reasons, event.Lifecycle.Reason)
		}
	}
	launches, err := db.ListLaunches(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	for _, launch := range launches {
		if launch.PID > 0 {
			pids = append(pids, launch.PID)
		}
	}
	return requests, reasons, pids, timings
}

func writeHTTP2BinaryEvidence(t *testing.T, evidence http2BinaryCase) {
	t.Helper()
	directory := os.Getenv("CCR_LIVE_HTTP2_EVIDENCE_DIR")
	if directory == "" {
		directory = t.TempDir()
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, fmt.Sprintf("%s-%s.json", evidence.Scenario, evidence.Alias))
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// http2TurnWriter sends the second user turn only after the first completed,
// rather than allowing Claude to coalesce queued user messages into one turn.
type http2TurnWriter struct {
	output  *bytes.Buffer
	pending []byte
	input   io.WriteCloser
	next    string
	sent    bool
}

func (w *http2TurnWriter) Write(p []byte) (int, error) {
	n, err := w.output.Write(p)
	if err != nil {
		return n, err
	}
	w.pending = append(w.pending, p...)
	for {
		index := bytes.IndexByte(w.pending, '\n')
		if index < 0 {
			break
		}
		var event struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(w.pending[:index], &event) == nil && event.Type == "result" && !w.sent {
			w.sent = true
			if _, writeErr := io.WriteString(w.input, w.next); writeErr != nil {
				return n, writeErr
			}
			if closeErr := w.input.Close(); closeErr != nil {
				return n, closeErr
			}
		}
		w.pending = w.pending[index+1:]
	}
	return n, nil
}

func http2ReceiptMatches(reason, expected string) bool {
	fields := make(map[string]string)
	for _, part := range strings.Split(reason, "; ") {
		key, value, ok := strings.Cut(part, "=")
		if ok {
			fields[key] = value
		}
	}
	return expected != "" && fields["owner"] == "ccr" && fields["protocol"] == "h2" && fields["idle_ping"] == "enabled" && fields["provider_model_id"] == expected && fields["provider_fallbacks"] == "0"
}

func TestHTTP2ReceiptAcceptanceRejectsFallback(t *testing.T) {
	base := "owner=ccr; protocol=h2; idle_ping=enabled; provider_model_id=expected"
	if !http2ReceiptMatches(base+"; provider_fallbacks=0", "expected") {
		t.Fatal("valid receipt rejected")
	}
	for _, suffix := range []string{"", "; provider_fallbacks=1", "; provider_fallbacks=mixed", "; provider_fallbacks=01"} {
		if http2ReceiptMatches(base+suffix, "expected") {
			t.Fatal("fallback or unverified receipt accepted")
		}
	}
	if http2ReceiptMatches(base+"; provider_fallbacks=0", "expect") {
		t.Fatal("partial identity accepted")
	}
}

// Use a normal-sized image: the one-pixel protocol fixture is not accepted by
// every real vision provider and can trigger LiteLLM's configured fallback.
func http2ImageData(t *testing.T) string {
	t.Helper()
	fixture := image.NewRGBA(image.Rect(0, 0, 128, 128))
	for y := 0; y < 128; y++ {
		for x := 0; x < 128; x++ {
			fixture.SetRGBA(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, fixture); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(encoded.Bytes())
}

func hasHTTP2ChildResult(raw []byte) bool {
	for _, line := range bytes.Split(raw, []byte("\n")) {
		var event struct {
			Type            string `json:"type"`
			ParentToolUseID string `json:"parent_tool_use_id"`
			Message         struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &event) != nil || event.Type != "assistant" || event.ParentToolUseID == "" {
			continue
		}
		for _, block := range event.Message.Content {
			if block.Type == "text" && strings.Contains(block.Text, "CCR_HTTP2_CHILD_OK") {
				return true
			}
		}
	}
	return false
}

func http2OutputMetadata(raw []byte) string {
	var events []map[string]any
	for _, line := range bytes.Split(raw, []byte("\n")) {
		var event struct {
			Type    string          `json:"type"`
			Parent  json.RawMessage `json:"parent_tool_use_id"`
			Result  string          `json:"result"`
			Message struct {
				Content []struct {
					Type    string                     `json:"type"`
					Text    string                     `json:"text"`
					IsError bool                       `json:"is_error"`
					Content json.RawMessage            `json:"content"`
					Input   map[string]json.RawMessage `json:"input"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &event) != nil {
			continue
		}
		child := false
		blocks := make([]string, 0, len(event.Message.Content))
		var markers, keys []string
		toolError := false
		for _, block := range event.Message.Content {
			if block.Type == "tool_use" {
				for key := range block.Input {
					keys = append(keys, key)
				}
			}
			if block.Type == "tool_result" {
				toolError = toolError || block.IsError
				for _, marker := range []string{"not found", "invalid", "required", "unknown", "permission", "denied", "unavailable", "error", "not allowed", "agent type"} {
					if strings.Contains(strings.ToLower(string(block.Content)), marker) {
						markers = append(markers, marker)
					}
				}
			}
			blocks = append(blocks, block.Type)
			if block.Type == "text" && strings.Contains(block.Text, "CCR_HTTP2_CHILD_OK") {
				child = true
			}
		}
		events = append(events, map[string]any{"tool_input_keys": keys, "tool_error": toolError, "error_markers": markers, "type": event.Type, "parent_present": len(event.Parent) > 0 && string(event.Parent) != "null", "blocks": blocks, "child_text": child, "parent_result": strings.Contains(event.Result, "CCR_HTTP2_BINARY_OK"), "child_result": strings.Contains(event.Result, "CCR_HTTP2_CHILD_OK")})
	}
	result, _ := json.Marshal(events)
	return string(result)
}

func http2GenerationReceiptsMatch(routes []http2RouteTiming, expected string) bool {
	observed := false
	for _, route := range routes {
		if strings.HasSuffix(route.Operation, "count_tokens") {
			continue
		}
		observed = true
		if route.Status != "succeeded" || !http2ReceiptMatches(route.TransportReason, expected) {
			return false
		}
	}
	return observed
}

func prepareHTTP2AgentRepository(t *testing.T, directory string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, args := range [][]string{
		{"-C", directory, "-c", "init.defaultBranch=main", "init", "-q"},
		{"-C", directory, "config", "--local", "core.hooksPath", "/dev/null"},
		{"-C", directory, "-c", "user.name=CCR Acceptance", "-c", "user.email=ccr-acceptance@example.invalid", "-c", "core.hooksPath=/dev/null", "-c", "commit.gpgSign=false", "commit", "--allow-empty", "-qm", "fixture"},
	} {
		if output, err := exec.CommandContext(ctx, "git", args...).CombinedOutput(); err != nil {
			t.Fatalf("prepare isolated Agent repository: %v (%d diagnostic bytes)", err, len(output))
		}
	}
}

func TestHTTP2AcceptancePreservesProviderSecretEnvironment(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "fixture-provider-secret")
	for _, entry := range http2BinaryEnvironment(t) {
		if entry == "ANTHROPIC_API_KEY=fixture-provider-secret" {
			return
		}
	}
	t.Fatal("provider secret environment erased before CCR launch")
}

func TestHTTP2AcceptanceDoesNotTreatLocalCountsAsGeneration(t *testing.T) {
	valid := http2RouteTiming{Operation: "messages", Status: "succeeded", TransportReason: "owner=ccr; protocol=h2; idle_ping=enabled; provider_model_id=expected; provider_fallbacks=0"}
	estimated := http2RouteTiming{Operation: "count_tokens", Status: "succeeded", TransportReason: "owner=ccr; protocol=unknown; idle_ping=unverified"}
	if !http2GenerationReceiptsMatch([]http2RouteTiming{valid, estimated}, "expected") {
		t.Fatal("local estimate invalidated generation proof")
	}
	if http2GenerationReceiptsMatch([]http2RouteTiming{estimated}, "expected") {
		t.Fatal("local estimate counted as generation proof")
	}
}
