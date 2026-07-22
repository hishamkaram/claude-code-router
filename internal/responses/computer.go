package responses

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

func openAIComputerActionsFromAnthropic(raw json.RawMessage) ([]json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return []json.RawMessage{json.RawMessage(`{"type":"screenshot"}`)}, nil
	}
	var input map[string]json.RawMessage
	if err := json.Unmarshal(raw, &input); err != nil {
		return nil, fmt.Errorf("decode computer tool input: %w", err)
	}
	if strings.EqualFold(rawStringField(input, "action"), "openai_computer_call") {
		return originalOpenAIComputerActions(input)
	}
	output, err := openAIComputerActionOutput(rawStringField(input, "action"), input, raw)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		return nil, fmt.Errorf("encode computer action: %w", err)
	}
	return []json.RawMessage{encoded}, nil
}

func originalOpenAIComputerActions(input map[string]json.RawMessage) ([]json.RawMessage, error) {
	rawActions, found := input["actions"]
	if !found {
		return nil, fmt.Errorf("openai computer call is missing actions")
	}
	var actions []json.RawMessage
	if err := json.Unmarshal(rawActions, &actions); err != nil || len(actions) == 0 {
		return nil, fmt.Errorf("openai computer call actions must be a non-empty array")
	}
	for index := range actions {
		var action map[string]json.RawMessage
		if err := json.Unmarshal(actions[index], &action); err != nil || action == nil {
			return nil, fmt.Errorf("openai computer call action %d must be an object", index)
		}
		actions[index] = append(json.RawMessage(nil), actions[index]...)
	}
	return actions, nil
}

func openAIComputerActionOutput(action string, input map[string]json.RawMessage, raw json.RawMessage) (map[string]any, error) {
	switch action {
	case "", "screenshot":
		return map[string]any{"type": "screenshot"}, nil
	case "left_click", "right_click", "middle_click":
		return openAIClickAction(input, strings.TrimSuffix(action, "_click"))
	case "double_click":
		return openAICoordinateAction(input, "double_click", "left")
	case "mouse_move":
		return openAICoordinateAction(input, "move", "")
	case "type":
		return map[string]any{"type": "type", "text": rawUntrimmedStringField(input, "text")}, nil
	case "key":
		return openAIKeypressAction(input), nil
	case "wait":
		return map[string]any{"type": "wait"}, nil
	case "left_click_drag":
		path, err := rawJSONField(input, "path")
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "drag", "path": path}, nil
	case "scroll":
		x, y, _ := coordinateField(input)
		scrollX, scrollY := scrollDelta(input)
		return map[string]any{"type": "scroll", "x": x, "y": y, "scroll_x": scrollX, "scroll_y": scrollY}, nil
	default:
		return map[string]any{"type": "anthropic_" + action, "input": raw}, nil
	}
}

func openAIClickAction(input map[string]json.RawMessage, button string) (map[string]any, error) {
	return openAICoordinateAction(input, "click", button)
}

func openAICoordinateAction(input map[string]json.RawMessage, actionType, button string) (map[string]any, error) {
	x, y, err := coordinateField(input)
	if err != nil {
		return nil, err
	}
	action := map[string]any{"type": actionType, "x": x, "y": y}
	if button != "" {
		action["button"] = button
	}
	return action, nil
}

func openAIKeypressAction(input map[string]json.RawMessage) map[string]any {
	key := rawStringField(input, "text")
	if key == "" {
		key = rawStringField(input, "key")
	}
	return map[string]any{"type": "keypress", "keys": []string{key}}
}

func rawStringField(fields map[string]json.RawMessage, key string) string {
	return strings.TrimSpace(rawUntrimmedStringField(fields, key))
}

func rawUntrimmedStringField(fields map[string]json.RawMessage, key string) string {
	raw, ok := fields[key]
	if !ok {
		return ""
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

func rawJSONField(fields map[string]json.RawMessage, key string) (json.RawMessage, error) {
	raw, ok := fields[key]
	if !ok {
		return nil, fmt.Errorf("computer action missing %q", key)
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("computer action field %q is invalid JSON", key)
	}
	return raw, nil
}

func coordinateField(fields map[string]json.RawMessage) (x, y int, resultErr error) {
	raw, ok := fields["coordinate"]
	if !ok {
		return 0, 0, fmt.Errorf("computer action missing coordinate")
	}
	var coordinate []float64
	if err := json.Unmarshal(raw, &coordinate); err != nil {
		return 0, 0, fmt.Errorf("decode computer action coordinate: %w", err)
	}
	if len(coordinate) < 2 {
		return 0, 0, fmt.Errorf("computer action coordinate requires x and y")
	}
	return roundedInt(coordinate[0]), roundedInt(coordinate[1]), nil
}

func scrollDelta(fields map[string]json.RawMessage) (x, y int) {
	direction := rawStringField(fields, "scroll_direction")
	amount := rawIntField(fields, "scroll_amount")
	if amount == 0 {
		amount = 1
	}
	pixels := amount * 100
	switch direction {
	case "left":
		return -pixels, 0
	case "right":
		return pixels, 0
	case "up":
		return 0, -pixels
	default:
		return 0, pixels
	}
}

func rawIntField(fields map[string]json.RawMessage, key string) int {
	raw, ok := fields[key]
	if !ok {
		return 0
	}
	var value float64
	if json.Unmarshal(raw, &value) != nil {
		return 0
	}
	return roundedInt(value)
}

func roundedInt(value float64) int {
	return int(math.Round(value))
}
