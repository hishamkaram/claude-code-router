package claudeaccount

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDiagnosticsClientReturnsRedactedIdentityAndQuotaWindows(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-oauth-token" {
			t.Fatalf("authorization header was not set")
		}
		switch r.URL.Path {
		case "/api/oauth/profile":
			_, _ = fmt.Fprint(w, `{"account":{"uuid":"account-id","has_claude_max":true},"organization":{"uuid":"org-id"}}`)
		case "/api/oauth/usage":
			_, _ = fmt.Fprint(w, `{"five_hour":{"utilization":15.5,"resets_at":"2026-07-25T16:50:00.360663Z"},"seven_day":{"utilization":100,"resets_at":"2026-07-28T02:00:00Z"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewDiagnosticsClient(server.Client())
	client.baseURL = server.URL
	got, err := client.Probe(context.Background(), "test-oauth-token")
	if err != nil {
		t.Fatalf("Probe() error = %v", err)
	}
	if got.IdentityFingerprint == "" || strings.Contains(got.IdentityFingerprint, "account-id") {
		t.Fatalf("identity fingerprint = %q", got.IdentityFingerprint)
	}
	if got.Plan != "max" || len(got.Windows) != 2 ||
		got.Windows[0].Kind != "five_hour" || got.Windows[0].Utilization != 15.5 ||
		got.Windows[1].Kind != "seven_day" || got.Windows[1].Utilization != 100 {
		t.Fatalf("diagnostics = %#v", got)
	}
}

func TestDiagnosticsClientWithholdsTokenAndResponseBodyFromErrors(t *testing.T) {
	t.Parallel()
	const token = "private-oauth-token"
	const body = "private-upstream-body"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, body, http.StatusForbidden)
	}))
	defer server.Close()

	client := NewDiagnosticsClient(server.Client())
	client.baseURL = server.URL
	_, err := client.Probe(context.Background(), token)
	if err == nil {
		t.Fatal("Probe() error = nil")
	}
	if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), body) {
		t.Fatalf("Probe() leaked sensitive data: %v", err)
	}
	if !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("Probe() error = %v, want HTTP status", err)
	}
}

func TestDiagnosticsClientRejectsInvalidUsageWithoutRawData(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/oauth/profile" {
			_, _ = fmt.Fprint(w, `{"account":{"uuid":"account-id"},"organization":{"uuid":"org-id"}}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"five_hour":{"utilization":101,"resets_at":"not-a-time"}}`)
	}))
	defer server.Close()

	client := NewDiagnosticsClient(server.Client())
	client.baseURL = server.URL
	_, err := client.Probe(context.Background(), "test-oauth-token")
	if err == nil || !strings.Contains(err.Error(), "invalid five_hour utilization") {
		t.Fatalf("Probe() error = %v", err)
	}
	if strings.Contains(err.Error(), "not-a-time") {
		t.Fatalf("Probe() leaked response data: %v", err)
	}
}

func TestDiagnosticsClientAcceptsUnavailableReset(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/oauth/profile" {
			_, _ = fmt.Fprint(w, `{"account":{"uuid":"account-id"},"organization":{"uuid":"org-id"}}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"five_hour":{"utilization":0,"resets_at":null}}`)
	}))
	defer server.Close()

	client := NewDiagnosticsClient(server.Client())
	client.baseURL = server.URL
	got, err := client.Probe(context.Background(), "test-oauth-token")
	if err != nil {
		t.Fatalf("Probe() error = %v", err)
	}
	if len(got.Windows) != 1 || got.Windows[0].Kind != "five_hour" ||
		got.Windows[0].Utilization != 0 || !got.Windows[0].ResetsAt.IsZero() {
		t.Fatalf("diagnostics = %#v", got)
	}
}

func TestDiagnosticsClientRejectsRedirectWithoutForwardingCredential(t *testing.T) {
	t.Parallel()
	credentialForwarded := false
	target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		credentialForwarded = r.Header.Get("Authorization") != ""
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer source.Close()

	client := NewDiagnosticsClient(source.Client())
	client.baseURL = source.URL
	_, err := client.Probe(context.Background(), "test-oauth-token")
	if err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Fatalf("Probe() error = %v", err)
	}
	if credentialForwarded {
		t.Fatal("diagnostics client forwarded OAuth credential across redirect")
	}
}

func TestDiagnosticsClientRejectsMalformedDocumentsWithoutLeakingData(t *testing.T) {
	t.Parallel()
	const validProfile = `{"account":{"uuid":"account-id"},"organization":{"uuid":"org-id"}}`
	tests := []struct {
		name    string
		profile string
		usage   string
		want    string
		private string
	}{
		{
			name: "malformed profile", profile: `{"private-profile":`, usage: `{}`,
			want: "decoding response JSON", private: "private-profile",
		},
		{
			name: "missing identity", profile: `{"account":{"uuid":"private-account"}}`, usage: `{}`,
			want: "omitted stable identity fields", private: "private-account",
		},
		{
			name: "malformed usage", profile: validProfile, usage: `{"private-usage":`,
			want: "decoding response JSON", private: "private-usage",
		},
		{
			name: "invalid reset", profile: validProfile,
			usage: `{"five_hour":{"utilization":50,"resets_at":"private-reset"}}`,
			want:  "invalid five_hour reset", private: "private-reset",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/oauth/profile" {
					_, _ = fmt.Fprint(w, test.profile)
					return
				}
				_, _ = fmt.Fprint(w, test.usage)
			}))
			defer server.Close()

			client := NewDiagnosticsClient(server.Client())
			client.baseURL = server.URL
			_, err := client.Probe(context.Background(), "private-oauth-token")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Probe() error = %v, want %q", err, test.want)
			}
			if strings.Contains(err.Error(), test.private) ||
				strings.Contains(err.Error(), "private-oauth-token") {
				t.Fatalf("Probe() leaked private response data: %v", err)
			}
		})
	}
}

func TestDiagnosticsClientRejectsOversizedResponseWithoutLeakingBody(t *testing.T) {
	t.Parallel()
	const token = "private-oauth-token"
	body := strings.Repeat("private-body-", maxDiagnosticsResponseBytes)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, body)
	}))
	defer server.Close()

	client := NewDiagnosticsClient(server.Client())
	client.baseURL = server.URL
	_, err := client.Probe(context.Background(), token)
	if err == nil || !strings.Contains(err.Error(), "response exceeded") {
		t.Fatalf("Probe() error = %v", err)
	}
	if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "private-body") {
		t.Fatalf("Probe() leaked oversized response data: %v", err)
	}
}
