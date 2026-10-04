package responses

import (
	"encoding/json"
	"testing"
)

func TestResponsesToolsPreserveAnthropicStrictnessOnWire(t *testing.T) {
	t.Parallel()

	const schema = `{"type":"object","properties":{"query":{"type":"string"},"domains":{"type":"array","items":{"type":"string","pattern":"^(?:[a-z]+\\.)+[a-z]+$"}}},"required":["query"],"additionalProperties":false}`
	for _, test := range []struct {
		name   string
		field  string
		strict bool
	}{
		{name: "omitted"},
		{name: "null", field: `,"strict":null`},
		{name: "false", field: `,"strict":false`},
		{name: "true", field: `,"strict":true`, strict: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			raw := `{"model":"claude","tools":[{"name":"search","input_schema":` + schema + test.field + `}],"messages":[{"role":"user","content":"hello"}]}`
			req, err := RequestFromAnthropicMessagesJSON([]byte(raw))
			if err != nil {
				t.Fatalf("RequestFromAnthropicMessagesJSON() error = %v", err)
			}
			encoded, err := json.Marshal(req.Tools)
			if err != nil {
				t.Fatal(err)
			}
			var wire []struct {
				Strict     *bool           `json:"strict"`
				Parameters json.RawMessage `json:"parameters"`
			}
			if err := json.Unmarshal(encoded, &wire); err != nil {
				t.Fatal(err)
			}
			if len(wire) != 1 || wire[0].Strict == nil || *wire[0].Strict != test.strict {
				t.Fatalf("wire tools = %s, want explicit strict=%t", encoded, test.strict)
			}
			assertJSONEqual(t, wire[0].Parameters, json.RawMessage(schema))
		})
	}
}
