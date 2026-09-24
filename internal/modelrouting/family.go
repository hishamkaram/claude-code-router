// Package modelrouting defines the private model identifiers shared by the
// Claude Code launcher and the gateway for model-family routing.
package modelrouting

import "strings"

// Family identifies one of Claude Code's model families.
type Family string

const (
	FamilyOpus   Family = "opus"
	FamilySonnet Family = "sonnet"
	FamilyHaiku  Family = "haiku"
)

const (
	// DefaultOpusModelEnv is Claude Code's configurable opus-family default.
	DefaultOpusModelEnv = "ANTHROPIC_DEFAULT_OPUS_MODEL"
	// DefaultSonnetModelEnv is Claude Code's configurable sonnet-family default.
	DefaultSonnetModelEnv = "ANTHROPIC_DEFAULT_SONNET_MODEL"
	// DefaultHaikuModelEnv is Claude Code's configurable haiku-family default.
	DefaultHaikuModelEnv = "ANTHROPIC_DEFAULT_HAIKU_MODEL"

	// The uppercase family suffix deliberately keeps these launch-private
	// controls outside the persisted model-alias grammar. Lowercase spellings
	// were valid aliases before family routing existed and must remain usable.
	opusOverrideModel   = "ccr-family-OPUS"
	sonnetOverrideModel = "ccr-family-SONNET"
	haikuOverrideModel  = "ccr-family-HAIKU"
)

// Model describes a family override received from Claude Code.
type Model struct {
	Family      Family
	LongContext bool
}

// DefaultModelEnvironment returns a new environment map for a launch-scoped
// Claude Code settings overlay. The caller may safely modify the returned map.
func DefaultModelEnvironment() map[string]string {
	return map[string]string{
		DefaultOpusModelEnv:   opusOverrideModel,
		DefaultSonnetModelEnv: sonnetOverrideModel,
		DefaultHaikuModelEnv:  haikuOverrideModel,
	}
}

// ParseOverride recognizes only CCR-owned family identifiers. An optional
// [1m] suffix is preserved so native fallback retains Claude Code's requested
// context-window variant.
func ParseOverride(value string) (Model, bool) {
	value = strings.TrimSpace(value)
	longContext := strings.HasSuffix(value, "[1m]")
	if longContext {
		value = strings.TrimSuffix(value, "[1m]")
	}
	var family Family
	switch value {
	case opusOverrideModel:
		family = FamilyOpus
	case sonnetOverrideModel:
		family = FamilySonnet
	case haikuOverrideModel:
		family = FamilyHaiku
	default:
		return Model{}, false
	}
	return Model{Family: family, LongContext: longContext}, true
}

// IsFamilyOverride reports whether value is reserved for CCR's family-routing
// protocol rather than a user-configured model alias.
func IsFamilyOverride(value string) bool {
	_, ok := ParseOverride(value)
	return ok
}

// NativeModelID returns the first-party family identifier to use when no CCR
// alias is active for the session.
func (m Model) NativeModelID() string {
	value := string(m.Family)
	if m.LongContext {
		value += "[1m]"
	}
	return value
}
