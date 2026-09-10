package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAIStreamTextFidelity(t *testing.T) {
	t.Parallel()
	const marker = "CCR_LIVE_REAL_ALIAS_33"
	for _, test := range []struct {
		name   string
		deltas []string
		want   string
	}{
		{"single delta", []string{marker}, marker},
		{"split marker", []string{"CCR_", "LIVE_REAL_", "ALIAS_33"}, marker},
		{"repeated deltas", []string{marker, marker}, marker + marker},
		{"repeated text in one delta", []string{marker + marker}, marker + marker},
		{"empty deltas", []string{"", "CCR_LIVE_REAL_", "", "ALIAS_33", ""}, marker},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			body := fidelityChatStream(t, test.deltas)
			buffered, err := decodeOpenAIChatStream(strings.NewReader(body), maxOpenAIChatResponseBytes, "")
			if err != nil {
				t.Fatal(err)
			}
			if len(buffered.Choices) != 1 || buffered.Choices[0].Message.Content == nil || *buffered.Choices[0].Message.Content != test.want {
				t.Fatalf("buffered text differs from upstream deltas: %#v", buffered.Choices)
			}
			recorder := httptest.NewRecorder()
			result := runTranslatedProviderStream(t.Context(), recorder,
				func(ctx context.Context, events chan<- upstreamStreamEvent) {
					if sendUpstreamStreamEvent(ctx, events, upstreamStreamEvent{kind: upstreamStreamReady, status: http.StatusOK}) {
						forwardOpenAIChatStream(ctx, strings.NewReader(body), events)
					}
				}, newOpenAIChatStreamAdapter("fixture", "", "msg_fidelity"))
			if result.TerminalPhase != "completed" || result.ErrorClass != "" {
				t.Fatalf("stream result = %#v", result)
			}
			assertFidelityText(t, recorder.Body.String(), test.want)
		})
	}
}

func fidelityChatStream(t *testing.T, deltas []string) string {
	t.Helper()
	var stream strings.Builder
	for _, delta := range deltas {
		encoded, err := json.Marshal(delta)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&stream, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%s}}]}\n\n", encoded)
	}
	stream.WriteString("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
	stream.WriteString("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":7}}\n\n")
	stream.WriteString("data: [DONE]\n\n")
	return stream.String()
}

func assertFidelityText(t *testing.T, body, want string) {
	t.Helper()
	scanner := newOpenAISSEScanner(strings.NewReader(body), maxOpenAIChatResponseBytes)
	var text strings.Builder
	counts := make(map[string]int)
	for {
		data, ok, err := scanner.next()
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		var event struct {
			Type         string `json:"type"`
			ContentBlock struct {
				Text string `json:"text"`
			} `json:"content_block"`
			Delta struct {
				Text string `json:"text"`
			} `json:"delta"`
		}
		if err := json.Unmarshal(data, &event); err != nil {
			t.Fatal(err)
		}
		counts[event.Type]++
		text.WriteString(event.ContentBlock.Text)
		text.WriteString(event.Delta.Text)
	}
	if text.String() != want {
		t.Fatalf("translated text = %q, want %q", text.String(), want)
	}
	for _, name := range []string{"message_start", "content_block_start", "content_block_stop", "message_delta", "message_stop"} {
		if counts[name] != 1 {
			t.Errorf("%s count = %d, want 1", name, counts[name])
		}
	}
}
