package cli

import (
	"slices"
	"testing"
)

func TestApplyClaudeEnvironmentSetsOverridesAndUnsetsInheritedValues(t *testing.T) {
	t.Parallel()

	base := []string{
		"HOME=/tmp/home",
		"ANTHROPIC_AUTH_TOKEN=stale-token",
		"CLAUDE_CODE_USE_GATEWAY=1",
		"ENABLE_TOOL_SEARCH=true",
	}
	overlay := ClaudeEnvironment{
		Set: []string{
			"ANTHROPIC_AUTH_TOKEN=fresh-token",
			"ENABLE_TOOL_SEARCH=",
		},
		Unset: []string{"CLAUDE_CODE_USE_GATEWAY"},
	}

	got := applyClaudeEnvironment(base, overlay)
	for _, want := range []string{"HOME=/tmp/home", "ANTHROPIC_AUTH_TOKEN=fresh-token", "ENABLE_TOOL_SEARCH="} {
		if !slices.Contains(got, want) {
			t.Fatalf("environment = %#v, missing %q", got, want)
		}
	}
	for _, unwanted := range []string{"ANTHROPIC_AUTH_TOKEN=stale-token", "CLAUDE_CODE_USE_GATEWAY=1", "ENABLE_TOOL_SEARCH=true"} {
		if slices.Contains(got, unwanted) {
			t.Fatalf("environment = %#v, contains %q", got, unwanted)
		}
	}
}
