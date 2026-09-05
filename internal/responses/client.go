package responses

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
)

const (
	defaultBaseURL          = "https://api.openai.com"
	defaultMaxErrorBytes    = int64(4096)
	defaultMaxResponseBytes = int64(32 << 20)
)

// ClientOptions configures Client.
type ClientOptions struct {
	BaseURL          string
	APIKey           string
	HTTPClient       *http.Client
	MaxErrorBytes    int64
	MaxResponseBytes int64
}

// Client posts typed Requests to POST /v1/responses.
type Client struct {
	endpoint         string
	apiKey           string
	httpClient       *http.Client
	maxErrorBytes    int64
	maxResponseBytes int64
}

// HTTPError is returned for non-2xx Responses API status codes. Body is bounded
// by ClientOptions.MaxErrorBytes and has the configured API key redacted.
type HTTPError struct {
	StatusCode int
	Status     string
	Body       string
	Truncated  bool
}

func (e *HTTPError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("responses API status %d", e.StatusCode)
	}
	if e.Truncated {
		return fmt.Sprintf("responses API status %d: %s... [truncated]", e.StatusCode, e.Body)
	}
	return fmt.Sprintf("responses API status %d: %s", e.StatusCode, e.Body)
}

// NewClient creates a small Responses API client. BaseURL defaults to
// https://api.openai.com; the client always posts to /v1/responses under it.
func NewClient(options ClientOptions) (*Client, error) {
	baseURL := strings.TrimSpace(options.BaseURL)
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse Responses base URL: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("responses base URL must be absolute")
	}
	basePath := strings.TrimRight(parsed.Path, "/")
	if pathEndsWithVersion(basePath) {
		parsed.Path = basePath + "/responses"
	} else {
		parsed.Path = basePath + "/v1/responses"
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	maxErrorBytes := options.MaxErrorBytes
	if maxErrorBytes <= 0 {
		maxErrorBytes = defaultMaxErrorBytes
	}
	maxResponseBytes := options.MaxResponseBytes
	if maxResponseBytes <= 0 {
		maxResponseBytes = defaultMaxResponseBytes
	}
	httpClient := options.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{
		endpoint:         parsed.String(),
		apiKey:           strings.TrimSpace(options.APIKey),
		httpClient:       httpClient,
		maxErrorBytes:    maxErrorBytes,
		maxResponseBytes: maxResponseBytes,
	}, nil
}

func pathEndsWithVersion(path string) bool {
	lastSlash := strings.LastIndex(path, "/")
	last := path[lastSlash+1:]
	if len(last) < 2 || last[0] != 'v' {
		return false
	}
	for _, char := range last[1:] {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

// Create posts request to /v1/responses and decodes the non-stream response.
func (c *Client) Create(ctx context.Context, request *Request) (*Response, error) {
	if c == nil {
		return nil, fmt.Errorf("responses client is nil")
	}
	if request == nil {
		return nil, fmt.Errorf("responses request is nil")
	}
	resp, err := c.post(ctx, request, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, c.errorFromResponse(resp)
	}
	data, err := readResponseBody(resp.Body, c.maxResponseBytes)
	if err != nil {
		return nil, err
	}
	var decoded Response
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, fmt.Errorf("%w: decode Responses response: %w", ErrMalformedProviderOutput, err)
	}
	return &decoded, nil
}

// Stream owns an HTTP Responses event stream. Callers must Close it after the
// terminal event or when their request context is canceled.
type Stream struct {
	response *http.Response
	scanner  *sseScanner
}

// StartStream posts a streaming Responses request and exposes its SSE payloads
// one at a time. It rejects a successful JSON response because treating it as a
// stream would silently restore buffered behavior.
func (c *Client) StartStream(ctx context.Context, request *Request) (*Stream, error) {
	if c == nil {
		return nil, fmt.Errorf("responses client is nil")
	}
	if request == nil {
		return nil, fmt.Errorf("responses request is nil")
	}
	resp, err := c.post(ctx, request, true)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		defer func() { _ = resp.Body.Close() }()
		return nil, c.errorFromResponse(resp)
	}
	mediaType, _, parseErr := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if parseErr != nil || !strings.EqualFold(mediaType, "text/event-stream") {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%w: Responses provider did not honor the streaming request", ErrMalformedProviderOutput)
	}
	return &Stream{
		response: resp,
		scanner:  newSSEScanner(resp.Body, c.maxResponseBytes),
	}, nil
}

