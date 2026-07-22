package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/modelcap"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestOpenAIRequestConvertsBase64Images(t *testing.T) {
	t.Parallel()
	req := anthropicRequest{Messages: []anthropicMessage{{
		Role: "user",
		Content: []any{
			map[string]any{"type": "text", "text": "describe"},
			map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "iVBORw0KGgo="}},
		},
	}}}
	messages, err := openAIMessagesFromRequestWithResolver(context.Background(), req, openAIModelRoute{}, newImageSourceResolver(nil))
	if err != nil {
		t.Fatalf("openAIMessagesFromRequestWithResolver() error = %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("messages = %#v", messages)
	}
	parts, ok := messages[0].Content.([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("user content = %#v", messages[0].Content)
	}
	imagePart, ok := parts[1].(map[string]any)
	if !ok || imagePart["type"] != "image_url" || !strings.Contains(imagePart["image_url"].(map[string]string)["url"], "data:image/png;base64,") {
		t.Fatalf("image part = %#v", imagePart)
	}
}

func TestURLImageDataURLRejectsDataThatWouldExceedGatewayRequestLimit(t *testing.T) {
	t.Parallel()

	_, err := urlImageDataURL("image/png", make([]byte, maxURLImageBytes+1))
	if err == nil || !strings.Contains(err.Error(), "base64-safe") {
		t.Fatalf("urlImageDataURL() error = %v, want base64-safe size rejection", err)
	}
}

func TestURLImageDataURLWithBudgetRejectsAggregateURLBytes(t *testing.T) {
	t.Parallel()

	budget := newImageFetchBudget(4)
	if _, err := urlImageDataURLWithBudget("image/png", []byte("123"), budget); err != nil {
		t.Fatalf("urlImageDataURLWithBudget() first image error = %v", err)
	}
	if budget.remaining != 1 {
		t.Fatalf("budget remaining = %d, want 1", budget.remaining)
	}
	_, err := urlImageDataURLWithBudget("image/png", []byte("12"), budget)
	if err == nil || !strings.Contains(err.Error(), "gateway request budget") {
		t.Fatalf("urlImageDataURLWithBudget() second image error = %v, want aggregate budget rejection", err)
	}
	if budget.remaining != 1 {
		t.Fatalf("budget remaining after rejection = %d, want 1", budget.remaining)
	}
}

func TestOpenAIChatConversionRejectsAggregateURLImageBudget(t *testing.T) {
	t.Parallel()

	budget := newImageFetchBudget(4)
	resolver := func(context.Context, map[string]any) (map[string]any, error) {
		return urlImageDataURLWithBudget("image/png", []byte("123"), budget)
	}
	req := anthropicRequest{Messages: []anthropicMessage{{
		Role: "user",
		Content: []any{
			map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "https://images.example/one.png"}},
			map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "https://images.example/two.png"}},
		},
	}}}

	_, err := toOpenAIChatRequestWithResolver(context.Background(), req, openAIModelRoute{}, resolver)
	if err == nil || !strings.Contains(err.Error(), "gateway request budget") {
		t.Fatalf("toOpenAIChatRequestWithResolver() error = %v, want aggregate budget rejection", err)
	}
	if budget.remaining != 1 {
		t.Fatalf("budget remaining = %d, want 1", budget.remaining)
	}
}

