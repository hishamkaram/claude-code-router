package executor

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/cua"
)

type macOSActionPayload struct {
	Kind         string      `json:"kind"`
	X            *int        `json:"x,omitempty"`
	Y            *int        `json:"y,omitempty"`
	From         *macOSPoint `json:"from,omitempty"`
	To           *macOSPoint `json:"to,omitempty"`
	Text         *string     `json:"text,omitempty"`
	Keys         *[]string   `json:"keys,omitempty"`
	DeltaX       *int        `json:"delta_x,omitempty"`
	DeltaY       *int        `json:"delta_y,omitempty"`
	Milliseconds *int        `json:"milliseconds,omitempty"`
}

type macOSPoint struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

func buildMacOSActionPayload(action cua.Action) (*macOSActionPayload, []string, error) {
	payload := &macOSActionPayload{Kind: string(action.Kind)}
	switch action.Kind {
	case cua.ActionScreenshot:
		return payload, nil, nil
	case cua.ActionClick, cua.ActionDoubleClick:
		return buildMacOSClickPayload(payload, action)
	case cua.ActionDrag:
		if err := rejectMacOSPointerModifiers(action); err != nil {
			return nil, nil, err
		}
		from, to, err := macOSDragEndpoints(action.Raw)
		if err != nil {
			return nil, nil, err
		}
		payload.From = &from
		payload.To = &to
		return payload, nil, nil
	case cua.ActionMove:
		if err := rejectMacOSPointerModifiers(action); err != nil {
			return nil, nil, err
		}
		payload.X = intPtr(action.X)
		payload.Y = intPtr(action.Y)
		return payload, nil, nil
	case cua.ActionType:
		text := action.Text
		payload.Text = &text
		return payload, []string{text}, nil
	case cua.ActionKeypress:
		keys := normalizeMacOSKeypressKeys(action.Keys)
		payload.Keys = &keys
		return payload, nil, nil
	case cua.ActionScroll:
		if err := rejectMacOSPointerModifiers(action); err != nil {
			return nil, nil, err
		}
		deltaX, deltaY, err := macOSScrollDeltas(action.Raw)
		if err != nil {
			return nil, nil, err
		}
		payload.X = intPtr(action.X)
		payload.Y = intPtr(action.Y)
		payload.DeltaX = &deltaX
		payload.DeltaY = &deltaY
		return payload, nil, nil
	case cua.ActionWait:
		milliseconds := macOSWaitMilliseconds(action.Raw)
		payload.Milliseconds = &milliseconds
		return payload, nil, nil
	default:
		return nil, nil, unsupportedAction(string(cua.ExecutorMacOSPreview), action.Kind, "action is not allowed by the macOS preview protocol")
	}
}

// normalizeMacOSKeypressKeys translates documented Responses key aliases to
// the canonical names accepted by the source-built macOS preview helper.
func normalizeMacOSKeypressKeys(keys []string) []string {
	normalized := make([]string, len(keys))
	for index, key := range keys {
		normalized[index] = normalizeMacOSKeypressKey(key)
	}
	return normalized
}

func normalizeMacOSKeypressKey(key string) string {
	normalized := normalizeKeyName(key)
	if canonical := normalizeMacOSModifierKey(normalized); canonical != "" {
		return canonical
	}
	if canonical := normalizeMacOSEditingKey(normalized); canonical != "" {
		return canonical
	}
	if canonical := normalizeMacOSNavigationKey(normalized); canonical != "" {
		return canonical
	}
	return strings.ToLower(strings.TrimSpace(key))
}

func normalizeMacOSModifierKey(key string) string {
	switch key {
	case "alt", "option":
		return "option"
	case "ctrl", "control":
		return "control"
	case "cmd", "command", "meta", "super":
		return "command"
	case "shift":
		return "shift"
	default:
		return ""
	}
}

func normalizeMacOSEditingKey(key string) string {
	switch key {
	case "enter", "return":
		return "return"
	case "esc", "escape":
		return "escape"
	case "tab":
		return "tab"
	case "space":
		return "space"
	case "backspace":
		return "delete"
	case "delete", "del", "forwarddelete":
		return "forward_delete"
	default:
		return ""
	}
}

func normalizeMacOSNavigationKey(key string) string {
	switch key {
	case "home":
		return "home"
	case "end":
		return "end"
	case "pageup":
		return "page_up"
	case "pagedown":
		return "page_down"
	default:
		return normalizeMacOSArrowKey(key)
	}
}

func normalizeMacOSArrowKey(key string) string {
	switch key {
	case "arrowleft", "left":
		return "left"
	case "arrowup", "up":
		return "up"
	case "arrowright", "right":
		return "right"
	case "arrowdown", "down":
		return "down"
	default:
		return ""
	}
}

func buildMacOSClickPayload(payload *macOSActionPayload, action cua.Action) (*macOSActionPayload, []string, error) {
	if err := rejectMacOSPointerModifiers(action); err != nil {
		return nil, nil, err
	}
	if err := rejectMacOSPointerButton(action); err != nil {
		return nil, nil, err
	}
	payload.X = intPtr(action.X)
	payload.Y = intPtr(action.Y)
	return payload, nil, nil
}

