package executor

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/hishamkaram/claude-code-router/internal/cua"
)

const (
	macOSProtocolVersion    = 1
	macOSMaxLineBytes       = 48 << 20
	macOSMaxScreenshotBytes = cua.MaxComputerScreenshotBytes
	macOSActionCount        = 9
)

type macOSProtocolMessage struct {
	Version     int
	ID          string
	Type        string
	Preview     bool
	Token       string
	Actions     []string
	Permissions macOSPermissions
	Action      string
	Result      macOSResult
	Error       *macOSFailure
}

type macOSPermissions struct {
	Accessibility   bool `json:"accessibility"`
	ScreenRecording bool `json:"screen_recording"`
}

type macOSResult struct {
	ContentType string
	Screenshot  []byte
	Status      string
	Width       int
	Height      int
}

type macOSFailure struct {
	Code        string   `json:"code"`
	Message     string   `json:"message"`
	Permissions []string `json:"permissions,omitempty"`
	PreviewOnly bool     `json:"preview_only"`
}

func decodeMacOSProtocolMessage(line []byte) (macOSProtocolMessage, error) {
	fields, err := decodeMacOSObject(line)
	if err != nil {
		return macOSProtocolMessage{}, err
	}
	messageType, err := requiredMacOSString(fields, "type")
	if err != nil {
		return macOSProtocolMessage{}, err
	}
	switch messageType {
	case "ready":
		return decodeMacOSReady(fields)
	case "started":
		return decodeMacOSStarted(fields)
	case "result":
		return decodeMacOSActionResult(fields)
	case "closed":
		return decodeMacOSClosed(fields)
	case "error":
		return decodeMacOSError(fields)
	default:
		return macOSProtocolMessage{}, fmt.Errorf("macOS CUA helper sent unsupported message type %q", messageType)
	}
}

func decodeMacOSReady(fields map[string]json.RawMessage) (macOSProtocolMessage, error) {
	if err := requireMacOSFields(fields, "version", "type", "preview", "token", "actions"); err != nil {
		return macOSProtocolMessage{}, err
	}
	message, err := decodeMacOSCommon(fields)
	if err != nil {
		return macOSProtocolMessage{}, err
	}
	if message.ID != "" {
		return macOSProtocolMessage{}, fmt.Errorf("macOS CUA ready message must not include an id")
	}
	token, err := requiredMacOSString(fields, "token")
	if err != nil {
		return macOSProtocolMessage{}, err
	}
	if token == "" || len(token) > 128 {
		return macOSProtocolMessage{}, fmt.Errorf("macOS CUA ready message contained an invalid token")
	}
	var actions []string
	if err := json.Unmarshal(fields["actions"], &actions); err != nil {
		return macOSProtocolMessage{}, fmt.Errorf("decoding macOS CUA ready actions: %w", err)
	}
	if err := validateMacOSActions(actions); err != nil {
		return macOSProtocolMessage{}, err
	}
	message.Token = token
	message.Actions = append([]string(nil), actions...)
	return message, nil
}

func decodeMacOSStarted(fields map[string]json.RawMessage) (macOSProtocolMessage, error) {
	if err := requireMacOSFields(fields, "version", "id", "type", "preview", "permissions"); err != nil {
		return macOSProtocolMessage{}, err
	}
	message, err := decodeMacOSCommon(fields)
	if err != nil {
		return macOSProtocolMessage{}, err
	}
	if validationErr := validateMacOSID(message.ID); validationErr != nil {
		return macOSProtocolMessage{}, validationErr
	}
	var permissions macOSPermissions
	if err := json.Unmarshal(fields["permissions"], &permissions); err != nil {
		return macOSProtocolMessage{}, fmt.Errorf("decoding macOS CUA permissions: %w", err)
	}
	if !permissions.Accessibility || !permissions.ScreenRecording {
		return macOSProtocolMessage{}, fmt.Errorf("macOS CUA started message did not confirm required permissions")
	}
	message.Permissions = permissions
	return message, nil
}

