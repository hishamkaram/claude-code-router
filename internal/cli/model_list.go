package cli

import (
	"fmt"
	"io"
	"sort"

	"github.com/hishamkaram/claude-code-router/internal/modelcap"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

type modelListEntry struct {
	Alias         string            `json:"alias"`
	Provider      string            `json:"provider"`
	ProviderModel string            `json:"provider_model"`
	Compatibility string            `json:"compatibility"`
	Effective     modelcap.Snapshot `json:"effective_capabilities"`
}

type modelListDocument struct {
	SchemaVersion int              `json:"schema_version"`
	Models        []modelListEntry `json:"models"`
}

func writeModelListJSON(out io.Writer, models []store.Model) error {
	document := modelListDocument{SchemaVersion: 1, Models: make([]modelListEntry, 0, len(models))}
	for i := range models {
		model := &models[i]
		effective, err := modelcap.Effective(model.DiscoveredCapabilities, model.CapabilityOverrides, model.ProviderModel)
		if err != nil {
			return fmt.Errorf("computing effective capabilities for model %q: %w", model.Alias, err)
		}
		document.Models = append(document.Models, modelListEntry{
			Alias: model.Alias, Provider: model.ProviderName, ProviderModel: model.ProviderModel,
			Compatibility: model.Status, Effective: effective,
		})
	}
	sort.Slice(document.Models, func(i, j int) bool { return document.Models[i].Alias < document.Models[j].Alias })
	return writeVersionedJSON(out, document)
}
