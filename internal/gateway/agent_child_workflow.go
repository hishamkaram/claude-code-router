package gateway

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

func workflowAgentPrompts(script string) ([]string, bool) {
	var prompts []string
	hasDynamicCall := false
	for offset := 0; offset < len(script); {
		index := findWorkflowAgent(script, offset)
		if index < 0 {
			break
		}
		callStart := skipWorkflowWhitespace(script, index+len("agent"))
		if isWorkflowAgentPropertyName(script, callStart) {
			offset = index + len("agent")
			continue
		}
		if callStart >= len(script) || script[callStart] != '(' {
			// A workflow may pass the injected function through a local alias
			// (for example, `const spawn = agent; await spawn(prompt)`). The
			// alias call is not statically safe to correlate by prompt here, but
			// the presence of the executable agent reference is enough to reserve
			// one marker-bound generic child route. Without this reservation the
			// marked child would be mistaken for an unrelated native request and
			// fail closed after the workflow has already started it.
			hasDynamicCall = true
			offset = index + len("agent")
			continue
		}
		prompt, end, ok := parseWorkflowAgentCall(script, index)
		if ok && strings.TrimSpace(prompt) != "" {
			prompts = append(prompts, strings.TrimSpace(prompt))
			offset = end
			continue
		}
		hasDynamicCall = true
		if end > index {
			offset = end
		} else {
			offset = index + len("agent")
		}
	}
	return prompts, hasDynamicCall
}

func findWorkflowAgent(script string, offset int) int {
	if offset < 0 {
		offset = 0
	}
	for index := offset; index < len(script); index++ {
		if agentIndex, next, handled := workflowAgentLiteralBoundary(script, index); handled {
			if agentIndex >= 0 {
				return agentIndex
			}
			index = next
			continue
		}
		if isWorkflowAgentTokenAt(script, index) {
			return index
		}
	}
	return -1
}

func workflowAgentLiteralBoundary(script string, index int) (agentIndex, next int, handled bool) {
	switch script[index] {
	case '\'', '"':
		return -1, skipWorkflowQuotedLiteral(script, index), true
	case '`':
		if agentIndex, found := findWorkflowTemplateAgent(script, index); found {
			return agentIndex, index, true
		}
		return -1, skipWorkflowQuotedLiteral(script, index), true
	case '/':
		if commentEnd, ok := workflowCommentEnd(script, index); ok {
			return -1, commentEnd, true
		}
		if regexEnd, ok := workflowRegexLiteralEnd(script, index); ok {
			return -1, regexEnd, true
		}
	}
	return -1, index, false
}

func isWorkflowAgentTokenAt(script string, index int) bool {
	if index+len("agent") > len(script) || !strings.EqualFold(script[index:index+len("agent")], "agent") {
		return false
	}
	if isWorkflowAgentMemberReference(script, index) {
		return false
	}
	return (index == 0 || !isWorkflowIdentifierPart(script[index-1])) &&
		(index+len("agent") == len(script) || !isWorkflowIdentifierPart(script[index+len("agent")]))
}

func isWorkflowAgentMemberReference(script string, index int) bool {
	for index--; index >= 0 && strings.ContainsRune(" \t\r\n", rune(script[index])); index-- {
	}
	return index >= 0 && (script[index] == '.' || script[index] == '#')
}

func isWorkflowAgentPropertyName(script string, index int) bool {
	return index < len(script) && script[index] == ':'
}

func findWorkflowTemplateAgent(script string, start int) (int, bool) {
	for index := start + 1; index < len(script); index++ {
		switch script[index] {
		case '\\':
			index++
		case '`':
			return -1, false
		case '$':
			if index+1 >= len(script) || script[index+1] != '{' {
				continue
			}
			end, closed := workflowInterpolationEnd(script, index+2)
			if !closed {
				return -1, false
			}
			if agentIndex := findWorkflowAgent(script[index+2:end], 0); agentIndex >= 0 {
				return index + 2 + agentIndex, true
			}
			index = end
		}
	}
	return -1, false
}

func workflowInterpolationEnd(script string, start int) (int, bool) {
	depth := 1
	for index := start; index < len(script); index++ {
		switch script[index] {
		case '\'', '"', '`':
			index = skipWorkflowQuotedLiteral(script, index)
		case '/':
			if commentEnd, ok := workflowCommentEnd(script, index); ok {
				index = commentEnd
			}
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return index, true
			}
		}
	}
	return len(script), false
}

func skipWorkflowQuotedLiteral(script string, start int) int {
	quote := script[start]
	for index := start + 1; index < len(script); index++ {
		if script[index] == '\\' {
			index++
			continue
		}
		if script[index] == quote {
			return index
		}
	}
	return len(script)
}

