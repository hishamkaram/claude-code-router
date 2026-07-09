package gateway

import (
	"fmt"
	"net/http"
	"strings"
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
	for _, model := range models {
		if model.Status == "blocked" {
			continue
		}
		display := fmt.Sprintf("CCR %s (%s)", model.Alias, model.ProviderModel)
		appendGatewayModelEntry(&entries, seen, gatewayModelEntry{ID: discoveryIDForAlias(model.Alias), DisplayName: display})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": entries})
}

type gatewayModelEntry struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name,omitempty"`
}

func appendGatewayModelEntry(entries *[]gatewayModelEntry, seen map[string]struct{}, entry gatewayModelEntry) {
	entry.ID = strings.TrimSpace(entry.ID)
	if entry.ID == "" {
		return
	}
	if _, ok := seen[entry.ID]; ok {
		return
	}
	seen[entry.ID] = struct{}{}
	*entries = append(*entries, entry)
}

func discoveryIDForAlias(alias string) string {
	alias = strings.TrimSpace(alias)
	return discoveryAliasPrefix + alias
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
	}
}
