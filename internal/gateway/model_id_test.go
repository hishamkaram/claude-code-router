package gateway

import (
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/modelcap"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

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
		{name: "legacy 1m", id: "claude-ccr-gpt[1m]", want: discoveryID{alias: "gpt", prefixed: true, valid: true}},
		{name: "canonical 1m", id: "anthropic.ccr.gpt[1m]", want: discoveryID{alias: "gpt", prefixed: true, valid: true}},
		{name: "escaped canonical 1m", id: "anthropic.ccr.s%6fnnet[1m]", want: discoveryID{alias: "sonnet", prefixed: true, valid: true}},
		{name: "not prefixed", id: "gpt", want: discoveryID{}},
		{name: "empty canonical", id: "anthropic.ccr.", want: discoveryID{prefixed: true}},
		{name: "empty legacy", id: "claude-ccr-", want: discoveryID{prefixed: true}},
		{name: "unescaped reserved family", id: "anthropic.ccr.sonnet", want: discoveryID{prefixed: true}},
		{name: "unknown percent escape", id: "anthropic.ccr.g%70t", want: discoveryID{prefixed: true}},
		{name: "noncanonical escape case", id: "anthropic.ccr.s%6Fnnet", want: discoveryID{prefixed: true}},
		{name: "noncanonical 1m case", id: "anthropic.ccr.gpt[1M]", want: discoveryID{prefixed: true}},
		{name: "unknown context suffix", id: "anthropic.ccr.gpt[2m]", want: discoveryID{prefixed: true}},
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

func TestDiscoveryIDForModelUsesEffectiveContextCapability(t *testing.T) {
	t.Parallel()
	discovered, err := modelcap.SnapshotFrom(modelcap.Values{ContextWindowTokens: modelcap.Int64(1_000_000)}, "fixture")
	if err != nil {
		t.Fatalf("SnapshotFrom() error = %v", err)
	}
	tests := []struct {
		name  string
		model store.Model
		want  string
	}{
		{
			name:  "provider model hint",
			model: store.Model{Alias: "glm", ProviderModel: "glm-5.2[1m]"},
			want:  "anthropic.ccr.glm[1m]",
		},
		{
			name:  "discovered and selectively escaped",
			model: store.Model{Alias: "sonnet-router", ProviderModel: "router", DiscoveredCapabilities: discovered},
			want:  "anthropic.ccr.s%6fnnet-router[1m]",
		},
		{
			name:  "override suppresses hint",
			model: store.Model{Alias: "glm", ProviderModel: "glm-5.2[1m]", CapabilityOverrides: modelcap.Values{ContextWindowTokens: modelcap.Int64(200_000)}},
			want:  "anthropic.ccr.glm",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := DiscoveryIDForModel(test.model)
			if err != nil {
				t.Fatalf("DiscoveryIDForModel() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("DiscoveryIDForModel() = %q, want %q", got, test.want)
			}
		})
	}
}
