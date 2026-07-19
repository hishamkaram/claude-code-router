package gateway

import (
	"fmt"
	"strings"

	"github.com/hishamkaram/claude-code-router/internal/modelcap"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

const (
	discoveryAliasPrefix       = "anthropic.ccr."
	legacyDiscoveryAliasPrefix = "claude-ccr-"
)

// DiscoveryAliasPrefix returns the Claude-compatible prefix used for CCR model aliases.
func DiscoveryAliasPrefix() string {
	return discoveryAliasPrefix
}

// DiscoveryIDForAlias returns the canonical Claude model ID for a CCR alias.
func DiscoveryIDForAlias(alias string) string {
	alias = strings.TrimSpace(alias)
	return discoveryAliasPrefix + escapeDiscoveryAlias(alias)
}

// DiscoveryIDForModel returns the Claude-facing ID for a stored CCR model.
func DiscoveryIDForModel(model store.Model) (string, error) {
	effective, err := modelcap.Effective(model.DiscoveredCapabilities, model.CapabilityOverrides, model.ProviderModel)
	if err != nil {
		return "", fmt.Errorf("gateway.DiscoveryIDForModel: computing capabilities for alias %q: %w", model.Alias, err)
	}
	id := DiscoveryIDForAlias(model.Alias)
	if modelcap.SupportsOneMillion(effective.Values) {
		id += "[1m]"
	}
	return id, nil
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
		oneMillion := strings.HasSuffix(encodedAlias, "[1m]")
		if oneMillion {
			encodedAlias = strings.TrimSuffix(encodedAlias, "[1m]")
		}
		if encodedAlias == "" || strings.ContainsAny(encodedAlias, "[]") {
			return discoveryID{prefixed: true}
		}
		alias := unescapeDiscoveryAlias(encodedAlias)
		canonical := DiscoveryIDForAlias(alias)
		if oneMillion {
			canonical += "[1m]"
		}
		if strings.Contains(alias, "%") || canonical != id {
			return discoveryID{prefixed: true}
		}
		return discoveryID{alias: alias, prefixed: true, valid: true}
	}
	if strings.HasPrefix(id, legacyDiscoveryAliasPrefix) {
		alias := strings.TrimPrefix(id, legacyDiscoveryAliasPrefix)
		alias = strings.TrimSuffix(alias, "[1m]")
		if alias == "" || strings.ContainsAny(alias, "[]") {
			return discoveryID{prefixed: true}
		}
		return discoveryID{alias: alias, prefixed: true, valid: true}
	}
	return discoveryID{}
}

func escapeDiscoveryAlias(alias string) string {
	alias = strings.ReplaceAll(alias, "sonnet", "s%6fnnet")
	alias = strings.ReplaceAll(alias, "opus", "%6fpus")
	return strings.ReplaceAll(alias, "haiku", "h%61iku")
}

func unescapeDiscoveryAlias(alias string) string {
	alias = strings.ReplaceAll(alias, "s%6fnnet", "sonnet")
	alias = strings.ReplaceAll(alias, "%6fpus", "opus")
	return strings.ReplaceAll(alias, "h%61iku", "haiku")
}
