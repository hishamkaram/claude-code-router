package cua

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestActionFromResponse(t *testing.T) {
	t.Parallel()

	action, err := ActionFromResponse("call_1", json.RawMessage(`{"type":"click","x":12,"y":34}`))
	if err != nil {
		t.Fatalf("ActionFromResponse() error = %v", err)
	}
	if action.CallID != "call_1" || action.Kind != ActionClick || action.X != 12 || action.Y != 34 {
		t.Fatalf("ActionFromResponse() = %#v", action)
	}
}

func TestActionFromResponsePreservesMouseModifiers(t *testing.T) {
	t.Parallel()

	action, err := ActionFromResponse("call_1", json.RawMessage(`{"type":"click","x":12,"y":34,"keys":["CTRL","SHIFT"]}`))
	if err != nil {
		t.Fatalf("ActionFromResponse() error = %v", err)
	}
	if got, want := strings.Join(action.Keys, ","), "CTRL,SHIFT"; got != want {
		t.Fatalf("ActionFromResponse() modifiers = %q, want %q", got, want)
	}
}

func TestActionFromResponsePreservesWhitespaceTypeText(t *testing.T) {
	t.Parallel()

	action, err := ActionFromResponse("call_1", json.RawMessage(`{"type":"type","text":" \n\t"}`))
	if err != nil {
		t.Fatalf("ActionFromResponse() error = %v", err)
	}
	if action.Kind != ActionType || action.Text != " \n\t" {
		t.Fatalf("ActionFromResponse() = %#v, want preserved whitespace text", action)
	}
}

func TestActionFromResponseRejectsUnsafeShape(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
	}{
		{name: "invalid JSON", raw: "{"},
		{name: "unsupported action", raw: `{"type":"shell"}`},
		{name: "missing click coordinate", raw: `{"type":"click","x":12}`},
		{name: "negative coordinate", raw: `{"type":"move","x":-1,"y":2}`},
		{name: "empty type text", raw: `{"type":"type","text":""}`},
		{name: "empty keys", raw: `{"type":"keypress","keys":[]}`},
		{name: "missing drag path", raw: `{"type":"drag"}`},
		{name: "invalid scroll", raw: `{"type":"scroll","x":1,"y":2,"scroll_x":0}`},
		{name: "oversized wait", raw: `{"type":"wait","milliseconds":30001}`},
		{name: "oversized type text", raw: `{"type":"type","text":"` + strings.Repeat("x", maxComputerActionTextBytes+1) + `"}`},
		{name: "unsupported mouse modifier", raw: `{"type":"click","x":1,"y":2,"keys":["A"]}`},
		{name: "duplicate mouse modifier", raw: `{"type":"click","x":1,"y":2,"keys":["CTRL","control"]}`},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := ActionFromResponse("call_1", json.RawMessage(test.raw)); err == nil {
				t.Fatalf("ActionFromResponse(%q) error = nil", test.name)
			}
		})
	}
}
