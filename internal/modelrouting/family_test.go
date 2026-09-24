package modelrouting

import "testing"

func TestDefaultModelEnvironment(t *testing.T) {
	got := DefaultModelEnvironment()
	want := map[string]string{
		DefaultOpusModelEnv:   "ccr-family-OPUS",
		DefaultSonnetModelEnv: "ccr-family-SONNET",
		DefaultHaikuModelEnv:  "ccr-family-HAIKU",
	}
	if len(got) != len(want) {
		t.Fatalf("environment length = %d, want %d", len(got), len(want))
	}
	for key, wantValue := range want {
		if got[key] != wantValue {
			t.Fatalf("environment[%q] = %q, want %q", key, got[key], wantValue)
		}
	}
}

func TestParseOverride(t *testing.T) {
	tests := []struct {
		value       string
		family      Family
		nativeModel string
		longContext bool
		want        bool
	}{
		{value: "ccr-family-OPUS", family: FamilyOpus, nativeModel: "opus", want: true},
		{value: "ccr-family-SONNET[1m]", family: FamilySonnet, nativeModel: "sonnet[1m]", longContext: true, want: true},
		{value: "ccr-family-HAIKU", family: FamilyHaiku, nativeModel: "haiku", want: true},
		{value: "sonnet", want: false},
		{value: "ccr-family-unknown", want: false},
		{value: "ccr-family-OPUS[2m]", want: false},
	}
	for _, test := range tests {
		t.Run(test.value, func(t *testing.T) {
			got, ok := ParseOverride(test.value)
			if ok != test.want {
				t.Fatalf("ParseOverride() ok = %v, want %v", ok, test.want)
			}
			if !test.want {
				return
			}
			if got.Family != test.family || got.LongContext != test.longContext {
				t.Fatalf("ParseOverride() = %#v, want family=%q longContext=%v", got, test.family, test.longContext)
			}
			if got.NativeModelID() != test.nativeModel {
				t.Fatalf("NativeModelID() = %q, want %q", got.NativeModelID(), test.nativeModel)
			}
		})
	}
}
