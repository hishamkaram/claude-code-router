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
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/modelcap"
	openairesponses "github.com/hishamkaram/claude-code-router/internal/responses"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestGatewayRoutesResponsesModelWithoutChatFallback(t *testing.T) {
	ctx := context.Background()
	var chatCalls int
	var responsesCalls int
	var providerRequest openairesponses.Request
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/chat/completions":
			chatCalls++
			http.Error(w, "Chat fallback is forbidden", http.StatusBadGateway)
		case "/v1/responses":
			responsesCalls++
			if err := json.NewDecoder(r.Body).Decode(&providerRequest); err != nil {
				t.Fatalf("decode Responses request: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"id":"resp_test","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"responses-routed"}]}],"usage":{"input_tokens":3,"output_tokens":2}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()

	s := newGatewayStore(t,
		store.Provider{Name: "openai", Type: "openai-compatible", BaseURL: provider.URL, SupportsTools: true, SupportsStreaming: true, SupportsThinking: true, SupportsResponses: true},
		store.Model{Alias: "responses", ProviderName: "openai", ProviderModel: "gpt-responses", Status: "degraded", CapabilityOverrides: modelcap.Values{
			Kind: modelcap.KindResponses, SupportsResponses: modelcap.Bool(true), SupportsThinking: modelcap.Bool(true),
		}},
	)
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() { _ = server.Shutdown(ctx) }()

	response := postGatewayMessage(t, ctx, server.URL(), `{"model":"responses","max_tokens":16,"metadata":{"user_id":"fixture-user"},"thinking":{"type":"enabled"},"output_config":{"effort":"medium"},"context_management":{"edits":[{"type":"clear_tool_uses"}]},"messages":[{"role":"user","content":"hello"}]}`)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("gateway status = %d, want 200: %s", response.StatusCode, body)
	}
	var payload struct {
		Model   string `json:"model"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatalf("decode gateway response: %v", err)
	}
	if chatCalls != 0 || responsesCalls != 1 {
		t.Fatalf("upstream calls chat=%d responses=%d, want 0 and 1", chatCalls, responsesCalls)
	}
	if providerRequest.Model != "gpt-responses" || providerRequest.Metadata["user_id"] != "fixture-user" || providerRequest.Reasoning == nil || providerRequest.Reasoning.Effort != "medium" || len(providerRequest.Input) != 1 ||
		len(providerRequest.Input[0].Content) != 1 || providerRequest.Input[0].Content[0].Text != "hello" {
		t.Fatalf("provider request = %#v", providerRequest)
	}
	if payload.Model != "responses" || len(payload.Content) != 1 || payload.Content[0].Text != "responses-routed" {
		t.Fatalf("gateway response = %#v", payload)
	}
}

func TestGatewayStreamsResponsesTextBeforeProviderCompletion(t *testing.T) {
	ctx := context.Background()
	firstChunk := make(chan struct{})
	release := make(chan struct{})
	var providerRequest openairesponses.Request
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&providerRequest); err != nil {
			t.Fatalf("decode Responses request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_stream\",\"status\":\"in_progress\",\"output\":[]}}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"msg_stream\",\"role\":\"assistant\",\"content\":[]}}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.content_part.added\",\"output_index\":0,\"content_index\":0,\"part\":{\"type\":\"output_text\",\"text\":\"\"}}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"first response\"}\n\n")
		w.(http.Flusher).Flush()
		close(firstChunk)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\" after completion\"}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"msg_stream\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"first response after completion\"}]}}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_stream\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"id\":\"msg_stream\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"first response after completion\"}]}],\"usage\":{\"input_tokens\":4,\"output_tokens\":3}}}\n\n")
		w.(http.Flusher).Flush()
	}))
	defer provider.Close()

	s := newGatewayStore(t,
		store.Provider{Name: "openai", Type: "openai-compatible", BaseURL: provider.URL, SupportsTools: true, SupportsStreaming: true, SupportsResponses: true},
		store.Model{Alias: "responses", ProviderName: "openai", ProviderModel: "gpt-responses", Status: "degraded", CapabilityOverrides: modelcap.Values{
			Kind: modelcap.KindResponses, SupportsResponses: modelcap.Bool(true),
		}},
	)
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() { _ = server.Shutdown(ctx) }()

	requestContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestContext, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(`{"model":"responses","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer local-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway stream request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("gateway status = %d body=%s", resp.StatusCode, body)
	}
	select {
	case <-firstChunk:
	case <-requestContext.Done():
		t.Fatalf("provider did not send first response chunk: %v", requestContext.Err())
	}
	reader := bufio.NewReader(resp.Body)
	var initial strings.Builder
	for !strings.Contains(initial.String(), "first response") {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			t.Fatalf("reading first stream event: %v; got %q", readErr, initial.String())
		}
		initial.WriteString(line)
	}
	if !strings.Contains(initial.String(), "event: message_start") {
		t.Fatalf("stream did not start before Responses completion: %q", initial.String())
	}
	if strings.Contains(initial.String(), `"input_tokens":0`) {
		t.Fatalf("Responses stream emitted zero input usage before provider completion: %q", initial.String())
	}
	if id := anthropicMessageStartID(t, initial.String()); !strings.HasPrefix(id, "msg_ccr_") {
		t.Fatalf("Responses stream message id = %q, want a generated gateway id", id)
	}
	close(release)
	remainder, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("reading completed stream: %v", err)
	}
	stream := initial.String() + string(remainder)
	if !strings.Contains(stream, "after completion") || !strings.Contains(stream, "event: message_stop") {
		t.Fatalf("completed stream = %q", stream)
	}
	if !providerRequest.Stream {
		t.Fatal("Responses provider did not receive stream=true")
	}
}

