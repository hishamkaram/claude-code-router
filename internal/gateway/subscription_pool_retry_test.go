package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestGatewayRetriesConfirmedSubscriptionLimitWithNextCredential(t *testing.T) {
	t.Parallel()

	pool := newGatewayTestSubscriptionPool("personal", "personal-oauth")
	pool.next = AnthropicSubscriptionCredential{
		AccountName: "work", OAuthToken: "work-oauth", Generation: 2,
	}
	var mu sync.Mutex
	var auth []string
	var apiKeys []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = append(auth, r.Header.Get("Authorization"))
		apiKeys = append(apiKeys, r.Header.Get("x-api-key"))
		call := len(auth)
		mu.Unlock()
		if call == 1 {
			writeConfirmedSubscriptionLimit(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"msg_ok","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"continued"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()
	server := startSubscriptionPoolRetryGateway(t, upstream.URL, pool, nil)
	defer shutdownGateway(t, context.Background(), server)

	resp := postSubscriptionGatewayMessage(t, context.Background(), server, "claude-sonnet-4-6")
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "continued") {
		t.Fatalf("gateway response = %d %s", resp.StatusCode, body)
	}
	mu.Lock()
	defer mu.Unlock()
	if got, want := auth, []string{"Bearer personal-oauth", "Bearer work-oauth"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("upstream auth = %v, want %v", got, want)
	}
	if apiKeys[0] != "" || apiKeys[1] != "" {
		t.Fatalf("upstream x-api-key values = %v, want none", apiKeys)
	}
	if pool.RotateCalls() != 1 || pool.ActiveAccount() != "work" {
		t.Fatalf("pool rotations=%d active=%q", pool.RotateCalls(), pool.ActiveAccount())
	}
}

func TestGatewaySubscriptionPoolOverridesEveryIncomingUpstreamAuthHeader(t *testing.T) {
	t.Parallel()

	pool := newGatewayTestSubscriptionPool("personal", "personal-oauth")
	var gotAuthorization []string
	var gotAPIKeys []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = append([]string(nil), r.Header.Values("Authorization")...)
		gotAPIKeys = append([]string(nil), r.Header.Values("x-api-key")...)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"msg_ok","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()
	server := startSubscriptionPoolRetryGateway(t, upstream.URL, pool, nil)
	defer shutdownGateway(t, context.Background(), server)

	req, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		server.URL()+"/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hello"}]}`),
	)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("X-CCR-Session-Token", "local-token")
	req.Header.Add("Authorization", "Bearer inherited-one")
	req.Header.Add("Authorization", "Bearer inherited-two")
	req.Header.Add("x-api-key", "inherited-api-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway status = %d", resp.StatusCode)
	}
	if len(gotAuthorization) != 1 || gotAuthorization[0] != "Bearer personal-oauth" {
		t.Fatalf("upstream Authorization = %v", gotAuthorization)
	}
	if len(gotAPIKeys) != 0 {
		t.Fatalf("upstream x-api-key = %v, want none", gotAPIKeys)
	}
}

func TestGatewaySubscriptionPoolNeverFallsBackToIncomingAuth(t *testing.T) {
	t.Parallel()

	pool := newGatewayTestSubscriptionPool("", "")
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	server := startSubscriptionPoolRetryGateway(t, upstream.URL, pool, nil)
	defer shutdownGateway(t, context.Background(), server)

	resp := postSubscriptionGatewayMessage(t, context.Background(), server, "claude-sonnet-4-6")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || upstreamCalls.Load() != 0 {
		t.Fatalf("status=%d upstream calls=%d, want visible failure without fallback",
			resp.StatusCode, upstreamCalls.Load())
	}
}

func TestGatewayPreservesConfirmedLimitWhenPoolHasNoReplacement(t *testing.T) {
	t.Parallel()

	pool := newGatewayTestSubscriptionPool("personal", "personal-oauth")
	const limitedBody = `{"type":"error","error":{"type":"rate_limit_error","message":"subscription exhausted"}}`
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(anthropicUnifiedRateLimitStatusHeader, anthropicUnifiedRateLimitRejected)
		w.Header().Set(anthropicUnifiedRateLimitRepresentativeClaimHeader, "five_hour")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = fmt.Fprint(w, limitedBody)
	}))
	defer upstream.Close()
	server := startSubscriptionPoolRetryGateway(t, upstream.URL, pool, nil)
	defer shutdownGateway(t, context.Background(), server)

	resp := postSubscriptionGatewayMessage(t, context.Background(), server, "claude-sonnet-4-6")
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if resp.StatusCode != http.StatusTooManyRequests || string(body) != limitedBody {
		t.Fatalf("gateway response = %d %s", resp.StatusCode, body)
	}
	if calls.Load() != 1 || pool.RotateCalls() != 1 {
		t.Fatalf("upstream calls=%d rotations=%d, want one each", calls.Load(), pool.RotateCalls())
	}
}

func TestGatewayDoesNotRotateModelSpecificOrAmbiguousLimits(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		claim    string
		fallback string
	}{
		{name: "fallback available", claim: "seven_day_sonnet", fallback: "available"},
		{name: "unknown claim", claim: "future_bucket"},
		{name: "missing claim"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pool := newGatewayTestSubscriptionPool("personal", "personal-oauth")
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(anthropicUnifiedRateLimitStatusHeader, anthropicUnifiedRateLimitRejected)
				w.Header().Set(anthropicUnifiedRateLimitRepresentativeClaimHeader, tc.claim)
				w.Header().Set(anthropicUnifiedRateLimitFallbackHeader, tc.fallback)
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			defer upstream.Close()
			server := startSubscriptionPoolRetryGateway(t, upstream.URL, pool, nil)
			defer shutdownGateway(t, context.Background(), server)

			resp := postSubscriptionGatewayMessage(t, context.Background(), server, "claude-sonnet-4-6")
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusTooManyRequests || pool.RotateCalls() != 0 {
				t.Fatalf("status=%d rotations=%d", resp.StatusCode, pool.RotateCalls())
			}
		})
	}
}

func TestGatewayClosesRejectedResponseBeforeRetry(t *testing.T) {
	t.Parallel()

	pool := newGatewayTestSubscriptionPool("personal", "personal-oauth")
	pool.next = AnthropicSubscriptionCredential{
		AccountName: "work", OAuthToken: "work-oauth", Generation: 2,
	}
	firstBody := &closeTrackingBody{Reader: strings.NewReader("limited")}
	var calls atomic.Int32
	var closedBeforeSecond atomic.Bool
	client := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		switch calls.Add(1) {
		case 1:
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header: http.Header{
					anthropicUnifiedRateLimitStatusHeader:              []string{anthropicUnifiedRateLimitRejected},
					anthropicUnifiedRateLimitRepresentativeClaimHeader: []string{"five_hour"},
				},
				Body: firstBody,
			}, nil
		default:
			closedBeforeSecond.Store(firstBody.closed.Load())
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(
					`{"id":"msg_ok","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
				)),
			}, nil
		}
	})}
	server := startSubscriptionPoolRetryGateway(t, "https://api.anthropic.test", pool, client)
	defer shutdownGateway(t, context.Background(), server)

	resp := postSubscriptionGatewayMessage(t, context.Background(), server, "claude-sonnet-4-6")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !closedBeforeSecond.Load() {
		t.Fatalf("status=%d first response closed before retry=%t", resp.StatusCode, closedBeforeSecond.Load())
	}
}

