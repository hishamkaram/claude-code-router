package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
)

const (
	defaultBrowserActionTimeout = 30 * time.Second
	maxCDPVersionBytes          = 1 << 20
	maxCDPMessageBytes          = 64 << 20
)

type cdpClient struct {
	conn   *websocket.Conn
	nextID int
}

func newCDPClient(ctx context.Context, client *http.Client, debugEndpoint string) (*cdpClient, error) {
	if client == nil {
		client = browserHTTPClient(nil)
	}
	websocketURL, err := browserDebuggerWebSocketURL(ctx, client, debugEndpoint)
	if err != nil {
		return nil, err
	}
	conn, response, err := websocket.Dial(ctx, websocketURL, &websocket.DialOptions{
		HTTPClient:      client,
		CompressionMode: websocket.CompressionDisabled,
	})
	if response != nil && response.Body != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxDiscardResponseBytes))
		_ = response.Body.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("connecting browser CDP websocket: %w", err)
	}
	conn.SetReadLimit(maxCDPMessageBytes)
	return &cdpClient{conn: conn}, nil
}

func (client *cdpClient) close() error {
	if client == nil || client.conn == nil {
		return nil
	}
	return client.conn.Close(websocket.StatusNormalClosure, "browser action complete")
}

func (client *cdpClient) call(ctx context.Context, method string, params any, sessionID string, result any) error {
	client.nextID++
	request := cdpRequest{ID: client.nextID, Method: method, Params: params, SessionID: sessionID}
	encoded, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("encoding browser CDP request %s: %w", method, err)
	}
	if err := client.conn.Write(ctx, websocket.MessageText, encoded); err != nil {
		return fmt.Errorf("writing browser CDP request %s: %w", method, err)
	}
	return client.readResponse(ctx, request.ID, method, result)
}

func (client *cdpClient) readResponse(ctx context.Context, requestID int, method string, result any) error {
	for {
		messageType, data, err := client.conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("reading browser CDP response %s: %w", method, err)
		}
		if messageType != websocket.MessageText {
			return fmt.Errorf("browser CDP response %s was not a text message", method)
		}
		var response cdpResponse
		if err := json.Unmarshal(data, &response); err != nil {
			return fmt.Errorf("decoding browser CDP response %s: %w", method, err)
		}
		if response.ID != requestID {
			continue
		}
		if response.Error != nil {
			return fmt.Errorf("browser CDP request %s failed: %s", method, response.Error.safeSummary())
		}
		if result != nil && len(response.Result) != 0 {
			if err := json.Unmarshal(response.Result, result); err != nil {
				return fmt.Errorf("decoding browser CDP result %s: %w", method, err)
			}
		}
		return nil
	}
}

func (client *cdpClient) attachPage(ctx context.Context) (string, error) {
	targetID, err := client.pageTargetID(ctx)
	if err != nil {
		return "", err
	}
	var result cdpAttachResult
	if err := client.call(ctx, "Target.attachToTarget", map[string]any{
		"targetId": targetID,
		"flatten":  true,
	}, "", &result); err != nil {
		return "", err
	}
	if result.SessionID == "" {
		return "", fmt.Errorf("browser CDP attach returned no session id")
	}
	if err := client.call(ctx, "Page.bringToFront", nil, result.SessionID, nil); err != nil {
		return "", err
	}
	return result.SessionID, nil
}

func (client *cdpClient) pageTargetID(ctx context.Context) (string, error) {
	var targets cdpTargetsResult
	if err := client.call(ctx, "Target.getTargets", nil, "", &targets); err != nil {
		return "", err
	}
	for _, target := range targets.TargetInfos {
		if target.Type == "page" && target.TargetID != "" {
			return target.TargetID, nil
		}
	}
	var created cdpCreateTargetResult
	if err := client.call(ctx, "Target.createTarget", map[string]any{"url": "about:blank"}, "", &created); err != nil {
		return "", err
	}
	if created.TargetID == "" {
		return "", fmt.Errorf("browser CDP create target returned no target id")
	}
	return created.TargetID, nil
}