func TestGatewayStreamsResponsesFunctionCall(t *testing.T) {
	ctx := context.Background()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"bash\",\"arguments\":\"\"}}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"delta\":\"{\\\"cmd\\\":\\\"pwd\\\"}\"}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"bash\",\"arguments\":\"{\\\"cmd\\\":\\\"pwd\\\"}\"}}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_tool\",\"status\":\"completed\",\"output\":[{\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"bash\",\"arguments\":\"{\\\"cmd\\\":\\\"pwd\\\"}\"}]}}\n\n")
	}))
	defer provider.Close()

	s := newGatewayStore(t,
		store.Provider{Name: "openai", Type: "openai-compatible", BaseURL: provider.URL, SupportsTools: true, SupportsStreaming: true, SupportsResponses: true},
		store.Model{Alias: "responses", ProviderName: "openai", ProviderModel: "gpt-responses", Status: "degraded", CapabilityOverrides: modelcap.Values{
			Kind: modelcap.KindResponses, SupportsResponses: modelcap.Bool(true),
		}},
	)
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() { _ = server.Shutdown(ctx) }()

	response := postGatewayMessage(t, ctx, server.URL(), `{"model":"responses","stream":true,"tools":[{"name":"bash","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"run pwd"}]}`)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading gateway stream: %v", err)
	}
	stream := string(body)
	for _, want := range []string{`"type":"tool_use"`, `"name":"bash"`, `"partial_json":"{\"cmd\":\"pwd\"}"`, `"stop_reason":"tool_use"`, "event: message_stop"} {
		if response.StatusCode != http.StatusOK || !strings.Contains(stream, want) {
			t.Fatalf("gateway status/body=%d %q, missing %q", response.StatusCode, stream, want)
		}
	}
}

