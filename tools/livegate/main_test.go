package main

import (
	"strings"
	"testing"
)

func TestLiveGateRejectsMissingSkippedFailedAndMalformedEvidence(t *testing.T) {
	t.Parallel()
	contract := liveContract{required: []string{"fixture/TestRequired"}, requiredPasses: 1, optional: map[string]string{"fixture/TestOptional": "requires real credentials"}}
	pass := "{\"Action\":\"pass\",\"Package\":\"fixture\",\"Test\":\"TestRequired\"}\n"
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "pass", input: pass},
		{name: "optional skip", input: pass + "{\"Action\":\"skip\",\"Package\":\"fixture\",\"Test\":\"TestOptional\"}"},
		{name: "no test files", input: pass + "{\"Action\":\"skip\",\"Package\":\"empty\"}"},
		{name: "empty log", want: "passing evidence count"},
		{name: "duplicate required result", input: pass + pass, want: "passing evidence count"},
		{name: "unknown skip", input: pass + "{\"Action\":\"skip\",\"Package\":\"fixture\",\"Test\":\"TestNewFixture\"}", want: "unexpected skip"},
		{name: "required skipped", input: "{\"Action\":\"skip\",\"Package\":\"fixture\",\"Test\":\"TestRequired\"}", want: "unexpected skip"},
		{name: "failure after pass", input: pass + "{\"Action\":\"fail\",\"Package\":\"fixture\"}", want: "failed:"},
		{name: "malformed", input: pass + "{", want: "decoding"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := checkEvents(strings.NewReader(test.input), contract)
			if test.want == "" {
				if err != nil {
					t.Fatalf("valid evidence rejected: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("checkEvents() = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLiveGateRequiresExactlyTenPassingPickerRuns(t *testing.T) {
	t.Parallel()
	contract := pickerLiveContract()
	pass := "{\"Action\":\"pass\",\"Package\":\"github.com/hishamkaram/claude-code-router/internal/cli\",\"Test\":\"TestLiveLaunchModelPickerShowsAnthropicAndRegisteredModels\"}\n"
	valid := strings.Repeat(pass, 10)
	for _, test := range []struct {
		name  string
		input string
		want  string
	}{
		{name: "ten passes", input: valid},
		{name: "no runs", want: "passing evidence count"},
		{name: "nine passes", input: strings.Repeat(pass, 9), want: "got 9, want 10"},
		{name: "eleven passes", input: valid + pass, want: "got 11, want 10"},
		{name: "skip after passes", input: valid + strings.Replace(pass, "\"pass\"", "\"skip\"", 1), want: "unexpected skip"},
		{name: "failure after passes", input: valid + strings.Replace(pass, "\"pass\"", "\"fail\"", 1), want: "failed:"},
		{name: "malformed after passes", input: valid + "{", want: "decoding"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := checkEvents(strings.NewReader(test.input), contract)
			if test.want == "" {
				if err != nil {
					t.Fatalf("valid picker evidence rejected: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("checkEvents() = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLiveGateRequiresBothLogPaths(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{nil, {"only-full.jsonl"}, {"full.jsonl", "picker.jsonl", "extra"}} {
		if err := run(args); err == nil || !strings.Contains(err.Error(), "usage:") {
			t.Fatalf("run(%q) = %v, want strict two-log usage", args, err)
		}
	}
}
