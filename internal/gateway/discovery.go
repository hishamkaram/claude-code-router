package gateway

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/hishamkaram/claude-code-router/internal/modelcap"
	"github.com/hishamkaram/claude-code-router/internal/providers"
)

func (h *handler) handleModels(w http.ResponseWriter, r *http.Request) {
	entries := make([]gatewayModelEntry, 0)
	seen := map[string]struct{}{}
	for _, entry := range firstPartyAnthropicModelEntries() {
		appendGatewayModelEntry(&entries, seen, entry)
	}
	models, err := h.cfg.Store.ListModels(r.Context())
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, fmt.Sprintf("listing configured model aliases: %v", err))
		return
	}
	configuredProviders, err := h.cfg.Store.ListProviders(r.Context())
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, fmt.Sprintf("listing configured providers: %v", err))
		return
	}
	providerTypes := make(map[string]string, len(configuredProviders))
	providerCapabilities := make(map[string]providers.Capabilities, len(configuredProviders))
	for index := range configuredProviders {
		provider := &configuredProviders[index]
		providerTypes[provider.Name] = provider.Type
		providerCapabilities[provider.Name] = effectiveProviderCapabilities(*provider)
	}
	for index := range models {
		model := &models[index]
		if model.Status == "blocked" {
			continue
		}
		if providers.IsProviderControlModel(providerTypes[model.ProviderName], model.ProviderModel) {
			continue
		}
		effective, err := modelcap.Effective(model.DiscoveredCapabilities, model.CapabilityOverrides, model.ProviderModel)
		if err != nil {
			writeAnthropicError(w, http.StatusInternalServerError, fmt.Sprintf("computing capabilities for model alias %q: %v", model.Alias, err))
			return
		}
		if !modelcap.IsRoutableKind(effective.Values.Kind) {
			continue
		}
		advertisedCapabilities := modelCapabilitiesForRoute(providerCapabilities[model.ProviderName], effective.Values)
		id, err := DiscoveryIDForModel(*model)
		if err != nil {
			writeAnthropicError(w, http.StatusInternalServerError, err.Error())
			return
		}
		entry := gatewayModelEntry{
			ID: id, DisplayName: fmt.Sprintf("CCR %s (%s)", model.Alias, model.ProviderModel), Type: "model",
			MaxInputTokens: effective.Values.ContextWindowTokens, MaxTokens: effective.Values.MaxOutputTokens,
			Capabilities: gatewayDiscoveryCapabilities(advertisedCapabilities),
		}
		if entry.MaxInputTokens == nil {
			entry.MaxInputTokens = effective.Values.MaxInputTokens
		}
		appendGatewayModelEntry(&entries, seen, entry)
	}
	firstID, lastID := "", ""
	if len(entries) > 0 {
		firstID, lastID = entries[0].ID, entries[len(entries)-1].ID
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data": entries, "first_id": firstID, "last_id": lastID, "has_more": false,
	})
}

type gatewayModelEntry struct {
	ID             string                    `json:"id"`
	DisplayName    string                    `json:"display_name,omitempty"`
	Type           string                    `json:"type,omitempty"`
	MaxInputTokens *int64                    `json:"max_input_tokens,omitempty"`
	MaxTokens      *int64                    `json:"max_tokens,omitempty"`
	Capabilities   *gatewayModelCapabilities `json:"capabilities,omitempty"`
}

type gatewayModelCapabilities struct {
	ImageInput        *gatewayCapabilitySupport  `json:"image_input,omitempty"`
	PDFInput          *gatewayCapabilitySupport  `json:"pdf_input,omitempty"`
	StructuredOutputs *gatewayCapabilitySupport  `json:"structured_outputs,omitempty"`
	Thinking          *gatewayThinkingCapability `json:"thinking,omitempty"`
}

type gatewayCapabilitySupport struct {
	Supported bool `json:"supported"`
}

type gatewayThinkingCapability struct {
	Supported bool                  `json:"supported"`
	Types     *gatewayThinkingTypes `json:"types,omitempty"`
}

type gatewayThinkingTypes struct {
	Enabled *gatewayCapabilitySupport `json:"enabled,omitempty"`
}

func gatewayDiscoveryCapabilities(values modelcap.Values) *gatewayModelCapabilities {
	capabilities := &gatewayModelCapabilities{
		ImageInput:        gatewayCapability(values.SupportsVision),
		PDFInput:          gatewayCapability(values.SupportsPDFInput),
		StructuredOutputs: gatewayCapability(values.SupportsResponseSchema),
		Thinking:          gatewayThinking(values.SupportsThinking),
	}
	if capabilities.ImageInput == nil && capabilities.PDFInput == nil &&
		capabilities.StructuredOutputs == nil && capabilities.Thinking == nil {
		return nil
	}
	return capabilities
}

func gatewayThinking(value *bool) *gatewayThinkingCapability {
	if value == nil {
		return nil
	}
	capability := &gatewayThinkingCapability{Supported: *value}
	if *value {
		capability.Types = &gatewayThinkingTypes{Enabled: &gatewayCapabilitySupport{Supported: true}}
	}
	return capability
}

func gatewayCapability(value *bool) *gatewayCapabilitySupport {
	if value == nil {
		return nil
	}
	return &gatewayCapabilitySupport{Supported: *value}
}

func appendGatewayModelEntry(entries *[]gatewayModelEntry, seen map[string]struct{}, entry gatewayModelEntry) {
	entry.ID = strings.TrimSpace(entry.ID)
	if entry.ID == "" {
		return
	}
	if _, ok := seen[entry.ID]; ok {
		return
	}
	if entry.Type == "" {
		entry.Type = "model"
	}
	seen[entry.ID] = struct{}{}
	*entries = append(*entries, entry)
}

func FirstPartyAnthropicModelIDs() []string {
	entries := firstPartyAnthropicModelEntries()
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, entry.ID)
	}
	return ids
}

func firstPartyAnthropicModelEntries() []gatewayModelEntry {
	return []gatewayModelEntry{
		{ID: "default", DisplayName: "Claude default"},
		{ID: "best", DisplayName: "Claude best"},
		{ID: "fable", DisplayName: "Claude fable"},
		{ID: "sonnet", DisplayName: "Claude Sonnet"},
		{ID: "opus", DisplayName: "Claude Opus"},
		{ID: "haiku", DisplayName: "Claude Haiku"},
		{ID: "sonnet[1m]", DisplayName: "Claude Sonnet 1M"},
		{ID: "opus[1m]", DisplayName: "Claude Opus 1M"},
		{ID: "opusplan", DisplayName: "Claude Opus planning"},
		{ID: "opusplan[1m]", DisplayName: "Claude Opus planning 1M"},
	}
}
