package responses

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/hishamkaram/claude-code-router/internal/cua"
)

func responsesTools(rawTools []json.RawMessage) (tools []Tool, hasComputer bool, functionNames map[string]bool, err error) {
	tools = make([]Tool, 0, len(rawTools))
	functionNames = make(map[string]bool)
	for _, raw := range rawTools {
		tool, computer, toolErr := responsesTool(raw)
		if toolErr != nil {
			return nil, false, nil, toolErr
		}
		if computer {
			if !hasComputer {
				tools = append(tools, tool)
				hasComputer = true
			}
			continue
		}
		functionNames[strings.ToLower(tool.Name)] = true
		tools = append(tools, tool)
	}
	if hasComputer && functionNames["computer"] {
		return nil, false, nil, fmt.Errorf("native computer tool cannot be combined with a function tool named %q; rename the function tool", "computer")
	}
	return tools, hasComputer, functionNames, nil
}

type anthropicToolChange struct {
	typeName string
	toolName string
}

func responsesToolsAfterAnthropicChanges(tools []Tool, system json.RawMessage, messages []anthropicMsg) ([]Tool, error) {
	changes, err := anthropicToolChanges(system, messages)
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
		name := responsesToolName(tool)
		defined[name] = true
		active[name] = !tool.deferred
	}
	for _, change := range changes {
		if !defined[change.toolName] {
			if change.typeName == "tool_reference" {
				continue
			}
			return nil, fmt.Errorf("system %s references unknown tool %q", change.typeName, change.toolName)
		}
		active[change.toolName] = change.typeName != "tool_removal"
	}

	filtered := make([]Tool, 0, len(tools))
	for _, tool := range tools {
		if active[responsesToolName(tool)] {
			filtered = append(filtered, tool)
		}
	}
	return filtered, nil
}

func responsesToolName(tool Tool) string {
	if tool.Type == "computer" {
		return "computer"
	}
	return tool.Name
}

func responsesToolCapabilities(tools []Tool) (hasComputer bool, functionNames map[string]bool) {
	functionNames = make(map[string]bool)
	for _, tool := range tools {
		if tool.Type == "computer" {
			hasComputer = true
			continue
		}
		functionNames[strings.ToLower(tool.Name)] = true
	}
	return hasComputer, functionNames
}

func anthropicToolChanges(system json.RawMessage, messages []anthropicMsg) ([]anthropicToolChange, error) {
	changes := make([]anthropicToolChange, 0)
	if len(system) > 0 && string(system) != "null" {
		var err error
		changes, err = appendAnthropicToolChanges(changes, system)
		if err != nil {
			return nil, err
		}
	}
	for _, message := range messages {
		if message.Role == "system" {
			var err error
			changes, err = appendAnthropicToolChanges(changes, message.Content)
			if err != nil {
				return nil, err
			}
		}
		if message.Role == "user" {
			var err error
			changes, err = appendAnthropicToolResultReferences(changes, message.Content)
			if err != nil {
				return nil, err
			}
		}
	}
	return changes, nil
}

func appendAnthropicToolChanges(changes []anthropicToolChange, raw json.RawMessage) ([]anthropicToolChange, error) {
	blocks, err := rawArrayIfPresent(raw)
	if err != nil {
		return nil, err
	}
	for _, rawBlock := range blocks {
		blockType, err := blockType(rawBlock)
		if err != nil {
			return nil, err
		}
		if blockType != "tool_addition" && blockType != "tool_removal" {
			continue
		}
		change, err := parseAnthropicToolChange(rawBlock, blockType)
		if err != nil {
			return nil, err
		}
		changes = append(changes, change)
	}
	return changes, nil
}

func appendAnthropicToolResultReferences(changes []anthropicToolChange, raw json.RawMessage) ([]anthropicToolChange, error) {
	blocks, err := rawArrayIfPresent(raw)
	if err != nil {
		return nil, err
	}
	for _, rawBlock := range blocks {
		names, err := anthropicToolResultReferenceNames(rawBlock)
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			changes = append(changes, anthropicToolChange{typeName: "tool_reference", toolName: name})
		}
	}
	return changes, nil
}

