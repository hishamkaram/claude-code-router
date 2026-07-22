package cua

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	maxComputerActionCoordinate       = 100_000
	maxComputerActionTextBytes        = 16 << 10
	maxComputerActionKeys             = 16
	maxComputerActionKeyBytes         = 128
	maxComputerActionMouseModifiers   = 4
	maxComputerActionDragPoints       = 128
	maxComputerActionScrollDelta      = 100_000
	maxComputerActionWaitMilliseconds = 30_000
)

func responseActionFields(raw json.RawMessage) (map[string]json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("computer-use action is empty")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, fmt.Errorf("computer-use action must be a JSON object")
	}
	return fields, nil
}

func responseActionKind(fields map[string]json.RawMessage) (ActionKind, error) {
	typeValue, err := requiredActionString(fields, "type")
	if err != nil {
		return "", err
	}
	kind := ActionKind(strings.ToLower(strings.TrimSpace(typeValue)))
	switch kind {
	case ActionScreenshot, ActionClick, ActionDoubleClick, ActionDrag, ActionMove, ActionType, ActionKeypress, ActionScroll, ActionWait:
		return kind, nil
	default:
		return "", fmt.Errorf("unsupported computer-use action %q", kind)
	}
}

func populateResponseAction(action *Action, fields map[string]json.RawMessage) error {
	switch action.Kind {
	case ActionScreenshot:
		return nil
	case ActionClick, ActionDoubleClick, ActionMove:
		return populateResponsePointerAction(action, fields)
	case ActionDrag:
		if err := validateResponseActionDrag(fields); err != nil {
			return err
		}
		return populateResponseActionMouseKeys(action, fields)
	case ActionType:
		text, err := requiredActionText(fields, "text")
		if err != nil {
			return err
		}
		if len(text) > maxComputerActionTextBytes {
			return fmt.Errorf("computer-use type action text exceeds %d bytes", maxComputerActionTextBytes)
		}
		action.Text = text
		return nil
	case ActionKeypress:
		keys, err := responseActionKeys(fields)
		if err != nil {
			return err
		}
		action.Keys = keys
		return nil
	case ActionScroll:
		return populateResponseScrollAction(action, fields)
	case ActionWait:
		return validateResponseActionWait(fields)
	default:
		return fmt.Errorf("unsupported computer-use action %q", action.Kind)
	}
}

func populateResponsePointerAction(action *Action, fields map[string]json.RawMessage) error {
	x, y, err := responseActionCoordinates(fields)
	if err != nil {
		return err
	}
	action.X, action.Y = x, y
	if err := validateResponseActionButton(action.Kind, fields); err != nil {
		return err
	}
	return populateResponseActionMouseKeys(action, fields)
}

func populateResponseScrollAction(action *Action, fields map[string]json.RawMessage) error {
	x, y, err := responseActionCoordinates(fields)
	if err != nil {
		return err
	}
	if err := validateResponseActionScroll(fields); err != nil {
		return err
	}
	action.X, action.Y = x, y
	return populateResponseActionMouseKeys(action, fields)
}

func populateResponseActionMouseKeys(action *Action, fields map[string]json.RawMessage) error {
	keys, err := responseActionMouseKeys(fields)
	if err != nil {
		return err
	}
	action.Keys = keys
	return nil
}

func requiredActionString(fields map[string]json.RawMessage, name string) (string, error) {
	raw, found := fields[name]
	if !found {
		return "", fmt.Errorf("computer-use action is missing %s", name)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("computer-use action has invalid %s", name)
	}
	return value, nil
}

