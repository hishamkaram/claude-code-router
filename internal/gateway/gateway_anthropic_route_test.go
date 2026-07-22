package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestGatewayModelDiscoveryIncludesConfiguredAliases(t *testing.T) {
	ctx := context.Background()
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.URL.Query().Get("limit") != "1000" {
			t.Fatalf("anthropic discovery path = %q rawQuery=%q", r.URL.Path, r.URL.RawQuery)
		}
		if got := r.Header.Get("anthropic-version"); got != "2023-06-01" {
			t.Fatalf("anthropic-version = %q, want 2023-06-01", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":[{"id":"claude-sonnet-4-6","display_name":"Claude Sonnet 4.6"}]}`)
	}))
	defer anthropic.Close()

	s := newGatewayStore(t, store.Provider{Name: "litellm", Type: "litellm", BaseURL: "http://127.0.0.1:1", SecretRef: ""}, store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"})
	if err := s.AddProvider(ctx, store.Provider{Name: "anthropic", Type: "anthropic", BaseURL: anthropic.URL, SecretRef: ""}); err != nil {
		t.Fatalf("AddProvider(anthropic) error = %v", err)
	}
	if err := s.AddModel(ctx, store.Model{Alias: "claude-custom", ProviderName: "litellm", ProviderModel: "claude-compatible", Status: "full"}); err != nil {
		t.Fatalf("AddModel(claude-custom) error = %v", err)
	}
	if err := s.AddModel(ctx, store.Model{Alias: "sonnet", ProviderName: "litellm", ProviderModel: "third-party-sonnet", Status: "degraded"}); err != nil {
		t.Fatalf("AddModel(sonnet) error = %v", err)
	}
	if err := s.AddModel(ctx, store.Model{Alias: "glm", ProviderName: "litellm", ProviderModel: "glm-5.2[1m]", Status: "degraded"}); err != nil {
		t.Fatalf("AddModel(glm) error = %v", err)
	}
	if err := s.AddModel(ctx, store.Model{Alias: "blocked", ProviderName: "litellm", ProviderModel: "blocked-model", Status: "blocked"}); err != nil {
		t.Fatalf("AddModel(blocked) error = %v", err)
	}
	server := startGatewayWithConfig(t, ctx, Config{Store: s, Secrets: fakeGatewaySecrets{}, Token: "local-token", AnthropicBaseURL: anthropic.URL})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL()+"/v1/models?limit=1000", http.NoBody)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("X-CCR-Session-Token", "local-token")
	req.Header.Set("Authorization", "Bearer anthropic-session")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway models request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway status = %d, want 200", resp.StatusCode)
	}
	var decoded struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("gateway model decode error = %v", err)
	}
	ids := make([]string, 0, len(decoded.Data))
	for _, item := range decoded.Data {
		ids = append(ids, item.ID)
	}
	for _, want := range []string{"default", "sonnet", "opus", "haiku", "anthropic.ccr.gpt", "anthropic.ccr.claude-custom", "anthropic.ccr.s%6fnnet", "anthropic.ccr.glm[1m]"} {
		if !containsString(ids, want) {
			t.Fatalf("discovery ids = %#v, missing %q", ids, want)
		}
	}
	for _, hidden := range []string{"gpt", "claude-custom", "claude-native-claude-sonnet-4-6", "anthropic.ccr.blocked", "claude-ccr-gpt"} {
		if containsString(ids, hidden) {
			t.Fatalf("discovery ids = %#v, should hide %q", ids, hidden)
		}
	}
}

