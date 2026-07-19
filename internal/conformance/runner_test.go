package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/modelcap"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestRunProviderOpenAICompatibleMatrix(t *testing.T) {
	t.Parallel()
	provider := newOpenAIConformanceFixture(t, false)
	s := conformanceStore(t, store.Provider{
		Name: "fixture", Type: "litellm", BaseURL: provider.URL,
	}, store.Model{
		Alias: "coder", ProviderName: "fixture", ProviderModel: "model-v1", Status: "degraded",
	})
	result, err := RunProvider(context.Background(), Config{Store: s, Alias: "coder", Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("RunProvider() error = %v", err)
	}
	assertConformancePassed(t, result)
}

func TestRunProviderAnthropicCompatibleMatrix(t *testing.T) {
	t.Parallel()
	provider := newAnthropicConformanceFixture(t)
	s := conformanceStore(t, store.Provider{
		Name: "fixture", Type: "anthropic", BaseURL: provider.URL,
	}, store.Model{
		Alias: "coder", ProviderName: "fixture", ProviderModel: "model-v1", Status: "full",
	})
	result, err := RunProvider(context.Background(), Config{Store: s, Alias: "coder", Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("RunProvider() error = %v", err)
	}
	assertConformancePassed(t, result)
}

func TestRunProviderErrorProbeAvoidsConfiguredAliasCollision(t *testing.T) {
	t.Parallel()
	provider := newOpenAIConformanceFixture(t, false)
	s := conformanceStore(t, store.Provider{
		Name: "fixture", Type: "litellm", BaseURL: provider.URL,
	}, store.Model{
		Alias: "coder", ProviderName: "fixture", ProviderModel: "model-v1", Status: "degraded",
	})
	if err := s.AddModel(context.Background(), store.Model{
		Alias: "ccr-conformance-unconfigured", ProviderName: "fixture",
		ProviderModel: "model-v1", Status: "degraded",
	}); err != nil {
		t.Fatalf("AddModel(collision) error = %v", err)
	}
	result, err := RunProvider(context.Background(), Config{Store: s, Alias: "coder", Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("RunProvider() error = %v", err)
	}
	assertConformancePassed(t, result)
}

func TestRunProviderFailsDeclaredCapabilityWithoutChangingCompatibility(t *testing.T) {
	t.Parallel()
	provider := newOpenAIConformanceFixture(t, true)
	s := conformanceStore(t, store.Provider{
		Name: "fixture", Type: "litellm", BaseURL: provider.URL,
	}, store.Model{
		Alias: "coder", ProviderName: "fixture", ProviderModel: "model-v1", Status: "degraded",
	})
	result, err := RunProvider(context.Background(), Config{Store: s, Alias: "coder", Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("RunProvider() error = %v", err)
	}
	if result.Status != StatusFailed {
		t.Fatalf("result status = %q, want failed", result.Status)
	}
	stream := findCheck(t, result.Checks, "stream")
	if stream.Status != StatusFailed {
		t.Fatalf("stream check = %#v", stream)
	}
	model, err := s.GetModel(context.Background(), "coder")
	if err != nil || model.Status != "degraded" {
		t.Fatalf("GetModel() = %#v, %v", model, err)
	}
}

func TestRunProviderSkipsForcedToolWhenToolChoiceIsUnsupported(t *testing.T) {
	t.Parallel()
	provider := newOpenAIConformanceFixture(t, false)
	s := conformanceStore(t, store.Provider{
		Name: "fixture", Type: "litellm", BaseURL: provider.URL,
	}, store.Model{
		Alias: "coder", ProviderName: "fixture", ProviderModel: "model-v1", Status: "degraded",
		CapabilityOverrides: modelcap.Values{SupportsToolChoice: modelcap.Bool(false)},
	})
	result, err := RunProvider(context.Background(), Config{Store: s, Alias: "coder", Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("RunProvider() error = %v", err)
	}
	if result.Status != StatusPassed {
		t.Fatalf("result = %#v", result)
	}
	forcedTool := findCheck(t, result.Checks, "forced_tool")
	if forcedTool.Status != StatusNotApplicable {
		t.Fatalf("forced_tool check = %#v", forcedTool)
	}
}

func TestRunProviderDisablesParallelCallsForForcedToolProbe(t *testing.T) {
	t.Parallel()
	provider := newOpenAIConformanceFixture(t, false)
	s := conformanceStore(t, store.Provider{
		Name: "fixture", Type: "litellm", BaseURL: provider.URL,
	}, store.Model{
		Alias: "coder", ProviderName: "fixture", ProviderModel: "model-v1", Status: "degraded",
		CapabilityOverrides: modelcap.Values{SupportsParallelTools: modelcap.Bool(false)},
	})
	result, err := RunProvider(context.Background(), Config{Store: s, Alias: "coder", Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("RunProvider() error = %v", err)
	}
	if result.Status != StatusPassed {
		t.Fatalf("result = %#v", result)
	}
	forcedTool := findCheck(t, result.Checks, "forced_tool")
	if forcedTool.Status != StatusPassed {
		t.Fatalf("forced_tool check = %#v", forcedTool)
	}
}

func assertConformancePassed(t *testing.T, result Result) {
	t.Helper()
	if result.Status != StatusPassed || len(result.Checks) != 9 {
		t.Fatalf("result = %#v", result)
	}
	for _, check := range result.Checks {
		if check.Status != StatusPassed {
			t.Fatalf("check %s = %#v", check.Name, check)
		}
		if check.Evidence == "" {
			t.Fatalf("check %s has no evidence", check.Name)
		}
	}
}

func findCheck(t *testing.T, checks []Check, name string) Check {
	t.Helper()
	for _, check := range checks {
		if check.Name == name {
			return check
		}
	}
	t.Fatalf("check %q not found in %#v", name, checks)
	return Check{}
}

func newOpenAIConformanceFixture(t *testing.T, breakStream bool) *httptest.Server {
	t.Helper()
	var chatCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"data":[{"id":"model-v1"}]}`)
		case "/v1/messages/count_tokens":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"input_tokens":9}`)
		case "/v1/chat/completions":
			call := chatCalls.Add(1)
			var payload struct {
				Messages []struct {
					Content string `json:"content"`
				} `json:"messages"`
				Tools []any `json:"tools"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			for _, message := range payload.Messages {
				if strings.Contains(message.Content, "CCR_CONFORMANCE_CANCEL") {
					select {
					case <-r.Context().Done():
						return
					case <-time.After(100 * time.Millisecond):
					}
				}
			}
			w.Header().Set("Content-Type", "application/json")
			if breakStream && call == 2 {
				http.Error(w, "stream unavailable", http.StatusBadGateway)
				return
			}
			if len(payload.Tools) > 0 {
				_, _ = fmt.Fprint(w, `{"id":"tool","choices":[{"message":{"content":"","tool_calls":[{"id":"toolu-1","type":"function","function":{"name":"ccr_probe","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`)
				return
			}
			_, _ = fmt.Fprint(w, `{"id":"text","choices":[{"message":{"content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func newAnthropicConformanceFixture(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Anthropic-Version") != conformanceAnthropicVersion {
			http.Error(w, "anthropic-version is required", http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/v1/messages/count_tokens":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"input_tokens":9}`)
		case "/v1/messages":
			var payload struct {
				Stream   bool  `json:"stream"`
				Tools    []any `json:"tools"`
				Messages []struct {
					Content string `json:"content"`
				} `json:"messages"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			for _, message := range payload.Messages {
				if strings.Contains(message.Content, "CCR_CONFORMANCE_CANCEL") {
					select {
					case <-r.Context().Done():
						return
					case <-time.After(100 * time.Millisecond):
					}
				}
			}
			if payload.Stream {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"model-v1\",\"usage\":{\"input_tokens\":5}}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if len(payload.Tools) > 0 {
				_, _ = fmt.Fprint(w, `{"id":"tool","type":"message","model":"model-v1","content":[{"type":"tool_use","id":"toolu-1","name":"ccr_probe","input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":2}}`)
				return
			}
			_, _ = fmt.Fprint(w, `{"id":"text","type":"message","model":"model-v1","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func conformanceStore(t *testing.T, provider store.Provider, model store.Model) *store.Store {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "ccr.db"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
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
