package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/cua"
	"github.com/hishamkaram/claude-code-router/internal/modelcap"
	openairesponses "github.com/hishamkaram/claude-code-router/internal/responses"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestGatewayManagedCUAExecutesSingularResponsesActionAndReturnsScreenshot(t *testing.T) {
	ctx := context.Background()
	var requests []openairesponses.Request
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		var request openairesponses.Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode Responses request: %v", err)
		}
		requests = append(requests, request)
		w.Header().Set("Content-Type", "application/json")
		switch len(requests) {
		case 1:
			_, _ = fmt.Fprint(w, `{"id":"resp_1","output":[{"type":"computer_call","call_id":"call_1","action":{"type":"screenshot"}}],"usage":{"input_tokens":2,"output_tokens":1}}`)
		case 2:
			_, _ = fmt.Fprint(w, `{"id":"resp_2","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"managed complete"}]}],"usage":{"input_tokens":3,"output_tokens":4}}`)
		default:
			http.Error(w, "unexpected request", http.StatusBadRequest)
		}
	}))
	defer provider.Close()

	executor := &gatewayManagedExecutor{observation: cua.Observation{Screenshot: []byte("png"), ContentType: "image/png"}}
	runtime := newGatewayManagedRuntime(t, executor, cua.DecisionApprove)
	s := newGatewayStore(t,
		store.Provider{Name: "openai", Type: "openai-compatible", BaseURL: provider.URL, SupportsTools: true, SupportsStreaming: true, SupportsResponses: true},
		store.Model{Alias: "cua", ProviderName: "openai", ProviderModel: "computer-model", Status: "degraded", CapabilityOverrides: modelcap.Values{
			Kind: modelcap.KindResponses, SupportsResponses: modelcap.Bool(true), SupportsComputerUse: modelcap.Bool(true),
		}},
	)
	server := startGatewayWithConfig(t, ctx, Config{
		Store: s, Secrets: fakeGatewaySecrets{}, Token: "local-token", ManagedCUA: runtime, ManagedCUAProject: "fixture-project",
	})
	defer func() { _ = server.Shutdown(ctx) }()

	response := postGatewayMessage(t, ctx, server.URL(), managedCUARequestBody("cua"))
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("gateway status = %d, want 200: %s", response.StatusCode, body)
	}
	var payload struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		Usage openairesponses.Usage `json:"usage"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatalf("decode gateway response: %v", err)
	}
	if len(payload.Content) != 1 || payload.Content[0].Text != "managed complete" || payload.Usage.InputTokens != 5 || payload.Usage.OutputTokens != 5 {
		t.Fatalf("gateway payload = %#v", payload)
	}
	if executor.CallCount() != 1 || len(requests) != 2 {
		t.Fatalf("executor calls=%d provider requests=%d, want 1 and 2", executor.CallCount(), len(requests))
	}
	if len(requests[0].Tools) != 1 ||
		requests[0].Tools[0].Type != "computer" ||
		requests[0].Tools[0].DisplayWidth != 0 ||
		requests[0].Tools[0].DisplayHeight != 0 ||
		requests[0].Tools[0].Environment != "" {
		t.Fatalf("initial Responses CUA tool = %#v, want GA computer tool without preview metadata", requests[0].Tools)
	}
	if requests[1].PreviousResponseID != "resp_1" || len(requests[1].Input) != 1 || requests[1].Input[0].Type != "computer_call_output" || requests[1].Input[0].CallID != "call_1" {
		t.Fatalf("managed follow-up request = %#v", requests[1])
	}
	if len(requests[1].Input[0].AcknowledgedSafetyChecks) != 0 {
		t.Fatalf("managed follow-up silently acknowledged safety checks: %s", requests[1].Input[0].AcknowledgedSafetyChecks)
	}
	encoded, err := json.Marshal(requests[1].Input[0].Output)
	if err != nil {
		t.Fatalf("encode follow-up screenshot: %v", err)
	}
	if !strings.Contains(string(encoded), "data:image/png;base64,cG5n") {
		t.Fatalf("managed follow-up omitted screenshot: %s", encoded)
	}
	if !strings.Contains(string(encoded), `"detail":"original"`) {
		t.Fatalf("managed follow-up screenshot detail = %s, want original", encoded)
	}
}

func TestGatewayStreamsManagedCUAResponseBeforeFinalProviderCompletion(t *testing.T) {
	ctx := context.Background()
	firstText := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseProvider := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseProvider()

	var (
		requestsMu sync.Mutex
		requests   []openairesponses.Request
	)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request openairesponses.Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode Responses request: %v", err)
		}
		requestsMu.Lock()
		requests = append(requests, request)
		requestNumber := len(requests)
		requestsMu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		switch requestNumber {
		case 1:
			_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"computer_call\",\"call_id\":\"call_1\",\"action\":{\"type\":\"screenshot\"}}}\n\n")
			_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"computer_call\",\"call_id\":\"call_1\",\"action\":{\"type\":\"screenshot\"}}}\n\n")
			_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_cua_1\",\"status\":\"completed\",\"output\":[{\"type\":\"computer_call\",\"call_id\":\"call_1\",\"action\":{\"type\":\"screenshot\"}}],\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}}\n\n")
			w.(http.Flusher).Flush()
		case 2:
			_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[]}}\n\n")
			_, _ = fmt.Fprint(w, "data: {\"type\":\"response.content_part.added\",\"output_index\":0,\"content_index\":0,\"part\":{\"type\":\"output_text\",\"text\":\"\"}}\n\n")
			_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"managed first text\"}\n\n")
			w.(http.Flusher).Flush()
			close(firstText)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"managed first text complete\"}]}}\n\n")
			_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_cua_2\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"managed first text complete\"}]}],\"usage\":{\"input_tokens\":3,\"output_tokens\":4}}}\n\n")
			w.(http.Flusher).Flush()
		default:
			http.Error(w, "unexpected Responses request", http.StatusBadRequest)
		}
	}))
	defer provider.Close()

	executor := &gatewayManagedExecutor{observation: cua.Observation{Screenshot: []byte("png"), ContentType: "image/png"}}
	runtime := newGatewayManagedRuntime(t, executor, cua.DecisionApprove)
	s := newGatewayStore(t,
		store.Provider{Name: "openai", Type: "openai-compatible", BaseURL: provider.URL, SupportsTools: true, SupportsStreaming: true, SupportsResponses: true},
		store.Model{Alias: "cua", ProviderName: "openai", ProviderModel: "computer-model", Status: "degraded", CapabilityOverrides: modelcap.Values{
			Kind: modelcap.KindResponses, SupportsResponses: modelcap.Bool(true), SupportsComputerUse: modelcap.Bool(true),
		}},
	)
	server := startGatewayWithConfig(t, ctx, Config{
		Store: s, Secrets: fakeGatewaySecrets{}, Token: "local-token", ManagedCUA: runtime, ManagedCUAProject: "fixture-project",
	})
	defer func() { _ = server.Shutdown(ctx) }()

	requestContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestContext, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(strings.Replace(managedCUARequestBody("cua"), `"max_tokens"`, `"stream":true,"max_tokens"`, 1)))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer local-token")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway stream request error = %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("gateway status = %d, want 200: %s", response.StatusCode, body)
	}
	select {
	case <-firstText:
	case <-requestContext.Done():
		t.Fatalf("managed Responses provider did not send its first text chunk: %v", requestContext.Err())
	}

	reader := bufio.NewReader(response.Body)
	var initial strings.Builder
	for !strings.Contains(initial.String(), "managed first text") {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			t.Fatalf("reading managed stream before provider completion: %v; got %q", readErr, initial.String())
		}
		initial.WriteString(line)
	}
	if !strings.Contains(initial.String(), "event: message_start") || strings.Contains(initial.String(), "event: error") {
		t.Fatalf("managed initial stream = %q", initial.String())
	}

	releaseProvider()
	remainder, readErr := io.ReadAll(reader)
	if readErr != nil {
		t.Fatalf("reading completed managed stream: %v", readErr)
	}
	stream := initial.String() + string(remainder)
	if strings.Contains(stream, "computer_call") || !strings.Contains(stream, "event: message_stop") || !strings.Contains(stream, `"output_tokens":5`) {
		t.Fatalf("managed stream = %q", stream)
	}
	if executor.CallCount() != 1 {
		t.Fatalf("executor calls=%d, want 1", executor.CallCount())
	}
	requestsMu.Lock()
	defer requestsMu.Unlock()
	if len(requests) != 2 || !requests[0].Stream || !requests[1].Stream || requests[1].PreviousResponseID != "resp_cua_1" {
		t.Fatalf("managed provider requests = %#v", requests)
	}
}

func TestGatewayManagedCUARejectsPendingSafetyChecksBeforeExecution(t *testing.T) {
	ctx := context.Background()
	providerCalls := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"resp_1","output":[{"type":"computer_call","call_id":"call_1","action":{"type":"screenshot"},"pending_safety_checks":[{"id":"safety_1","code":"suspicious_url","message":"acknowledge"}]}]}`)
	}))
	defer provider.Close()

	executor := &gatewayManagedExecutor{observation: cua.Observation{Screenshot: []byte("png"), ContentType: "image/png"}}
	runtime := newGatewayManagedRuntime(t, executor, cua.DecisionApprove)
	s := newGatewayStore(t,
		store.Provider{Name: "openai", Type: "openai-compatible", BaseURL: provider.URL, SupportsTools: true, SupportsStreaming: true, SupportsResponses: true},
		store.Model{Alias: "cua", ProviderName: "openai", ProviderModel: "computer-model", Status: "degraded", CapabilityOverrides: modelcap.Values{
			Kind: modelcap.KindResponses, SupportsResponses: modelcap.Bool(true), SupportsComputerUse: modelcap.Bool(true),
		}},
	)
	server := startGatewayWithConfig(t, ctx, Config{
		Store: s, Secrets: fakeGatewaySecrets{}, Token: "local-token", ManagedCUA: runtime, ManagedCUAProject: "fixture-project",
	})
	defer func() { _ = server.Shutdown(ctx) }()

	response := postGatewayMessage(t, ctx, server.URL(), managedCUARequestBody("cua"))
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusNotImplemented ||
		!strings.Contains(string(body), "pending safety checks") ||
		!strings.Contains(string(body), "no action was executed") {
		t.Fatalf("gateway response status=%d body=%s, want visible pending safety check rejection", response.StatusCode, body)
	}
	if providerCalls != 1 || executor.CallCount() != 0 {
		t.Fatalf("provider calls=%d executor calls=%d, want 1 and 0", providerCalls, executor.CallCount())
	}
}

