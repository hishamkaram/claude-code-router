package routeidentity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/hishamkaram/claude-code-router/internal/modelcap"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

const fingerprintVersion = 1

type providerIdentity struct {
	Name                   string `json:"name"`
	Type                   string `json:"type"`
	BaseURL                string `json:"base_url"`
	SecretRef              string `json:"secret_ref"`
	Protocol               string `json:"protocol"`
	Mode                   string `json:"mode"`
	SupportsTools          bool   `json:"supports_tools"`
	SupportsStreaming      bool   `json:"supports_streaming"`
	SupportsThinking       bool   `json:"supports_thinking"`
	SupportsModelDiscovery bool   `json:"supports_model_discovery"`
	SupportsCountTokens    bool   `json:"supports_count_tokens"`
	SupportsResponses      bool   `json:"supports_responses"`
}

type modelIdentity struct {
	Alias                 string          `json:"alias"`
	ProviderName          string          `json:"provider_name"`
	ProviderModel         string          `json:"provider_model"`
	Status                string          `json:"status"`
	EffectiveCapabilities modelcap.Values `json:"effective_capabilities"`
}

type identity struct {
	Version  int              `json:"version"`
	Provider providerIdentity `json:"provider"`
	Model    modelIdentity    `json:"model"`
}

// Fingerprint binds a routed session to the behavior-affecting provider and
// model configuration observed by the gateway. It includes only the secret
// reference identity; secret values are never accepted or persisted here.
func Fingerprint(model store.Model, provider store.Provider, effective modelcap.Values) (string, error) {
	effective = modelcap.CloneValues(effective)
	slices.Sort(effective.InputModalities)
	slices.Sort(effective.OutputModalities)
	payload, err := json.Marshal(identity{
		Version: fingerprintVersion,
		Provider: providerIdentity{
			Name: provider.Name, Type: provider.Type, BaseURL: provider.BaseURL,
			SecretRef: provider.SecretRef, Protocol: provider.Protocol, Mode: provider.Mode,
			SupportsTools: provider.SupportsTools, SupportsStreaming: provider.SupportsStreaming,
			SupportsThinking:       provider.SupportsThinking,
			SupportsModelDiscovery: provider.SupportsModelDiscovery,
			SupportsCountTokens:    provider.SupportsCountTokens,
			SupportsResponses:      provider.SupportsResponses,
		},
		Model: modelIdentity{
			Alias: model.Alias, ProviderName: model.ProviderName,
			ProviderModel: model.ProviderModel, Status: model.Status,
			EffectiveCapabilities: effective,
		},
	})
	if err != nil {
		return "", fmt.Errorf("routeidentity.Fingerprint: encoding route identity: %w", err)
	}
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}