func rejectMacOSPointerModifiers(action cua.Action) error {
	if len(action.Keys) == 0 {
		return nil
	}
	return unsupportedAction(string(cua.ExecutorMacOSPreview), action.Kind, "pointer modifiers are not supported by the macOS preview protocol")
}

func rejectMacOSPointerButton(action cua.Action) error {
	if len(action.Raw) == 0 || string(action.Raw) == "null" {
		return nil
	}
	fields, err := macOSRawActionFields(action.Raw)
	if err != nil {
		return fmt.Errorf("decoding macOS CUA pointer action: %w", err)
	}
	rawButton, found := fields["button"]
	if !found {
		return nil
	}
	var button string
	if err := json.Unmarshal(rawButton, &button); err != nil || !strings.EqualFold(strings.TrimSpace(button), "left") {
		return unsupportedAction(string(cua.ExecutorMacOSPreview), action.Kind, "only the left mouse button is supported by the macOS preview protocol")
	}
	return nil
}

func macOSDragEndpoints(raw json.RawMessage) (from, to macOSPoint, err error) {
	fields, err := macOSRawActionFields(raw)
	if err != nil {
		return macOSPoint{}, macOSPoint{}, fmt.Errorf("macOS CUA drag action requires raw coordinates: %w", err)
	}
	if _, hasPath := fields["path"]; hasPath {
		return macOSPoint{}, macOSPoint{}, unsupportedAction(string(cua.ExecutorMacOSPreview), cua.ActionDrag, "path-based drags are not supported by the macOS preview protocol")
	}
	rawFrom, hasFrom := fields["from"]
	rawTo, hasTo := fields["to"]
	if hasFrom != hasTo {
		return macOSPoint{}, macOSPoint{}, fmt.Errorf("macOS CUA drag action requires both source and destination points")
	}
	if hasFrom {
		from, sourceErr := decodeMacOSPoint(rawFrom)
		if sourceErr != nil {
			return macOSPoint{}, macOSPoint{}, fmt.Errorf("decoding macOS CUA drag source: %w", sourceErr)
		}
		to, destinationErr := decodeMacOSPoint(rawTo)
		if destinationErr != nil {
			return macOSPoint{}, macOSPoint{}, fmt.Errorf("decoding macOS CUA drag destination: %w", destinationErr)
		}
		return from, to, nil
	}
	return macOSPoint{}, macOSPoint{}, fmt.Errorf("macOS CUA drag action requires from/to points")
}

func macOSScrollDeltas(raw json.RawMessage) (deltaX, deltaY int, err error) {
	fields, err := macOSRawActionFields(raw)
	if err != nil {
		return 0, 0, fmt.Errorf("macOS CUA scroll action requires raw deltas: %w", err)
	}
	for _, names := range [][2]string{{"scroll_x", "scroll_y"}, {"scrollX", "scrollY"}, {"delta_x", "delta_y"}, {"deltaX", "deltaY"}} {
		deltaX, okX, valueErr := macOSRawInt(fields, names[0])
		if valueErr != nil {
			return 0, 0, valueErr
		}
		deltaY, okY, valueErr := macOSRawInt(fields, names[1])
		if valueErr != nil {
			return 0, 0, valueErr
		}
		if okX && okY {
			return deltaX, deltaY, nil
		}
	}
	return 0, 0, fmt.Errorf("macOS CUA scroll action requires scroll deltas")
}

func macOSWaitMilliseconds(raw json.RawMessage) int {
	milliseconds := int(waitDuration(raw) / time.Millisecond)
	fields, err := macOSRawActionFields(raw)
	if err != nil {
		return milliseconds
	}
	for _, key := range []string{"milliseconds", "duration_ms", "timeout_ms", "ms"} {
		value, ok, err := macOSRawInt(fields, key)
		if err == nil && ok {
			return value
		}
	}
	return milliseconds
}

func macOSRawActionFields(raw json.RawMessage) (map[string]json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("raw action is empty")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("decoding raw action: %w", err)
	}
	if fields == nil {
		return nil, fmt.Errorf("raw action must be a JSON object")
	}
	return fields, nil
}

func decodeMacOSPoint(raw json.RawMessage) (macOSPoint, error) {
	var point macOSPoint
	if err := json.Unmarshal(raw, &point); err == nil {
		return point, nil
	}
	var tuple []float64
	if err := json.Unmarshal(raw, &tuple); err != nil {
		return macOSPoint{}, err
	}
	if len(tuple) < 2 {
		return macOSPoint{}, fmt.Errorf("point requires x and y")
	}
	return macOSPoint{X: tuple[0], Y: tuple[1]}, nil
}

func macOSRawInt(fields map[string]json.RawMessage, key string) (value int, found bool, err error) {
	raw, found := fields[key]
	if !found {
		return 0, false, nil
	}
	if err = json.Unmarshal(raw, &value); err != nil {
		return 0, true, fmt.Errorf("decoding macOS CUA action field %q: %w", key, err)
	}
	return value, true, nil
}

func intPtr(value int) *int {
	return &value
}