func TestGatewayManagedCUARejectsComputerCallWithoutResponseIDBeforeExecution(t *testing.T) {
	ctx := context.Background()
	providerCalls := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"output":[{"type":"computer_call","call_id":"call_1","actions":[{"type":"click","x":1,"y":2}]}]}`)
	}))
	defer provider.Close()

	executor := &gatewayManagedExecutor{}
	runtime := newGatewayManagedRuntime(t, executor, cua.DecisionApprove)
	s := newGatewayStore(t,
		store.Provider{Name: "openai", Type: "openai-compatible", BaseURL: provider.URL, SupportsTools: true, SupportsStreaming: true, SupportsResponses: true},
		store.Model{Alias: "cua", ProviderName: "openai", ProviderModel: "computer-model", Status: "degraded", CapabilityOverrides: modelcap.Values{
			Kind: modelcap.KindResponses, SupportsResponses: modelcap.Bool(true), SupportsComputerUse: modelcap.Bool(true),
		}},
	)
	server := startGatewayWithConfig(t, ctx, Config{Store: s, Secrets: fakeGatewaySecrets{}, Token: "local-token", ManagedCUA: runtime, ManagedCUAProject: "fixture-project"})
	defer func() { _ = server.Shutdown(ctx) }()

	response := postGatewayMessage(t, ctx, server.URL(), managedCUARequestBody("cua"))
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "did not include a response id") {
		t.Fatalf("gateway response status=%d body=%s, want missing response id rejection", response.StatusCode, body)
	}
	if providerCalls != 1 || executor.CallCount() != 0 {
		t.Fatalf("provider calls=%d executor calls=%d, want 1 and 0", providerCalls, executor.CallCount())
	}
}

func TestGatewayManagedCUARejectsUnsuccessfulResponseStatusBeforeExecution(t *testing.T) {
	ctx := context.Background()
	providerCalls := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"resp_1","status":"failed","output":[{"type":"computer_call","call_id":"call_1","action":{"type":"screenshot"}}]}`)
	}))
	defer provider.Close()

	executor := &gatewayManagedExecutor{}
	runtime := newGatewayManagedRuntime(t, executor, cua.DecisionApprove)
	s := newGatewayStore(t,
		store.Provider{Name: "openai", Type: "openai-compatible", BaseURL: provider.URL, SupportsTools: true, SupportsStreaming: true, SupportsResponses: true},
		store.Model{Alias: "cua", ProviderName: "openai", ProviderModel: "computer-model", Status: "degraded", CapabilityOverrides: modelcap.Values{
			Kind: modelcap.KindResponses, SupportsResponses: modelcap.Bool(true), SupportsComputerUse: modelcap.Bool(true),
		}},
	)
	server := startGatewayWithConfig(t, ctx, Config{Store: s, Secrets: fakeGatewaySecrets{}, Token: "local-token", ManagedCUA: runtime, ManagedCUAProject: "fixture-project"})
	defer func() { _ = server.Shutdown(ctx) }()

	response := postGatewayMessage(t, ctx, server.URL(), managedCUARequestBody("cua"))
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusBadGateway ||
		!strings.Contains(string(body), "returned status") ||
		!strings.Contains(string(body), "failed") {
		t.Fatalf("gateway response status=%d body=%s, want failed status rejection", response.StatusCode, body)
	}
	if providerCalls != 1 || executor.CallCount() != 0 {
		t.Fatalf("provider calls=%d executor calls=%d, want 1 and 0", providerCalls, executor.CallCount())
	}
}

