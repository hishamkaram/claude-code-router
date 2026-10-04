package gateway

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestOpenAIToolsPreserveAnthropicStrictnessOnWire(t *testing.T) {
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
			raw := json.RawMessage(`{"name":"search","input_schema":` + schema + test.field + `}`)
			tools, err := openAIToolsFromAnthropic([]json.RawMessage{raw})
			if err != nil {
				t.Fatalf("openAIToolsFromAnthropic() error = %v", err)
			}
			encoded, err := json.Marshal(tools)
			if err != nil {
				t.Fatal(err)
			}
			var wire []struct {
				Function struct {
					Strict     *bool `json:"strict"`
					Parameters any   `json:"parameters"`
				} `json:"function"`
			}
			if err := json.Unmarshal(encoded, &wire); err != nil {
				t.Fatal(err)
			}
			if len(wire) != 1 || wire[0].Function.Strict == nil || *wire[0].Function.Strict != test.strict {
				t.Fatalf("wire tools = %s, want explicit strict=%t", encoded, test.strict)
			}
			var wantSchema any
			if err := json.Unmarshal([]byte(schema), &wantSchema); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(wire[0].Function.Parameters, wantSchema) {
				t.Fatalf("input schema changed: %s", encoded)
			}
		})
	}
}
