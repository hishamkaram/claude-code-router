package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/hishamkaram/claude-code-router/internal/providers"
)

func (h *handler) handleModels(w http.ResponseWriter, r *http.Request) {
	entries := make([]gatewayModelEntry, 0)
	seen := map[string]struct{}{}
	anthropicEntries, err := h.discoverAnthropicModels(r.Context(), r.Header)
	if err != nil && !errors.Is(err, errNoAnthropicPassThroughProvider) && !errors.Is(err, errAnthropicModelDiscoveryUnsupported) {
		writeAnthropicError(w, http.StatusBadGateway, fmt.Sprintf("discovering Anthropic models: %v", err))
		return
	}
	if err == nil {
		for _, entry := range anthropicEntries {
			entry = h.discoveryEntryForAnthropicModel(entry)
			appendGatewayModelEntry(&entries, seen, entry)
		}
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
	if strings.HasPrefix(alias, "claude") || strings.HasPrefix(alias, "anthropic") {
		return alias
	}
	return discoveryAliasPrefix + alias
}

func (h *handler) discoveryEntryForAnthropicModel(entry gatewayModelEntry) gatewayModelEntry {
	if strings.TrimSpace(h.cfg.DefaultModelAlias) == "" {
		return entry
	}
	id := strings.TrimSpace(entry.ID)
	if !looksLikeAnthropicModelID(id) {
		return entry
	}
	entry.ID = nativeAnthropicDiscoveryPrefix + id
	if strings.TrimSpace(entry.DisplayName) != "" {
		entry.DisplayName += " (native)"
	}
	return entry
}

func (h *handler) discoverAnthropicModels(ctx context.Context, incoming http.Header) ([]gatewayModelEntry, error) {
	provider, err := h.defaultAnthropicProvider(ctx)
	if err != nil {
		return nil, err
	}
	if !effectiveProviderCapabilities(provider).SupportsModelDiscovery {
		return nil, errAnthropicModelDiscoveryUnsupported
	}
	endpoint, err := providers.ModelsEndpoint(provider.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("building Anthropic models endpoint: %w", err)
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("parsing Anthropic models endpoint: %w", err)
	}
	query := parsed.Query()
	query.Set("limit", "1000")
	parsed.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("creating Anthropic models request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	version := strings.TrimSpace(incoming.Get("anthropic-version"))
	if version == "" {
		version = "2023-06-01"
	}
	req.Header.Set("anthropic-version", version)
	if beta := strings.TrimSpace(incoming.Get("anthropic-beta")); beta != "" {
		req.Header.Set("anthropic-beta", beta)
	}
	apiKey, err := resolveProviderSecret(ctx, h.cfg.Secrets, provider.SecretRef)
	if err != nil {
		return nil, fmt.Errorf("resolving Anthropic provider secret: %w", err)
	}
	if apiKey != "" {
		req.Header.Set("x-api-key", apiKey)
	}
	resp, err := h.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("requesting Anthropic models: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("anthropic models discovery returned HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	var payload struct {
		Data []gatewayModelEntry `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decoding Anthropic models response: %w", err)
	}
	h.cacheAnthropicModels(payload.Data)
	return payload.Data, nil
}
