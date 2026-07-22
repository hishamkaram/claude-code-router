package responses

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestResponsesToolChoiceKeepsFunctionNamedComputer(t *testing.T) {
	t.Parallel()

	request, err := RequestFromAnthropicMessagesJSON([]byte(`{
  "model":"fixture",
  "tools":[{"name":"computer","input_schema":{"type":"object"}}],
  "tool_choice":{"type":"tool","name":"computer"},
  "messages":[{"role":"user","content":"go"}]
}`))
	if err != nil {
		t.Fatalf("RequestFromAnthropicMessagesJSON() error = %v", err)
	}
	assertResponsesJSON(t, request.ToolChoice, `{"type":"function","name":"computer"}`)
}

func TestResponsesToolChoiceUsesNativeComputerWhenConfigured(t *testing.T) {
	t.Parallel()

	request, err := RequestFromAnthropicMessagesJSON([]byte(`{
  "model":"fixture",
  "tools":[{"type":"computer_20250124","name":"computer"}],
  "tool_choice":{"type":"tool","name":"computer"},
  "messages":[{"role":"user","content":"go"}]
}`))
	if err != nil {
		t.Fatalf("RequestFromAnthropicMessagesJSON() error = %v", err)
	}
	assertResponsesJSON(t, request.ToolChoice, `{"type":"computer"}`)
}

func TestResponsesRejectsAmbiguousComputerFunctionWithNativeTool(t *testing.T) {
	t.Parallel()

	_, err := RequestFromAnthropicMessagesJSON([]byte(`{
  "model":"fixture",
  "tools":[
    {"type":"computer_20250124","name":"computer"},
    {"name":"computer","input_schema":{"type":"object"}}
  ],
  "tool_choice":{"type":"tool","name":"computer"},
  "messages":[{"role":"user","content":"go"}]
}`))
	if err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("RequestFromAnthropicMessagesJSON() error = %v, want ambiguous computer tool rejection", err)
	}
}

func TestResponsesDisablesParallelToolCallsWhenRequested(t *testing.T) {
	t.Parallel()

	request, err := RequestFromAnthropicMessagesJSON([]byte(`{
  "model":"fixture",
  "tools":[{"name":"lookup","input_schema":{"type":"object"}}],
  "tool_choice":{"type":"any","disable_parallel_tool_use":true},
  "messages":[{"role":"user","content":"go"}]
}`))
	if err != nil {
		t.Fatalf("RequestFromAnthropicMessagesJSON() error = %v", err)
	}
	if request.ParallelToolCalls == nil || *request.ParallelToolCalls {
		t.Fatalf("ParallelToolCalls = %#v, want false", request.ParallelToolCalls)
	}
}

func TestResponsesConvertsToolReferenceResult(t *testing.T) {
	t.Parallel()

	request, err := RequestFromAnthropicMessagesJSON([]byte(`{
  "model":"fixture",
  "tools":[{"name":"ToolSearch","input_schema":{"type":"object"}}],
  "messages":[
    {"role":"assistant","content":[{"type":"tool_use","id":"toolu_search","name":"ToolSearch","input":{}}]},
    {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_search","content":[
      {"type":"text","text":"Loaded matching tools:"},
      {"type":"tool_reference","tool_name":"Agent"}
    ]}]}
  ]
}`))
	if err != nil {
		t.Fatalf("RequestFromAnthropicMessagesJSON() error = %v", err)
	}
	if len(request.Input) != 2 || request.Input[1].Type != "function_call_output" ||
		request.Input[1].Output != "Loaded matching tools:\n[Loaded tool: Agent]" {
		t.Fatalf("converted input = %#v", request.Input)
	}
}

func TestResponsesComputerScrollHistoryUsesCanonicalDeltas(t *testing.T) {
	t.Parallel()

	request, err := RequestFromAnthropicMessagesJSON([]byte(`{
  "model":"fixture",
  "tools":[{"type":"computer_20250124","name":"computer"}],
  "messages":[
    {"role":"assistant","content":[{"type":"tool_use","id":"toolu_screen","name":"computer","input":{
      "action":"scroll",
      "coordinate":[500,400],
      "scroll_direction":"down",
      "scroll_amount":3
    }}]}
  ]
}`))
	if err != nil {
		t.Fatalf("RequestFromAnthropicMessagesJSON() error = %v", err)
	}
	if len(request.Input) != 1 || request.Input[0].Type != "computer_call" || len(request.Input[0].Actions) != 1 {
		t.Fatalf("converted input = %#v", request.Input)
	}
	assertResponsesJSON(t, request.Input[0].Actions[0], `{"type":"scroll","x":500,"y":400,"scroll_x":0,"scroll_y":300}`)
}

func TestResponsesComputerTypeHistoryPreservesWhitespace(t *testing.T) {
	t.Parallel()

	request, err := RequestFromAnthropicMessagesJSON([]byte(`{
  "model":"fixture",
  "tools":[{"type":"computer_20250124","name":"computer"}],
  "messages":[
    {"role":"assistant","content":[{"type":"tool_use","id":"toolu_type","name":"computer","input":{
      "action":"type",
      "text":" pass \n"
    }}]}
  ]
}`))
	if err != nil {
		t.Fatalf("RequestFromAnthropicMessagesJSON() error = %v", err)
	}
	if len(request.Input) != 1 || request.Input[0].Type != "computer_call" || len(request.Input[0].Actions) != 1 {
		t.Fatalf("converted input = %#v", request.Input)
	}
	assertResponsesJSON(t, request.Input[0].Actions[0], `{"type":"type","text":" pass \n"}`)
}

func assertResponsesJSON(t *testing.T, value any, want string) {
	t.Helper()
	got, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal value: %v", err)
	}

	var gotValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("unmarshal actual JSON: %v", err)
	}
	var wantValue any
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("unmarshal expected JSON: %v", err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("JSON = %s, want %s", got, want)
	}
}
