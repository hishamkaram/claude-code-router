//go:build live

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/gateway"
	"github.com/hishamkaram/claude-code-router/internal/modelcap"
	"github.com/hishamkaram/claude-code-router/internal/providers"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

const (
	liveFixtureGatewayToken = "ccr-live-fixture-token"
	liveGatewayRequestLimit = 32 << 20
)

func TestLiveFixtureUnsupportedPDFAndAudioDoNotReachUpstream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var upstreamCalls atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"content":"unexpected"},"finish_reason":"stop"}]}`)
	}))
	defer provider.Close()

	gatewayURL := startLiveRouteFixtureGateway(t, ctx, liveRouteFixtureConfig{
		ProviderType:  "litellm",
		ProviderURL:   provider.URL,
		ModelAlias:    "fixture",
		ProviderModel: "gpt-5",
		Capabilities: modelcap.Values{
			Kind:               modelcap.KindChat,
			InputModalities:    []string{"text", "image"},
			SupportsPDFInput:   modelcap.Bool(false),
			SupportsAudioInput: modelcap.Bool(false),
			SupportsVision:     modelcap.Bool(true),
		},
	})

	for _, test := range []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "pdf",
			content: `[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0x"}}]`,
			want:    "does not support PDF input",
		},
		{
			name:    "audio",
			content: `[{"type":"input_audio","source":{"type":"base64","media_type":"audio/wav","data":"UklGRg=="}}]`,
			want:    "does not support audio input",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			status, raw := postLiveGatewayMessage(t, ctx, gatewayURL, []byte(`{"model":"fixture","max_tokens":16,"messages":[{"role":"user","content":`+test.content+`}]}`), nil)
			if status != http.StatusNotImplemented || !strings.Contains(raw, test.want) {
				t.Fatalf("gateway status=%d body=%s, want 501 containing %q", status, raw, test.want)
			}
			if got := upstreamCalls.Load(); got != 0 {
				t.Fatalf("unsupported %s reached upstream %d times", test.name, got)
			}
		})
	}
}

func TestLiveFixtureRequestSizeBoundaryAndSingleAttempt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	var upstreamCalls atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Errorf("discarding provider body: %v", err)
		}
		http.Error(w, "fixture upstream failure", http.StatusBadGateway)
	}))
	defer provider.Close()

	gatewayURL := startLiveRouteFixtureGateway(t, ctx, liveRouteFixtureConfig{
		ProviderType:  "litellm",
		ProviderURL:   provider.URL,
		ModelAlias:    "fixture",
		ProviderModel: "gpt-5",
		Capabilities: modelcap.Values{
			Kind:            modelcap.KindChat,
			InputModalities: []string{"text"},
		},
	})

	status, raw := postLiveGatewayMessage(t, ctx, gatewayURL, liveGatewayBodyWithTotalSize(t, liveGatewayRequestLimit), nil)
	if status != http.StatusBadGateway || !strings.Contains(raw, "fixture upstream failure") {
		t.Fatalf("gateway status=%d body=%s, want one surfaced upstream failure", status, raw)
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("upstream attempts after boundary request = %d, want 1", got)
	}

	status, raw = postLiveGatewayMessage(t, ctx, gatewayURL, liveGatewayBodyWithTotalSize(t, liveGatewayRequestLimit+1), nil)
	if status != http.StatusRequestEntityTooLarge || !strings.Contains(raw, "32 MiB") {
		t.Fatalf("gateway status=%d body=%s, want 413 32 MiB boundary error", status, raw)
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("oversized request made hidden upstream attempts: calls=%d", got)
	}
}

