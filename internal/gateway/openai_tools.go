package gateway

import (
	"encoding/json"
	"fmt"
	"strings"
)

func openAIToolsFromAnthropic(rawTools []json.RawMessage) ([]openAITool, error) {
	if len(rawTools) == 0 {
		return nil, nil
	}
	tools := make([]openAITool, 0, len(rawTools))
	for _, raw := range rawTools {
		var payload map[string]any
		if err := json.Unmarshal(raw, &payload); err != nil {
			return nil, fmt.Errorf("unsupported tool definition: %w", err)
		}
		name, _ := payload["name"].(string)
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, fmt.Errorf("tool definition missing name")
		}
		description, _ := payload["description"].(string)
		parameters := payload["input_schema"]
		if parameters == nil {
			parameters = map[string]any{"type": "object"}
		}
		var strict *bool
		if value, ok := payload["strict"].(bool); ok {
			strict = &value
		}
		tools = append(tools, openAITool{
			Type: "function",
			Function: openAIFunction{
				Name:        name,
				Description: description,
				Parameters:  parameters,
				Strict:      strict,
			},
		})
	}
	return tools, nil
}

func openAIToolChoiceFromAnthropic(raw json.RawMessage) (choice any, parallelTools *bool, err error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil, nil
	}
	var payload map[string]json.RawMessage
	err = json.Unmarshal(raw, &payload)
	if err != nil {
		return nil, nil, fmt.Errorf("unsupported tool_choice: %w", err)
	}
	parallelTools, err = openAIParallelToolCallsFromAnthropic(payload)
	if err != nil {
		return nil, nil, err
	}
	var choiceType string
	if rawType, ok := payload["type"]; ok {
		if err := json.Unmarshal(rawType, &choiceType); err != nil {
			return nil, nil, fmt.Errorf("tool_choice.type must be a string")
		}
	}
	switch choiceType {
	case "", "auto":
		return "auto", parallelTools, nil
	case "none":
		return "none", parallelTools, nil
	case "any":
		return "required", parallelTools, nil
	case "tool":
		var name string
		if rawName, ok := payload["name"]; ok {
			if err := json.Unmarshal(rawName, &name); err != nil {
				return nil, nil, fmt.Errorf("tool_choice.name must be a string")
			}
		}
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, nil, fmt.Errorf("tool_choice type %q requires name", choiceType)
		}
		return map[string]any{
			"type": "function",
			"function": map[string]string{
				"name": name,
			},
		}, parallelTools, nil
	default:
		return nil, nil, fmt.Errorf("tool_choice type %q is not supported by the OpenAI-compatible gateway path", choiceType)
	}
}

func openAIParallelToolCallsFromAnthropic(payload map[string]json.RawMessage) (*bool, error) {
	raw, ok := payload["disable_parallel_tool_use"]
	if !ok {
		return nil, nil
	}
	var disabled bool
	if err := json.Unmarshal(raw, &disabled); err != nil {
		return nil, fmt.Errorf("tool_choice.disable_parallel_tool_use must be a boolean")
	}
	if !disabled {
		return nil, nil
	}
	parallel := false
	return &parallel, nil
}
