package anthropictoolchange

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		raw        string
		wantBlock  string
		wantKind   Kind
		wantName   string
		wantDefine string
		wantError  string
	}{
		{
			name:      "reference addition with cache control",
			raw:       `{"type":"tool_addition","tool":{"type":"tool_reference","name":" read "},"cache_control":{"type":"ephemeral"}}`,
			wantBlock: "tool_addition",
			wantKind:  Reference,
			wantName:  "read",
		},
		{
			name:       "definition addition",
			raw:        `{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"read","input_schema":{"type":"object"}}}}`,
			wantBlock:  "tool_addition",
			wantKind:   Definition,
			wantDefine: `{"name":"read","input_schema":{"type":"object"}}`,
		},
		{
			name:      "reference removal",
			raw:       `{"type":"tool_removal","tool":{"type":"tool_reference","name":"read"}}`,
			wantBlock: "tool_removal",
			wantKind:  Reference,
			wantName:  "read",
		},
		{
			name:      "hybrid reference and definition",
			raw:       `{"type":"tool_addition","tool":{"type":"tool_reference","name":"read","definition":{}}}`,
			wantError: `field "definition" is not supported`,
		},
		{
			name:      "hybrid definition and name",
			raw:       `{"type":"tool_addition","tool":{"type":"tool_definition","name":"read","definition":{}}}`,
			wantError: `field "name" is not supported`,
		},
		{
			name:      "definition on removal",
			raw:       `{"type":"tool_removal","tool":{"type":"tool_definition","definition":{"name":"read"}}}`,
			wantError: `only supported for tool_addition`,
		},
		{
			name:      "unknown outer field",
			raw:       `{"type":"tool_addition","tool":{"type":"tool_reference","name":"read"},"extra":true}`,
			wantError: `field "extra" is not supported`,
		},
		{
			name:      "unknown nested field",
			raw:       `{"type":"tool_addition","tool":{"type":"tool_reference","name":"read","extra":true}}`,
			wantError: `field "extra" is not supported`,
		},
		{
			name:      "missing reference name",
			raw:       `{"type":"tool_addition","tool":{"type":"tool_reference"}}`,
			wantError: `name is required`,
		},
		{
			name:      "malformed definition",
			raw:       `{"type":"tool_addition","tool":{"type":"tool_definition","definition":"read"}}`,
			wantError: `definition must be an object`,
		},
		{
			name:      "unknown union type",
			raw:       `{"type":"tool_addition","tool":{"type":"other","name":"read"}}`,
			wantError: `tool reference type "other" is not supported`,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := Parse(json.RawMessage(test.raw))
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("Parse() error = %v, want substring %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if got.BlockType != test.wantBlock || got.Kind != test.wantKind || got.Name != test.wantName {
				t.Fatalf("Parse() = %#v, want block=%q kind=%q name=%q", got, test.wantBlock, test.wantKind, test.wantName)
			}
			if test.wantDefine != "" {
				var gotDefinition, wantDefinition map[string]json.RawMessage
				if err := json.Unmarshal(got.Definition, &gotDefinition); err != nil {
					t.Fatalf("definition is invalid JSON: %v", err)
				}
				if err := json.Unmarshal([]byte(test.wantDefine), &wantDefinition); err != nil {
					t.Fatalf("want definition is invalid JSON: %v", err)
				}
				if !reflect.DeepEqual(gotDefinition, wantDefinition) {
					t.Fatalf("Parse() definition = %s, want %s", got.Definition, test.wantDefine)
				}
			}
		})
	}
}