func workflowCommentEnd(script string, start int) (int, bool) {
	if start+1 >= len(script) || (script[start+1] != '/' && script[start+1] != '*') {
		return start, false
	}
	if script[start+1] == '/' {
		for index := start + 2; index < len(script); index++ {
			if script[index] == '\n' || script[index] == '\r' {
				return index, true
			}
		}
		return len(script), true
	}
	for index := start + 2; index+1 < len(script); index++ {
		if script[index] == '*' && script[index+1] == '/' {
			return index + 1, true
		}
	}
	return len(script), true
}

// workflowRegexLiteralEnd skips JavaScript regular-expression literals before
// looking for executable agent(...) calls. A regex can contain arbitrary text
// that resembles a call, including quoted parentheses and escaped slashes.
// This is intentionally a lexical check rather than a JavaScript parser: the
// workflow scanner only needs to distinguish a regex-start slash from the
// division operator at the point where it encounters one.
func workflowRegexLiteralEnd(script string, start int) (int, bool) {
	if start < 0 || start >= len(script) || script[start] != '/' || !workflowCanStartRegex(script, start) {
		return start, false
	}
	inClass := false
	for index := start + 1; index < len(script); index++ {
		switch script[index] {
		case '\\':
			if index+1 < len(script) {
				index++
			}
		case '[':
			inClass = true
		case ']':
			inClass = false
		case '\n', '\r':
			// A line break ends a JavaScript regex literal. Leave this slash
			// visible to the normal scanner when the literal is malformed.
			return start, false
		case '/':
			if inClass {
				continue
			}
			for index+1 < len(script) && isWorkflowIdentifierPart(script[index+1]) {
				index++
			}
			return index, true
		}
	}
	// Treat an unterminated regex as opaque through the end of the script. A
	// malformed workflow cannot safely produce a child reservation from text
	// after the unmatched slash.
	return len(script), true
}

func workflowCanStartRegex(script string, slash int) bool {
	index := slash - 1
	for index >= 0 && strings.ContainsRune(" \t\r\n", rune(script[index])) {
		index--
	}
	if index < 0 {
		return true
	}
	switch script[index] {
	case '(', '[', '{', ',', ';', ':', '=', '!', '&', '|', '?', '+', '-', '*', '%', '^', '~', '<', '>':
		return true
	}
	// After a control-flow or expression keyword, a regex literal is valid even
	// though the preceding token is alphabetic (for example `return /.../`).
	end := index + 1
	for index >= 0 && isWorkflowIdentifierPart(script[index]) {
		index--
	}
	switch strings.ToLower(script[index+1 : end]) {
	case "case", "delete", "else", "in", "instanceof", "of", "return", "throw", "typeof", "void", "yield", "await":
		return true
	default:
		return false
	}
}

func parseWorkflowAgentCall(script string, index int) (prompt string, end int, ok bool) {
	cursor := index + len("agent")
	cursor = skipWorkflowWhitespace(script, cursor)
	if cursor >= len(script) || script[cursor] != '(' {
		return "", index + len("agent"), false
	}
	cursor = skipWorkflowWhitespace(script, cursor+1)
	return parseWorkflowString(script, cursor)
}

func skipWorkflowWhitespace(script string, start int) int {
	for start < len(script) && strings.ContainsRune(" \t\r\n", rune(script[start])) {
		start++
	}
	return start
}

func isWorkflowIdentifierPart(value byte) bool {
	return value == '_' || value == '$' || value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
}

func parseWorkflowString(script string, start int) (value string, end int, ok bool) {
	if start >= len(script) || !strings.ContainsRune("'\"`", rune(script[start])) {
		return "", start, false
	}
	quote := script[start]
	var decoded strings.Builder
	for index := start + 1; index < len(script); index++ {
		if script[index] == quote {
			return decoded.String(), index + 1, true
		}
		if quote == '`' && script[index] == '$' && index+1 < len(script) && script[index+1] == '{' {
			// Template interpolation is evaluated at workflow runtime. The
			// source text is not the child prompt, so reserve a generic child
			// route instead of claiming an incorrect literal correlation.
			return "", workflowStringEnd(script, start), false
		}
		if script[index] != '\\' || index+1 >= len(script) {
			decoded.WriteByte(script[index])
			continue
		}
		decodedValue, next, parsed := parseWorkflowEscape(script, index)
		if !parsed {
			return "", workflowStringEnd(script, start), false
		}
		decoded.WriteString(decodedValue)
		index = next - 1
	}
	return "", start, false
}

func parseWorkflowEscape(script string, slash int) (decoded string, next int, ok bool) {
	if slash < 0 || slash+1 >= len(script) || script[slash] != '\\' {
		return "", slash, false
	}
	escape := script[slash+1]
	if decoded, simple := parseWorkflowSimpleEscape(escape); simple {
		return decoded, slash + 2, true
	}
	switch escape {
	case '0':
		return parseWorkflowNullEscape(script, slash)
	case 'x':
		return parseWorkflowHexLiteral(script, slash)
	case 'u':
		return parseWorkflowUnicodeLiteral(script, slash)
	case '\n':
		return "", slash + 2, true
	case '\r':
		return parseWorkflowCarriageReturn(script, slash)
	default:
		// JavaScript accepts additional legacy escape forms whose exact
		// semantics are not represented by this small parser. Marking the
		// call dynamic preserves a generic reservation instead of recording
		// a prompt that cannot reliably correlate the child request.
		return "", slash, false
	}
}