func TestGatewayRejectsMalformedStreamedResponsesFunctionCall(t *testing.T) {
	ctx := context.Background()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"call_id\":\"call_bad\",\"name\":\"bash\",\"arguments\":\"not-json\"}}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_bad\",\"status\":\"completed\",\"output\":[{\"type\":\"function_call\",\"call_id\":\"call_bad\",\"name\":\"bash\",\"arguments\":\"not-json\"}]}}\n\n")
	}))
	defer provider.Close()

	s := newGatewayStore(t,
		store.Provider{Name: "openai", Type: "openai-compatible", BaseURL: provider.URL, SupportsTools: true, SupportsStreaming: true, SupportsResponses: true},
		store.Model{Alias: "responses", ProviderName: "openai", ProviderModel: "gpt-responses", Status: "degraded", CapabilityOverrides: modelcap.Values{
			Kind: modelcap.KindResponses, SupportsResponses: modelcap.Bool(true),
		}},
	)
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() { _ = server.Shutdown(ctx) }()

	response := postGatewayMessage(t, ctx, server.URL(), `{"model":"responses","stream":true,"tools":[{"name":"bash","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"run pwd"}]}`)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading gateway stream: %v", err)
	}
	stream := string(body)
	if response.StatusCode != http.StatusOK || !strings.Contains(stream, "event: error") ||
		!strings.Contains(stream, "arguments are not valid JSON") || strings.Contains(stream, `"type":"tool_use"`) {
		t.Fatalf("gateway status/body=%d %q", response.StatusCode, stream)
	}
}

func TestValidateResponsesStreamTool(t *testing.T) {
	tests := []struct {
		name    string
		tool    openAIToolCall
		wantErr string
	}{
		{
			name:    "missing call ID",
			tool:    openAIToolCall{Function: openAIFunctionCall{Name: "bash", Arguments: `{}`}},
			wantErr: "missing call_id or name",
		},
		{
			name:    "missing name",
			tool:    openAIToolCall{ID: "call_1", Function: openAIFunctionCall{Arguments: `{}`}},
			wantErr: "missing call_id or name",
		},
		{
			name:    "invalid arguments",
			tool:    openAIToolCall{ID: "call_1", Function: openAIFunctionCall{Name: "bash", Arguments: `not-json`}},
			wantErr: "arguments are not valid JSON",
		},
		{
			name: "valid scalar arguments",
			tool: openAIToolCall{ID: "call_1", Function: openAIFunctionCall{
				Name:      "bash",
				Arguments: `"pwd"`,
			}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateResponsesStreamTool(test.tool)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("validateResponsesStreamTool() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("validateResponsesStreamTool() error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestGatewayRejectsBufferedResponsesProviderForStreamRequest(t *testing.T) {
	ctx := context.Background()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"resp_buffered","status":"completed","output":[]}`)
	}))
	defer provider.Close()

	s := newGatewayStore(t,
		store.Provider{Name: "openai", Type: "openai-compatible", BaseURL: provider.URL, SupportsTools: true, SupportsStreaming: true, SupportsResponses: true},
		store.Model{Alias: "responses", ProviderName: "openai", ProviderModel: "gpt-responses", Status: "degraded", CapabilityOverrides: modelcap.Values{
			Kind: modelcap.KindResponses, SupportsResponses: modelcap.Bool(true),
		}},
	)
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() { _ = server.Shutdown(ctx) }()

	response := postGatewayMessage(t, ctx, server.URL(), `{"model":"responses","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading gateway error: %v", err)
	}
	if response.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "did not honor the streaming request") {
		t.Fatalf("gateway status/body = %d %q", response.StatusCode, body)
	}
}

func TestGatewayResponsesRouteInjectsIdentityInstructionsForModelQuestion(t *testing.T) {
	ctx := context.Background()
	var providerRequest openairesponses.Request
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&providerRequest); err != nil {
			t.Fatalf("decode Responses request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"resp_test","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"glm"}]}]}`)
	}))
	defer provider.Close()

	s := newGatewayStore(t,
		store.Provider{Name: "openai", Type: "openai-compatible", BaseURL: provider.URL, SupportsTools: true, SupportsStreaming: true, SupportsResponses: true},
		store.Model{Alias: "responses", ProviderName: "openai", ProviderModel: "gpt-responses", Status: "degraded", CapabilityOverrides: modelcap.Values{
			Kind: modelcap.KindResponses, SupportsResponses: modelcap.Bool(true), SupportsSystemMessages: modelcap.Bool(true),
		}},
	)
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() { _ = server.Shutdown(ctx) }()

	response := postGatewayMessage(t, ctx, server.URL(), `{
		"model":"anthropic.ccr.responses",
		"system":"Be concise.",
		"messages":[
			{"role":"user","content":"which model were you earlier?"},
			{"role":"assistant","content":"I am Sonnet."},
			{"role":"user","content":"which model are you now?"}
		]
	}`)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("gateway status = %d, want 200: %s", response.StatusCode, body)
	}
	if !strings.Contains(providerRequest.Instructions, "Be concise.") ||
		!strings.Contains(providerRequest.Instructions, `CCR alias "responses"`) ||
		!strings.Contains(providerRequest.Instructions, `provider "openai"`) ||
		!strings.Contains(providerRequest.Instructions, `provider model "gpt-responses"`) ||
		!strings.Contains(providerRequest.Instructions, `Claude Code requested model ID "anthropic.ccr.responses"`) {
		t.Fatalf("Responses instructions = %q", providerRequest.Instructions)
	}
	if len(providerRequest.Input) != 3 || providerRequest.Input[1].Role != "assistant" {
		t.Fatalf("Responses input = %#v, want original transcript preserved", providerRequest.Input)
	}
}

func TestGatewayResponsesRouteSuppressesIdentityWhenSystemMessagesUnsupported(t *testing.T) {
	ctx := context.Background()
	var providerRequest openairesponses.Request
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&providerRequest); err != nil {
			t.Fatalf("decode Responses request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"resp_test","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"unknown"}]}]}`)
	}))
	defer provider.Close()

	s := newGatewayStore(t,
		store.Provider{Name: "openai", Type: "openai-compatible", BaseURL: provider.URL, SupportsTools: true, SupportsStreaming: true, SupportsResponses: true},
		store.Model{Alias: "responses", ProviderName: "openai", ProviderModel: "gpt-responses", Status: "degraded", CapabilityOverrides: modelcap.Values{
			Kind: modelcap.KindResponses, SupportsResponses: modelcap.Bool(true), SupportsSystemMessages: modelcap.Bool(false),
		}},
	)
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() { _ = server.Shutdown(ctx) }()

	response := postGatewayMessage(t, ctx, server.URL(), `{"model":"anthropic.ccr.responses","messages":[{"role":"user","content":"which model are you now?"}]}`)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("gateway status = %d, want 200: %s", response.StatusCode, body)
	}
	if providerRequest.Instructions != "" {
		t.Fatalf("Responses instructions = %q, want no injected route identity", providerRequest.Instructions)
	}
}

