package gateway

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestGatewayPinnedRouteUsesAdmittedProviderSecretAfterBackendMutation(t *testing.T) {
	ctx := context.Background()
	const (
		secretRef = "env:PROVIDER_KEY"
		admitted  = "provider-secret-a"
		mutated   = "provider-secret-b"
	)
	var gotAuthorization string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-pinned","choices":[{"message":{"content":"routed"},"finish_reason":"stop"}]}`)
	}))
	defer provider.Close()

	configuredProvider := store.Provider{
		Name: "fixture", Type: "openai-compatible", BaseURL: provider.URL, SecretRef: secretRef,
	}
	model := store.Model{Alias: "coder", ProviderName: configuredProvider.Name, ProviderModel: "model-v1", Status: "degraded"}
	s := newGatewayStore(t, configuredProvider, model)
	secrets := fakeGatewaySecrets{secretRef: admitted}
	pinned, err := NewPinnedRouteWithSecret(model, configuredProvider, admitted)
	if err != nil {
		t.Fatal(err)
	}
	secrets[secretRef] = mutated

	server := startGatewayWithConfig(t, ctx, Config{
		Store:             s,
		Secrets:           secrets,
		Token:             "local-token",
		DefaultModelAlias: model.Alias,
		PinnedRoute:       &pinned,
	})
	defer func() { _ = server.Shutdown(ctx) }()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(`{"model":"coder","max_tokens":20,"messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-CCR-Session-Token", "local-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway status = %d, want 200", resp.StatusCode)
	}
	if gotAuthorization != "Bearer "+admitted {
		t.Fatalf("provider authorization = %q, want admitted credential", gotAuthorization)
	}
}
