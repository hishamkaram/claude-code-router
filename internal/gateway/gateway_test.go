package gateway

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestGatewayRejectsMissingLocalToken(t *testing.T) {
	ctx := context.Background()
	s := newGatewayStore(t, store.Provider{Name: "litellm", Type: "litellm", BaseURL: "http://127.0.0.1:1", SecretRef: ""}, store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"})
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	resp, err := http.Post(server.URL()+"/v1/messages", "application/json", strings.NewReader(`{"model":"gpt","messages":[]}`))
	if err != nil {
		t.Fatalf("gateway request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("gateway status = %d, want 401", resp.StatusCode)
	}
}

func TestGatewayAcceptsCCRSessionTokenHeaderAndRejectsWrongToken(t *testing.T) {
	ctx := context.Background()
	s := newGatewayStore(t, store.Provider{Name: "litellm", Type: "litellm", BaseURL: "http://127.0.0.1:1", SecretRef: ""}, store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"})
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	okReq, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL()+"/v1/models", http.NoBody)
	if err != nil {
		t.Fatalf("NewRequest(ok) error = %v", err)
	}
	okReq.Header.Set("X-CCR-Session-Token", "local-token")
	okResp, err := http.DefaultClient.Do(okReq)
	if err != nil {
		t.Fatalf("gateway ok request error = %v", err)
	}
	defer okResp.Body.Close()
	if okResp.StatusCode != http.StatusOK {
		t.Fatalf("gateway ok status = %d, want 200", okResp.StatusCode)
	}

	badReq, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL()+"/v1/models", http.NoBody)
	if err != nil {
		t.Fatalf("NewRequest(bad) error = %v", err)
	}
	badReq.Header.Set("X-CCR-Session-Token", "wrong-token")
	badResp, err := http.DefaultClient.Do(badReq)
	if err != nil {
		t.Fatalf("gateway bad request error = %v", err)
	}
	defer badResp.Body.Close()
	if badResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("gateway bad status = %d, want 401", badResp.StatusCode)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func newGatewayStore(t *testing.T, provider store.Provider, model store.Model) *store.Store {
	t.Helper()
	return newGatewayStoreWithContext(t, context.Background(), provider, model)
}

func newGatewayStoreWithContext(t *testing.T, ctx context.Context, provider store.Provider, model store.Model) *store.Store {
	t.Helper()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "ccr.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if err := s.AddProvider(ctx, provider); err != nil {
		t.Fatalf("AddProvider() error = %v", err)
	}
	if err := s.AddModel(ctx, model); err != nil {
		t.Fatalf("AddModel() error = %v", err)
	}
	return s
}

func startGateway(t *testing.T, ctx context.Context, s *store.Store, secrets fakeGatewaySecrets) *Server {
	t.Helper()
	return startGatewayWithConfig(t, ctx, Config{Store: s, Secrets: secrets, Token: "local-token"})
}

func startGatewayWithConfig(t *testing.T, ctx context.Context, cfg Config) *Server {
	t.Helper()
	server, err := Start(ctx, cfg)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	return server
}

type fakeGatewaySecrets map[string]string

func (f fakeGatewaySecrets) Available(ctx context.Context) error {
	return ctx.Err()
}

func (f fakeGatewaySecrets) Store(ctx context.Context, ref string, value string) error {
	return ctx.Err()
}

func (f fakeGatewaySecrets) Resolve(ctx context.Context, ref string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	value, ok := f[ref]
	if !ok {
		return "", fmt.Errorf("missing secret ref")
	}
	return value, nil
}
