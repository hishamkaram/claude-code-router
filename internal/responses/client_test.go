package responses

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientCreatePostsResponsesRequest(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotAuth string
	var gotRequest Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotRequest); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_ok","model":"gpt","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`))
	}))
	defer server.Close()

	client, err := NewClient(ClientOptions{
		BaseURL:    server.URL,
		APIKey:     "test-secret",
		HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	resp, err := client.Create(context.Background(), &Request{
		Model: "gpt",
		Input: []InputItem{{Type: "message", Role: "user", Content: []Content{{Type: "input_text", Text: "hello"}}}},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if gotPath != "/v1/responses" {
		t.Fatalf("path = %q, want /v1/responses", gotPath)
	}
	if gotAuth != "Bearer test-secret" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotRequest.Model != "gpt" || len(gotRequest.Input) != 1 {
		t.Fatalf("request body = %#v", gotRequest)
	}
	if resp.ID != "resp_ok" {
		t.Fatalf("response ID = %q, want resp_ok", resp.ID)
	}
}

func TestClientKeepsVersionedBaseURL(t *testing.T) {
	t.Parallel()

	client, err := NewClient(ClientOptions{BaseURL: "https://proxy.example/api/v1"})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if client.endpoint != "https://proxy.example/api/v1/responses" {
		t.Fatalf("endpoint = %q, want versioned Responses endpoint", client.endpoint)
	}
}

func TestClientCreateBoundsAndRedactsErrorBody(t *testing.T) {
	t.Parallel()

	body := "prefix test-secret " + strings.Repeat("x", 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, body, http.StatusBadGateway)
	}))
	defer server.Close()

	client, err := NewClient(ClientOptions{
		BaseURL:       server.URL,
		APIKey:        "test-secret",
		HTTPClient:    server.Client(),
		MaxErrorBytes: 24,
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	_, err = client.Create(context.Background(), &Request{Model: "gpt"})
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("error = %T %[1]v, want *HTTPError", err)
	}
	if httpErr.StatusCode != http.StatusBadGateway || !httpErr.Truncated {
		t.Fatalf("HTTPError = %#v", httpErr)
	}
	if strings.Contains(err.Error(), "test-secret") {
		t.Fatalf("error leaked API key: %v", err)
	}
	if len(httpErr.Body) > 24 {
		t.Fatalf("bounded body length = %d, want <= 24", len(httpErr.Body))
	}
}

func TestClientCreateRedactsAPIKeyAcrossErrorBodyLimit(t *testing.T) {
	t.Parallel()

	const (
		apiKey        = "test-secret"
		maxErrorBytes = 24
	)
	body := strings.Repeat("x", maxErrorBytes-4) + apiKey + strings.Repeat("y", 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, body, http.StatusBadGateway)
	}))
	defer server.Close()

	client, err := NewClient(ClientOptions{
		BaseURL:       server.URL,
		APIKey:        apiKey,
		HTTPClient:    server.Client(),
		MaxErrorBytes: maxErrorBytes,
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	_, err = client.Create(context.Background(), &Request{Model: "gpt"})
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("error = %T %[1]v, want *HTTPError", err)
	}
	if strings.Contains(httpErr.Body, apiKey) || strings.Contains(httpErr.Body, apiKey[:4]) {
		t.Fatalf("bounded error body leaked API key fragment: %q", httpErr.Body)
	}
	if !httpErr.Truncated || len(httpErr.Body) > maxErrorBytes {
		t.Fatalf("HTTPError = %#v", httpErr)
	}
}

func TestClientMalformedSuccessBodyWrapsProviderOutput(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{`))
	}))
	defer server.Close()

	client, err := NewClient(ClientOptions{BaseURL: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	_, err = client.Create(context.Background(), &Request{Model: "gpt"})
	if !errors.Is(err, ErrMalformedProviderOutput) {
		t.Fatalf("error = %v, want ErrMalformedProviderOutput", err)
	}
}

func TestClientCreateBoundsSuccessfulResponseBody(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(strings.Repeat(" ", 33)))
	}))
	defer server.Close()

	client, err := NewClient(ClientOptions{
		BaseURL:          server.URL,
		HTTPClient:       server.Client(),
		MaxResponseBytes: 32,
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	_, err = client.Create(context.Background(), &Request{Model: "gpt"})
	if !errors.Is(err, ErrMalformedProviderOutput) || !strings.Contains(err.Error(), "exceeds the 32 byte limit") {
		t.Fatalf("Create() error = %v, want bounded malformed-provider-output error", err)
	}
}
