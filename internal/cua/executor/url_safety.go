package executor

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const defaultExternalHTTPTimeout = 30 * time.Second

// NetIPResolver resolves hostnames to IP addresses.
type NetIPResolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

type defaultNetIPResolver struct{}

func (defaultNetIPResolver) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, network, host)
}

// ValidatePublicHTTPSURL rejects non-public or credential-bearing HTTPS URLs.
func ValidatePublicHTTPSURL(ctx context.Context, raw string, resolver NetIPResolver) (*url.URL, error) {
	if ctx == nil {
		return nil, fmt.Errorf("executor URL validation context is required")
	}
	parsed, err := url.ParseRequestURI(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return nil, fmt.Errorf("executor URL must be an absolute public HTTPS URL")
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("executor URL must not include credentials")
	}
	if err := validateURLPort(parsed); err != nil {
		return nil, err
	}
	host := parsed.Hostname()
	if host == "" {
		return nil, fmt.Errorf("executor URL must include a host")
	}
	if err := ValidatePublicHost(ctx, host, resolver); err != nil {
		return nil, fmt.Errorf("executor URL host is not public: %w", err)
	}
	return parsed, nil
}

// ValidateExternalBaseURL validates the external executor base URL.
func ValidateExternalBaseURL(ctx context.Context, raw string, resolver NetIPResolver) (*url.URL, error) {
	parsed, err := ValidatePublicHTTPSURL(ctx, raw, resolver)
	if err != nil {
		return nil, err
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" || strings.Contains(strings.TrimSpace(raw), "#") {
		return nil, fmt.Errorf("external executor base URL must not include query or fragment")
	}
	return parsed, nil
}

// ValidatePublicHost rejects localhost, private, link-local, metadata, and reserved hosts.
func ValidatePublicHost(ctx context.Context, host string, resolver NetIPResolver) error {
	host = strings.TrimSpace(host)
	if host == "" {
		return fmt.Errorf("host is required")
	}
	host = strings.TrimSuffix(host, ".")
	if isBlockedHostname(host) {
		return fmt.Errorf("host is reserved")
	}
	if address, err := netip.ParseAddr(host); err == nil {
		if !isPublicAddress(address) {
			return fmt.Errorf("address is private or reserved")
		}
		return nil
	}
	if resolver == nil {
		resolver = defaultNetIPResolver{}
	}
	addresses, err := resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("resolving host: %w", err)
	}
	if len(addresses) == 0 {
		return fmt.Errorf("host has no addresses")
	}
	for _, address := range addresses {
		if !isPublicAddress(address) {
			return fmt.Errorf("address is private or reserved")
		}
	}
	return nil
}

// SafeExternalHTTPClient returns an HTTP client that rejects redirects and DNS rebinding.
func SafeExternalHTTPClient(resolver NetIPResolver, timeout time.Duration) *http.Client {
	if resolver == nil {
		resolver = defaultNetIPResolver{}
	}
	if timeout == 0 {
		timeout = defaultExternalHTTPTimeout
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           safeExternalDialContext(resolver, dialer),
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

type contextDialer interface {
	DialContext(context.Context, string, string) (net.Conn, error)
}

func safeExternalDialContext(resolver NetIPResolver, dialer contextDialer) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("invalid executor target")
		}
		resolved, err := resolvePublicDialAddress(ctx, resolver, host)
		if err != nil {
			return nil, fmt.Errorf("executor target is not public: %w", err)
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort(resolved.String(), port))
	}
}

func resolvePublicDialAddress(ctx context.Context, resolver NetIPResolver, host string) (netip.Addr, error) {
	host = strings.TrimSuffix(strings.TrimSpace(host), ".")
	if isBlockedHostname(host) {
		return netip.Addr{}, fmt.Errorf("host is reserved")
	}
	if address, err := netip.ParseAddr(host); err == nil {
		if !isPublicAddress(address) {
			return netip.Addr{}, fmt.Errorf("address is private or reserved")
		}
		return address, nil
	}
	if resolver == nil {
		resolver = defaultNetIPResolver{}
	}
	addresses, err := resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("resolving host: %w", err)
	}
	if len(addresses) == 0 {
		return netip.Addr{}, fmt.Errorf("host has no addresses")
	}
	for _, address := range addresses {
		if !isPublicAddress(address) {
			return netip.Addr{}, fmt.Errorf("address is private or reserved")
		}
	}
	return addresses[0].Unmap(), nil
}

func validateURLPort(parsed *url.URL) error {
	port := parsed.Port()
	if port == "" {
		return nil
	}
	value, err := strconv.Atoi(port)
	if err != nil || value < 1 || value > 65535 {
		return fmt.Errorf("executor URL port must be between 1 and 65535")
	}
	return nil
}

func isBlockedHostname(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	switch host {
	case "metadata", "metadata.google.internal":
		return true
	default:
		return false
	}
}

func isPublicAddress(address netip.Addr) bool {
	if !address.IsValid() {
		return false
	}
	address = address.Unmap()
	if !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() ||
		address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() || address.IsUnspecified() ||
		address.IsMulticast() {
		return false
	}
	for _, prefix := range reservedAddressPrefixes() {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

func reservedAddressPrefixes() []netip.Prefix {
	return []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"),
		netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("169.254.0.0/16"),
		netip.MustParsePrefix("192.0.0.0/24"),
		netip.MustParsePrefix("192.0.2.0/24"),
		netip.MustParsePrefix("198.18.0.0/15"),
		netip.MustParsePrefix("198.51.100.0/24"),
		netip.MustParsePrefix("203.0.113.0/24"),
		netip.MustParsePrefix("224.0.0.0/4"),
		netip.MustParsePrefix("240.0.0.0/4"),
		netip.MustParsePrefix("::/128"),
		netip.MustParsePrefix("::1/128"),
		netip.MustParsePrefix("::ffff:0:0/96"),
		netip.MustParsePrefix("64:ff9b::/96"),
		netip.MustParsePrefix("64:ff9b:1::/48"),
		netip.MustParsePrefix("100::/64"),
		netip.MustParsePrefix("2001::/23"),
		netip.MustParsePrefix("2001:2::/48"),
		netip.MustParsePrefix("2001:db8::/32"),
		netip.MustParsePrefix("2002::/16"),
		netip.MustParsePrefix("fc00::/7"),
		netip.MustParsePrefix("fe80::/10"),
		netip.MustParsePrefix("ff00::/8"),
	}
}
