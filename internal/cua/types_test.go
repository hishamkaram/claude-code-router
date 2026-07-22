package cua

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestConfigNormalize(t *testing.T) {
	t.Parallel()

	config, err := (Config{}).Normalize()
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if config.Mode != ModeClient || config.MaxTurns != DefaultMaxTurns ||
		config.MaxActions != DefaultMaxActions || config.Timeout != DefaultTimeout {
		t.Fatalf("Normalize() = %#v", config)
	}
	for _, config := range []Config{
		{Mode: ModeClient, Executor: "docker"},
		{Mode: ModeManaged, Executor: "unknown"},
		{Mode: ModeManaged, Timeout: time.Millisecond},
	} {
		if _, err := config.Normalize(); err == nil {
			t.Fatalf("Normalize(%#v) unexpectedly succeeded", config)
		}
	}
}

func TestUsesComputerTool(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		raw  json.RawMessage
		want bool
	}{
		{name: "typed native Anthropic tool", raw: json.RawMessage(`{"type":"computer_20250124","name":"computer"}`), want: true},
		{name: "legacy native Anthropic tool", raw: json.RawMessage(`{"name":"computer"}`), want: true},
		{name: "function named computer", raw: json.RawMessage(`{"name":"computer","input_schema":{"type":"object"}}`)},
		{name: "ordinary function", raw: json.RawMessage(`{"name":"read_file","input_schema":{"type":"object"}}`)},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if got := UsesComputerTool([]json.RawMessage{test.raw}); got != test.want {
				t.Fatalf("UsesComputerTool(%s) = %t, want %t", test.raw, got, test.want)
			}
		})
	}
}

func TestActionValidationAndRisk(t *testing.T) {
	t.Parallel()

	action := Action{CallID: "call_1", Kind: ActionScreenshot}
	if err := action.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if action.Risk() != RiskLow {
		t.Fatalf("Risk() = %q, want %q", action.Risk(), RiskLow)
	}
	invalid := Action{CallID: "call_1", Kind: "unknown"}
	if err := invalid.Validate(); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("invalid Validate() error = %v", err)
	}
}