func TestGatewayManagedCUADenialStopsBeforeFollowUp(t *testing.T) {
	ctx := context.Background()
	providerCalls := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"resp_1","output":[{"type":"computer_call","call_id":"call_1","actions":[{"type":"click","x":1,"y":2}]}]}`)
	}))
	defer provider.Close()

	executor := &gatewayManagedExecutor{}
	runtime := newGatewayManagedRuntime(t, executor, cua.DecisionDeny)
	s := newGatewayStore(t,
		store.Provider{Name: "openai", Type: "openai-compatible", BaseURL: provider.URL, SupportsTools: true, SupportsStreaming: true, SupportsResponses: true},
		store.Model{Alias: "cua", ProviderName: "openai", ProviderModel: "computer-model", Status: "degraded", CapabilityOverrides: modelcap.Values{
			Kind: modelcap.KindResponses, SupportsResponses: modelcap.Bool(true), SupportsComputerUse: modelcap.Bool(true),
		}},
	)
	server := startGatewayWithConfig(t, ctx, Config{Store: s, Secrets: fakeGatewaySecrets{}, Token: "local-token", ManagedCUA: runtime, ManagedCUAProject: "fixture-project"})
	defer func() { _ = server.Shutdown(ctx) }()

	response := postGatewayMessage(t, ctx, server.URL(), managedCUARequestBody("cua"))
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "was denied") {
		t.Fatalf("gateway response status=%d body=%s, want 403 denial", response.StatusCode, body)
	}
	if providerCalls != 1 || executor.CallCount() != 0 {
		t.Fatalf("provider calls=%d executor calls=%d, want 1 and 0", providerCalls, executor.CallCount())
	}
}

func TestGatewayManagedCUATurnLimitReturnsControlledError(t *testing.T) {
	ctx := context.Background()
	providerCalls := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"resp_1","output":[{"type":"computer_call","call_id":"call_1","actions":[{"type":"screenshot"}]}]}`)
	}))
	defer provider.Close()

	executor := &gatewayManagedExecutor{}
	runtime, err := cua.NewManagedRuntime(ctx, cua.Config{
		Mode:     cua.ModeManaged,
		Executor: executor.Name(),
		MaxTurns: 1,
	}, executor, gatewayManagedAuthorizer{decision: cua.DecisionApprove}, nil)
	if err != nil {
		t.Fatalf("NewManagedRuntime() error = %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	if err := runtime.BeginTurn(ctx); err != nil {
		t.Fatalf("BeginTurn() setup error = %v", err)
	}

	s := newGatewayStore(t,
		store.Provider{Name: "openai", Type: "openai-compatible", BaseURL: provider.URL, SupportsTools: true, SupportsStreaming: true, SupportsResponses: true},
		store.Model{Alias: "cua", ProviderName: "openai", ProviderModel: "computer-model", Status: "degraded", CapabilityOverrides: modelcap.Values{
			Kind: modelcap.KindResponses, SupportsResponses: modelcap.Bool(true), SupportsComputerUse: modelcap.Bool(true),
		}},
	)
	server := startGatewayWithConfig(t, ctx, Config{Store: s, Secrets: fakeGatewaySecrets{}, Token: "local-token", ManagedCUA: runtime, ManagedCUAProject: "fixture-project"})
	defer func() { _ = server.Shutdown(ctx) }()

	response := postGatewayMessage(t, ctx, server.URL(), managedCUARequestBody("cua"))
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusTooManyRequests || !strings.Contains(string(body), "turn safety limit") {
		t.Fatalf("gateway response status=%d body=%s, want 429 turn-limit error", response.StatusCode, body)
	}
	if providerCalls != 1 || executor.CallCount() != 0 {
		t.Fatalf("provider calls=%d executor calls=%d, want 1 and 0", providerCalls, executor.CallCount())
	}
}