func requiredActionText(fields map[string]json.RawMessage, name string) (string, error) {
	raw, found := fields[name]
	if !found {
		return "", fmt.Errorf("computer-use action is missing %s", name)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || value == "" {
		return "", fmt.Errorf("computer-use action has invalid %s", name)
	}
	return value, nil
}

func responseActionCoordinates(fields map[string]json.RawMessage) (x, y int, err error) {
	x, err = requiredActionCoordinate(fields, "x")
	if err != nil {
		return
	}
	y, err = requiredActionCoordinate(fields, "y")
	return
}

func requiredActionCoordinate(fields map[string]json.RawMessage, name string) (int, error) {
	raw, found := fields[name]
	if !found {
		return 0, fmt.Errorf("computer-use action is missing %s coordinate", name)
	}
	return decodeActionCoordinate(raw)
}

func decodeActionCoordinate(raw json.RawMessage) (int, error) {
	var value int
	if err := json.Unmarshal(raw, &value); err != nil || value < 0 || value > maxComputerActionCoordinate {
		return 0, fmt.Errorf("computer-use action coordinate is outside the allowed range")
	}
	return value, nil
}

func validateResponseActionButton(kind ActionKind, fields map[string]json.RawMessage) error {
	raw, found := fields["button"]
	if !found {
		return nil
	}
	var button string
	if err := json.Unmarshal(raw, &button); err != nil {
		return fmt.Errorf("computer-use action button is invalid")
	}
	button = strings.ToLower(strings.TrimSpace(button))
	if button != "left" && button != "right" && button != "middle" {
		return fmt.Errorf("computer-use action button is unsupported")
	}
	if kind == ActionDoubleClick && button != "left" {
		return fmt.Errorf("computer-use double_click action requires the left button")
	}
	return nil
}

func responseActionMouseKeys(fields map[string]json.RawMessage) ([]string, error) {
	raw, found := fields["keys"]
	if !found {
		return nil, nil
	}
	var keys []string
	if err := json.Unmarshal(raw, &keys); err != nil {
		return nil, fmt.Errorf("computer-use mouse action keys are invalid")
	}
	return validateResponseActionMouseKeys(keys)
}

func validateResponseActionMouseKeys(keys []string) ([]string, error) {
	if len(keys) > maxComputerActionMouseModifiers {
		return nil, fmt.Errorf("computer-use mouse action supports at most %d modifiers", maxComputerActionMouseModifiers)
	}
	validated := make([]string, 0, len(keys))
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		key = strings.TrimSpace(key)
		canonical, ok := canonicalMouseModifier(key)
		if !ok {
			return nil, fmt.Errorf("computer-use mouse action modifier %q is unsupported", key)
		}
		if _, duplicate := seen[canonical]; duplicate {
			return nil, fmt.Errorf("computer-use mouse action repeats modifier %q", key)
		}
		seen[canonical] = struct{}{}
		validated = append(validated, key)
	}
	return validated, nil
}

func canonicalMouseModifier(value string) (string, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.NewReplacer("_", "", "-", "", " ", "").Replace(value)
	switch value {
	case "alt", "option":
		return "alt", true
	case "ctrl", "control":
		return "control", true
	case "cmd", "command", "meta", "super":
		return "meta", true
	case "shift":
		return "shift", true
	default:
		return "", false
	}
}

func validateResponseActionDrag(fields map[string]json.RawMessage) error {
	path, hasPath := fields["path"]
	from, hasFrom := fields["from"]
	to, hasTo := fields["to"]
	if hasPath && (hasFrom || hasTo) {
		return fmt.Errorf("computer-use drag action must use path or from/to, not both")
	}
	if hasPath {
		return validateResponseActionPath(path)
	}
	if hasFrom != hasTo {
		return fmt.Errorf("computer-use drag action requires both from and to points")
	}
	if hasFrom {
		if err := validateResponseActionPoint(from); err != nil {
			return err
		}
		return validateResponseActionPoint(to)
	}
	return fmt.Errorf("computer-use drag action requires a path or from/to points")
}

func validateResponseActionPath(raw json.RawMessage) error {
	var points []json.RawMessage
	if err := json.Unmarshal(raw, &points); err != nil || len(points) < 2 || len(points) > maxComputerActionDragPoints {
		return fmt.Errorf("computer-use drag action requires 2 to %d path points", maxComputerActionDragPoints)
	}
	for _, point := range points {
		if err := validateResponseActionPoint(point); err != nil {
			return err
		}
	}
	return nil
}