func TestGatewayClosesRejectedResponseWithoutReadingBeforeRetry(t *testing.T) {
	t.Parallel()

	pool := newGatewayTestSubscriptionPool("personal", "personal-oauth")
	pool.next = AnthropicSubscriptionCredential{
		AccountName: "work", OAuthToken: "work-oauth", Generation: 2,
	}
	firstBody := &readTrackingBody{}
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return confirmedLimitResponse(firstBody), nil
		}
		return successfulSubscriptionResponse(), nil
	})}
	server := startSubscriptionPoolRetryGateway(t, "https://api.anthropic.test", pool, client)
	defer shutdownGateway(t, context.Background(), server)

	resp := postSubscriptionGatewayMessage(t, context.Background(), server, "claude-sonnet-4-6")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || firstBody.read.Load() || !firstBody.closed.Load() {
		t.Fatalf("status=%d rejected body read=%t closed=%t",
			resp.StatusCode, firstBody.read.Load(), firstBody.closed.Load())
	}
}

func TestGatewayRecordsFinalConfirmedLimitAtRetryBound(t *testing.T) {
	t.Parallel()

	pool := newGatewayTestSubscriptionPool("account-1", "oauth-1")
	pool.autoAdvance = true
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeConfirmedSubscriptionLimit(w)
	}))
	defer upstream.Close()
	server := startSubscriptionPoolRetryGateway(t, upstream.URL, pool, nil)
	defer shutdownGateway(t, context.Background(), server)

	resp := postSubscriptionGatewayMessage(t, context.Background(), server, "claude-sonnet-4-6")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("gateway status = %d, want 429", resp.StatusCode)
	}
	if calls.Load() != maxAnthropicSubscriptionAttempts ||
		pool.RotateCalls() != maxAnthropicSubscriptionAttempts ||
		pool.ActiveAccount() != "account-65" {
		t.Fatalf("calls=%d rotations=%d active=%q",
			calls.Load(), pool.RotateCalls(), pool.ActiveAccount())
	}
}