func TestGatewayManagedCUARejectsMalformedActionBeforeExecution(t *testing.T) {
	ctx := context.Background()
	providerCalls := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"resp_1","output":[{"type":"computer_call","call_id":"call_1","actions":[{"type":"click","x":1}]}]}`)
	}))
	defer provider.Close()

	executor := &gatewayManagedExecutor{}
	runtime := newGatewayManagedRuntime(t, executor, cua.DecisionApprove)
	s := newGatewayStore(t,
		store.Provider{Name: "openai", Type: "openai-compatible", BaseURL: provider.URL, SupportsTools: true, SupportsStreaming: true, SupportsResponses: true},
		store.Model{Alias: "cua", ProviderName: "openai", ProviderModel: "computer-model", Status: "degraded", CapabilityOverrides: modelcap.Values{
			Kind: modelcap.KindResponses, SupportsResponses: modelcap.Bool(true), SupportsComputerUse: modelcap.Bool(true),
		}},
	)
	server := startGatewayWithConfig(t, ctx, Config{Store: s, Secrets: fakeGatewaySecrets{}, Token: "local-token", ManagedCUA: runtime, ManagedCUAProject: "fixture-project"})
	defer func() { _ = server.Shutdown(ctx) }()

	response := postGatewayMessage(t, ctx, server.URL(), managedCUARequestBody("cua"))
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusNotImplemented || !strings.Contains(string(body), "provider action is unsupported") {
		t.Fatalf("gateway response status=%d body=%s, want malformed action rejection", response.StatusCode, body)
	}
	if providerCalls != 1 || executor.CallCount() != 0 {
		t.Fatalf("provider calls=%d executor calls=%d, want 1 and 0", providerCalls, executor.CallCount())
	}
}

func TestGatewayManagedCUARejectsAnthropicRouteBeforeProviderSubmission(t *testing.T) {
	ctx := context.Background()
	providerCalled := false
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalled = true
		http.Error(w, "must not be called", http.StatusInternalServerError)
	}))
	defer provider.Close()

	runtime := newGatewayManagedRuntime(t, &gatewayManagedExecutor{}, cua.DecisionApprove)
	s := newGatewayStore(t,
		store.Provider{Name: "anthropic", Type: "anthropic-compatible", BaseURL: provider.URL, SupportsTools: true, SupportsStreaming: true},
		store.Model{Alias: "native", ProviderName: "anthropic", ProviderModel: "claude-cua", Status: "full", CapabilityOverrides: modelcap.Values{SupportsComputerUse: modelcap.Bool(true)}},
	)
	server := startGatewayWithConfig(t, ctx, Config{Store: s, Secrets: fakeGatewaySecrets{}, Token: "local-token", ManagedCUA: runtime, ManagedCUAProject: "fixture-project"})
	defer func() { _ = server.Shutdown(ctx) }()

	response := postGatewayMessage(t, ctx, server.URL(), managedCUARequestBody("native"))
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusNotImplemented || !strings.Contains(string(body), "client-managed CUA") {
		t.Fatalf("gateway response status=%d body=%s", response.StatusCode, body)
	}
	if providerCalled {
		t.Fatal("Anthropic provider was called for managed CUA")
	}
}

func TestManagedComputerScreenshotRejectsDataThatWouldExceedGatewayRequestLimit(t *testing.T) {
	t.Parallel()

	_, err := managedComputerScreenshot(cua.Observation{
		Screenshot:  make([]byte, maxManagedComputerScreenshotBytes+1),
		ContentType: "image/png",
	})
	if err == nil || !strings.Contains(err.Error(), "base64-safe") {
		t.Fatalf("managedComputerScreenshot() error = %v, want base64-safe size rejection", err)
	}
}

func TestManagedComputerFollowUpClearsForcedToolChoice(t *testing.T) {
	t.Parallel()

	initial := &openairesponses.Request{
		ToolChoice: map[string]any{"type": "computer"},
		Tools: []openairesponses.Tool{{
			Type: "computer",
		}},
	}
	followUp := managedComputerFollowUp(initial, "resp_1", []openairesponses.InputItem{{Type: "computer_call_output", CallID: "call_1"}})
	if followUp.ToolChoice != nil {
		t.Fatalf("follow-up tool choice = %#v, want nil", followUp.ToolChoice)
	}
	if initial.ToolChoice == nil {
		t.Fatal("initial request tool choice was mutated")
	}
	if followUp.PreviousResponseID != "resp_1" || len(followUp.Input) != 1 || followUp.Input[0].CallID != "call_1" {
		t.Fatalf("follow-up request = %#v", followUp)
	}
}

func managedCUARequestBody(alias string) string {
	return `{"model":"` + alias + `","max_tokens":16,"tools":[{"type":"computer_20250124","name":"computer"}],"messages":[{"role":"user","content":"take a screenshot"}]}`
}

func newGatewayManagedRuntime(t *testing.T, executor cua.Executor, decision cua.Decision) *cua.ManagedRuntime {
	t.Helper()
	runtime, err := cua.NewManagedRuntime(context.Background(), cua.Config{Mode: cua.ModeManaged, Executor: executor.Name()}, executor, gatewayManagedAuthorizer{decision: decision}, nil)
	if err != nil {
		t.Fatalf("NewManagedRuntime() error = %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	return runtime
}

type gatewayManagedAuthorizer struct {
	decision cua.Decision
}

func (a gatewayManagedAuthorizer) Authorize(context.Context, string, cua.Action) (cua.Decision, error) {
	return a.decision, nil
}

type gatewayManagedExecutor struct {
	observation cua.Observation
	mu          sync.Mutex
	calls       int
}

func (e *gatewayManagedExecutor) Name() string { return "external:fixture" }

func (*gatewayManagedExecutor) Check(context.Context) error { return nil }

func (e *gatewayManagedExecutor) Execute(_ context.Context, _ cua.Action) (cua.Observation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	return e.observation, nil
}

func (*gatewayManagedExecutor) Close() error { return nil }

func (e *gatewayManagedExecutor) CallCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}