func TestLiveFixtureAnthropicNativeVisionAndCUAPassThrough(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, test := range []struct {
		name       string
		content    string
		wantBlocks []string
	}{
		{
			name:       "base64 image",
			content:    `[{"type":"text","text":"inspect base64"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}}]`,
			wantBlocks: []string{`"type":"image"`, `"type":"base64"`, `"media_type":"image/png"`},
		},
		{
			name:       "url image",
			content:    `[{"type":"text","text":"inspect url"},{"type":"image","source":{"type":"url","url":"https://example.com/live-fixture.png"}}]`,
			wantBlocks: []string{`"type":"image"`, `"type":"url"`, `"url":"https://example.com/live-fixture.png"`},
		},
		{
			name:       "tool result image",
			content:    `[{"type":"tool_result","tool_use_id":"toolu_image","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}}]}]`,
			wantBlocks: []string{`"type":"tool_result"`, `"tool_use_id":"toolu_image"`, `"type":"image"`},
		},
		{
			name:       "native cua",
			content:    `[{"type":"text","text":"use computer"}]`,
			wantBlocks: []string{`"type":"computer_20250124"`, `"name":"computer"`},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var upstreamCalls atomic.Int64
			var gotBody []byte
			var gotBeta string
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamCalls.Add(1)
				gotBeta = r.Header.Get("anthropic-beta")
				var readErr error
				gotBody, readErr = io.ReadAll(r.Body)
				if readErr != nil {
					t.Errorf("reading provider body: %v", readErr)
					http.Error(w, "bad request", http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, `{"id":"msg_fixture","type":"message","role":"assistant","model":"claude-fixture","content":[{"type":"text","text":"pass-through-ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
			}))
			defer provider.Close()

			gatewayURL := startLiveRouteFixtureGateway(t, ctx, liveRouteFixtureConfig{
				ProviderType:  "anthropic",
				ProviderURL:   provider.URL,
				ModelAlias:    "fixture",
				ProviderModel: "claude-fixture",
				Capabilities: modelcap.Values{
					Kind:                modelcap.KindChat,
					InputModalities:     []string{"text", "image"},
					SupportsTools:       modelcap.Bool(true),
					SupportsComputerUse: modelcap.Bool(true),
				},
			})

			body := []byte(`{"model":"fixture","max_tokens":16,"tools":[{"type":"computer_20250124","name":"computer","display_width_px":1024,"display_height_px":768,"display_number":1}],"messages":[{"role":"user","content":` + test.content + `}]}`)
			status, raw := postLiveGatewayMessage(t, ctx, gatewayURL, body, map[string]string{"anthropic-beta": "computer-use-2025-01-24"})
			if status != http.StatusOK || !strings.Contains(raw, "pass-through-ok") {
				t.Fatalf("gateway status=%d body=%s, want pass-through response", status, raw)
			}
			if got := upstreamCalls.Load(); got != 1 {
				t.Fatalf("upstream calls=%d, want 1", got)
			}
			if gotBeta != "computer-use-2025-01-24" {
				t.Fatalf("anthropic-beta header = %q, want computer-use-2025-01-24", gotBeta)
			}
			compact := compactLiveJSON(t, gotBody)
			for _, want := range test.wantBlocks {
				if !strings.Contains(compact, want) {
					t.Fatalf("upstream body missing %q\n%s", want, compact)
				}
			}
		})
	}
}

func TestLiveFixtureOpenAIResponsesNoChatFallback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var chatCalls atomic.Int64
	var responsesCalls atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/chat/completions":
			chatCalls.Add(1)
			http.Error(w, "chat fallback is forbidden for Responses models", http.StatusBadGateway)
		case "/v1/responses":
			responsesCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"id":"resp_fixture","output":[{"type":"message","content":[{"type":"output_text","text":"responses-ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()

	gatewayURL := startLiveRouteFixtureGateway(t, ctx, liveRouteFixtureConfig{
		ProviderType:      "litellm",
		ProviderURL:       provider.URL,
		ProviderResponses: true,
		ModelAlias:        "responses",
		ProviderModel:     "gpt-5-responses",
		Capabilities: modelcap.Values{
			Kind:              modelcap.KindResponses,
			SupportsResponses: modelcap.Bool(true),
		},
	})
	status, raw := postLiveGatewayMessage(t, ctx, gatewayURL, []byte(`{"model":"responses","max_tokens":16,"messages":[{"role":"user","content":"reply"}]}`), nil)
	if status != http.StatusOK || !strings.Contains(raw, "responses-ok") {
		t.Fatalf("gateway status=%d body=%s, want Responses success", status, raw)
	}
	if chatCalls.Load() != 0 || responsesCalls.Load() != 1 {
		t.Fatalf("responses route calls: chat=%d responses=%d, want chat=0 responses=1", chatCalls.Load(), responsesCalls.Load())
	}
}

func TestLiveFixtureOpenAIResponsesCUARequiresManagedExecutor(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var providerCalls atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalls.Add(1)
		http.Error(w, "must not be called", http.StatusInternalServerError)
	}))
	defer provider.Close()

	gatewayURL := startLiveRouteFixtureGateway(t, ctx, liveRouteFixtureConfig{
		ProviderType:      "litellm",
		ProviderURL:       provider.URL,
		ProviderResponses: true,
		ModelAlias:        "responses-cua",
		ProviderModel:     "gpt-5-computer",
		Capabilities: modelcap.Values{
			Kind:                modelcap.KindResponses,
			SupportsResponses:   modelcap.Bool(true),
			SupportsComputerUse: modelcap.Bool(true),
		},
	})
	body := []byte(`{"model":"responses-cua","max_tokens":16,"tools":[{"type":"computer_20250124","name":"computer","display_width_px":1024,"display_height_px":768,"display_number":1}],"messages":[{"role":"user","content":"take a screenshot"}]}`)
	status, raw := postLiveGatewayMessage(t, ctx, gatewayURL, body, nil)
	if status != http.StatusNotImplemented || !strings.Contains(raw, "requires a CCR managed CUA executor") {
		t.Fatalf("gateway status=%d body=%s, want managed CUA rejection", status, raw)
	}
	if providerCalls.Load() != 0 {
		t.Fatalf("Responses CUA reached provider: calls=%d", providerCalls.Load())
	}
}

func TestLiveFixtureOpenAIImageConversion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var providerBody []byte
	var providerCalls atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
		var err error
		providerBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading image provider request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"content":"images-ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer provider.Close()
	gatewayURL := startLiveRouteFixtureGateway(t, ctx, liveRouteFixtureConfig{
		ProviderType:  "litellm",
		ProviderURL:   provider.URL,
		ModelAlias:    "vision",
		ProviderModel: "gpt-5-vision",
		Capabilities: modelcap.Values{
			Kind:            modelcap.KindChat,
			InputModalities: []string{"text", "image"},
			SupportsVision:  modelcap.Bool(true),
		},
	})
	body := []byte(`{"model":"vision","max_tokens":16,"messages":[{"role":"user","content":[{"type":"text","text":"inspect"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}}]}]}`)
	status, raw := postLiveGatewayMessage(t, ctx, gatewayURL, body, nil)
	if status != http.StatusOK || !strings.Contains(raw, "images-ok") {
		t.Fatalf("gateway status=%d body=%s, want image conversion success", status, raw)
	}
	compact := compactLiveJSON(t, providerBody)
	if !strings.Contains(compact, "data:image/png;base64,iVBORw0KGgo=") {
		t.Fatalf("converted OpenAI request missing image data URL\n%s", compact)
	}

	toolResultBody := []byte(`{"model":"vision","max_tokens":16,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_image","content":[{"type":"image","source":{"type":"base64","media_type":"image/webp","data":"UklGRg=="}}]}]}]}`)
	status, raw = postLiveGatewayMessage(t, ctx, gatewayURL, toolResultBody, nil)
	if status != http.StatusNotImplemented || !strings.Contains(raw, "image tool_result content is not supported") {
		t.Fatalf("gateway status=%d body=%s, want Chat Completions tool-result image rejection", status, raw)
	}
	if got := providerCalls.Load(); got != 1 {
		t.Fatalf("image tool_result reached the Chat Completions provider: calls=%d", got)
	}
}

type liveRouteFixtureConfig struct {
	ProviderType      string
	ProviderURL       string
	ProviderResponses bool
	ModelAlias        string
	ProviderModel     string
	Capabilities      modelcap.Values
}

func startLiveRouteFixtureGateway(t *testing.T, ctx context.Context, cfg liveRouteFixtureConfig) string {
	t.Helper()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "ccr.db"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	provider := providerWithCapabilities("fixture", cfg.ProviderType, cfg.ProviderURL, "", providers.ModeFull)
	provider.SupportsResponses = cfg.ProviderResponses
	if err := s.AddProvider(ctx, provider); err != nil {
		t.Fatalf("AddProvider() error = %v", err)
	}
	if err := s.AddModel(ctx, store.Model{
		Alias:               cfg.ModelAlias,
		ProviderName:        provider.Name,
		ProviderModel:       cfg.ProviderModel,
		Status:              providers.ModeFull,
		CapabilityOverrides: cfg.Capabilities,
	}); err != nil {
		t.Fatalf("AddModel() error = %v", err)
	}
	server, err := gateway.Start(ctx, gateway.Config{Store: s, Token: liveFixtureGatewayToken})
	if err != nil {
		t.Fatalf("gateway.Start() error = %v", err)
	}
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	})
	return server.URL()
}

func postLiveGatewayMessage(t *testing.T, ctx context.Context, gatewayURL string, body []byte, headers map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gatewayURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CCR-Session-Token", liveFixtureGatewayToken)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway request error = %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading gateway response: %v", err)
	}
	return resp.StatusCode, string(raw)
}

func liveGatewayBodyWithTotalSize(t *testing.T, size int) []byte {
	t.Helper()
	const prefix = `{"model":"fixture","max_tokens":16,"messages":[{"role":"user","content":"`
	const suffix = `"}]}`
	if size < len(prefix)+len(suffix) {
		t.Fatalf("target body size %d is smaller than fixture envelope", size)
	}
	return []byte(prefix + strings.Repeat("x", size-len(prefix)-len(suffix)) + suffix)
}

func compactLiveJSON(t *testing.T, raw []byte) string {
	t.Helper()
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("upstream body is not JSON: %v\n%s", err, string(raw))
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("compacting JSON: %v", err)
	}
	return string(encoded)
}
