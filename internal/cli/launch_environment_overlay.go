package cli

import (
	"fmt"
	"io"
	"runtime"
	"slices"
	"strings"
)

// Entries are owned by the launch builder. Projections are derived once and
// never mutate the overlay or copy arbitrary caller credentials into settings.
type launchEnvironment []launchEnvironmentEntry

type launchEnvironmentEntry struct {
	name   string
	value  string
	unset  bool
	pinned bool
}

func (env *launchEnvironment) set(name, value string, pinned bool) {
	*env = append(*env, launchEnvironmentEntry{name: name, value: value, pinned: pinned})
}

func (env *launchEnvironment) unset(name string, pinned bool) {
	*env = append(*env, launchEnvironmentEntry{name: name, unset: true, pinned: pinned})
}

func (env launchEnvironment) processEnvironment() ClaudeEnvironment {
	result := ClaudeEnvironment{}
	for _, entry := range env {
		if entry.unset {
			result.Unset = append(result.Unset, entry.name)
		} else {
			result.Set = append(result.Set, entry.name+"="+entry.value)
		}
	}
	return result
}

func (env launchEnvironment) settingsEnvironment() map[string]string {
	result := make(map[string]string)
	for _, entry := range env {
		if entry.pinned {
			// Native provider selection treats an empty setting as unset.
			result[entry.name] = entry.value
		}
	}
	return result
}

func cloudProviderSelectors() []string {
	return []string{
		"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX",
		"CLAUDE_CODE_USE_FOUNDRY", "CLAUDE_CODE_USE_MANTLE",
		"CLAUDE_CODE_USE_ANTHROPIC_AWS", "CLAUDE_CODE_USE_GATEWAY",
	}
}

func writeLaunchEnvironmentNotice(out io.Writer, env launchEnvironment, base []string) {
	caseInsensitive := runtime.GOOS == "windows"
	controlled := make(map[string]struct{}, len(env))
	for _, entry := range env {
		controlled[environmentName(entry.name, caseInsensitive)] = struct{}{}
	}
	names := make([]string, 0, len(controlled))
	for _, entry := range base {
		name, value, ok := strings.Cut(entry, "=")
		if _, owned := controlled[environmentName(name, caseInsensitive)]; ok && owned && value != "" {
			names = append(names, name)
		}
	}
	if len(names) != 0 {
		slices.Sort(names)
		_, _ = fmt.Fprintf(out, "CCR controls inherited launch variables: %s (values hidden)\n", strings.Join(slices.Compact(names), ", "))
	}
}
