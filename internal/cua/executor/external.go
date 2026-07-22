package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/hishamkaram/claude-code-router/internal/cua"
)

const maxExternalObservationBytes = 32 << 20

// ExternalOptions configures an external public-HTTPS CUA executor.
type ExternalOptions struct {
	BaseURL     string
	BearerToken string
	Resolver    NetIPResolver
}

// ExternalHTTP forwards validated actions to a named external HTTPS executor.
type ExternalHTTP struct {
	name        string
	baseURL     *url.URL
	bearerToken string
	httpClient  *http.Client
	resolver    NetIPResolver
}

func NewExternalHTTP(ctx context.Context, name string, opts ExternalOptions) (*ExternalHTTP, error) {
	if err := validateExternalName(strings.TrimSpace(name)); err != nil {
		return nil, err
	}
	bearerToken := strings.TrimSpace(opts.BearerToken)
	if bearerToken == "" {
		return nil, fmt.Errorf("external CUA executor bearer token is required")
	}
	resolver := opts.Resolver
	if resolver == nil {
		resolver = defaultNetIPResolver{}
	}
	baseURL, err := ValidateExternalBaseURL(ctx, opts.BaseURL, resolver)
	if err != nil {
		return nil, err
	}
	client := SafeExternalHTTPClient(resolver, 0)
	return &ExternalHTTP{
		name:        strings.TrimSpace(name),
		baseURL:     baseURL,
		bearerToken: bearerToken,
		httpClient:  client,
		resolver:    resolver,
	}, nil
}

func (executor *ExternalHTTP) Name() string {
	if executor == nil {
		return string(cua.ExecutorExternal)
	}
	return string(cua.ExecutorExternal) + ":" + executor.name
}

func (executor *ExternalHTTP) Check(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("external CUA health check context is required")
	}
	endpoint, err := executor.endpoint(ctx, "health")
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return fmt.Errorf("creating external CUA health request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	executor.authorize(req)
	resp, err := executor.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("requesting external CUA health endpoint: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDiscardResponseBytes))
		_ = resp.Body.Close()
	}()
	if isRedirectStatus(resp.StatusCode) {
		return fmt.Errorf("external CUA health endpoint redirects are not allowed")
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("external CUA health endpoint returned HTTP %d", resp.StatusCode)
	}
	return nil
}

