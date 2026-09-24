package gateway

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	openairesponses "github.com/hishamkaram/claude-code-router/internal/responses"
)

func TestAnthropicUsageFromJSON(t *testing.T) {
	t.Parallel()
	usage := anthropicUsageFromJSON([]byte(`{
		"usage": {
			"input_tokens": 12,
			"output_tokens": 7,
			"cache_read_input_tokens": 5,
			"cache_creation_input_tokens": 3
		}
	}`))
	if !usage.Observed || usage.InputTokens != 12 || usage.OutputTokens != 7 ||
		usage.CacheReadTokens != 5 || usage.CacheWriteTokens != 3 {
		t.Fatalf("anthropicUsageFromJSON() = %#v", usage)
	}
}

func TestAnthropicUsageFromCountTokensJSON(t *testing.T) {
	t.Parallel()
	usage := anthropicUsageFromJSON([]byte(`{"input_tokens":42}`))
	if !usage.Observed || usage.InputTokens != 42 || usage.OutputTokens != 0 {
		t.Fatalf("anthropicUsageFromJSON() = %#v", usage)
	}
}

func TestCopyAndRewriteSSECapturesUsage(t *testing.T) {
	t.Parallel()
	stream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"model":"provider-model","usage":{"input_tokens":13,"cache_read_input_tokens":2}}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","usage":{"output_tokens":8}}`,
		``,
	}, "\n")
	response := httptest.NewRecorder()
	usage := copyAndRewriteSSE(response, strings.NewReader(stream), response, "coder")
	if !usage.Observed || usage.InputTokens != 13 || usage.OutputTokens != 8 || usage.CacheReadTokens != 2 {
		t.Fatalf("copyAndRewriteSSE() usage = %#v", usage)
	}
	if !bytes.Contains(response.Body.Bytes(), []byte(`"model":"coder"`)) {
		t.Fatalf("rewritten stream = %s", response.Body.String())
	}
}

func TestOpenAIUsagePresenceDistinguishesUnavailableFromZero(t *testing.T) {
	t.Parallel()
	var withUsage openAIChatResponse
	if err := withUsage.UnmarshalJSON([]byte(`{"choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0}}`)); err != nil {
		t.Fatalf("UnmarshalJSON(with usage) error = %v", err)
	}
	if usage := tokenUsageFromOpenAI(withUsage); !usage.Observed {
		t.Fatalf("tokenUsageFromOpenAI(with usage) = %#v", usage)
	}
	var withoutUsage openAIChatResponse
	if err := withoutUsage.UnmarshalJSON([]byte(`{"choices":[]}`)); err != nil {
		t.Fatalf("UnmarshalJSON(without usage) error = %v", err)
	}
	if usage := tokenUsageFromOpenAI(withoutUsage); usage.Observed {
		t.Fatalf("tokenUsageFromOpenAI(without usage) = %#v", usage)
	}
}

func TestResponsesUsagePresenceDistinguishesUnavailableFromZero(t *testing.T) {
	t.Parallel()
	var withUsage openairesponses.Response
	if err := json.Unmarshal([]byte(`{"output":[],"usage":{"input_tokens":0,"output_tokens":0}}`), &withUsage); err != nil {
		t.Fatalf("UnmarshalJSON(with usage) error = %v", err)
	}
	if usage := tokenUsageFromResponses(&withUsage); !usage.Observed {
		t.Fatalf("tokenUsageFromResponses(with usage) = %#v", usage)
	}
	var withoutUsage openairesponses.Response
	if err := json.Unmarshal([]byte(`{"output":[]}`), &withoutUsage); err != nil {
		t.Fatalf("UnmarshalJSON(without usage) error = %v", err)
	}
	if usage := tokenUsageFromResponses(&withoutUsage); usage.Observed {
		t.Fatalf("tokenUsageFromResponses(without usage) = %#v", usage)
	}
}