func decodeMacOSActionResult(fields map[string]json.RawMessage) (macOSProtocolMessage, error) {
	if err := requireMacOSFields(fields, "version", "id", "type", "preview", "action", "result"); err != nil {
		return macOSProtocolMessage{}, err
	}
	message, err := decodeMacOSCommon(fields)
	if err != nil {
		return macOSProtocolMessage{}, err
	}
	if validationErr := validateMacOSID(message.ID); validationErr != nil {
		return macOSProtocolMessage{}, validationErr
	}
	action, err := requiredMacOSString(fields, "action")
	if err != nil {
		return macOSProtocolMessage{}, err
	}
	if !isMacOSAllowedAction(action) {
		return macOSProtocolMessage{}, fmt.Errorf("macOS CUA result used unsupported action %q", action)
	}
	result, err := decodeMacOSResult(action, fields["result"])
	if err != nil {
		return macOSProtocolMessage{}, err
	}
	message.Action = action
	message.Result = result
	return message, nil
}

func decodeMacOSClosed(fields map[string]json.RawMessage) (macOSProtocolMessage, error) {
	if err := requireMacOSFields(fields, "version", "id", "type", "preview"); err != nil {
		return macOSProtocolMessage{}, err
	}
	message, err := decodeMacOSCommon(fields)
	if err != nil {
		return macOSProtocolMessage{}, err
	}
	if err := validateMacOSID(message.ID); err != nil {
		return macOSProtocolMessage{}, err
	}
	return message, nil
}

func decodeMacOSError(fields map[string]json.RawMessage) (macOSProtocolMessage, error) {
	if err := requireMacOSFields(fields, "version", "id", "type", "preview", "error"); err != nil {
		return macOSProtocolMessage{}, err
	}
	message, err := decodeMacOSCommon(fields)
	if err != nil {
		return macOSProtocolMessage{}, err
	}
	if message.ID != "" {
		if validationErr := validateMacOSID(message.ID); validationErr != nil {
			return macOSProtocolMessage{}, validationErr
		}
	}
	failure, err := decodeMacOSFailure(fields["error"])
	if err != nil {
		return macOSProtocolMessage{}, err
	}
	message.Error = &failure
	return message, nil
}

func decodeMacOSCommon(fields map[string]json.RawMessage) (macOSProtocolMessage, error) {
	version, err := requiredMacOSInt(fields, "version")
	if err != nil {
		return macOSProtocolMessage{}, err
	}
	if version != macOSProtocolVersion {
		return macOSProtocolMessage{}, fmt.Errorf("macOS CUA helper used unsupported protocol version %d", version)
	}
	messageType, err := requiredMacOSString(fields, "type")
	if err != nil {
		return macOSProtocolMessage{}, err
	}
	preview, err := requiredMacOSBool(fields, "preview")
	if err != nil {
		return macOSProtocolMessage{}, err
	}
	if !preview {
		return macOSProtocolMessage{}, fmt.Errorf("macOS CUA helper did not mark response as preview-only")
	}
	id := ""
	if raw, ok := fields["id"]; ok {
		if err := json.Unmarshal(raw, &id); err != nil {
			return macOSProtocolMessage{}, fmt.Errorf("decoding macOS CUA response id: %w", err)
		}
	}
	return macOSProtocolMessage{
		Version: version,
		ID:      id,
		Type:    messageType,
		Preview: preview,
	}, nil
}

func decodeMacOSResult(action string, raw json.RawMessage) (macOSResult, error) {
	fields, err := decodeMacOSObject(raw)
	if err != nil {
		return macOSResult{}, fmt.Errorf("decoding macOS CUA result: %w", err)
	}
	if action == "screenshot" {
		return decodeMacOSScreenshotResult(fields)
	}
	return decodeMacOSActionStatusResult(fields)
}

