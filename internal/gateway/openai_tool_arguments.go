package gateway

import (
	"encoding/json"
	"strings"

	"github.com/hishamkaram/claude-code-router/internal/agentinput"
)

func openAIToolArguments(raw string) any {
	return openAIToolArgumentsForTool("", raw)
}

func openAIToolArgumentsForTool(toolName, raw string) any {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return map[string]any{}
	}
	decoded, ok := decodeOpenAIToolArguments(raw)
	if !ok {
		return map[string]any{"arguments": raw}
	}
	if decoded == nil {
		return map[string]any{}
	}
	decodedObject, ok := decoded.(map[string]any)
	if !ok {
		return map[string]any{"value": decoded}
	}
	if strings.EqualFold(strings.TrimSpace(toolName), "Agent") {
		return agentinput.Normalize(decodedObject)
	}
	return decoded
}

func trimmedStringField(input map[string]any, key string) string {
	value, _ := input[key].(string)
	return strings.TrimSpace(value)
}

func decodeOpenAIToolArguments(raw string) (any, bool) {
	var decoded any
	if err := json.Unmarshal([]byte(raw), &decoded); err == nil {
		return decoded, true
	}
	suffix, ok := missingJSONClosingSuffix(raw)
	if !ok {
		return nil, false
	}
	if err := json.Unmarshal([]byte(raw+suffix), &decoded); err != nil {
		return nil, false
	}
	return decoded, true
}

func missingJSONClosingSuffix(raw string) (string, bool) {
	state := jsonClosingSuffixState{stack: make([]byte, 0, 4)}
	for i := 0; i < len(raw); i++ {
		if !state.consume(raw[i]) {
			return "", false
		}
	}
	if state.inString || len(state.stack) == 0 {
		return "", false
	}
	suffix := make([]byte, 0, len(state.stack))
	for i := len(state.stack) - 1; i >= 0; i-- {
		suffix = append(suffix, state.stack[i])
	}
	return string(suffix), true
}

type jsonClosingSuffixState struct {
	stack    []byte
	inString bool
	escaped  bool
}

func (s *jsonClosingSuffixState) consume(b byte) bool {
	if s.inString {
		s.consumeStringByte(b)
		return true
	}
	switch b {
	case '"':
		s.inString = true
	case '{':
		s.stack = append(s.stack, '}')
	case '[':
		s.stack = append(s.stack, ']')
	case '}', ']':
		return s.consumeClosingDelimiter(b)
	}
	return true
}

func (s *jsonClosingSuffixState) consumeStringByte(b byte) {
	if s.escaped {
		s.escaped = false
		return
	}
	if b == '\\' {
		s.escaped = true
		return
	}
	if b == '"' {
		s.inString = false
	}
}

func (s *jsonClosingSuffixState) consumeClosingDelimiter(b byte) bool {
	if len(s.stack) == 0 || s.stack[len(s.stack)-1] != b {
		return false
	}
	s.stack = s.stack[:len(s.stack)-1]
	return true
}
