package gateway

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/hishamkaram/claude-code-router/internal/anthropictoolchange"
)

type openAIToolChange struct {
	typeName   string
	toolName   string
	inlineTool *openAITool
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
		// Preserve Anthropic's opt-in strictness rather than the upstream default.
		strict, _ := payload["strict"].(bool)
		deferred, _ := payload["defer_loading"].(bool)
		tools = append(tools, openAITool{
			Type:     "function",
			deferred: deferred,
			Function: openAIFunction{
				Name:        name,
				Description: description,
				Parameters:  parameters,
				Strict:      &strict,
			},
		})
	}
	return tools, nil
}

func openAIToolsAfterAnthropicChanges(tools []openAITool, system any, messages []anthropicMessage) ([]openAITool, error) {
	changes, err := openAIToolChangesFromAnthropic(system, messages)
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

	working := append([]openAITool(nil), tools...)
	defined := make(map[string]int, len(working))
	active := make(map[string]bool, len(tools))
	for index, tool := range working {
		name := tool.Function.Name
		if _, exists := defined[name]; !exists {
			defined[name] = index
		}
		active[name] = !tool.deferred
	}
	for _, change := range changes {
		if err := applyOpenAIToolChange(&working, defined, active, change); err != nil {
			return nil, err
		}
	}

	filtered := make([]openAITool, 0, len(working))
	for _, tool := range working {
		if active[tool.Function.Name] {
			filtered = append(filtered, tool)
		}
	}
	return filtered, nil
}

func applyOpenAIToolChange(tools *[]openAITool, defined map[string]int, active map[string]bool, change openAIToolChange) error {
	if change.inlineTool != nil {
		if index, exists := defined[change.toolName]; exists {
			(*tools)[index] = *change.inlineTool
		} else {
			defined[change.toolName] = len(*tools)
			*tools = append(*tools, *change.inlineTool)
		}
		active[change.toolName] = true
		return nil
	}
	if _, exists := defined[change.toolName]; !exists {
		// A ToolSearch result may name a tool from another turn's set; that activates
		// nothing rather than failing the request.
		if change.typeName == "tool_reference" {
			return nil
		}
		return fmt.Errorf("system %s references unknown tool %q", change.typeName, change.toolName)
	}
	active[change.toolName] = change.typeName != "tool_removal"
	return nil
}

func openAIToolChangesFromAnthropic(system any, messages []anthropicMessage) ([]openAIToolChange, error) {
	changes := make([]openAIToolChange, 0)
	var err error
	changes, err = appendOpenAISystemToolChanges(changes, system)
	if err != nil {
		return nil, err
	}
	for _, message := range messages {
		if message.Role != "system" {
			changes = appendOpenAIToolResultReferences(changes, message)
			continue
		}
		changes, err = appendOpenAISystemToolChanges(changes, message.Content)
		if err != nil {
			return nil, err
		}
	}
	return changes, nil
}

func appendOpenAISystemToolChanges(changes []openAIToolChange, content any) ([]openAIToolChange, error) {
	blocks, ok := content.([]any)
	if !ok {
		return changes, nil
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
	return changes, nil
}

// ToolSearch reports the tools it loaded as tool_reference blocks inside a tool_result.
// Unread, a deferred tool stays filtered out of the upstream request after the model has
// been told it is callable, and its call silently lands on some other tool.
func appendOpenAIToolResultReferences(changes []openAIToolChange, message anthropicMessage) []openAIToolChange {
	blocks, ok := message.Content.([]any)
	if !ok {
		return changes
	}
	for _, item := range blocks {
		for _, name := range openAIToolResultReferenceNames(item) {
			// Appended in message order, so availability stays chronological.
			changes = append(changes, openAIToolChange{typeName: "tool_reference", toolName: name})
		}
	}
	return changes
}

func openAIToolResultReferenceNames(item any) []string {
	block, ok := item.(map[string]any)
	if !ok {
		return nil
	}
	if blockType, _ := block["type"].(string); blockType != "tool_result" {
		return nil
	}
	contents, ok := block["content"].([]any)
	if !ok {
		return nil
	}
	names := make([]string, 0, len(contents))
	for _, rawContent := range contents {
		content, ok := rawContent.(map[string]any)
		if !ok {
			continue
		}
		if contentType, _ := content["type"].(string); contentType != "tool_reference" {
			continue
		}
		name, _ := content["tool_name"].(string)
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}
	return names
}

func openAIToolChangeFromAnthropic(block map[string]any) (openAIToolChange, error) {
	blockType, _ := block["type"].(string)
	rawBlock, err := json.Marshal(block)
	if err != nil {
		return openAIToolChange{}, fmt.Errorf("encode system %s block: %w", blockType, err)
	}
	change, err := anthropictoolchange.Parse(rawBlock)
	if err != nil {
		return openAIToolChange{}, err
	}
	switch change.Kind {
	case anthropictoolchange.Reference:
		return openAIToolChange{typeName: change.BlockType, toolName: change.Name}, nil
	case anthropictoolchange.Definition:
		inlineTool, err := openAIInlineToolFromAnthropic(change.Definition, change.BlockType)
		if err != nil {
			return openAIToolChange{}, err
		}
		return openAIToolChange{
			typeName:   change.BlockType,
			toolName:   inlineTool.Function.Name,
			inlineTool: inlineTool,
		}, nil
	default:
		return openAIToolChange{}, fmt.Errorf("system %s tool type %q is not supported", change.BlockType, change.Kind)
	}
}

func openAIInlineToolFromAnthropic(rawDefinition json.RawMessage, blockType string) (*openAITool, error) {
	parsed, err := openAIToolsFromAnthropic([]json.RawMessage{rawDefinition})
	if err != nil {
		return nil, fmt.Errorf("system %s tool definition is invalid: %w", blockType, err)
	}
	if len(parsed) != 1 {
		return nil, fmt.Errorf("system %s tool definition must contain exactly one tool", blockType)
	}
	toolName := strings.TrimSpace(parsed[0].Function.Name)
	if strings.IndexFunc(toolName, unicode.IsControl) >= 0 {
		return nil, fmt.Errorf("system %s tool definition name must not contain control characters", blockType)
	}
	parsed[0].Function.Name = toolName
	return &parsed[0], nil
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
