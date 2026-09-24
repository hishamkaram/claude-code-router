package providers

import "testing"

func TestIsFirstPartyAnthropicEndpoint(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		want    bool
	}{
		{name: "default endpoint", baseURL: "https://api.anthropic.com", want: true},
		{name: "equivalent spelling", baseURL: "https://API.ANTHROPIC.COM:443/", want: true},
		{name: "different host", baseURL: "https://anthropic.example.com", want: false},
		{name: "different path", baseURL: "https://api.anthropic.com/v1", want: false},
		{name: "different scheme", baseURL: "http://api.anthropic.com", want: false},
		{name: "userinfo", baseURL: "https://user@api.anthropic.com", want: false},
		{name: "query", baseURL: "https://api.anthropic.com?route=custom", want: false},
		{name: "empty query", baseURL: "https://api.anthropic.com?", want: false},
		{name: "fragment", baseURL: "https://api.anthropic.com#custom", want: false},
		{name: "invalid URL", baseURL: "https://api.anthropic.com:invalid", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := IsFirstPartyAnthropicEndpoint(test.baseURL); got != test.want {
				t.Fatalf("IsFirstPartyAnthropicEndpoint(%q) = %t, want %t", test.baseURL, got, test.want)
			}
		})
	}
}