func parseWorkflowNullEscape(script string, slash int) (decoded string, next int, ok bool) {
	if slash+2 < len(script) && script[slash+2] >= '0' && script[slash+2] <= '9' {
		return "", slash, false
	}
	return "\x00", slash + 2, true
}

func parseWorkflowHexLiteral(script string, slash int) (decoded string, next int, ok bool) {
	codePoint, escapeNext, parsed := parseWorkflowHexEscape(script, slash+2, 2)
	if !parsed || !validWorkflowCodePoint(codePoint) {
		return "", slash, false
	}
	return string(rune(codePoint)), escapeNext, true
}

func parseWorkflowUnicodeLiteral(script string, slash int) (decoded string, next int, ok bool) {
	codePoint, escapeNext, parsed := parseWorkflowUnicodeEscape(script, slash)
	if !parsed {
		return "", slash, false
	}
	return string(codePoint), escapeNext, true
}

func parseWorkflowCarriageReturn(script string, slash int) (decoded string, next int, ok bool) {
	next = slash + 2
	if next < len(script) && script[next] == '\n' {
		next++
	}
	return "", next, true
}

func parseWorkflowSimpleEscape(escape byte) (decoded string, ok bool) {
	switch escape {
	case 'n':
		return "\n", true
	case 'r':
		return "\r", true
	case 't':
		return "\t", true
	case 'b':
		return "\b", true
	case 'f':
		return "\f", true
	case 'v':
		return "\v", true
	case '\\', '\'', '"', '`', '/', '$':
		return string(escape), true
	default:
		return "", false
	}
}

func workflowStringEnd(script string, start int) int {
	end := skipWorkflowQuotedLiteral(script, start)
	if end < len(script) {
		return end + 1
	}
	return len(script)
}

func parseWorkflowHexEscape(script string, start, digits int) (value uint32, next int, ok bool) {
	if digits <= 0 || start < 0 || start+digits > len(script) {
		return 0, start, false
	}
	parsedValue, err := strconv.ParseUint(script[start:start+digits], 16, 32)
	if err != nil {
		return 0, start, false
	}
	return uint32(parsedValue), start + digits, true
}

func parseWorkflowUnicodeEscape(script string, slash int) (value rune, next int, ok bool) {
	if slash < 0 || slash+1 >= len(script) || script[slash] != '\\' || script[slash+1] != 'u' {
		return 0, slash, false
	}
	if slash+2 < len(script) && script[slash+2] == '{' {
		return parseWorkflowBracedUnicodeEscape(script, slash)
	}
	first, escapeNext, parsed := parseWorkflowHexEscape(script, slash+2, 4)
	if !parsed {
		return 0, slash, false
	}
	if first >= 0xd800 && first <= 0xdbff {
		return parseWorkflowSurrogatePair(script, slash, escapeNext, first)
	}
	if !validWorkflowCodePoint(first) {
		return 0, slash, false
	}
	return rune(first), escapeNext, true
}

func parseWorkflowBracedUnicodeEscape(script string, slash int) (value rune, next int, ok bool) {
	closingOffset := strings.IndexByte(script[slash+3:], '}')
	if closingOffset < 0 {
		return 0, slash, false
	}
	start := slash + 3
	end := start + closingOffset
	if end == start || end-start > 6 {
		return 0, slash, false
	}
	codePoint, parsed := parseWorkflowHexDigits(script[start:end])
	if !parsed || !validWorkflowCodePoint(codePoint) {
		return 0, slash, false
	}
	return rune(codePoint), end + 1, true
}

func parseWorkflowSurrogatePair(script string, slash, next int, first uint32) (value rune, end int, ok bool) {
	second, secondEnd, parsed := parseWorkflowHexEscapeAtUnicode(script, next)
	if !parsed || second < 0xdc00 || second > 0xdfff {
		return 0, slash, false
	}
	codePoint := 0x10000 + ((first - 0xd800) << 10) + (second - 0xdc00)
	return rune(codePoint), secondEnd, true
}

func parseWorkflowHexEscapeAtUnicode(script string, slash int) (value uint32, next int, ok bool) {
	if slash < 0 || slash+1 >= len(script) || script[slash] != '\\' || script[slash+1] != 'u' {
		return 0, slash, false
	}
	return parseWorkflowHexEscape(script, slash+2, 4)
}

func parseWorkflowHexDigits(value string) (uint32, bool) {
	if value == "" {
		return 0, false
	}
	parsed, err := strconv.ParseUint(value, 16, 32)
	return uint32(parsed), err == nil
}

func validWorkflowCodePoint(value uint32) bool {
	return value <= utf8.MaxRune && (value < 0xd800 || value > 0xdfff)
}
