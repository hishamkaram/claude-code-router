package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestConfiguredAnthropicAliasForwardsIncomingAuthOnlyToFirstParty(t *testing.T) {
	ctx := context.Background()
	upstreamHeaders := make(map[string]http.Header)
	client := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		upstreamHeaders[req.URL.Host] = req.Header.Clone()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"id":"msg_test","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
			)),
			Request: req,
		}, nil
	})}
	s := newGatewayStore(t,
		store.Provider{Name: "first-party", Type: "anthropic", BaseURL: "https://api.anthropic.com"},
		store.Model{Alias: "subscription", ProviderName: "first-party", ProviderModel: "claude-sonnet-4-5", Status: "full"},
	)
	if err := s.AddProvider(ctx, store.Provider{
		Name: "custom-anthropic", Type: "anthropic-compatible", BaseURL: "https://compat.example.com",
	}); err != nil {
		t.Fatalf("AddProvider() error = %v", err)
	}
	if err := s.AddModel(ctx, store.Model{
		Alias: "custom", ProviderName: "custom-anthropic", ProviderModel: "custom-model", Status: "full",
	}); err != nil {
		t.Fatalf("AddModel() error = %v", err)
	}
	routeHandler := handler{cfg: Config{Store: s}, activeModel: newActiveModelSelection("")}
	route, validationErr := routeHandler.selectMessageRouteForRequest(ctx, "subscription-session", anthropicRequest{Model: "subscription"})
	if validationErr != nil || route.firstPartyAnthropic || !route.usesClaudeSubscriptionAuth() || route.agentChildSpawnAlias() != "subscription" {
		t.Fatalf("configured first-party alias route = %#v, error = %#v; want Claude auth semantics without losing alias identity", route, validationErr)
	}
	if got := routeHandler.activeModel.currentAlias("subscription-session"); got != "subscription" {
		t.Fatalf("active alias = %q, want subscription", got)
	}
	server := startGatewayWithConfig(t, ctx, Config{
		Store: s, Secrets: fakeGatewaySecrets{}, Token: "local-token", HTTPClient: client,
	})
	defer shutdownGateway(t, ctx, server)

	for _, alias := range []string{"subscription", "custom"} {
		body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hello"}]}`, alias)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(body))
		if err != nil {
			t.Fatalf("NewRequest() error = %v", err)
		}
		req.Header.Set("X-CCR-Session-Token", "local-token")
		req.Header.Set("Authorization", "Bearer claude-subscription-credential")
		req.Header.Set("X-Api-Key", "claude-api-key")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("gateway request for %q: %v", alias, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("gateway status for %q = %d, want %d", alias, resp.StatusCode, http.StatusOK)
		}
	}

	if got := upstreamHeaders["api.anthropic.com"].Get("Authorization"); got != "Bearer claude-subscription-credential" {
		t.Fatalf("first-party Authorization = %q, want forwarded Claude credential", got)
	}
	if got := upstreamHeaders["api.anthropic.com"].Get("X-Api-Key"); got != "claude-api-key" {
		t.Fatalf("first-party x-api-key = %q, want forwarded Claude API key", got)
	}
	if got := upstreamHeaders["api.anthropic.com"].Get("X-CCR-Session-Token"); got != "" {
		t.Fatalf("first-party upstream received CCR local token %q", got)
	}
	if got := upstreamHeaders["compat.example.com"].Get("Authorization"); got != "" {
		t.Fatalf("custom Anthropic-compatible upstream received Claude credential %q", got)
	}
	if got := upstreamHeaders["compat.example.com"].Get("X-Api-Key"); got != "" {
		t.Fatalf("custom Anthropic-compatible upstream received Claude API key %q", got)
	}
	if got := upstreamHeaders["compat.example.com"].Get("X-CCR-Session-Token"); got != "" {
		t.Fatalf("custom upstream received CCR local token %q", got)
	}
}
