package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/cua"
)

func TestExternalHTTPSendsBearerTokenToHealthAndActions(t *testing.T) {
	t.Parallel()

	resolver := staticResolver(map[string][]netip.Addr{
		"executor.example": {netip.MustParseAddr("93.184.216.34")},
	})
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if got := req.Header.Get("Authorization"); got != "Bearer test-external-token" {
			t.Fatalf("Authorization header = %q", got)
		}
		if req.Method == http.MethodGet {
			if req.URL.String() != "https://executor.example/api/health" {
				t.Fatalf("health URL = %q", req.URL.String())
			}
			return textResponse(http.StatusOK, "application/json", `{}`), nil
		}
		if req.Method != http.MethodPost || req.URL.String() != "https://executor.example/api/actions" {
			t.Fatalf("action request = %s %s", req.Method, req.URL.String())
		}
		var payload externalActionRequest
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatalf("request body is not JSON: %v", err)
		}
		assertExternalActionString(t, payload.Action, "call_id", "call_1")
		assertExternalActionString(t, payload.Action, "type", string(cua.ActionScreenshot))
		if _, exists := payload.Action["CallID"]; exists {
			t.Fatalf("request action used Go field name CallID: %#v", payload.Action)
		}
		if _, exists := payload.Action["Kind"]; exists {
			t.Fatalf("request action used Go field name Kind: %#v", payload.Action)
		}
		return textResponse(http.StatusOK, "application/json", `{"text":"ok","content_type":"text/plain","raw":{"state":"ready"}}`), nil
	})}
	executor, err := NewExternalHTTP(context.Background(), "browser-prod", ExternalOptions{
		BaseURL:     "https://executor.example/api",
		BearerToken: "test-external-token",
		Resolver:    resolver,
	})
	if err != nil {
		t.Fatalf("NewExternalHTTP() error = %v", err)
	}
	executor.httpClient = client
	if checkErr := executor.Check(context.Background()); checkErr != nil {
		t.Fatalf("Check() error = %v", checkErr)
	}

	observation, err := executor.Execute(context.Background(), cua.Action{CallID: "call_1", Kind: cua.ActionScreenshot})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if observation.Text != "ok" || string(observation.Raw) != `{"state":"ready"}` {
		t.Fatalf("observation = %#v", observation)
	}
}

func TestExternalActionRequestPreservesStableRawResponsesShape(t *testing.T) {
	t.Parallel()

	request, err := newExternalActionRequest(cua.Action{
		CallID: "call_drag",
		Kind:   cua.ActionDrag,
		Raw:    json.RawMessage(`{"type":"drag","path":[{"x":1,"y":2},{"x":3,"y":4}],"button":"left"}`),
	})
	if err != nil {
		t.Fatalf("newExternalActionRequest() error = %v", err)
	}
	assertExternalActionString(t, request.Action, "call_id", "call_drag")
	assertExternalActionString(t, request.Action, "type", string(cua.ActionDrag))
	if _, exists := request.Action["path"]; !exists {
		t.Fatalf("request action dropped raw Responses path: %#v", request.Action)
	}

	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	for _, forbidden := range []string{"CallID", "Kind", "Raw"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("external action request leaked Go field %q in %s", forbidden, encoded)
		}
	}
}

func TestExternalHTTPRejectsRedirects(t *testing.T) {
	t.Parallel()

	resolver := staticResolver(map[string][]netip.Addr{
		"executor.example": {netip.MustParseAddr("93.184.216.34")},
	})
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return textResponse(http.StatusFound, "text/plain", "redirect"), nil
	})}
	executor, err := NewExternalHTTP(context.Background(), "browser-prod", ExternalOptions{
		BaseURL:     "https://executor.example/api",
		BearerToken: "test-external-token",
		Resolver:    resolver,
	})
	if err != nil {
		t.Fatalf("NewExternalHTTP() error = %v", err)
	}
	executor.httpClient = client
	if err := executor.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "redirects") {
		t.Fatalf("Check() error = %v, want redirect rejection", err)
	}
}

func TestExternalHTTPMapsHTTP501ToUnsupportedAction(t *testing.T) {
	t.Parallel()

	resolver := staticResolver(map[string][]netip.Addr{
		"executor.example": {netip.MustParseAddr("93.184.216.34")},
	})
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return textResponse(http.StatusNotImplemented, "application/json", `{}`), nil
	})}
	executor, err := NewExternalHTTP(context.Background(), "browser-prod", ExternalOptions{
		BaseURL:     "https://executor.example/api",
		BearerToken: "test-external-token",
		Resolver:    resolver,
	})
	if err != nil {
		t.Fatalf("NewExternalHTTP() error = %v", err)
	}
	executor.httpClient = client
	_, err = executor.Execute(context.Background(), cua.Action{CallID: "call_1", Kind: cua.ActionDrag})
	if !IsUnsupportedAction(err) {
		t.Fatalf("Execute() error = %v, want unsupported action", err)
	}
}

func TestExternalHTTPRequiresBearerToken(t *testing.T) {
	t.Parallel()

	resolver := staticResolver(map[string][]netip.Addr{
		"executor.example": {netip.MustParseAddr("93.184.216.34")},
	})
	_, err := NewExternalHTTP(context.Background(), "browser-prod", ExternalOptions{
		BaseURL:  "https://executor.example/api",
		Resolver: resolver,
	})
	if err == nil || !strings.Contains(err.Error(), "bearer token is required") {
		t.Fatalf("NewExternalHTTP() error = %v, want missing bearer token rejection", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func textResponse(status int, contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func assertExternalActionString(t *testing.T, action externalActionPayload, name, want string) {
	t.Helper()
	raw, exists := action[name]
	if !exists {
		t.Fatalf("request action missing %q: %#v", name, action)
	}
	var got string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("request action %q is not a string: %v", name, err)
	}
	if got != want {
		t.Fatalf("request action %q = %q, want %q", name, got, want)
	}
}