func (executor *ExternalHTTP) Execute(ctx context.Context, action cua.Action) (cua.Observation, error) {
	if ctx == nil {
		return cua.Observation{}, fmt.Errorf("external CUA action context is required")
	}
	if err := action.Validate(); err != nil {
		return cua.Observation{}, err
	}
	endpoint, err := executor.endpoint(ctx, "actions")
	if err != nil {
		return cua.Observation{}, err
	}
	request, err := newExternalActionRequest(action)
	if err != nil {
		return cua.Observation{}, err
	}
	body, err := json.Marshal(request)
	if err != nil {
		return cua.Observation{}, fmt.Errorf("encoding external CUA action: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return cua.Observation{}, fmt.Errorf("creating external CUA action request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	executor.authorize(req)
	resp, err := executor.httpClient.Do(req)
	if err != nil {
		return cua.Observation{}, fmt.Errorf("requesting external CUA action endpoint: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDiscardResponseBytes))
		_ = resp.Body.Close()
	}()
	if isRedirectStatus(resp.StatusCode) {
		return cua.Observation{}, fmt.Errorf("external CUA action endpoint redirects are not allowed")
	}
	if resp.StatusCode == http.StatusNotImplemented {
		return cua.Observation{}, unsupportedAction(executor.Name(), action.Kind, "external executor returned HTTP 501")
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return cua.Observation{}, fmt.Errorf("external CUA action endpoint returned HTTP %d", resp.StatusCode)
	}
	if contentErr := requireJSONContent(resp.Header.Get("Content-Type")); contentErr != nil {
		return cua.Observation{}, contentErr
	}
	observation, err := decodeExternalObservation(resp.Body)
	if err != nil {
		return cua.Observation{}, err
	}
	return observation, nil
}

func (executor *ExternalHTTP) authorize(req *http.Request) {
	if executor == nil || req == nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+executor.bearerToken)
}

func (executor *ExternalHTTP) Close() error {
	return nil
}

func (executor *ExternalHTTP) BaseURL() string {
	if executor == nil || executor.baseURL == nil {
		return ""
	}
	return executor.baseURL.String()
}

func (executor *ExternalHTTP) endpoint(ctx context.Context, path string) (string, error) {
	if executor == nil || executor.baseURL == nil {
		return "", fmt.Errorf("external CUA executor is not configured")
	}
	next := *executor.baseURL
	next.Path = joinURLPath(next.Path, path)
	next.RawQuery = ""
	next.Fragment = ""
	parsed, err := ValidatePublicHTTPSURL(ctx, next.String(), executor.resolver)
	if err != nil {
		return "", err
	}
	return parsed.String(), nil
}

type externalActionRequest struct {
	Action externalActionPayload `json:"action"`
}

type externalActionPayload map[string]json.RawMessage

func newExternalActionRequest(action cua.Action) (externalActionRequest, error) {
	payload, err := externalActionPayloadFromAction(action)
	if err != nil {
		return externalActionRequest{}, err
	}
	return externalActionRequest{Action: payload}, nil
}

func externalActionPayloadFromAction(action cua.Action) (externalActionPayload, error) {
	payload := make(externalActionPayload)
	if len(action.Raw) != 0 {
		if err := json.Unmarshal(action.Raw, &payload); err != nil || payload == nil {
			return nil, fmt.Errorf("encoding external CUA action: raw action must be a JSON object")
		}
	}
	if err := payload.set("call_id", action.CallID); err != nil {
		return nil, err
	}
	if err := payload.set("type", string(action.Kind)); err != nil {
		return nil, err
	}
	if len(action.Raw) != 0 {
		return payload, nil
	}
	return externalActionPayloadFromFields(payload, action)
}

func externalActionPayloadFromFields(payload externalActionPayload, action cua.Action) (externalActionPayload, error) {
	switch action.Kind {
	case cua.ActionScreenshot, cua.ActionWait:
		return payload, nil
	case cua.ActionClick, cua.ActionDoubleClick, cua.ActionMove, cua.ActionScroll:
		if err := payload.set("x", action.X); err != nil {
			return nil, err
		}
		if err := payload.set("y", action.Y); err != nil {
			return nil, err
		}
	case cua.ActionType:
		if err := payload.set("text", action.Text); err != nil {
			return nil, err
		}
	case cua.ActionKeypress, cua.ActionDrag:
		if len(action.Keys) > 0 {
			if err := payload.set("keys", action.Keys); err != nil {
				return nil, err
			}
		}
	}
	return payload, nil
}

func (payload externalActionPayload) set(name string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encoding external CUA action field %q: %w", name, err)
	}
	payload[name] = raw
	return nil
}

type externalObservationResponse struct {
	Screenshot  []byte          `json:"screenshot,omitempty"`
	ContentType string          `json:"content_type,omitempty"`
	Text        string          `json:"text,omitempty"`
	Raw         json.RawMessage `json:"raw,omitempty"`
}

func decodeExternalObservation(body io.Reader) (cua.Observation, error) {
	limited := io.LimitReader(body, maxExternalObservationBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return cua.Observation{}, fmt.Errorf("reading external CUA observation: %w", err)
	}
	if len(data) > maxExternalObservationBytes {
		return cua.Observation{}, fmt.Errorf("external CUA observation exceeds the 32 MiB limit")
	}
	var response externalObservationResponse
	if err := json.Unmarshal(data, &response); err != nil {
		return cua.Observation{}, fmt.Errorf("decoding external CUA observation: %w", err)
	}
	return cua.Observation{
		Screenshot:  response.Screenshot,
		ContentType: response.ContentType,
		Text:        response.Text,
		Raw:         response.Raw,
	}, nil
}

func requireJSONContent(value string) error {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return fmt.Errorf("external CUA observation content type is required")
	}
	if mediaType != "application/json" {
		return fmt.Errorf("external CUA observation content type must be application/json")
	}
	return nil
}

func joinURLPath(basePath, child string) string {
	basePath = strings.TrimRight(basePath, "/")
	child = strings.TrimLeft(child, "/")
	if basePath == "" {
		return "/" + child
	}
	if child == "" {
		return basePath
	}
	return basePath + "/" + child
}

func isRedirectStatus(status int) bool {
	return status >= http.StatusMultipleChoices && status < http.StatusBadRequest
}

func IsUnsupportedAction(err error) bool {
	return errors.Is(err, ErrUnsupportedAction)
}