func decodeMacOSActionStatusResult(fields map[string]json.RawMessage) (macOSResult, error) {
	if requiredErr := requireMacOSFields(fields, "status"); requiredErr != nil {
		return macOSResult{}, requiredErr
	}
	status, err := requiredMacOSString(fields, "status")
	if err != nil {
		return macOSResult{}, err
	}
	if status != "ok" {
		return macOSResult{}, fmt.Errorf("macOS CUA action result status %q is not ok", status)
	}
	return macOSResult{Status: status}, nil
}

func decodeMacOSScreenshotResult(fields map[string]json.RawMessage) (macOSResult, error) {
	if requiredErr := requireMacOSFields(fields, "content_type", "data_base64", "width", "height"); requiredErr != nil {
		return macOSResult{}, requiredErr
	}
	contentType, err := requiredMacOSString(fields, "content_type")
	if err != nil {
		return macOSResult{}, err
	}
	if contentType != "image/png" {
		return macOSResult{}, fmt.Errorf("macOS CUA screenshot content type %q is not image/png", contentType)
	}
	encoded, err := requiredMacOSString(fields, "data_base64")
	if err != nil {
		return macOSResult{}, err
	}
	if base64.StdEncoding.DecodedLen(len(encoded)) > macOSMaxScreenshotBytes {
		return macOSResult{}, fmt.Errorf("macOS CUA screenshot exceeds the 32 MiB limit")
	}
	screenshot, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return macOSResult{}, fmt.Errorf("decoding macOS CUA screenshot: %w", err)
	}
	if len(screenshot) == 0 {
		return macOSResult{}, fmt.Errorf("macOS CUA screenshot was empty")
	}
	width, err := requiredMacOSInt(fields, "width")
	if err != nil {
		return macOSResult{}, err
	}
	height, err := requiredMacOSInt(fields, "height")
	if err != nil {
		return macOSResult{}, err
	}
	if width < 1 || height < 1 {
		return macOSResult{}, fmt.Errorf("macOS CUA screenshot dimensions must be positive")
	}
	return macOSResult{
		ContentType: contentType,
		Screenshot:  screenshot,
		Width:       width,
		Height:      height,
	}, nil
}

func decodeMacOSFailure(raw json.RawMessage) (macOSFailure, error) {
	fields, err := decodeMacOSObject(raw)
	if err != nil {
		return macOSFailure{}, fmt.Errorf("decoding macOS CUA error: %w", err)
	}
	if err := requireMacOSFieldsWithOptional(fields, []string{"code", "message", "preview_only"}, "permissions"); err != nil {
		return macOSFailure{}, err
	}
	var failure macOSFailure
	if err := json.Unmarshal(raw, &failure); err != nil {
		return macOSFailure{}, fmt.Errorf("decoding macOS CUA error payload: %w", err)
	}
	if strings.TrimSpace(failure.Code) == "" {
		return macOSFailure{}, fmt.Errorf("macOS CUA error code is required")
	}
	if strings.TrimSpace(failure.Message) == "" {
		return macOSFailure{}, fmt.Errorf("macOS CUA error message is required")
	}
	if !failure.PreviewOnly {
		return macOSFailure{}, fmt.Errorf("macOS CUA error was not marked preview-only")
	}
	if !isMacOSExpectedErrorCode(failure.Code) {
		return macOSFailure{}, fmt.Errorf("macOS CUA helper returned unknown error code %q", failure.Code)
	}
	for _, permission := range failure.Permissions {
		if permission != "accessibility" && permission != "screen_recording" {
			return macOSFailure{}, fmt.Errorf("macOS CUA helper returned unknown permission %q", permission)
		}
	}
	return failure, nil
}

func decodeMacOSObject(raw []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var fields map[string]json.RawMessage
	if err := decoder.Decode(&fields); err != nil {
		return nil, fmt.Errorf("decoding macOS CUA JSON object: %w", err)
	}
	var trailing struct{}
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("macOS CUA message must contain exactly one JSON object")
	}
	if fields == nil {
		return nil, fmt.Errorf("macOS CUA message must be a JSON object")
	}
	return fields, nil
}