func TestGatewayResponsesRouteForcesSerialToolCallsWhenModelDisallowsParallelTools(t *testing.T) {
	ctx := context.Background()
	var providerRequest openairesponses.Request
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&providerRequest); err != nil {
			t.Fatalf("decode Responses request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"resp_test","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`)
	}))
	defer provider.Close()

	s := newGatewayStore(t,
		store.Provider{Name: "openai", Type: "openai-compatible", BaseURL: provider.URL, SupportsTools: true, SupportsStreaming: true, SupportsResponses: true},
		store.Model{Alias: "responses", ProviderName: "openai", ProviderModel: "gpt-responses", Status: "degraded", CapabilityOverrides: modelcap.Values{
			Kind: modelcap.KindResponses, SupportsResponses: modelcap.Bool(true), SupportsParallelTools: modelcap.Bool(false),
		}},
	)
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() { _ = server.Shutdown(ctx) }()

	response := postGatewayMessage(t, ctx, server.URL(), `{
		"model":"responses","max_tokens":16,
		"tools":[{"name":"bash","description":"run shell","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"tool","name":"bash"},
		"messages":[{"role":"user","content":"hello"}]
	}`)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("gateway status = %d, want 200: %s", response.StatusCode, body)
	}
	if providerRequest.ParallelToolCalls == nil || *providerRequest.ParallelToolCalls {
		t.Fatalf("Responses provider saw parallel_tool_calls=%v, want false", providerRequest.ParallelToolCalls)
	}
}

func TestGatewayRejectsResponsesComputerUseWithoutManagedExecutor(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name      string
		secretRef string
		secrets   fakeGatewaySecrets
		content   string
	}{
		{
			name:      "before secret resolution",
			secretRef: "env:PROVIDER_KEY",
			secrets:   fakeGatewaySecrets{},
			content:   `"take a screenshot"`,
		},
		{
			name:      "before image normalization",
			secretRef: "env:PROVIDER_KEY",
			secrets:   fakeGatewaySecrets{"env:PROVIDER_KEY": "provider-secret"},
			content: `[{"type":"text","text":"take a screenshot"},
				{"type":"image","source":{"type":"url","url":"https://127.0.0.1/private.png"}}]`,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			providerCalled := false
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				providerCalled = true
				http.Error(w, "must not be called", http.StatusInternalServerError)
			}))
			defer provider.Close()

			s := newGatewayStoreWithContext(t, ctx,
				store.Provider{
					Name: "openai", Type: "openai-compatible", BaseURL: provider.URL, SecretRef: test.secretRef,
					SupportsTools: true, SupportsStreaming: true, SupportsResponses: true,
				},
				store.Model{Alias: "cua", ProviderName: "openai", ProviderModel: "computer-model", Status: "degraded", CapabilityOverrides: modelcap.Values{
					Kind: modelcap.KindResponses, SupportsResponses: modelcap.Bool(true), SupportsComputerUse: modelcap.Bool(true),
					SupportsVision: modelcap.Bool(true),
				}},
			)
			server := startGatewayWithConfig(t, ctx, Config{
				Store: s, Secrets: test.secrets, Token: "local-token",
			})
			defer func() { _ = server.Shutdown(ctx) }()

			response := postGatewayMessage(t, ctx, server.URL(), `{
				"model":"cua","max_tokens":16,
				"tools":[{"type":"computer_20250124","name":"computer","display_width_px":1024,"display_height_px":768,"display_number":1}],
				"messages":[{"role":"user","content":`+test.content+`}]
			}`)
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatalf("read gateway response: %v", err)
			}
			if response.StatusCode != http.StatusNotImplemented || !strings.Contains(string(body), "requires a CCR managed CUA executor") {
				t.Fatalf("gateway status=%d body=%s, want managed CUA rejection", response.StatusCode, body)
			}
			if strings.Contains(string(body), "provider secret") || strings.Contains(string(body), "image URL") {
				t.Fatalf("gateway performed work before managed CUA rejection: %s", body)
			}
			if providerCalled {
				t.Fatal("Responses provider was called for unmanaged computer use")
			}
		})
	}
}

func TestGatewayRejectsResponsesOnlyAliasOnAnthropicCompatibleProvider(t *testing.T) {
	ctx := context.Background()
	providerCalled := false
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalled = true
		http.Error(w, "must not be called", http.StatusInternalServerError)
	}))
	defer provider.Close()

	s := newGatewayStore(t,
		store.Provider{Name: "anthropic", Type: "anthropic-compatible", BaseURL: provider.URL, SupportsTools: true, SupportsStreaming: true},
		store.Model{Alias: "responses", ProviderName: "anthropic", ProviderModel: "responses-model", Status: "degraded", CapabilityOverrides: modelcap.Values{
			Kind: modelcap.KindResponses, SupportsResponses: modelcap.Bool(true),
		}},
	)
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() { _ = server.Shutdown(ctx) }()

	response := postGatewayMessage(t, ctx, server.URL(), `{"model":"responses","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read gateway response: %v", err)
	}
	if response.StatusCode != http.StatusNotImplemented || !strings.Contains(string(body), "requires the OpenAI Responses API") {
		t.Fatalf("gateway status=%d body=%s, want Responses route rejection", response.StatusCode, body)
	}
	if providerCalled {
		t.Fatal("Anthropic-compatible provider was called for a Responses-only alias")
	}
}

func TestGatewayModelDiscoveryExcludesResponsesAliasWithoutProviderSupport(t *testing.T) {
	ctx := context.Background()
	s := newGatewayStore(t,
		store.Provider{Name: "openai", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1", SupportsTools: true, SupportsStreaming: true},
		store.Model{Alias: "responses", ProviderName: "openai", ProviderModel: "responses-model", Status: "degraded", CapabilityOverrides: modelcap.Values{
			Kind: modelcap.KindResponses, SupportsResponses: modelcap.Bool(true),
		}},
	)
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() { _ = server.Shutdown(ctx) }()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL()+"/v1/models", http.NoBody)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("X-CCR-Session-Token", "local-token")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway discovery request error = %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("gateway discovery status=%d body=%s, want 200", response.StatusCode, body)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatalf("decode gateway discovery response: %v", err)
	}
	for _, model := range payload.Data {
		if model.ID == "anthropic.ccr.responses" {
			t.Fatalf("gateway discovery advertised a Responses alias whose provider lacks Responses support: %#v", payload.Data)
		}
	}
}

func TestGatewayReturnsBadGatewayForUnknownResponsesOutput(t *testing.T) {
	ctx := context.Background()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"resp_unknown","output":[{"type":"future_item"}]}`)
	}))
	defer provider.Close()

	s := newGatewayStore(t,
		store.Provider{Name: "openai", Type: "openai-compatible", BaseURL: provider.URL, SupportsTools: true, SupportsStreaming: true, SupportsResponses: true},
		store.Model{Alias: "responses", ProviderName: "openai", ProviderModel: "responses-model", Status: "degraded", CapabilityOverrides: modelcap.Values{
			Kind: modelcap.KindResponses, SupportsResponses: modelcap.Bool(true),
		}},
	)
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() { _ = server.Shutdown(ctx) }()

	response := postGatewayMessage(t, ctx, server.URL(), `{"model":"responses","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read gateway response: %v", err)
	}
	if response.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "unsupported Responses output item type") {
		t.Fatalf("gateway status=%d body=%s, want 502 malformed provider output", response.StatusCode, body)
	}
}