func anthropicToolResultReferenceNames(rawBlock json.RawMessage) ([]string, error) {
	resultType, err := blockType(rawBlock)
	if err != nil {
		return nil, err
	}
	if resultType != "tool_result" {
		return nil, nil
	}
	var fields map[string]json.RawMessage
	if decodeErr := json.Unmarshal(rawBlock, &fields); decodeErr != nil || fields == nil {
		return nil, fmt.Errorf("decode tool_result block")
	}
	content, ok := fields["content"]
	if !ok || string(content) == "null" {
		return nil, nil
	}
	contentBlocks, err := rawArrayIfPresent(content)
	if err != nil {
		return nil, fmt.Errorf("tool_result content must be a string or array: %w", err)
	}
	names := make([]string, 0, len(contentBlocks))
	for _, rawContent := range contentBlocks {
		contentType, err := blockType(rawContent)
		if err != nil {
			return nil, err
		}
		if contentType != "tool_reference" {
			continue
		}
		name, err := toolReferenceName(rawContent)
		if err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, nil
}

func rawArrayIfPresent(raw json.RawMessage) ([]json.RawMessage, error) {
	if _, ok, err := rawString(raw); ok || err != nil {
		if err != nil {
			return nil, err
		}
		return nil, nil
	}
	blocks, err := rawArray(raw)
	if err != nil {
		return nil, fmt.Errorf("system content must be a string or array: %w", err)
	}
	return blocks, nil
}

func parseAnthropicToolChange(raw json.RawMessage, blockType string) (anthropicToolChange, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return anthropicToolChange{}, fmt.Errorf("decode system %s block", blockType)
	}
	toolRaw, err := anthropicToolReferenceRaw(fields, blockType)
	if err != nil {
		return anthropicToolChange{}, err
	}
	name, err := anthropicToolReferenceName(toolRaw, blockType)
	if err != nil {
		return anthropicToolChange{}, err
	}
	return anthropicToolChange{typeName: blockType, toolName: name}, nil
}

func anthropicToolReferenceRaw(fields map[string]json.RawMessage, blockType string) (json.RawMessage, error) {
	for key := range fields {
		if key != "type" && key != "tool" {
			return nil, fmt.Errorf("system %s block field %q is not supported by the OpenAI Responses gateway path", blockType, key)
		}
	}
	toolRaw, ok := fields["tool"]
	if !ok {
		return nil, fmt.Errorf("system %s block requires a tool reference object", blockType)
	}
	return toolRaw, nil
}

func anthropicToolReferenceName(toolRaw json.RawMessage, blockType string) (string, error) {
	var reference map[string]json.RawMessage
	if err := json.Unmarshal(toolRaw, &reference); err != nil || reference == nil {
		return "", fmt.Errorf("system %s block requires a tool reference object", blockType)
	}
	for key := range reference {
		if key != "type" && key != "name" {
			return "", fmt.Errorf("system %s tool reference field %q is not supported by the OpenAI Responses gateway path", blockType, key)
		}
	}
	var referenceType string
	if err := json.Unmarshal(reference["type"], &referenceType); err != nil || referenceType != "tool_reference" {
		return "", fmt.Errorf("system %s tool reference type %q is not supported by the OpenAI Responses gateway path", blockType, referenceType)
	}
	var name string
	if err := json.Unmarshal(reference["name"], &name); err != nil {
		return "", fmt.Errorf("system %s tool reference name must be a string", blockType)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("system %s tool reference name is required", blockType)
	}
	if strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("system %s tool reference name must not contain control characters", blockType)
	}
	return name, nil
}

