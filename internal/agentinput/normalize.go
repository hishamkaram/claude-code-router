// Package agentinput normalizes Claude Code Agent tool arguments received from
// external providers before they are returned to Claude Code.
package agentinput

import "strings"

// Normalize returns only the Agent tool fields Claude Code accepts. It accepts
// provider variants such as agent_type and derives a description from prompt
// when the provider omitted one.
func Normalize(input map[string]any) map[string]any {
	normalized := make(map[string]any, 6)
	prompt := trimmedStringField(input, "prompt")
	if prompt != "" {
		normalized["prompt"] = prompt
	}
	description := trimmedStringField(input, "description")
	if description == "" {
		description = descriptionFromPrompt(prompt)
	}
	if description != "" {
		normalized["description"] = description
	}
	if subagentType := firstTrimmedStringField(input, "subagent_type", "agent_type"); subagentType != "" {
		normalized["subagent_type"] = subagentType
	}
	if isolation := allowedStringField(input, "isolation", "worktree", "remote"); isolation != "" {
		normalized["isolation"] = isolation
	}
	if model := allowedStringField(input, "model", "sonnet", "opus", "haiku", "fable"); model != "" {
		normalized["model"] = model
	}
	if runInBackground, ok := boolField(input, "run_in_background"); ok {
		normalized["run_in_background"] = runInBackground
	}
	return normalized
}

func descriptionFromPrompt(prompt string) string {
	words := strings.Fields(prompt)
	if len(words) > 5 {
		words = words[:5]
	}
	return strings.Join(words, " ")
}

func firstTrimmedStringField(input map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := trimmedStringField(input, key); value != "" {
			return value
		}
	}
	return ""
}

func trimmedStringField(input map[string]any, key string) string {
	value, _ := input[key].(string)
	return strings.TrimSpace(value)
}

func allowedStringField(input map[string]any, key string, allowed ...string) string {
	value := trimmedStringField(input, key)
	for _, candidate := range allowed {
		if value == candidate {
			return value
		}
	}
	return ""
}

func boolField(input map[string]any, key string) (value, ok bool) {
	switch value := input[key].(type) {
	case bool:
		return value, true
	case string:
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "true":
			return true, true
		case "false":
			return false, true
		}
	}
	return false, false
}
