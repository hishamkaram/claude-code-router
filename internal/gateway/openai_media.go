package gateway

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/cua"
)

const (
	maxImageBytes    = 32 << 20
	maxURLImageBytes = cua.MaxComputerScreenshotBytes
)

type imageSourceResolver func(context.Context, map[string]any) (map[string]any, error)

type imageFetchBudget struct {
	remaining int64
}

func newImageFetchBudget(limit int64) *imageFetchBudget {
	return &imageFetchBudget{remaining: limit}
}

func firstImageFetchBudget(budgets ...*imageFetchBudget) *imageFetchBudget {
	for _, budget := range budgets {
		if budget != nil {
			return budget
		}
	}
	return nil
}

func (budget *imageFetchBudget) limit() int64 {
	if budget == nil || budget.remaining > int64(maxURLImageBytes) {
		return int64(maxURLImageBytes)
	}
	if budget.remaining < 0 {
		return 0
	}
	return budget.remaining
}

func (budget *imageFetchBudget) consume(size int) error {
	if budget == nil {
		return nil
	}
	if size < 0 || int64(size) > budget.remaining {
		return fmt.Errorf("URL image inputs exceed the base64-safe 32 MiB gateway request budget")
	}
	budget.remaining -= int64(size)
	return nil
}

func newImageSourceResolver(client *http.Client, budgets ...*imageFetchBudget) imageSourceResolver {
	budget := firstImageFetchBudget(budgets...)
	return func(ctx context.Context, block map[string]any) (map[string]any, error) {
		return resolveAnthropicImageSource(ctx, client, block, budget)
	}
}

func resolveAnthropicImageSource(ctx context.Context, client *http.Client, block map[string]any, budgets ...*imageFetchBudget) (map[string]any, error) {
	source, ok := block["source"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("image block source must be an object")
	}
	sourceType, _ := source["type"].(string)
	switch strings.ToLower(strings.TrimSpace(sourceType)) {
	case "base64":
		return resolveBase64Image(source)
	case "url":
		return resolveURLImage(ctx, client, source, firstImageFetchBudget(budgets...))
	default:
		return nil, fmt.Errorf("image source type %q is not supported", sourceType)
	}
}

func resolveBase64Image(source map[string]any) (map[string]any, error) {
	mediaType, _ := source["media_type"].(string)
	if !supportedImageMediaType(mediaType) {
		return nil, fmt.Errorf("image media type %q is not supported; expected JPEG, PNG, GIF, or WebP", mediaType)
	}
	data, _ := source["data"].(string)
	data = strings.TrimSpace(data)
	if data == "" {
		return nil, fmt.Errorf("base64 image source requires media_type and data")
	}
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return nil, fmt.Errorf("image base64 data is invalid")
	}
	if len(decoded) > maxImageBytes {
		return nil, fmt.Errorf("image exceeds the 32 MiB gateway limit")
	}
	return openAIImageURLPart("data:" + normalizeImageMediaType(mediaType) + ";base64," + data), nil
}

