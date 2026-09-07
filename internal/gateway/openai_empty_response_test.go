package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAIEmptyCompletedTurnIsNotSuccessful(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, body string
		wantError  bool
	}{
		{"empty stop", `{"choices":[{"message":{"content":""},"finish_reason":"stop"}]}`, true},
		{"null stop", `{"choices":[{"message":{"content":null},"finish_reason":"stop"}]}`, true},
		{"length", `{"choices":[{"message":{"content":""},"finish_reason":"length"}]}`, false},
		{"text", `{"choices":[{"message":{"content":"OK"},"finish_reason":"stop"}]}`, false},
		{"tool", `{"choices":[{"message":{"tool_calls":[{"id":"t","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"stop"}]}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var response openAIChatResponse
			if err := json.Unmarshal([]byte(test.body), &response); err != nil {
				t.Fatal(err)
			}
			if _, err := anthropicStopReasonFromOpenAI(response); (err != nil) != test.wantError {
				t.Fatalf("stop reason error = %v", err)
			}
		})
	}
}

func TestOpenAIEmptyStreamPreservesReportedUsage(t *testing.T) {
	t.Parallel()
	result := runTranslatedProviderStream(context.Background(), httptest.NewRecorder(),
		func(ctx context.Context, events chan<- upstreamStreamEvent) {
			sendUpstreamStreamEvent(ctx, events, upstreamStreamEvent{kind: upstreamStreamReady, status: http.StatusOK})
			sendUpstreamStreamEvent(ctx, events, upstreamStreamEvent{kind: upstreamStreamData, data: []byte(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":7}}`)})
			sendUpstreamStreamEvent(ctx, events, upstreamStreamEvent{kind: upstreamStreamDone})
		}, newOpenAIChatStreamAdapter("fixture", "", "msg_fixture"))
	if result.TerminalPhase != "failed" || result.ErrorClass != "provider_protocol" {
		t.Fatalf("empty reply was not rejected: %+v", result)
	}
	if !result.Usage.Observed || result.Usage.InputTokens != 100 || result.Usage.OutputTokens != 7 {
		t.Fatalf("reported usage lost on empty reply: %+v", result.Usage)
	}
}

func TestOpenAIEmptyStreamDoesNotFabricateContent(t *testing.T) {
	t.Parallel()
	for _, finish := range []string{"stop", "length"} {
		t.Run(finish, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			writer := newAnthropicSSEWriter(recorder)
			if err := writer.Start(); err != nil {
				t.Fatal(err)
			}
			adapter := &openAIChatStreamAdapter{sawChoice: true, finishReason: finish}
			_, err := adapter.Finish(writer)
			if finish == "stop" {
				if err == nil || strings.Contains(recorder.Body.String(), "message_stop") || strings.Contains(recorder.Body.String(), "content_block_start") {
					t.Fatalf("empty successful turn was fabricated: error=%v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}
