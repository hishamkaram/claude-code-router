package responses

import (
	"strings"
	"testing"
)

func TestResponsesToolDefinitionChanges(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		raw         string
		wantNames   []string
		wantDetails map[string]string
	}{
		{
			name: "replacement keeps original position and avoids duplicates",
			raw: `{
				"model":"claude",
				"tools":[
					{"name":"bash","input_schema":{"type":"object"}},
					{"name":"read","description":"old","defer_loading":true,"input_schema":{"type":"object"}},
					{"name":"write","input_schema":{"type":"object"}}
				],
				"messages":[{"role":"system","content":[
					{"type":"tool_addition","tool":{"type":"tool_definition","definition":{
						"name":"read","description":"new","input_schema":{"type":"object"}
					}}}
				]}]
			}`,
			wantNames:   []string{"bash", "read", "write"},
			wantDetails: map[string]string{"read": "new"},
		},
		{
			name: "append puts a new definition at the end",
			raw: `{
				"model":"claude",
				"tools":[{"name":"bash","input_schema":{"type":"object"}}],
				"messages":[{"role":"system","content":[
					{"type":"tool_addition","tool":{"type":"tool_definition","definition":{
						"name":"read","description":"Read a file","input_schema":{"type":"object"}
					}}}
				]}]
			}`,
			wantNames:   []string{"bash", "read"},
			wantDetails: map[string]string{"read": "Read a file"},
		},
		{
			name: "removal filters a reference without reordering survivors",
			raw: `{
				"model":"claude",
				"tools":[
					{"name":"bash","input_schema":{"type":"object"}},
					{"name":"read","input_schema":{"type":"object"}},
					{"name":"write","input_schema":{"type":"object"}}
				],
				"messages":[{"role":"system","content":[
					{"type":"tool_removal","tool":{"type":"tool_reference","name":"read"}}
				]}]
			}`,
			wantNames: []string{"bash", "write"},
		},
		{
			name: "chronological changes preserve replacement and append order",
			raw: `{
				"model":"claude",
				"tools":[
					{"name":"first","input_schema":{"type":"object"}},
					{"name":"second","description":"old","input_schema":{"type":"object"}},
					{"name":"third","input_schema":{"type":"object"}}
				],
				"messages":[{"role":"system","content":[
					{"type":"tool_addition","tool":{"type":"tool_definition","definition":{
						"name":"second","description":"replaced","input_schema":{"type":"object"}
					}}},
					{"type":"tool_removal","tool":{"type":"tool_reference","name":"first"}},
					{"type":"tool_addition","tool":{"type":"tool_definition","definition":{
						"name":"fourth","description":"appended","input_schema":{"type":"object"}
					}}}
				]}]
			}`,
			wantNames:   []string{"second", "third", "fourth"},
			wantDetails: map[string]string{"second": "replaced", "fourth": "appended"},
		},
		{
			name: "captured cached definition shape",
			raw: `{
				"model":"claude",
				"tools":[{"name":"bash","input_schema":{"type":"object"}}],
				"messages":[{"role":"system","content":[
					{"type":"tool_addition","tool":{"type":"tool_definition","definition":{
						"name":"read","input_schema":{"type":"object"}
					}},"cache_control":{"type":"ephemeral"}}
				]}]
			}`,
			wantNames: []string{"bash", "read"},
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
			if gotNames := responseToolNames(got.Tools); !equalStrings(gotNames, test.wantNames) {
				t.Fatalf("tool names = %#v, want %#v", gotNames, test.wantNames)
			}
			for name, wantDescription := range test.wantDetails {
				for _, tool := range got.Tools {
					if tool.Name == name && tool.Description != wantDescription {
						t.Fatalf("tool %q description = %q, want %q", name, tool.Description, wantDescription)
					}
				}
			}
		})
	}
}

func TestResponsesToolDefinitionChangesRejectInvalidUnion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "reference and definition hybrid",
			raw:  `{"type":"tool_addition","tool":{"type":"tool_reference","name":"read","definition":{}}}`,
			want: `field "definition" is not supported`,
		},
		{
			name: "definition and reference name hybrid",
			raw:  `{"type":"tool_addition","tool":{"type":"tool_definition","name":"read","definition":{}}}`,
			want: `field "name" is not supported`,
		},
		{
			name: "definition on removal",
			raw:  `{"type":"tool_removal","tool":{"type":"tool_definition","definition":{"name":"read","input_schema":{"type":"object"}}}}`,
			want: "only supported for tool_addition",
		},
		{
			name: "malformed definition",
			raw:  `{"type":"tool_addition","tool":{"type":"tool_definition","definition":"read"}}`,
			want: "definition must be an object",
		},
		{
			name: "unknown nested field",
			raw:  `{"type":"tool_addition","tool":{"type":"tool_reference","name":"read","extra":true}}`,
			want: `field "extra" is not supported`,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			raw := `{"model":"claude","messages":[{"role":"system","content":[` + test.raw + `]}]}`
			_, err := RequestFromAnthropicMessagesJSON([]byte(raw))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("RequestFromAnthropicMessagesJSON() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func responseToolNames(tools []Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, responsesToolName(tool))
	}
	return names
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}
