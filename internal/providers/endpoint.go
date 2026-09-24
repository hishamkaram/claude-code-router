package providers

import (
	"net/url"
	"strings"
)

// IsFirstPartyAnthropicEndpoint reports whether baseURL is Anthropic's official
// API endpoint, independent of hostname casing, default HTTPS port, or a
// trailing slash.
func IsFirstPartyAnthropicEndpoint(baseURL string) bool {
	profile, ok := (Registry{}).Profile("anthropic")
	if !ok {
		return false
	}
	return sameProviderEndpoint(baseURL, profile.DefaultBaseURL)
}

func sameProviderEndpoint(left, right string) bool {
	leftURL, ok := parseProviderEndpoint(left)
	if !ok {
		return false
	}
	rightURL, ok := parseProviderEndpoint(right)
	if !ok {
		return false
	}
	return strings.EqualFold(leftURL.Scheme, rightURL.Scheme) &&
		strings.EqualFold(leftURL.Hostname(), rightURL.Hostname()) &&
		providerEndpointPort(leftURL) == providerEndpointPort(rightURL) &&
		strings.TrimRight(leftURL.EscapedPath(), "/") == strings.TrimRight(rightURL.EscapedPath(), "/")
}

func parseProviderEndpoint(value string) (*url.URL, bool) {
	endpoint, err := url.Parse(value)
	if err != nil || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery ||
		endpoint.Fragment != "" || endpoint.RawFragment != "" {
		return nil, false
	}
	return endpoint, true
}

func providerEndpointPort(endpoint *url.URL) string {
	port := endpoint.Port()
	switch strings.ToLower(endpoint.Scheme) {
	case "http":
		if port == "80" {
			return ""
		}
	case "https":
		if port == "443" {
			return ""
		}
	}
	return port
}
