package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/cua"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestGatewayFirstPartyClaudeRoutePassesThroughClientManagedCUAWithManagedRuntime(t *testing.T) {
	ctx := context.Background()
	var gotAuth string
	var gotBody string
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("Anthropic path = %q, want /v1/messages", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("reading Anthropic request: %v", err)
		}
		gotBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"client-managed"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer anthropic.Close()

	s := newGatewayStore(t,
		store.Provider{Name: "fallback", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1", SupportsTools: true, SupportsStreaming: true},
		store.Model{Alias: "fallback", ProviderName: "fallback", ProviderModel: "fallback-model", Status: "degraded"},
	)
	managedRuntime := newGatewayManagedRuntime(t, &gatewayManagedExecutor{}, cua.DecisionApprove)
	server := startGatewayWithConfig(t, ctx, Config{
		Store: s, Secrets: fakeGatewaySecrets{}, Token: "local-token", AnthropicBaseURL: anthropic.URL,
		ManagedCUA: managedRuntime, ManagedCUAProject: "fixture-project",
	})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-6","max_tokens":16,"tools":[{"type":"computer_20250124","name":"computer","display_width_px":1024,"display_height_px":768}],"messages":[{"role":"user","content":"take a screenshot"}]}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("X-CCR-Session-Token", "local-token")
	req.Header.Set("Authorization", "Bearer anthropic-session")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("gateway status = %d, want 200: %s", resp.StatusCode, body)
	}
	if gotAuth != "Bearer anthropic-session" {
		t.Fatalf("Anthropic auth = %q, want incoming subscription auth", gotAuth)
	}
	if !strings.Contains(gotBody, `"type":"computer_20250124"`) {
		t.Fatalf("Anthropic body omitted client-managed computer tool: %s", gotBody)
	}
}