func requireMacOSFields(fields map[string]json.RawMessage, allowed ...string) error {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		allowedSet[key] = struct{}{}
		if _, ok := fields[key]; !ok {
			return fmt.Errorf("macOS CUA message missing required field %q", key)
		}
	}
	for key := range fields {
		if _, ok := allowedSet[key]; !ok {
			return fmt.Errorf("macOS CUA message contains unsupported field %q", key)
		}
	}
	return nil
}

func requireMacOSFieldsWithOptional(fields map[string]json.RawMessage, required []string, optional ...string) error {
	allowedSet := make(map[string]struct{}, len(required)+len(optional))
	for _, key := range required {
		allowedSet[key] = struct{}{}
		if _, ok := fields[key]; !ok {
			return fmt.Errorf("macOS CUA message missing required field %q", key)
		}
	}
	for _, key := range optional {
		allowedSet[key] = struct{}{}
	}
	for key := range fields {
		if _, ok := allowedSet[key]; !ok {
			return fmt.Errorf("macOS CUA message contains unsupported field %q", key)
		}
	}
	return nil
}

func requiredMacOSString(fields map[string]json.RawMessage, key string) (string, error) {
	raw, ok := fields[key]
	if !ok {
		return "", fmt.Errorf("macOS CUA message missing required field %q", key)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("decoding macOS CUA field %q: %w", key, err)
	}
	return value, nil
}

func requiredMacOSInt(fields map[string]json.RawMessage, key string) (int, error) {
	raw, ok := fields[key]
	if !ok {
		return 0, fmt.Errorf("macOS CUA message missing required field %q", key)
	}
	var value int
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, fmt.Errorf("decoding macOS CUA field %q: %w", key, err)
	}
	return value, nil
}

func requiredMacOSBool(fields map[string]json.RawMessage, key string) (bool, error) {
	raw, ok := fields[key]
	if !ok {
		return false, fmt.Errorf("macOS CUA message missing required field %q", key)
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, fmt.Errorf("decoding macOS CUA field %q: %w", key, err)
	}
	return value, nil
}

func validateMacOSActions(actions []string) error {
	if len(actions) != macOSActionCount {
		return fmt.Errorf("macOS CUA ready message advertised an unexpected action set")
	}
	seen := make(map[string]struct{}, len(actions))
	for _, action := range actions {
		if !isMacOSAllowedAction(action) {
			return fmt.Errorf("macOS CUA ready message advertised unsupported action %q", action)
		}
		if _, exists := seen[action]; exists {
			return fmt.Errorf("macOS CUA ready message advertised duplicate action %q", action)
		}
		seen[action] = struct{}{}
	}
	return nil
}

func isMacOSAllowedAction(action string) bool {
	switch action {
	case "screenshot", "click", "double_click", "drag", "move", "type", "keypress", "scroll", "wait":
		return true
	default:
		return false
	}
}

func isMacOSExpectedErrorCode(code string) bool {
	switch code {
	case "invalid_json", "invalid_request", "unauthenticated", "not_started", "invalid_state",
		"permission_required", "unsupported_platform", "invalid_action", "action_failed":
		return true
	default:
		return false
	}
}

func validateMacOSID(id string) error {
	if id == "" || len(id) > 128 {
		return fmt.Errorf("macOS CUA response id must use 1 to 128 ASCII letters, digits, dots, underscores, or hyphens")
	}
	for _, char := range id {
		if isASCIILetter(char) || isASCIIDigit(char) || char == '.' || char == '_' || char == '-' {
			continue
		}
		return fmt.Errorf("macOS CUA response id must use 1 to 128 ASCII letters, digits, dots, underscores, or hyphens")
	}
	return nil
}
