package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/modelcap"
	"github.com/hishamkaram/claude-code-router/internal/observability"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestGatewayClampsConfiguredOutputLimitForOpenAIChat(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			ctx := context.Background()
			var gotMaxTokens int
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload struct {
					MaxTokens int `json:"max_tokens"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Fatalf("provider decode error = %v", err)
				}
				gotMaxTokens = payload.MaxTokens
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, `{"id":"chatcmpl-limit","choices":[{"message":{"content":"OK"},"finish_reason":"stop"}]}`)
			}))
			defer provider.Close()

			s := newGatewayStore(t, store.Provider{Name: "local", Type: "openai-compatible", BaseURL: provider.URL}, store.Model{
				Alias: "qwen", ProviderName: "local", ProviderModel: "qwen3", Status: "degraded",
				CapabilityOverrides: modelcap.Values{MaxOutputTokens: modelcap.Int64(32)},
			})
			server := startGateway(t, ctx, s, fakeGatewaySecrets{})
			defer func() { _ = server.Shutdown(ctx) }()

			body := fmt.Sprintf(`{"model":"qwen","max_tokens":64,"stream":%t,"messages":[{"role":"user","content":"hello"}]}`, stream)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(body))
			if err != nil {
				t.Fatalf("NewRequest() error = %v", err)
			}
			req.Header.Set("Authorization", "Bearer local-token")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("gateway request error = %v", err)
			}
			defer resp.Body.Close()
			if _, err := io.ReadAll(resp.Body); err != nil {
				t.Fatalf("reading gateway response = %v", err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("gateway status = %d, want 200", resp.StatusCode)
			}
			if gotMaxTokens != 32 {
				t.Fatalf("provider max_tokens = %d, want 32", gotMaxTokens)
			}
			if got := resp.Header.Get(ccrOutputLimitHeader); got != "clamped;requested=64;applied=32" {
				t.Fatalf("output limit header = %q", got)
			}
		})
	}
}

func TestGatewayRecordsOutputLimitClampInTrace(t *testing.T) {
	ctx := context.Background()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-limit","choices":[{"message":{"content":"OK"},"finish_reason":"stop"}]}`)
	}))
	defer provider.Close()
	s := newGatewayStore(t, store.Provider{Name: "local", Type: "openai-compatible", BaseURL: provider.URL}, store.Model{
		Alias: "qwen", ProviderName: "local", ProviderModel: "qwen3", Status: "degraded",
		CapabilityOverrides: modelcap.Values{MaxOutputTokens: modelcap.Int64(32)},
	})
	launchID, err := s.CreateLaunch(ctx, "qwen", "running", "running")
	if err != nil {
		t.Fatalf("CreateLaunch() error = %v", err)
	}
	recorder := observability.NewRecorder(ctx, observability.Config{Store: s, LaunchID: launchID, Enabled: true})
	server := startGatewayWithConfig(t, ctx, Config{Store: s, Secrets: fakeGatewaySecrets{}, Token: "local-token", Recorder: recorder})
	defer func() { _ = server.Shutdown(ctx) }()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(`{"model":"qwen","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer local-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway request error = %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway status = %d, want 200", resp.StatusCode)
	}
	events, err := s.ListTraceEvents(ctx, store.TraceFilter{LaunchID: launchID, Kind: "lifecycle", Name: "compatibility_degradation", Limit: 5})
	if err != nil {
		t.Fatalf("ListTraceEvents() error = %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("compatibility degradation events = %#v, want one", events)
	}
	event := events[0].Lifecycle
	if event.Status != "degraded" || event.ExternalID != resp.Header.Get(ccrRequestIDHeader) ||
		event.Reason != "max_tokens_clamped; requested=64; applied=32; model_limit=32; alias=qwen" {
		t.Fatalf("compatibility degradation event = %#v", event)
	}
}

func TestNormalizeModelOutputLimitLeavesRequestsWithinLimitUnchanged(t *testing.T) {
	limit := int64(32)
	route := messageRoute{model: store.Model{Alias: "qwen"}, modelCapabilities: modelcap.Values{MaxOutputTokens: &limit}}
	req := anthropicRequest{MaxTokens: 32}
	got, clamp, validationErr := normalizeModelOutputLimit(route, req)
	if validationErr != nil || clamp != nil || got.MaxTokens != req.MaxTokens {
		t.Fatalf("normalizeModelOutputLimit() = req=%#v clamp=%#v error=%v", got, clamp, validationErr)
	}
}
