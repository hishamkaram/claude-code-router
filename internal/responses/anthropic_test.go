package responses

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestRequestFromAnthropicMessagesJSONFullFixture(t *testing.T) {
	t.Parallel()

	temp := 0.2
	got, err := RequestFromAnthropicMessagesJSON([]byte(fixture(t, "anthropic_request_full.json")))
	if err != nil {
		t.Fatalf("RequestFromAnthropicMessagesJSON() error = %v", err)
	}
	assertJSONEqual(t, got, wantFullFixtureRequest(&temp))
}

func TestRequestFromAnthropicMessagesJSONBasicCases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want *Request
	}{
		{
			name: "string user content",
			raw:  `{"model":"claude","metadata":{"user_id":"fixture-user"},"messages":[{"role":"user","content":"hello"}]}`,
			want: &Request{
				Model:    "claude",
				Metadata: map[string]string{"user_id": "fixture-user"},
				Input: []InputItem{{
					Type:    "message",
					Role:    "user",
					Content: []Content{{Type: "input_text", Text: "hello"}},
				}},
			},
		},
		{
			name: "assistant text history",
			raw:  `{"model":"claude","messages":[{"role":"assistant","content":[{"type":"text","text":"prior"}]}]}`,
			want: &Request{
				Model: "claude",
				Input: []InputItem{{
					Type:    "message",
					Role:    "assistant",
					Content: []Content{{Type: "output_text", Text: "prior"}},
				}},
			},
		},
		{
			name: "system message history uses developer role",
			raw:  `{"model":"claude","messages":[{"role":"system","content":[{"type":"text","text":"do not disclose secrets"}]},{"role":"user","content":"hello"}]}`,
			want: &Request{
				Model: "claude",
				Input: []InputItem{
					{Type: "message", Role: "developer", Content: []Content{{Type: "input_text", Text: "do not disclose secrets"}}},
					{Type: "message", Role: "user", Content: []Content{{Type: "input_text", Text: "hello"}}},
				},
			},
		},
		{
			name: "thinking and structured output options",
			raw: `{
				"model":"claude",
				"thinking":{"type":"enabled"},
				"output_config":{"effort":"medium","format":{"type":"json_schema","schema":{"type":"object","properties":{"answer":{"type":"string"}}}}},
				"messages":[{"role":"user","content":"go"}]
			}`,
			want: &Request{
				Model: "claude",
				Input: []InputItem{{
					Type: "message", Role: "user", Content: []Content{{Type: "input_text", Text: "go"}},
				}},
				Reasoning: &Reasoning{Effort: "medium"},
				Text: &Text{Format: &TextFormat{
					Type: "json_schema", Name: "claude_output", Strict: true,
					Schema: rawJSON(`{"type":"object","properties":{"answer":{"type":"string"}}}`),
				}},
			},
		},
		{
			name: "tool choice converts forced function",
			raw: `{
				"model":"claude",
				"tools":[{"name":"lookup","input_schema":{"type":"object"}}],
				"tool_choice":{"type":"tool","name":"lookup"},
				"messages":[{"role":"user","content":"go"}]
			}`,
			want: &Request{
				Model:      "claude",
				Input:      []InputItem{{Type: "message", Role: "user", Content: []Content{{Type: "input_text", Text: "go"}}}},
				Tools:      []Tool{{Type: "function", Name: "lookup", Parameters: rawJSON(`{"type":"object"}`)}},
				ToolChoice: map[string]string{"type": "function", "name": "lookup"},
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := RequestFromAnthropicMessagesJSON([]byte(test.raw))
			if err != nil {
				t.Fatalf("RequestFromAnthropicMessagesJSON() error = %v", err)
			}
			assertJSONEqual(t, got, test.want)
		})
	}
}

func wantFullFixtureRequest(temp *float64) *Request {
	return &Request{
		Model:           "claude-sonnet-4-5",
		Instructions:    "Be concise.\nUse tools when helpful.",
		MaxOutputTokens: 256,
		Temperature:     temp,
		Stop:            []string{"END"},
		Tools: []Tool{
			{
				Type:        "function",
				Name:        "get_weather",
				Description: "Get weather by city.",
				Parameters:  rawJSON(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
			},
			{Type: "computer"},
		},
		Input: []InputItem{
			{
				Type: "message",
				Role: "user",
				Content: []Content{
					{Type: "input_text", Text: "What is in these images?"},
					{Type: "input_image", ImageURL: "data:image/png;base64,iVBORw0KGgo="},
					{Type: "input_image", ImageURL: "https://example.com/cat.jpg"},
				},
			},
			{
				Type:      "function_call",
				CallID:    "toolu_weather",
				Name:      "get_weather",
				Arguments: `{"city":"Berlin"}`,
			},
			{
				Type:    "computer_call",
				CallID:  "call_screen",
				Status:  "completed",
				Actions: []json.RawMessage{rawJSON(`{"type":"screenshot"}`)},
			},
			{Type: "function_call_output", CallID: "toolu_weather", Output: "Sunny"},
			{
				Type:   "computer_call_output",
				CallID: "call_screen",
				Output: ComputerScreenshot{
					Type:     "computer_screenshot",
					ImageURL: "data:image/png;base64,screenbase64",
					Detail:   "original",
				},
			},
		},
	}
}

func TestRequestFromAnthropicMessagesJSONErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		err  error
	}{
		{
			name: "unsupported PDF document",
			raw: `{"model":"claude","messages":[{"role":"user","content":[
				{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"abc"}}
			]}]}`,
			err: ErrUnsupportedPDF,
		},
		{
			name: "unsupported audio block",
			raw: `{"model":"claude","messages":[{"role":"user","content":[
				{"type":"audio","source":{"type":"base64","media_type":"audio/mpeg","data":"abc"}}
			]}]}`,
			err: ErrUnsupportedAudio,
		},
		{
			name: "tool result image without native CUA",
			raw: `{"model":"claude","messages":[{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_1","content":[
					{"type":"image","source":{"type":"base64","media_type":"image/png","data":"abc"}}
				]}
			]}]}`,
			err: ErrNativeCUARequired,
		},
		{
			name: "reject non HTTPS image URL",
			raw: `{"model":"claude","messages":[{"role":"user","content":[
				{"type":"image","source":{"type":"url","url":"http://example.com/image.png"}}
			]}]}`,
		},
		{
			name: "function tool missing schema",
			raw:  `{"model":"claude","tools":[{"name":"lookup"}],"messages":[{"role":"user","content":"go"}]}`,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := RequestFromAnthropicMessagesJSON([]byte(test.raw))
			if err == nil {
				t.Fatal("RequestFromAnthropicMessagesJSON() unexpectedly succeeded")
			}
			if test.err != nil && !errors.Is(err, test.err) {
				t.Fatalf("error = %v, want errors.Is(%v)", err, test.err)
			}
		})
	}
}