func (c *Client) post(ctx context.Context, request *Request, stream bool) (*http.Response, error) {
	payloadRequest := *request
	payloadRequest.Stream = stream
	payload, err := json.Marshal(&payloadRequest)
	if err != nil {
		return nil, fmt.Errorf("encode Responses request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build Responses request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	} else {
		httpReq.Header.Set("Accept", "application/json")
	}
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("post Responses request: %w", err)
	}
	return resp, nil
}

func (s *Stream) StatusCode() int {
	if s == nil || s.response == nil {
		return 0
	}
	return s.response.StatusCode
}

func (s *Stream) Header() http.Header {
	if s == nil || s.response == nil {
		return nil
	}
	return s.response.Header.Clone()
}

// Next returns the next decoded SSE data payload. A false ok result means the
// provider closed the stream; callers still need to verify a terminal event.
func (s *Stream) Next() (data []byte, ok bool, err error) {
	if s == nil || s.scanner == nil {
		return nil, false, fmt.Errorf("responses stream is nil")
	}
	return s.scanner.next()
}

func (s *Stream) Close() error {
	if s == nil || s.response == nil || s.response.Body == nil {
		return nil
	}
	return s.response.Body.Close()
}

type sseScanner struct {
	scanner   *bufio.Scanner
	limited   *io.LimitedReader
	eventData bytes.Buffer
	maxBytes  int64
	finished  bool
}

func newSSEScanner(body io.Reader, maxBytes int64) *sseScanner {
	limited := &io.LimitedReader{R: body, N: maxBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 64<<10), int(maxBytes+1))
	return &sseScanner{scanner: scanner, limited: limited, maxBytes: maxBytes}
}

func (s *sseScanner) next() (data []byte, ok bool, err error) {
	if s.finished {
		return nil, false, nil
	}
	for s.scanner.Scan() {
		line := bytes.TrimSuffix(s.scanner.Bytes(), []byte("\r"))
		if len(line) == 0 {
			if data := s.takeData(); len(data) > 0 {
				return data, true, nil
			}
			continue
		}
		if line[0] == ':' || !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		part := bytes.TrimPrefix(line, []byte("data:"))
		if len(part) > 0 && part[0] == ' ' {
			part = part[1:]
		}
		if s.eventData.Len() > 0 {
			s.eventData.WriteByte('\n')
		}
		_, _ = s.eventData.Write(part)
	}
	s.finished = true
	if s.limited.N <= 0 {
		return nil, false, fmt.Errorf("%w: Responses stream exceeds the %d byte limit", ErrMalformedProviderOutput, s.maxBytes)
	}
	if err := s.scanner.Err(); err != nil {
		return nil, false, fmt.Errorf("%w: read Responses stream: %w", ErrMalformedProviderOutput, err)
	}
	if data := s.takeData(); len(data) > 0 {
		return data, true, nil
	}
	return nil, false, nil
}

func (s *sseScanner) takeData() []byte {
	data := bytes.Clone(bytes.TrimSpace(s.eventData.Bytes()))
	s.eventData.Reset()
	return data
}

func readResponseBody(body io.Reader, maxBytes int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read Responses response: %w", ErrMalformedProviderOutput, err)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%w: Responses response exceeds the %d byte limit", ErrMalformedProviderOutput, maxBytes)
	}
	return data, nil
}

func (c *Client) errorFromResponse(resp *http.Response) error {
	// Read enough past the reporting limit to redact an API key that crosses it.
	limit := c.maxErrorBytes + int64(len(c.apiKey)) + 1
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return fmt.Errorf("read Responses error body: %w", err)
	}
	truncated := int64(len(data)) > c.maxErrorBytes
	body := string(data)
	if c.apiKey != "" {
		body = strings.ReplaceAll(body, c.apiKey, "[redacted]")
	}
	body = strings.TrimSpace(body)
	if int64(len(body)) > c.maxErrorBytes {
		body = body[:c.maxErrorBytes]
		truncated = true
	}
	return &HTTPError{
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		Body:       body,
		Truncated:  truncated,
	}
}