func TestGatewayClosesRejectedResponseWhenRotationFails(t *testing.T) {
	t.Parallel()

	pool := newGatewayTestSubscriptionPool("personal", "personal-oauth")
	pool.rotateErr = errors.New("rotation unavailable")
	rejectedBody := &closeTrackingBody{Reader: strings.NewReader("limited")}
	client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header: http.Header{
				anthropicUnifiedRateLimitStatusHeader:              []string{anthropicUnifiedRateLimitRejected},
				anthropicUnifiedRateLimitRepresentativeClaimHeader: []string{"five_hour"},
			},
			Body: rejectedBody,
		}, nil
	})}
	server := startSubscriptionPoolRetryGateway(t, "https://api.anthropic.test", pool, client)
	defer shutdownGateway(t, context.Background(), server)

	resp := postSubscriptionGatewayMessage(t, context.Background(), server, "claude-sonnet-4-6")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || !rejectedBody.closed.Load() {
		t.Fatalf("status=%d rejected response closed=%t", resp.StatusCode, rejectedBody.closed.Load())
	}
}

func startSubscriptionPoolRetryGateway(
	t *testing.T,
	anthropicBaseURL string,
	pool AnthropicSubscriptionPool,
	client *http.Client,
) *Server {
	t.Helper()
	ctx := context.Background()
	s := newGatewayStoreWithContext(t, ctx,
		store.Provider{Name: "litellm", Type: "litellm", BaseURL: "http://127.0.0.1:1"},
		store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"},
	)
	return startGatewayWithConfig(t, ctx, Config{
		Store: s, Secrets: fakeGatewaySecrets{}, HTTPClient: client,
		Token: "local-token", DefaultModelAlias: "gpt",
		AnthropicBaseURL: anthropicBaseURL, AnthropicSubscriptionPool: pool,
	})
}

func writeConfirmedSubscriptionLimit(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set(anthropicUnifiedRateLimitStatusHeader, anthropicUnifiedRateLimitRejected)
	w.Header().Set(anthropicUnifiedRateLimitRepresentativeClaimHeader, "five_hour")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = fmt.Fprint(w, `{"type":"error","error":{"type":"rate_limit_error","message":"limited"}}`)
}

type gatewayTestSubscriptionPool struct {
	mu          sync.Mutex
	current     AnthropicSubscriptionCredential
	next        AnthropicSubscriptionCredential
	rotateErr   error
	autoAdvance bool
	rotateCalls int
}

func newGatewayTestSubscriptionPool(name, token string) *gatewayTestSubscriptionPool {
	return &gatewayTestSubscriptionPool{current: AnthropicSubscriptionCredential{
		AccountName: name, OAuthToken: token, Generation: 1,
	}}
}

func (p *gatewayTestSubscriptionPool) CurrentCredential(
	context.Context,
) (AnthropicSubscriptionCredential, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.current, nil
}

func (p *gatewayTestSubscriptionPool) RotateCredential(
	_ context.Context,
	failed AnthropicSubscriptionCredential,
	_ AnthropicSubscriptionExhaustionEvent,
) (AnthropicSubscriptionCredential, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rotateCalls++
	if p.rotateErr != nil {
		return AnthropicSubscriptionCredential{}, false, p.rotateErr
	}
	if failed.Generation != p.current.Generation {
		return p.current, true, nil
	}
	if p.autoAdvance {
		generation := p.current.Generation + 1
		p.current = AnthropicSubscriptionCredential{
			AccountName: fmt.Sprintf("account-%d", generation),
			OAuthToken:  fmt.Sprintf("oauth-%d", generation),
			Generation:  generation,
		}
		return p.current, true, nil
	}
	if p.next.Generation == 0 {
		return AnthropicSubscriptionCredential{}, false, nil
	}
	p.current = p.next
	return p.current, true, nil
}

func (p *gatewayTestSubscriptionPool) ActiveAccount() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.current.AccountName
}

func (p *gatewayTestSubscriptionPool) RotateCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.rotateCalls
}

type closeTrackingBody struct {
	io.Reader
	closed atomic.Bool
}

type readTrackingBody struct {
	read   atomic.Bool
	closed atomic.Bool
}

func (b *readTrackingBody) Read([]byte) (int, error) {
	b.read.Store(true)
	return 0, io.EOF
}

func (b *readTrackingBody) Close() error {
	b.closed.Store(true)
	return nil
}

func confirmedLimitResponse(body io.ReadCloser) *http.Response {
	return &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header: http.Header{
			anthropicUnifiedRateLimitStatusHeader:              []string{anthropicUnifiedRateLimitRejected},
			anthropicUnifiedRateLimitRepresentativeClaimHeader: []string{"five_hour"},
		},
		Body: body,
	}
}

func successfulSubscriptionResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(
			`{"id":"msg_ok","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
		)),
	}
}

func (b *closeTrackingBody) Close() error {
	b.closed.Store(true)
	return nil
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