func TestNativeComputerToolDetectionDoesNotCaptureFunctionNamedComputer(t *testing.T) {
	t.Parallel()

	raw := `{
		"model":"claude",
		"tools":[{"name":"computer","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}],
		"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_computer","name":"computer","input":{"path":"/tmp/a"}}]},
			{"role":"user","content":"call function"}
		]
	}`
	got, err := RequestFromAnthropicMessagesJSON([]byte(raw))
	if err != nil {
		t.Fatalf("RequestFromAnthropicMessagesJSON() error = %v", err)
	}
	if len(got.Tools) != 1 || got.Tools[0].Type != "function" || got.Tools[0].Name != "computer" {
		t.Fatalf("Tools = %#v, want function tool named computer", got.Tools)
	}
	if got.Input[0].Type != "function_call" || got.Input[0].Name != "computer" {
		t.Fatalf("first input item = %#v, want function_call named computer", got.Input[0])
	}
}

func TestResponsesComputerToolUsesGAComputerTool(t *testing.T) {
	t.Parallel()

	got, err := RequestFromAnthropicMessagesJSON([]byte(`{
		"model":"fixture",
		"tools":[{"type":"computer_20250124","name":"computer","display_width_px":1366,"display_height_px":900}],
		"messages":[{"role":"user","content":"take a screenshot"}]
	}`))
	if err != nil {
		t.Fatalf("RequestFromAnthropicMessagesJSON() error = %v", err)
	}
	if len(got.Tools) != 1 {
		t.Fatalf("tools = %#v, want one native computer tool", got.Tools)
	}
	assertResponsesJSON(t, got.Tools[0], `{"type":"computer"}`)
}

func TestResponsesComputerCallRequiresManagedExecutor(t *testing.T) {
	t.Parallel()

	_, err := AnthropicResponseFromResponsesJSON([]byte(`{
		"id":"resp_1",
		"model":"fixture",
		"output":[{"type":"computer_call","call_id":"call_screen","actions":[
			{"type":"click","x":10,"y":20},
			{"type":"type","text":"search"}
		]}]
	}`))
	if !errors.Is(err, ErrMalformedProviderOutput) || !strings.Contains(err.Error(), "requires a CCR managed CUA executor") {
		t.Fatalf("AnthropicResponseFromResponsesJSON() error = %v, want managed CUA rejection", err)
	}
}

func TestNativeComputerRejectsImageResultForFunctionCall(t *testing.T) {
	t.Parallel()

	_, err := RequestFromAnthropicMessagesJSON([]byte(`{
		"model":"fixture",
		"tools":[
			{"type":"computer_20250124","name":"computer"},
			{"name":"lookup","input_schema":{"type":"object"}}
		],
		"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_lookup","name":"lookup","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_lookup","content":[
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"c2NyZWVu"}}
			]}]}
		]
	}`))
	if !errors.Is(err, ErrNativeCUARequired) {
		t.Fatalf("RequestFromAnthropicMessagesJSON() error = %v, want ErrNativeCUARequired", err)
	}
}

func assertJSONEqual(t *testing.T, got, want any) {
	t.Helper()

	gotBytes, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal got: %v", err)
	}
	wantBytes, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}
	var gotJSON any
	var wantJSON any
	if err := json.Unmarshal(gotBytes, &gotJSON); err != nil {
		t.Fatalf("unmarshal got JSON: %v", err)
	}
	if err := json.Unmarshal(wantBytes, &wantJSON); err != nil {
		t.Fatalf("unmarshal want JSON: %v", err)
	}
	if !reflect.DeepEqual(gotJSON, wantJSON) {
		t.Fatalf("JSON mismatch\ngot:  %s\nwant: %s", gotBytes, wantBytes)
	}
}

func rawJSON(value string) json.RawMessage {
	return json.RawMessage(value)
}

func fixture(t *testing.T, name string) string {
	t.Helper()

	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return strings.TrimSpace(string(data))
}