func resolveURLImage(ctx context.Context, configured *http.Client, source map[string]any, budgets ...*imageFetchBudget) (map[string]any, error) {
	rawURL, _ := source["url"].(string)
	parsed, err := validatePublicImageURL(rawURL)
	if err != nil {
		return nil, err
	}
	client := safeImageHTTPClient(configured)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("creating image request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching image URL: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= http.StatusMultipleChoices && resp.StatusCode < http.StatusBadRequest {
		return nil, fmt.Errorf("image URL redirects are not allowed")
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("image URL returned HTTP %d", resp.StatusCode)
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || !supportedImageMediaType(mediaType) {
		return nil, fmt.Errorf("image URL returned unsupported content type")
	}
	budget := firstImageFetchBudget(budgets...)
	data, err := io.ReadAll(io.LimitReader(resp.Body, budget.limit()+1))
	if err != nil {
		return nil, fmt.Errorf("reading image URL response: %w", err)
	}
	return urlImageDataURLWithBudget(mediaType, data, budget)
}

func urlImageDataURL(mediaType string, data []byte) (map[string]any, error) {
	return urlImageDataURLWithBudget(mediaType, data, nil)
}

func urlImageDataURLWithBudget(mediaType string, data []byte, budget *imageFetchBudget) (map[string]any, error) {
	if len(data) > maxURLImageBytes {
		return nil, fmt.Errorf("image URL exceeds the base64-safe 32 MiB gateway limit")
	}
	if err := budget.consume(len(data)); err != nil {
		return nil, err
	}
	encoded := base64.StdEncoding.EncodeToString(data)
	return openAIImageURLPart("data:" + normalizeImageMediaType(mediaType) + ";base64," + encoded), nil
}

func openAIImageURLPart(value string) map[string]any {
	return map[string]any{
		"type":      "image_url",
		"image_url": map[string]string{"url": value},
	}
}

func supportedImageMediaType(value string) bool {
	switch normalizeImageMediaType(value) {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return true
	default:
		return false
	}
}

func normalizeImageMediaType(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "image/jpg" {
		return "image/jpeg"
	}
	return value
}

func validatePublicImageURL(raw string) (*url.URL, error) {
	parsed, err := url.ParseRequestURI(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return nil, fmt.Errorf("image URL must be a public HTTPS URL without credentials")
	}
	host := parsed.Hostname()
	if host == "" {
		return nil, fmt.Errorf("image URL must include a public host")
	}
	if address, err := netip.ParseAddr(host); err == nil && !isPublicAddress(address) {
		return nil, fmt.Errorf("image URL host is not public: address is private or reserved")
	}
	return parsed, nil
}

func safeImageHTTPClient(configured *http.Client) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           safeImageDialContext(dialer),
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
	}
	timeout := 30 * time.Second
	if configured != nil && configured.Timeout > 0 {
		timeout = configured.Timeout
	}
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func safeImageDialContext(dialer *net.Dialer) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		targets, err := publicImageDialTargets(ctx, address)
		if err != nil {
			return nil, err
		}
		var dialErrs []error
		for _, target := range targets {
			conn, err := dialer.DialContext(ctx, network, target)
			if err == nil {
				return conn, nil
			}
			dialErrs = append(dialErrs, err)
		}
		return nil, fmt.Errorf("dialing image target: %w", errors.Join(dialErrs...))
	}
}

func publicImageDialTargets(ctx context.Context, address string) ([]string, error) {
	return publicImageDialTargetsWithResolver(ctx, address, publicHostAddresses)
}

func publicImageDialTargetsWithResolver(
	ctx context.Context,
	address string,
	resolve func(context.Context, string) ([]netip.Addr, error),
) ([]string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("invalid image target")
	}
	addresses, err := resolve(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("image target is not public: %w", err)
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("image target is not public: host has no addresses")
	}
	targets := make([]string, 0, len(addresses))
	for _, address := range addresses {
		address = address.Unmap()
		if !isPublicAddress(address) {
			return nil, fmt.Errorf("image target is not public: address is private or reserved")
		}
		targets = append(targets, net.JoinHostPort(address.String(), port))
	}
	return targets, nil
}

func publicHostAddresses(ctx context.Context, host string) ([]netip.Addr, error) {
	if address, err := netip.ParseAddr(host); err == nil {
		address = address.Unmap()
		if !isPublicAddress(address) {
			return nil, fmt.Errorf("address is private or reserved")
		}
		return []netip.Addr{address}, nil
	}
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("resolving host")
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("host has no addresses")
	}
	publicAddresses := make([]netip.Addr, 0, len(addresses))
	for _, address := range addresses {
		address = address.Unmap()
		if !isPublicAddress(address) {
			return nil, fmt.Errorf("address is private or reserved")
		}
		publicAddresses = append(publicAddresses, address)
	}
	return publicAddresses, nil
}

func isPublicAddress(address netip.Addr) bool {
	address = address.Unmap()
	return address.IsValid() && address.IsGlobalUnicast() && !address.IsPrivate() && !address.IsLoopback() &&
		!address.IsLinkLocalUnicast() && !address.IsLinkLocalMulticast() && !address.IsUnspecified() &&
		!isReservedAddress(address)
}

func isReservedAddress(address netip.Addr) bool {
	for _, prefix := range [...]netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"),
		netip.MustParsePrefix("100.64.0.0/10"),
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
	} {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}
