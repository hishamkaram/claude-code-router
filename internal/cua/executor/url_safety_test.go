package executor

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
)

func TestValidatePublicHTTPSURL(t *testing.T) {
	t.Parallel()

	resolver := staticResolver(map[string][]netip.Addr{
		"executor.example": {netip.MustParseAddr("93.184.216.34")},
	})
	parsed, err := ValidatePublicHTTPSURL(context.Background(), "https://executor.example/cua", resolver)
	if err != nil {
		t.Fatalf("ValidatePublicHTTPSURL() error = %v", err)
	}
	if parsed.String() != "https://executor.example/cua" {
		t.Fatalf("parsed URL = %q", parsed.String())
	}
}

func TestValidatePublicHTTPSURLRejectsUnsafeTargets(t *testing.T) {
	t.Parallel()

	resolver := staticResolver(map[string][]netip.Addr{
		"private.example":  {netip.MustParseAddr("10.0.0.7")},
		"mixed.example":    {netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("172.16.0.1")},
		"metadata.example": {netip.MustParseAddr("169.254.169.254")},
	})
	for _, raw := range []string{
		"http://executor.example",
		"https://user:pass@executor.example",
		"https://localhost",
		"https://service.localhost",
		"https://127.0.0.1",
		"https://10.0.0.1",
		"https://169.254.169.254",
		"https://[::]",
		"https://[::1]",
		"https://[fe80::1]",
		"https://[64:ff9b::7f00:1]",
		"https://[64:ff9b:1::7f00:1]",
		"https://[100::1]",
		"https://[2001::1]",
		"https://[2001:2::1]",
		"https://[2002:c0a8:101::1]",
		"https://metadata.google.internal",
		"https://private.example",
		"https://mixed.example",
		"https://metadata.example",
	} {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()

			if _, err := ValidatePublicHTTPSURL(context.Background(), raw, resolver); err == nil {
				t.Fatalf("ValidatePublicHTTPSURL(%q) unexpectedly succeeded", raw)
			}
		})
	}
}

func TestValidateExternalBaseURLRejectsQueryAndFragment(t *testing.T) {
	t.Parallel()

	resolver := staticResolver(map[string][]netip.Addr{
		"executor.example": {netip.MustParseAddr("93.184.216.34")},
	})
	for _, raw := range []string{
		"https://executor.example/cua?token=value",
		"https://executor.example/cua#fragment",
	} {
		if _, err := ValidateExternalBaseURL(context.Background(), raw, resolver); err == nil {
			t.Fatalf("ValidateExternalBaseURL(%q) unexpectedly succeeded", raw)
		}
	}
}

func TestSafeExternalDialContextRejectsDNSRebinding(t *testing.T) {
	t.Parallel()

	resolver := &sequenceResolver{
		responses: [][]netip.Addr{
			{netip.MustParseAddr("93.184.216.34")},
			{netip.MustParseAddr("10.0.0.1")},
		},
	}
	if _, err := ValidatePublicHTTPSURL(context.Background(), "https://rebind.example", resolver); err != nil {
		t.Fatalf("initial validation error = %v", err)
	}

	calledDialer := false
	dial := safeExternalDialContext(resolver, dialContextFunc(func(context.Context, string, string) (net.Conn, error) {
		calledDialer = true
		return nil, errors.New("dialer should not be called")
	}))
	if _, err := dial(context.Background(), "tcp", "rebind.example:443"); err == nil ||
		!strings.Contains(err.Error(), "not public") {
		t.Fatalf("dial error = %v, want not public", err)
	}
	if calledDialer {
		t.Fatal("dialer was called after rebinding to a private address")
	}
}

func TestSafeExternalDialContextRejectsReservedIPv6TransitionRange(t *testing.T) {
	t.Parallel()

	resolver := &sequenceResolver{
		responses: [][]netip.Addr{
			{netip.MustParseAddr("93.184.216.34")},
			{netip.MustParseAddr("64:ff9b::7f00:1")},
		},
	}
	if _, err := ValidatePublicHTTPSURL(context.Background(), "https://rebind.example", resolver); err != nil {
		t.Fatalf("initial validation error = %v", err)
	}

	calledDialer := false
	dial := safeExternalDialContext(resolver, dialContextFunc(func(context.Context, string, string) (net.Conn, error) {
		calledDialer = true
		return nil, errors.New("dialer should not be called")
	}))
	if _, err := dial(context.Background(), "tcp", "rebind.example:443"); err == nil ||
		!strings.Contains(err.Error(), "not public") {
		t.Fatalf("dial error = %v, want not public", err)
	}
	if calledDialer {
		t.Fatal("dialer was called after rebinding to a reserved IPv6 transition range")
	}
}

func TestSafeExternalDialContextDialsResolvedPublicIP(t *testing.T) {
	t.Parallel()

	resolver := staticResolver(map[string][]netip.Addr{
		"executor.example": {netip.MustParseAddr("93.184.216.34")},
	})
	var gotAddress string
	dial := safeExternalDialContext(resolver, dialContextFunc(func(_ context.Context, _, address string) (net.Conn, error) {
		gotAddress = address
		return nil, errors.New("stop")
	}))
	if _, err := dial(context.Background(), "tcp", "executor.example:443"); err == nil ||
		!strings.Contains(err.Error(), "stop") {
		t.Fatalf("dial error = %v, want stop", err)
	}
	if gotAddress != "93.184.216.34:443" {
		t.Fatalf("dial address = %q", gotAddress)
	}
}

type fakeResolver struct {
	mu        sync.Mutex
	addresses map[string][]netip.Addr
	err       error
}

func staticResolver(addresses map[string][]netip.Addr) *fakeResolver {
	return &fakeResolver{addresses: addresses}
}

func (resolver *fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	if resolver.err != nil {
		return nil, resolver.err
	}
	addresses := append([]netip.Addr(nil), resolver.addresses[host]...)
	return addresses, nil
}

type sequenceResolver struct {
	mu        sync.Mutex
	responses [][]netip.Addr
}

func (resolver *sequenceResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	if len(resolver.responses) == 0 {
		return nil, nil
	}
	addresses := append([]netip.Addr(nil), resolver.responses[0]...)
	resolver.responses = resolver.responses[1:]
	return addresses, nil
}

type dialContextFunc func(context.Context, string, string) (net.Conn, error)

func (fn dialContextFunc) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return fn(ctx, network, address)
}
