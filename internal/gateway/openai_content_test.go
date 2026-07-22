package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestAnthropicToolResultTextConvertsToolReferences(t *testing.T) {
	t.Parallel()

	got, err := anthropicToolResultText([]any{
		map[string]any{"type": "text", "text": "Loaded:"},
		map[string]any{"type": "tool_reference", "tool_name": "Agent"},
		map[string]any{"type": "text", "text": "Also loaded:"},
		map[string]any{"type": "tool_reference", "tool_name": "Workflow"},
	})
	if err != nil {
		t.Fatalf("anthropicToolResultText() error = %v", err)
	}
	want := "Loaded:\n[Loaded tool: Agent]\nAlso loaded:\n[Loaded tool: Workflow]"
	if got != want {
		t.Fatalf("anthropicToolResultText() = %q, want %q", got, want)
	}
}

func TestAnthropicToolResultTextRejectsInvalidToolReferences(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		block map[string]any
		want  string
	}{
		"missing name": {
			block: map[string]any{"type": "tool_reference"},
			want:  "tool_name must be a string",
		},
		"empty name": {
			block: map[string]any{"type": "tool_reference", "tool_name": " \t "},
			want:  "tool_name is required",
		},
		"non-string name": {
			block: map[string]any{"type": "tool_reference", "tool_name": 12},
			want:  "tool_name must be a string",
		},
		"control character": {
			block: map[string]any{"type": "tool_reference", "tool_name": "Tool\nSearch"},
			want:  "control characters",
		},
		"unknown field": {
			block: map[string]any{"type": "tool_reference", "tool_name": "Agent", "extra": true},
			want:  "field",
		},
		"unsupported type": {
			block: map[string]any{"type": "image", "source": "x"},
			want:  "not supported",
		},
	}
	for name, tt := range tests {
		tt := tt
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := anthropicToolResultText([]any{tt.block})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("anthropicToolResultText() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestToolReferenceOnlyAllowedInsideToolResult(t *testing.T) {
	t.Parallel()

	_, err := anthropicContentText([]any{
		map[string]any{"type": "tool_reference", "tool_name": "Agent"},
	})
	if err == nil || !strings.Contains(err.Error(), `content block field "tool_name" is not supported`) {
		t.Fatalf("anthropicContentText() error = %v", err)
	}
}

func TestGatewayRoutesToolSearchToolReferenceResultToOpenAIProvider(t *testing.T) {
	ctx := context.Background()
	var gotMessages []openAIMessage
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Messages []openAIMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("provider decode error = %v", err)
		}
		gotMessages = payload.Messages
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-test","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
	}))
	defer provider.Close()

	s := newGatewayStore(t, store.Provider{Name: "litellm", Type: "litellm", BaseURL: provider.URL, SecretRef: ""}, store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"})
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	body := `{
		"model":"gpt",
		"messages":[
			{"role":"assistant","content":[
				{"type":"tool_use","id":"toolu_toolsearch","name":"ToolSearch","input":{"query":"agent tools"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_toolsearch","content":[
					{"type":"text","text":"Loaded matching tools:"},
					{"type":"tool_reference","tool_name":"Agent"},
					{"type":"text","text":"Use it for child work."}
				]}
			]},
			{"role":"user","content":"start the child agent"}
		]
	}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer local-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway status = %d, want 200", resp.StatusCode)
	}
	if len(gotMessages) < 2 {
		t.Fatalf("provider messages = %#v, want assistant and tool messages", gotMessages)
	}
	if gotMessages[1].Role != "tool" || gotMessages[1].ToolCallID != "toolu_toolsearch" {
		t.Fatalf("tool result message = %#v", gotMessages[1])
	}
	wantContent := "Loaded matching tools:\n[Loaded tool: Agent]\nUse it for child work."
	if gotMessages[1].Content != wantContent {
		t.Fatalf("tool result content = %q, want %q", gotMessages[1].Content, wantContent)
	}
}

func openAIUserMessagesFromAnthropicForTest(t *testing.T, blocks []any) []openAIMessage {
	t.Helper()
	messages, err := openAIUserMessagesFromAnthropicWithResolver(context.Background(), blocks, testImageSourceResolver)
	if err != nil {
		t.Fatalf("openAIUserMessagesFromAnthropicWithResolver() error = %v", err)
	}
	return messages
}

func testImageSourceResolver(_ context.Context, block map[string]any) (map[string]any, error) {
	source, ok := block["source"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("image block source must be an object")
	}
	sourceType, _ := source["type"].(string)
	switch strings.TrimSpace(sourceType) {
	case "base64":
		return resolveBase64Image(source)
	case "url":
		rawURL, _ := source["url"].(string)
		rawURL = strings.TrimSpace(rawURL)
		if rawURL == "" {
			return nil, fmt.Errorf("url image source requires a url")
		}
		return openAIImageURLPart(rawURL), nil
	default:
		return nil, fmt.Errorf("image source type %q is not supported", sourceType)
	}
}

func messageContentParts(t *testing.T, message openAIMessage) []map[string]any {
	t.Helper()
	rawParts, ok := message.Content.([]any)
	if !ok {
		t.Fatalf("message content = %#v, want multipart array", message.Content)
	}
	parts := make([]map[string]any, 0, len(rawParts))
	for _, raw := range rawParts {
		part, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("content part = %#v, want object", raw)
		}
		parts = append(parts, part)
	}
	return parts
}

func partText(part map[string]any) string {
	text, _ := part["text"].(string)
	return text
}

func partImageURL(t *testing.T, part map[string]any) string {
	t.Helper()
	switch image := part["image_url"].(type) {
	case map[string]string:
		return image["url"]
	case map[string]any:
		url, _ := image["url"].(string)
		return url
	default:
		t.Fatalf("image_url = %#v, want object", image)
		return ""
	}
}

func TestOpenAIUserMessagesConvertBase64Image(t *testing.T) {
	t.Parallel()

	messages := openAIUserMessagesFromAnthropicForTest(t, []any{
		map[string]any{"type": "text", "text": "describe this"},
		map[string]any{"type": "image", "source": map[string]any{
			"type": "base64", "media_type": "image/png", "data": "AAAA",
		}},
	})
	if len(messages) != 1 || messages[0].Role != "user" {
		t.Fatalf("messages = %#v", messages)
	}
	parts := messageContentParts(t, messages[0])
	if len(parts) != 2 || parts[0]["type"] != "text" || partText(parts[0]) != "describe this" {
		t.Fatalf("text part = %#v", parts)
	}
	if parts[1]["type"] != "image_url" || partImageURL(t, parts[1]) != "data:image/png;base64,AAAA" {
		t.Fatalf("image part = %#v", parts[1])
	}
	encoded, err := json.Marshal(messages[0])
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	want := `{"role":"user","content":[{"text":"describe this","type":"text"},{"image_url":{"url":"data:image/png;base64,AAAA"},"type":"image_url"}]}`
	if string(encoded) != want {
		t.Fatalf("marshaled = %s, want %s", encoded, want)
	}
}

func TestOpenAIUserMessagesConvertURLImage(t *testing.T) {
	t.Parallel()

	messages := openAIUserMessagesFromAnthropicForTest(t, []any{
		map[string]any{"type": "image", "source": map[string]any{
			"type": "url", "url": "https://example.com/cat.png",
		}},
	})
	if len(messages) != 1 {
		t.Fatalf("messages = %#v", messages)
	}
	parts := messageContentParts(t, messages[0])
	if len(parts) != 1 || parts[0]["type"] != "image_url" || partImageURL(t, parts[0]) != "https://example.com/cat.png" {
		t.Fatalf("image parts = %#v", parts)
	}
}

func TestOpenAIUserMessagesKeepTextOnlyAsString(t *testing.T) {
	t.Parallel()

	messages := openAIUserMessagesFromAnthropicForTest(t, []any{
		map[string]any{"type": "text", "text": "line one"},
		map[string]any{"type": "text", "text": "line two"},
	})
	if len(messages) != 1 || messages[0].Content != "line one\nline two" {
		t.Fatalf("messages = %#v", messages)
	}
	encoded, err := json.Marshal(messages[0])
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if string(encoded) != `{"role":"user","content":"line one\nline two"}` {
		t.Fatalf("marshaled = %s", encoded)
	}
}

func TestOpenAIUserMessagesEmitToolBeforeUserContentForContiguity(t *testing.T) {
	t.Parallel()

	// A user turn that mixes a top-level image, a tool_result, and trailing text
	// must still emit the tool message directly after the assistant tool_calls
	// message. The tool message therefore comes first; all user content (the
	// image and the text) collapses into a single trailing user message.
	messages := openAIUserMessagesFromAnthropicForTest(t, []any{
		map[string]any{"type": "image", "source": map[string]any{
			"type": "base64", "media_type": "image/jpeg", "data": "ZZZZ",
		}},
		map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "ok"},
		map[string]any{"type": "text", "text": "after tool"},
	})
	if len(messages) != 2 {
		t.Fatalf("messages = %#v, want tool message then a single user message", messages)
	}
	if messages[0].Role != "tool" || messages[0].ToolCallID != "toolu_1" || messages[0].Content != "ok" {
		t.Fatalf("tool message = %#v", messages[0])
	}
	parts := messageContentParts(t, messages[1])
	if messages[1].Role != "user" || len(parts) != 2 {
		t.Fatalf("user message = %#v", messages[1])
	}
	if parts[0]["type"] != "image_url" || parts[1]["type"] != "text" || partText(parts[1]) != "after tool" {
		t.Fatalf("user parts = %#v", parts)
	}
}

func TestOpenAIUserMessagesKeepParallelToolResultsContiguous(t *testing.T) {
	t.Parallel()

	// Parallel tool calls stay contiguous even when the user turn also carries
	// top-level image/text content.
	messages := openAIUserMessagesFromAnthropicForTest(t, []any{
		map[string]any{"type": "image", "source": map[string]any{
			"type": "base64", "media_type": "image/png", "data": "AAAA",
		}},
		map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "first result"},
		map[string]any{"type": "tool_result", "tool_use_id": "toolu_2", "content": "second result"},
		map[string]any{"type": "text", "text": "after tools"},
	})
	if len(messages) != 3 {
		t.Fatalf("messages = %#v, want tool, tool, user(image+text)", messages)
	}
	if messages[0].Role != "tool" || messages[0].ToolCallID != "toolu_1" || messages[0].Content != "first result" {
		t.Fatalf("first tool message = %#v", messages[0])
	}
	if messages[1].Role != "tool" || messages[1].ToolCallID != "toolu_2" || messages[1].Content != "second result" {
		t.Fatalf("second tool message = %#v", messages[1])
	}
	parts := messageContentParts(t, messages[2])
	if messages[2].Role != "user" || len(parts) != 2 || parts[0]["type"] != "image_url" ||
		partImageURL(t, parts[0]) != "data:image/png;base64,AAAA" || partText(parts[1]) != "after tools" {
		t.Fatalf("trailing user message = %#v", messages[2])
	}
}

func TestOpenAIToolResultRejectsImageContent(t *testing.T) {
	t.Parallel()

	_, err := openAIUserMessagesFromAnthropicWithResolver(context.Background(), []any{
		map[string]any{"type": "tool_result", "tool_use_id": "toolu_shot", "content": []any{
			map[string]any{"type": "text", "text": "screenshot captured"},
			map[string]any{"type": "image", "source": map[string]any{
				"type": "base64", "media_type": "image/png", "data": "SHOT",
			}},
		}},
	}, testImageSourceResolver)
	if err == nil || !strings.Contains(err.Error(), "image tool_result content is not supported") {
		t.Fatalf("error = %v, want image tool_result rejection", err)
	}
}

func TestOpenAIUserMessagesRejectInvalidImageSources(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		block map[string]any
		want  string
	}{
		"missing source": {
			block: map[string]any{"type": "image"},
			want:  "source must be an object",
		},
		"base64 missing data": {
			block: map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png"}},
			want:  "media_type and data",
		},
		"url missing url": {
			block: map[string]any{"type": "image", "source": map[string]any{"type": "url"}},
			want:  "requires a url",
		},
		"unsupported source type": {
			block: map[string]any{"type": "image", "source": map[string]any{"type": "file_id"}},
			want:  "is not supported",
		},
	}
	for name, tc := range tests {
		tc := tc
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := openAIUserMessagesFromAnthropicWithResolver(context.Background(), []any{tc.block}, testImageSourceResolver)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestOpenAIUserMessagesPreserveMultiImageOrder(t *testing.T) {
	t.Parallel()

	messages := openAIUserMessagesFromAnthropicForTest(t, []any{
		map[string]any{"type": "text", "text": "first"},
		map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "T05F"}},
		map[string]any{"type": "text", "text": "second"},
		map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "https://example.com/two.png"}},
	})
	if len(messages) != 1 {
		t.Fatalf("messages = %#v", messages)
	}
	parts := messageContentParts(t, messages[0])
	if len(parts) != 4 ||
		parts[0]["type"] != "text" || partText(parts[0]) != "first" ||
		parts[1]["type"] != "image_url" || partImageURL(t, parts[1]) != "data:image/png;base64,T05F" ||
		parts[2]["type"] != "text" || partText(parts[2]) != "second" ||
		parts[3]["type"] != "image_url" || partImageURL(t, parts[3]) != "https://example.com/two.png" {
		t.Fatalf("multi-image order = %#v", parts)
	}
}

func TestOpenAIUserMessagesDropEmptyTextPartInMultipart(t *testing.T) {
	t.Parallel()

	messages := openAIUserMessagesFromAnthropicForTest(t, []any{
		map[string]any{"type": "text", "text": ""},
		map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "AAAA"}},
	})
	parts := messageContentParts(t, messages[0])
	if len(messages) != 1 || len(parts) != 1 || parts[0]["type"] != "image_url" {
		t.Fatalf("messages = %#v, want empty text part dropped", messages)
	}
	encoded, err := json.Marshal(messages[0])
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	want := `{"role":"user","content":[{"image_url":{"url":"data:image/png;base64,AAAA"},"type":"image_url"}]}`
	if string(encoded) != want {
		t.Fatalf("marshaled = %s, want %s", encoded, want)
	}
}