func responsesTool(raw json.RawMessage) (Tool, bool, error) {
	var tool struct {
		Type            string          `json:"type"`
		Name            string          `json:"name"`
		Description     string          `json:"description"`
		InputSchema     json.RawMessage `json:"input_schema"`
		Strict          *bool           `json:"strict"`
		DeferLoading    bool            `json:"defer_loading"`
		DisplayWidthPX  *int            `json:"display_width_px"`
		DisplayHeightPX *int            `json:"display_height_px"`
		DisplayWidth    *int            `json:"display_width"`
		DisplayHeight   *int            `json:"display_height"`
		Environment     string          `json:"environment"`
	}
	if err := json.Unmarshal(raw, &tool); err != nil {
		return Tool{}, false, fmt.Errorf("decode tool: %w", err)
	}
	if cua.IsNativeComputerTool(tool.Type, tool.Name, tool.InputSchema) {
		computerTool, err := responsesComputerTool(tool.DisplayWidthPX, tool.DisplayHeightPX, tool.DisplayWidth, tool.DisplayHeight, tool.Environment)
		computerTool.deferred = tool.DeferLoading
		return computerTool, true, err
	}
	name := strings.TrimSpace(tool.Name)
	if name == "" {
		return Tool{}, false, fmt.Errorf("function tool missing name")
	}
	if len(tool.InputSchema) == 0 || string(tool.InputSchema) == "null" {
		return Tool{}, false, fmt.Errorf("function tool %q missing input_schema", name)
	}
	return Tool{
		Type:        "function",
		Name:        name,
		Description: tool.Description,
		Parameters:  tool.InputSchema,
		Strict:      tool.Strict,
		deferred:    tool.DeferLoading,
	}, false, nil
}

func responsesComputerTool(widthPX, heightPX, width, height *int, environment string) (Tool, error) {
	if err := validatePositiveInt("computer display width", firstInt(widthPX, width)); err != nil {
		return Tool{}, err
	}
	if err := validatePositiveInt("computer display height", firstInt(heightPX, height)); err != nil {
		return Tool{}, err
	}
	if err := validateComputerEnvironment(environment); err != nil {
		return Tool{}, err
	}
	return Tool{
		Type: "computer",
	}, nil
}

func firstInt(values ...*int) *int {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func validatePositiveInt(name string, value *int) error {
	if value == nil {
		return nil
	}
	if *value <= 0 {
		return fmt.Errorf("%s must be positive", name)
	}
	return nil
}

func validateComputerEnvironment(value string) error {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return nil
	}
	switch value {
	case "windows", "mac", "linux", "ubuntu", "browser":
		return nil
	default:
		return fmt.Errorf("computer environment %q is not supported by the OpenAI Responses API", value)
	}
}

func responsesToolChoice(raw json.RawMessage, hasNativeComputer bool) (toolChoice any, parallelToolCalls *bool, resultErr error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil, nil
	}
	var choice struct {
		Type                   string `json:"type"`
		Name                   string `json:"name"`
		DisableParallelToolUse *bool  `json:"disable_parallel_tool_use"`
	}
	if err := json.Unmarshal(raw, &choice); err != nil {
		return nil, nil, fmt.Errorf("decode tool_choice: %w", err)
	}
	parallelToolCalls = responsesParallelToolCalls(choice.DisableParallelToolUse)
	switch strings.TrimSpace(choice.Type) {
	case "", "auto":
		return "auto", parallelToolCalls, nil
	case "none":
		return "none", parallelToolCalls, nil
	case "any":
		return "required", parallelToolCalls, nil
	case "tool":
		name := strings.TrimSpace(choice.Name)
		if name == "" {
			return nil, nil, fmt.Errorf("tool_choice type tool requires name")
		}
		if hasNativeComputer && strings.EqualFold(name, "computer") {
			return map[string]string{"type": "computer"}, parallelToolCalls, nil
		}
		return map[string]string{"type": "function", "name": name}, parallelToolCalls, nil
	default:
		return nil, nil, fmt.Errorf("tool_choice type %q is not supported", choice.Type)
	}
}

func validateResponsesToolChoiceAgainstTools(raw json.RawMessage, tools []Tool) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var choice struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &choice); err != nil {
		return fmt.Errorf("decode tool_choice: %w", err)
	}
	if strings.TrimSpace(choice.Type) != "tool" {
		return nil
	}
	name := strings.TrimSpace(choice.Name)
	for _, tool := range tools {
		if tool.Type == "computer" && strings.EqualFold(name, "computer") {
			return nil
		}
		if tool.Type != "computer" && tool.Name == name {
			return nil
		}
	}
	return fmt.Errorf("tool_choice references unavailable tool %q", name)
}

func responsesParallelToolCalls(disabled *bool) *bool {
	if disabled == nil || !*disabled {
		return nil
	}
	parallel := false
	return &parallel
}