type cdpRequest struct {
	ID        int    `json:"id"`
	Method    string `json:"method"`
	Params    any    `json:"params,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
}

type cdpResponse struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *cdpError       `json:"error,omitempty"`
}

type cdpError struct {
	Code int `json:"code"`
}

func (err cdpError) safeSummary() string {
	if err.Code == 0 {
		return "error response"
	}
	return "code " + strconv.Itoa(err.Code)
}

type cdpTargetsResult struct {
	TargetInfos []cdpTargetInfo `json:"targetInfos"`
}

type cdpTargetInfo struct {
	TargetID string `json:"targetId"`
	Type     string `json:"type"`
}

type cdpAttachResult struct {
	SessionID string `json:"sessionId"`
}

type cdpCreateTargetResult struct {
	TargetID string `json:"targetId"`
}

func browserDebuggerWebSocketURL(ctx context.Context, client *http.Client, debugEndpoint string) (string, error) {
	endpoint, err := validateCDPLoopbackHTTPEndpoint(debugEndpoint)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), http.NoBody)
	if err != nil {
		return "", fmt.Errorf("creating browser CDP version request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("requesting browser CDP version endpoint: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDiscardResponseBytes))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("browser CDP version endpoint returned HTTP %d", resp.StatusCode)
	}
	var version cdpVersionResponse
	limited := io.LimitReader(resp.Body, maxCDPVersionBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return "", fmt.Errorf("reading browser CDP version response: %w", err)
	}
	if len(data) > maxCDPVersionBytes {
		return "", fmt.Errorf("browser CDP version response exceeds the 1 MiB limit")
	}
	if decodeErr := json.Unmarshal(data, &version); decodeErr != nil {
		return "", fmt.Errorf("decoding browser CDP version response: %w", decodeErr)
	}
	websocketURL, err := validateCDPLoopbackWebSocket(version.WebSocketDebuggerURL, endpoint)
	if err != nil {
		return "", err
	}
	return websocketURL.String(), nil
}

type cdpVersionResponse struct {
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

func validateCDPLoopbackHTTPEndpoint(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("browser CDP endpoint is invalid: %w", err)
	}
	if parsed.Scheme != "http" || parsed.User != nil || parsed.Hostname() == "" {
		return nil, fmt.Errorf("browser CDP endpoint must be an absolute loopback HTTP URL")
	}
	if err := validateLoopbackHostPort(parsed.Hostname(), parsed.Port()); err != nil {
		return nil, fmt.Errorf("browser CDP endpoint is not loopback: %w", err)
	}
	return parsed, nil
}

func validateCDPLoopbackWebSocket(raw string, endpoint *url.URL) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("browser CDP websocket URL is invalid: %w", err)
	}
	if parsed.Scheme != "ws" && parsed.Scheme != "wss" {
		return nil, fmt.Errorf("browser CDP websocket URL must use ws or wss")
	}
	if parsed.User != nil || parsed.Hostname() == "" {
		return nil, fmt.Errorf("browser CDP websocket URL must not include credentials")
	}
	if err := validateLoopbackHostPort(parsed.Hostname(), parsed.Port()); err != nil {
		return nil, fmt.Errorf("browser CDP websocket target is not loopback: %w", err)
	}
	if endpoint != nil && parsed.Port() != endpoint.Port() {
		return nil, fmt.Errorf("browser CDP websocket target must use the debug endpoint port")
	}
	return parsed, nil
}

func validateLoopbackHostPort(host, port string) error {
	if !isLoopbackHost(host) {
		return fmt.Errorf("host must be loopback")
	}
	if _, err := parseTCPPort(port); err != nil {
		return err
	}
	return nil
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "localhost" {
		return true
	}
	if parsed, err := netip.ParseAddr(host); err == nil {
		return parsed.IsLoopback()
	}
	return false
}

func parseTCPPort(port string) (int, error) {
	if strings.TrimSpace(port) == "" {
		return 0, fmt.Errorf("port is required")
	}
	parsed, err := strconv.Atoi(port)
	if err != nil || parsed < 1 || parsed > 65535 {
		return 0, fmt.Errorf("port must be between 1 and 65535")
	}
	return parsed, nil
}
