package store

import (
	"encoding/json"
	"fmt"

	"github.com/hishamkaram/claude-code-router/internal/modelcap"
)

func encodeModelCapabilities(model Model) (discoveredEncoded, overridesEncoded string, encodeErr error) {
	discovered, err := modelcap.NormalizeSnapshot(model.DiscoveredCapabilities)
	if err != nil {
		return "", "", fmt.Errorf("normalizing discovered capabilities: %w", err)
	}
	overrides, err := modelcap.NormalizeValues(model.CapabilityOverrides)
	if err != nil {
		return "", "", fmt.Errorf("normalizing capability overrides: %w", err)
	}
	discoveredJSON, err := json.Marshal(discovered)
	if err != nil {
		return "", "", fmt.Errorf("marshaling discovered capabilities: %w", err)
	}
	overridesJSON, err := json.Marshal(overrides)
	if err != nil {
		return "", "", fmt.Errorf("marshaling capability overrides: %w", err)
	}
	return string(discoveredJSON), string(overridesJSON), nil
}

func decodeModelCapabilities(model *Model, discoveredJSON, overridesJSON string) error {
	if discoveredJSON != "" {
		if err := json.Unmarshal([]byte(discoveredJSON), &model.DiscoveredCapabilities); err != nil {
			return fmt.Errorf("unmarshaling discovered capabilities: %w", err)
		}
	}
	if overridesJSON != "" {
		if err := json.Unmarshal([]byte(overridesJSON), &model.CapabilityOverrides); err != nil {
			return fmt.Errorf("unmarshaling capability overrides: %w", err)
		}
	}
	var err error
	model.DiscoveredCapabilities, err = modelcap.NormalizeSnapshot(model.DiscoveredCapabilities)
	if err != nil {
		return fmt.Errorf("normalizing discovered capabilities: %w", err)
	}
	model.CapabilityOverrides, err = modelcap.NormalizeValues(model.CapabilityOverrides)
	if err != nil {
		return fmt.Errorf("normalizing capability overrides: %w", err)
	}
	return nil
}
