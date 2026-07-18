package gateway

import "strings"

const (
	discoveryAliasPrefix       = "anthropic.ccr."
	legacyDiscoveryAliasPrefix = "claude-ccr-"
)

var (
	discoveryAliasEscaper = strings.NewReplacer(
		"sonnet", "s%6fnnet",
		"opus", "%6fpus",
		"haiku", "h%61iku",
	)
	discoveryAliasUnescaper = strings.NewReplacer(
		"s%6fnnet", "sonnet",
		"%6fpus", "opus",
		"h%61iku", "haiku",
	)
)

// DiscoveryAliasPrefix returns the Claude-compatible prefix used for CCR model aliases.
func DiscoveryAliasPrefix() string {
	return discoveryAliasPrefix
}

// DiscoveryIDForAlias returns the canonical Claude model ID for a CCR alias.
func DiscoveryIDForAlias(alias string) string {
	alias = strings.TrimSpace(alias)
	return discoveryAliasPrefix + discoveryAliasEscaper.Replace(alias)
}

type discoveryID struct {
	alias    string
	prefixed bool
	valid    bool
}

func parseDiscoveryID(id string) discoveryID {
	id = strings.TrimSpace(id)
	if strings.HasPrefix(id, discoveryAliasPrefix) {
		encodedAlias := strings.TrimPrefix(id, discoveryAliasPrefix)
		if encodedAlias == "" {
			return discoveryID{prefixed: true}
		}
		alias := discoveryAliasUnescaper.Replace(encodedAlias)
		if strings.Contains(alias, "%") || DiscoveryIDForAlias(alias) != id {
			return discoveryID{prefixed: true}
		}
		return discoveryID{alias: alias, prefixed: true, valid: true}
	}
	if strings.HasPrefix(id, legacyDiscoveryAliasPrefix) {
		alias := strings.TrimPrefix(id, legacyDiscoveryAliasPrefix)
		if alias == "" {
			return discoveryID{prefixed: true}
		}
		return discoveryID{alias: alias, prefixed: true, valid: true}
	}
	return discoveryID{}
}
