package responses

import (
	"encoding/json"
	"fmt"
	"strings"

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

func responsesTool(raw json.RawMessage) (Tool, bool, error) {
	var tool struct {
		Type            string          `json:"type"`
		Name            string          `json:"name"`
		Description     string          `json:"description"`
		InputSchema     json.RawMessage `json:"input_schema"`
		Strict          *bool           `json:"strict"`
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

func responsesParallelToolCalls(disabled *bool) *bool {
	if disabled == nil || !*disabled {
		return nil
	}
	parallel := false
	return &parallel
}