func TestGatewayAnthropicPassThroughPreservesToolsAndHeaders(t *testing.T) {
	ctx := context.Background()
	var gotBeta string
	var gotSession string
	var gotLocalAuth string
	var gotAPIKey string
	var gotCCRToken string
	var gotBody string
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.URL.RawQuery != "beta=true" {
			t.Fatalf("anthropic path = %q rawQuery=%q", r.URL.Path, r.URL.RawQuery)
		}
		gotBeta = r.Header.Get("anthropic-beta")
		gotSession = r.Header.Get("x-claude-code-session-id")
		gotLocalAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("x-api-key")
		gotCCRToken = r.Header.Get("X-CCR-Session-Token")
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("reading pass-through body: %v", err)
		}
		gotBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer anthropic.Close()

	s := newGatewayStore(t, store.Provider{Name: "anthropic", Type: "anthropic", BaseURL: anthropic.URL, SecretRef: "env:ANTHROPIC_API_KEY"}, store.Model{Alias: "gpt", ProviderName: "anthropic", ProviderModel: "claude-opus", Status: "full"})
	server := startGateway(t, ctx, s, fakeGatewaySecrets{"env:ANTHROPIC_API_KEY": "upstream-secret"})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	body := `{"model":"gpt","tools":[{"name":"bash","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hello"}],"future_field":{"kept":true}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages?beta=true", strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("X-CCR-Session-Token", "local-token")
	req.Header.Set("Authorization", "Bearer anthropic-session")
	req.Header.Set("anthropic-beta", "tools-2026")
	req.Header.Set("x-claude-code-session-id", "session-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway pass-through error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway status = %d, want 200", resp.StatusCode)
	}
	if gotAPIKey != "upstream-secret" {
		t.Fatalf("pass-through did not receive expected API key header")
	}
	if gotBeta != "tools-2026" || gotSession != "session-1" || gotLocalAuth != "" || gotCCRToken != "" {
		t.Fatalf("pass-through headers beta=%q session=%q auth=%q ccr=%q", gotBeta, gotSession, gotLocalAuth, gotCCRToken)
	}
	if !strings.Contains(gotBody, `"model":"claude-opus"`) || !strings.Contains(gotBody, `"future_field"`) {
		t.Fatalf("pass-through body = %s, want provider model rewrite with future field", gotBody)
	}
}

func TestGatewayAnthropicPassThroughFlushesEventStream(t *testing.T) {
	ctx := context.Background()
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "event: message_start\n")
		_, _ = fmt.Fprint(w, `data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":0}}}`+"\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(750 * time.Millisecond)
		_, _ = fmt.Fprint(w, "event: message_stop\n")
		w.(http.Flusher).Flush()
	}))
	defer anthropic.Close()

	s := newGatewayStore(t, store.Provider{Name: "anthropic", Type: "anthropic", BaseURL: anthropic.URL, SecretRef: ""}, store.Model{Alias: "claude", ProviderName: "anthropic", ProviderModel: "claude-opus", Status: "full"})
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	reqCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(`{"model":"claude","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer local-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway stream request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway status = %d, want 200", resp.StatusCode)
	}
	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("reading first stream line: %v", err)
	}
	if line != "event: message_start\n" {
		t.Fatalf("first stream line = %q, want message_start event", line)
	}
	line, err = reader.ReadString('\n')
	if err != nil {
		t.Fatalf("reading stream data line: %v", err)
	}
	if !strings.Contains(line, `"model":"claude"`) {
		t.Fatalf("stream data line = %q, want alias model", line)
	}
}

func TestGatewayCountTokensUsesAnthropicAliasProviderModel(t *testing.T) {
	ctx := context.Background()
	var gotBody string
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages/count_tokens" {
			t.Fatalf("anthropic path = %q, want /v1/messages/count_tokens", r.URL.Path)
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("reading count_tokens body: %v", err)
		}
		gotBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"input_tokens":7}`)
	}))
	defer anthropic.Close()

	s := newGatewayStore(t, store.Provider{Name: "anthropic", Type: "anthropic", BaseURL: anthropic.URL, SecretRef: ""}, store.Model{Alias: "claude", ProviderName: "anthropic", ProviderModel: "claude-opus", Status: "full"})
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages/count_tokens", strings.NewReader(`{"model":"claude","messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer local-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway count_tokens error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(gotBody, `"model":"claude-opus"`) {
		t.Fatalf("count_tokens body = %s, want provider model rewrite", gotBody)
	}
}
