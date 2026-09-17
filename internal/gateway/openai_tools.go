package gateway

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
)

type openAIToolChange struct {
	typeName string
	toolName string
}

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
		deferred, _ := payload["defer_loading"].(bool)
		tools = append(tools, openAITool{
			Type:     "function",
			deferred: deferred,
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

func openAIToolsAfterAnthropicChanges(tools []openAITool, messages []anthropicMessage) ([]openAITool, error) {
	changes, err := openAIToolChangesFromAnthropic(messages)
	if err != nil {
		return nil, err
	}
	needsFilter := len(changes) > 0
	for _, tool := range tools {
		if tool.deferred {
			needsFilter = true
			break
		}
	}
	if !needsFilter {
		return tools, nil
	}

	defined := make(map[string]bool, len(tools))
	active := make(map[string]bool, len(tools))
	for _, tool := range tools {
		name := tool.Function.Name
		defined[name] = true
		active[name] = !tool.deferred
	}
	for _, change := range changes {
		if !defined[change.toolName] {
			return nil, fmt.Errorf("system %s references unknown tool %q", change.typeName, change.toolName)
		}
		active[change.toolName] = change.typeName == "tool_addition"
	}

	filtered := make([]openAITool, 0, len(tools))
	for _, tool := range tools {
		if active[tool.Function.Name] {
			filtered = append(filtered, tool)
		}
	}
	return filtered, nil
}

func openAIToolChangesFromAnthropic(messages []anthropicMessage) ([]openAIToolChange, error) {
	changes := make([]openAIToolChange, 0)
	for _, message := range messages {
		if message.Role != "system" {
			continue
		}
		blocks, ok := message.Content.([]any)
		if !ok {
			continue
		}
		for _, item := range blocks {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			blockType, _ := block["type"].(string)
			if blockType != "tool_addition" && blockType != "tool_removal" {
				continue
			}
			change, err := openAIToolChangeFromAnthropic(block)
			if err != nil {
				return nil, err
			}
			changes = append(changes, change)
		}
	}
	return changes, nil
}

func openAIToolChangeFromAnthropic(block map[string]any) (openAIToolChange, error) {
	blockType, _ := block["type"].(string)
	for key := range block {
		if key != "type" && key != "tool" {
			return openAIToolChange{}, fmt.Errorf("system %s block field %q is not supported by the OpenAI-compatible gateway path", blockType, key)
		}
	}
	reference, ok := block["tool"].(map[string]any)
	if !ok {
		return openAIToolChange{}, fmt.Errorf("system %s block requires a tool reference object", blockType)
	}
	for key := range reference {
		if key != "type" && key != "name" {
			return openAIToolChange{}, fmt.Errorf("system %s tool reference field %q is not supported by the OpenAI-compatible gateway path", blockType, key)
		}
	}
	referenceType, _ := reference["type"].(string)
	if referenceType != "tool_reference" {
		return openAIToolChange{}, fmt.Errorf("system %s tool reference type %q is not supported by the OpenAI-compatible gateway path", blockType, referenceType)
	}
	toolName, ok := reference["name"].(string)
	if !ok {
		return openAIToolChange{}, fmt.Errorf("system %s tool reference name must be a string", blockType)
	}
	toolName = strings.TrimSpace(toolName)
	if toolName == "" {
		return openAIToolChange{}, fmt.Errorf("system %s tool reference name is required", blockType)
	}
	if strings.IndexFunc(toolName, unicode.IsControl) >= 0 {
		return openAIToolChange{}, fmt.Errorf("system %s tool reference name must not contain control characters", blockType)
	}
	return openAIToolChange{typeName: blockType, toolName: toolName}, nil
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

func validateOpenAIToolChoiceAgainstTools(raw json.RawMessage, tools []openAITool) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var payload struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("unsupported tool_choice: %w", err)
	}
	if strings.TrimSpace(payload.Type) != "tool" {
		return nil
	}
	name := strings.TrimSpace(payload.Name)
	for _, tool := range tools {
		if tool.Function.Name == name {
			return nil
		}
	}
	return fmt.Errorf("tool_choice references unavailable tool %q", name)
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