func validateResponseActionPoint(raw json.RawMessage) error {
	var object map[string]json.RawMessage
	if decodeErr := json.Unmarshal(raw, &object); decodeErr == nil && object != nil {
		_, _, err := responseActionCoordinates(object)
		return err
	}
	var array []json.RawMessage
	if decodeErr := json.Unmarshal(raw, &array); decodeErr != nil || len(array) != 2 {
		return fmt.Errorf("computer-use drag path point must include x and y")
	}
	if _, err := decodeActionCoordinate(array[0]); err != nil {
		return err
	}
	if _, err := decodeActionCoordinate(array[1]); err != nil {
		return err
	}
	return nil
}

func responseActionKeys(fields map[string]json.RawMessage) ([]string, error) {
	keysRaw, hasKeys := fields["keys"]
	keyRaw, hasKey := fields["key"]
	if hasKeys && hasKey {
		return nil, fmt.Errorf("computer-use keypress action must use keys or key, not both")
	}
	if hasKeys {
		var keys []string
		if err := json.Unmarshal(keysRaw, &keys); err != nil {
			return nil, fmt.Errorf("computer-use keypress action keys are invalid")
		}
		return validateResponseActionKeys(keys)
	}
	if hasKey {
		key, err := requiredActionString(map[string]json.RawMessage{"key": keyRaw}, "key")
		if err != nil {
			return nil, err
		}
		return validateResponseActionKeys([]string{key})
	}
	return nil, fmt.Errorf("computer-use keypress action requires keys")
}

func validateResponseActionKeys(keys []string) ([]string, error) {
	if len(keys) == 0 || len(keys) > maxComputerActionKeys {
		return nil, fmt.Errorf("computer-use keypress action requires 1 to %d keys", maxComputerActionKeys)
	}
	validated := make([]string, 0, len(keys))
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" || len(key) > maxComputerActionKeyBytes {
			return nil, fmt.Errorf("computer-use keypress action key is invalid")
		}
		validated = append(validated, key)
	}
	return validated, nil
}

func validateResponseActionScroll(fields map[string]json.RawMessage) error {
	for _, names := range [][2]string{{"scroll_x", "scroll_y"}, {"scrollX", "scrollY"}, {"delta_x", "delta_y"}, {"deltaX", "deltaY"}} {
		xRaw, hasX := fields[names[0]]
		yRaw, hasY := fields[names[1]]
		if !hasX && !hasY {
			continue
		}
		if !hasX || !hasY {
			return fmt.Errorf("computer-use scroll action requires both scroll deltas")
		}
		x, err := decodeActionScrollDelta(xRaw)
		if err != nil {
			return err
		}
		y, err := decodeActionScrollDelta(yRaw)
		if err != nil {
			return err
		}
		if x == 0 && y == 0 {
			return fmt.Errorf("computer-use scroll action requires a non-zero delta")
		}
		return nil
	}
	return fmt.Errorf("computer-use scroll action requires scroll deltas")
}

func decodeActionScrollDelta(raw json.RawMessage) (int, error) {
	var value int
	if err := json.Unmarshal(raw, &value); err != nil || value < -maxComputerActionScrollDelta || value > maxComputerActionScrollDelta {
		return 0, fmt.Errorf("computer-use scroll delta is outside the allowed range")
	}
	return value, nil
}

func validateResponseActionWait(fields map[string]json.RawMessage) error {
	for _, name := range []string{"milliseconds", "duration_ms", "timeout_ms", "ms"} {
		raw, found := fields[name]
		if !found {
			continue
		}
		var milliseconds int
		if err := json.Unmarshal(raw, &milliseconds); err != nil || milliseconds < 1 || milliseconds > maxComputerActionWaitMilliseconds {
			return fmt.Errorf("computer-use wait action duration must be between 1 and %d milliseconds", maxComputerActionWaitMilliseconds)
		}
	}
	return nil
}
