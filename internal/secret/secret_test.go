package secret

import "testing"

func TestRedactRef(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ref  string
		want string
	}{
		{name: "empty", ref: "", want: ""},
		{name: "env", ref: "env:OPENROUTER_API_KEY", want: "env:OPENROUTER_API_KEY"},
		{name: "keyring", ref: "keyring:provider/openrouter/api-key", want: "keyring:***"},
		{name: "opaque", ref: "plain-secret", want: "***"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := RedactRef(tt.ref); got != tt.want {
				t.Fatalf("RedactRef(%q) = %q, want %q", tt.ref, got, tt.want)
			}
		})
	}
}
