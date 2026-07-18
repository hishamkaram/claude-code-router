package gateway

import "testing"

func TestDiscoveryIDForAlias(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		alias string
		want  string
	}{
		{name: "ordinary alias", alias: "gpt", want: "anthropic.ccr.gpt"},
		{name: "sonnet", alias: "sonnet", want: "anthropic.ccr.s%6fnnet"},
		{name: "opus", alias: "opus", want: "anthropic.ccr.%6fpus"},
		{name: "haiku", alias: "haiku", want: "anthropic.ccr.h%61iku"},
		{name: "embedded family", alias: "my-sonnet", want: "anthropic.ccr.my-s%6fnnet"},
		{name: "multiple families", alias: "sonnet-opus-haiku", want: "anthropic.ccr.s%6fnnet-%6fpus-h%61iku"},
		{name: "repeated family", alias: "sonnet-sonnet", want: "anthropic.ccr.s%6fnnet-s%6fnnet"},
		{name: "similar literal", alias: "s6fnnet", want: "anthropic.ccr.s6fnnet"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := DiscoveryIDForAlias(tt.alias); got != tt.want {
				t.Fatalf("DiscoveryIDForAlias(%q) = %q, want %q", tt.alias, got, tt.want)
			}
			parsed := parseDiscoveryID(tt.want)
			if !parsed.prefixed || !parsed.valid || parsed.alias != tt.alias {
				t.Fatalf("parseDiscoveryID(%q) = %#v, want valid alias %q", tt.want, parsed, tt.alias)
			}
		})
	}
}

func TestParseDiscoveryIDCompatibilityAndValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		id   string
		want discoveryID
	}{
		{name: "legacy", id: "claude-ccr-gpt", want: discoveryID{alias: "gpt", prefixed: true, valid: true}},
		{name: "not prefixed", id: "gpt", want: discoveryID{}},
		{name: "empty canonical", id: "anthropic.ccr.", want: discoveryID{prefixed: true}},
		{name: "empty legacy", id: "claude-ccr-", want: discoveryID{prefixed: true}},
		{name: "unescaped reserved family", id: "anthropic.ccr.sonnet", want: discoveryID{prefixed: true}},
		{name: "unknown percent escape", id: "anthropic.ccr.g%70t", want: discoveryID{prefixed: true}},
		{name: "noncanonical escape case", id: "anthropic.ccr.s%6Fnnet", want: discoveryID{prefixed: true}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := parseDiscoveryID(tt.id); got != tt.want {
				t.Fatalf("parseDiscoveryID(%q) = %#v, want %#v", tt.id, got, tt.want)
			}
		})
	}
}
