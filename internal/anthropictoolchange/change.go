package anthropictoolchange

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
)

// Kind identifies the strict union carried by an Anthropic mid-conversation
// tool change block.
type Kind string

const (
	Reference  Kind = "tool_reference"
	Definition Kind = "tool_definition"
)

// Change is the adapter-neutral form of a tool_addition or tool_removal block.
// Definition is populated only for a tool_definition addition.
type Change struct {
	BlockType  string
	Kind       Kind
	Name       string
	Definition json.RawMessage
}

// Parse validates and decodes the Anthropic tool change discriminated union.
// The outer block may carry only type, tool, and cache_control. A definition
// is valid only in a tool_addition block; references are valid for both change
// types.
func Parse(raw json.RawMessage) (Change, error) {
	block, decodeErr := decodeObject(raw, "tool change block must be an object")
	if decodeErr != nil {
		return Change{}, decodeErr
	}
	if fieldErr := rejectUnknownFields(block, "type", "tool", "cache_control"); fieldErr != nil {
		return Change{}, fieldErr
	}

	blockType, parseErr := requiredString(block, "type", "tool change block")
	if parseErr != nil {
		return Change{}, parseErr
	}
	if blockType != "tool_addition" && blockType != "tool_removal" {
		return Change{}, fmt.Errorf("tool change block type %q is not supported", blockType)
	}

	rawTool, ok := block["tool"]
	if !ok {
		return Change{}, fmt.Errorf("system %s block requires a tool object", blockType)
	}
	tool, decodeErr := decodeObject(rawTool, fmt.Sprintf("system %s block requires a tool object", blockType))
	if decodeErr != nil {
		return Change{}, decodeErr
	}
	return parseTool(blockType, tool)
}

func parseTool(blockType string, tool map[string]json.RawMessage) (Change, error) {
	toolType, parseErr := requiredString(tool, "type", fmt.Sprintf("system %s tool", blockType))
	if parseErr != nil {
		return Change{}, parseErr
	}

	switch Kind(toolType) {
	case Reference:
		if fieldErr := rejectUnknownFields(tool, "type", "name"); fieldErr != nil {
			return Change{}, fmt.Errorf("system %s %w", blockType, fieldErr)
		}
		name, nameErr := toolReferenceName(tool, blockType)
		if nameErr != nil {
			return Change{}, nameErr
		}
		return Change{BlockType: blockType, Kind: Reference, Name: name}, nil
	case Definition:
		if blockType != "tool_addition" {
			return Change{}, fmt.Errorf("system %s tool type %q is only supported for tool_addition", blockType, toolType)
		}
		if fieldErr := rejectUnknownFields(tool, "type", "definition"); fieldErr != nil {
			return Change{}, fmt.Errorf("system %s %w", blockType, fieldErr)
		}
		rawDefinition, ok := tool["definition"]
		if !ok {
			return Change{}, fmt.Errorf("system %s tool_definition requires a definition", blockType)
		}
		if _, decodeErr := decodeObject(rawDefinition, fmt.Sprintf("system %s tool definition must be an object", blockType)); decodeErr != nil {
			return Change{}, decodeErr
		}
		return Change{
			BlockType:  blockType,
			Kind:       Definition,
			Definition: append(json.RawMessage(nil), rawDefinition...),
		}, nil
	default:
		return Change{}, fmt.Errorf("system %s tool reference type %q is not supported", blockType, toolType)
	}
}

func decodeObject(raw json.RawMessage, message string) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if unmarshalErr := json.Unmarshal(raw, &object); unmarshalErr != nil || object == nil {
		return nil, fmt.Errorf("%s", message)
	}
	return object, nil
}

func rejectUnknownFields(fields map[string]json.RawMessage, allowed ...string) error {
	allowedFields := make(map[string]struct{}, len(allowed))
	for _, field := range allowed {
		allowedFields[field] = struct{}{}
	}
	for field := range fields {
		if _, ok := allowedFields[field]; !ok {
			return fmt.Errorf("field %q is not supported", field)
		}
	}
	return nil
}

func requiredString(fields map[string]json.RawMessage, key, context string) (string, error) {
	raw, ok := fields[key]
	if !ok {
		return "", fmt.Errorf("%s requires %s", context, key)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("%s %s must be a string", context, key)
	}
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s %s is required", context, key)
	}
	return value, nil
}

func toolReferenceName(tool map[string]json.RawMessage, blockType string) (string, error) {
	rawName, ok := tool["name"]
	if !ok {
		return "", fmt.Errorf("system %s tool reference name is required", blockType)
	}
	var name string
	if err := json.Unmarshal(rawName, &name); err != nil {
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
