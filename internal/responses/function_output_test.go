package responses

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFunctionImageToolResults(t *testing.T) {
	t.Parallel()
	const image = `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"abc"}}`
	const urlImage = `{"type":"image","source":{"type":"url","url":"https://example.com/screen.png"}}`
	for _, test := range []struct{ name, content, want string }{
		{"image", `[` + image + `]`, `[{"type":"input_image","image_url":"data:image/png;base64,abc","detail":"auto"}]`},
		{"mixed", `[{"type":"text","text":"before"},` + image + `,{"type":"text","text":""},` + urlImage + `,{"type":"text","text":"after"}]`, `[{"type":"input_text","text":"before"},{"type":"input_image","image_url":"data:image/png;base64,abc","detail":"auto"},{"type":"input_text","text":""},{"type":"input_image","image_url":"https://example.com/screen.png","detail":"auto"},{"type":"input_text","text":"after"}]`},
		{"text", `[{"type":"text","text":"a"},{"type":"text","text":"b"}]`, `"a\nb"`},
		{"empty", `[]`, `""`},
		{"null", `null`, `""`},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := `{"model":"gpt","tools":[{"name":"screenshot","input_schema":{"type":"object"}}],"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_image","name":"screenshot","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_image","content":` + test.content + `}]}]}`
			got, err := RequestFromAnthropicMessagesJSON([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			assertResponsesJSON(t, got.Input[1], `{"type":"function_call_output","call_id":"call_image","output":`+test.want+`}`)
			if len(got.Tools) != 1 || got.Tools[0].Type != "function" {
				t.Fatalf("unexpected native tool: %#v", got.Tools)
			}
		})
	}
}

func TestFunctionToolResultRejectsInvalidContent(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		`[{"type":"image","source":{"type":"url","url":"http://example.com/image.png"}}]`,
		`[{"type":"image","source":{"type":"base64","media_type":"image/png","data":""}}]`,
		`[{"type":"document"}]`, `[{"type":"audio"}]`, `[{"type":"unknown"}]`, `{}`, `42`,
	} {
		if _, err := functionToolResultOutput(json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted invalid content: %s", raw)
		}
	}
	_, err := toolResultInputItems(json.RawMessage(`{"content":[]}`), &convertState{})
	if err == nil || !strings.Contains(err.Error(), "missing tool_use_id") {
		t.Fatalf("missing ID error = %v", err)
	}
}