func TestGatewayReturnsBadGatewayForUnsuccessfulResponsesStatus(t *testing.T) {
	ctx := context.Background()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"resp_failed","status":"failed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"must not be surfaced"}]}]}`)
	}))
	defer provider.Close()

	s := newGatewayStore(t,
		store.Provider{Name: "openai", Type: "openai-compatible", BaseURL: provider.URL, SupportsTools: true, SupportsStreaming: true, SupportsResponses: true},
		store.Model{Alias: "responses", ProviderName: "openai", ProviderModel: "responses-model", Status: "degraded", CapabilityOverrides: modelcap.Values{
			Kind: modelcap.KindResponses, SupportsResponses: modelcap.Bool(true),
		}},
	)
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() { _ = server.Shutdown(ctx) }()

	response := postGatewayMessage(t, ctx, server.URL(), `{"model":"responses","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read gateway response: %v", err)
	}
	if response.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "status") || !strings.Contains(string(body), "failed") || strings.Contains(string(body), "must not be surfaced") {
		t.Fatalf("gateway status=%d body=%s, want visible unsuccessful provider status", response.StatusCode, body)
	}
}

func TestGatewayRejectsComputerUseOnChatCompletionsRoute(t *testing.T) {
	ctx := context.Background()
	called := false
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		http.Error(w, "must not be called", http.StatusInternalServerError)
	}))
	defer provider.Close()

	s := newGatewayStore(t,
		store.Provider{Name: "openai", Type: "openai-compatible", BaseURL: provider.URL},
		store.Model{Alias: "chat", ProviderName: "openai", ProviderModel: "chat-model", Status: "degraded", CapabilityOverrides: modelcap.Values{
			SupportsComputerUse: modelcap.Bool(true),
		}},
	)
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() { _ = server.Shutdown(ctx) }()

	response := postGatewayMessage(t, ctx, server.URL(), `{
		"model":"chat","max_tokens":16,
		"tools":[{"type":"computer_20250124","name":"computer"}],
		"messages":[{"role":"user","content":"take a screenshot"}]
	}`)
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotImplemented {
		t.Fatalf("gateway status = %d, want 501", response.StatusCode)
	}
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read gateway response: %v", err)
	}
	if !strings.Contains(string(raw), "Chat Completions fallback is disabled") {
		t.Fatalf("gateway response = %s", raw)
	}
	if called {
		t.Fatal("chat provider was called for computer use")
	}
}

func postGatewayMessage(t *testing.T, ctx context.Context, gatewayURL, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gatewayURL+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer local-token")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway request error = %v", err)
	}
	return response
}
