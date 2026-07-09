package gateway

import (
	"context"
	"net/http"
	"strings"

	"github.com/hishamkaram/claude-code-router/internal/providers"
	"github.com/hishamkaram/claude-code-router/internal/secret"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func (h *handler) httpClient() *http.Client {
	if h.cfg.HTTPClient != nil {
		return h.cfg.HTTPClient
	}
	return http.DefaultClient
}

func resolveProviderSecret(ctx context.Context, backend secret.Backend, ref string) (string, error) {
	if strings.TrimSpace(ref) == "" {
		return "", nil
	}
	value, err := backend.Resolve(ctx, ref)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(value), nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func effectiveProviderCapabilities(provider store.Provider) providers.Capabilities {
	return providers.NormalizeCapabilities(provider.Type, providers.Capabilities{
		Protocol:               provider.Protocol,
		SupportsTools:          provider.SupportsTools,
		SupportsStreaming:      provider.SupportsStreaming,
		SupportsThinking:       provider.SupportsThinking,
		SupportsModelDiscovery: provider.SupportsModelDiscovery,
		SupportsCountTokens:    provider.SupportsCountTokens,
		Mode:                   provider.Mode,
	})
}