func TestGatewayRejectsImageToolResultForOpenAIChat(t *testing.T) {
	ctx := context.Background()
	var providerCalls atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalls.Add(1)
		http.Error(w, "Chat Completions must not receive image tool results", http.StatusBadRequest)
	}))
	defer provider.Close()

	s := newGatewayStore(t,
		store.Provider{Name: "litellm", Type: "litellm", BaseURL: provider.URL},
		store.Model{
			Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded",
			CapabilityOverrides: modelcap.Values{SupportsVision: modelcap.Bool(true)},
		},
	)
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() { _ = server.Shutdown(ctx) }()

	body := `{"model":"gpt","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_image","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}}]}]}]}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer local-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway request error = %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading gateway response: %v", err)
	}
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("gateway status = %d body=%s, want %d", resp.StatusCode, raw, http.StatusNotImplemented)
	}
	if !strings.Contains(string(raw), "image tool_result content is not supported") {
		t.Fatalf("gateway response = %s", raw)
	}
	if providerCalls.Load() != 0 {
		t.Fatalf("provider calls = %d, want 0", providerCalls.Load())
	}
}

func TestOpenAIImageConversionRejectsUnsupportedOrPrivateSources(t *testing.T) {
	t.Parallel()
	for name, block := range map[string]map[string]any{
		"unsupported media type": {"type": "image", "source": map[string]any{"type": "base64", "media_type": "application/pdf", "data": "AA=="}},
		"private HTTPS host":     {"type": "image", "source": map[string]any{"type": "url", "url": "https://127.0.0.1/private.png"}},
		"reserved HTTPS host":    {"type": "image", "source": map[string]any{"type": "url", "url": "https://192.0.2.10/reserved.png"}},
		"URL credentials":        {"type": "image", "source": map[string]any{"type": "url", "url": "https://user:pass@example.com/image.png"}},
		"non-HTTPS URL":          {"type": "image", "source": map[string]any{"type": "url", "url": "http://example.com/image.png"}},
		"mapped loopback host":   {"type": "image", "source": map[string]any{"type": "url", "url": "https://[::ffff:127.0.0.1]/image.png"}},
	} {
		name, block := name, block
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := resolveAnthropicImageSource(context.Background(), nil, block)
			if err == nil {
				t.Fatal("resolveAnthropicImageSource() succeeded, want error")
			}
		})
	}
}

func TestOpenAIImageConversionDoesNotUseUnsafeCustomClientForReservedTarget(t *testing.T) {
	t.Parallel()
	called := false
	client := &http.Client{
		Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			called = true
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"image/png"}},
				Body:       io.NopCloser(strings.NewReader("png")),
			}, nil
		}),
	}
	block := map[string]any{
		"type":   "image",
		"source": map[string]any{"type": "url", "url": "https://192.0.2.10/image.png"},
	}
	_, err := resolveAnthropicImageSource(context.Background(), client, block)
	if err == nil {
		t.Fatal("resolveAnthropicImageSource() succeeded, want error")
	}
	if called {
		t.Fatal("custom image HTTP client transport was called for an unsafe target")
	}
}

func TestSafeImageHTTPClientRejectsRedirectsDespiteConfiguredPolicy(t *testing.T) {
	t.Parallel()
	configured := &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return nil
		},
	}
	client := safeImageHTTPClient(configured)
	err := client.CheckRedirect(&http.Request{}, []*http.Request{{}})
	if !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("CheckRedirect() error = %v, want %v", err, http.ErrUseLastResponse)
	}
}

func TestSafeImageHTTPClientReplacesConfiguredTransportAndPreservesTimeout(t *testing.T) {
	t.Parallel()
	configuredTransport := &recordingRoundTripper{}
	configured := &http.Client{
		Transport: configuredTransport,
		Timeout:   123 * time.Millisecond,
	}
	client := safeImageHTTPClient(configured)
	if client.Transport == configuredTransport {
		t.Fatal("safeImageHTTPClient preserved configured transport")
	}
	if client.Timeout != configured.Timeout {
		t.Fatalf("client timeout = %v, want %v", client.Timeout, configured.Timeout)
	}
}

func TestPublicImageDialTargetsUseResolvedPublicIP(t *testing.T) {
	t.Parallel()
	calls := 0
	targets, err := publicImageDialTargetsWithResolver(
		context.Background(),
		"images.example:443",
		func(context.Context, string) ([]netip.Addr, error) {
			calls++
			if calls > 1 {
				return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
			}
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		},
	)
	if err != nil {
		t.Fatalf("publicImageDialTargetsWithResolver() error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("resolver calls = %d, want 1", calls)
	}
	if len(targets) != 1 || targets[0] != "93.184.216.34:443" {
		t.Fatalf("targets = %#v, want validated IP target", targets)
	}
}

func TestPublicImageDialTargetsRejectsMixedPrivateResolution(t *testing.T) {
	t.Parallel()
	_, err := publicImageDialTargetsWithResolver(
		context.Background(),
		"images.example:443",
		func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{
				netip.MustParseAddr("93.184.216.34"),
				netip.MustParseAddr("127.0.0.1"),
			}, nil
		},
	)
	if err == nil {
		t.Fatal("publicImageDialTargetsWithResolver() succeeded, want error")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type recordingRoundTripper struct{}

func (*recordingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("unexpected transport call")
}
