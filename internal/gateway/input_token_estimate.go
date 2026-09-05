package gateway

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

const inlineImageInputTokenEstimate = 1024

// estimateTranslatedInputTokens supplies the input usage required by the
// Anthropic message_start event before an OpenAI-style upstream stream provides
// terminal usage. It stays entirely local so translated streams never wait for
// a count-token round trip before their first byte.
func estimateTranslatedInputTokens(request any) int {
	raw, err := json.Marshal(request)
	if err != nil {
		return 1
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return max(1, (utf8.RuneCount(raw)+3)/4)
	}
	characters, images := translatedInputCharacters(value)
	return max(1, (characters+3)/4+images*inlineImageInputTokenEstimate)
}

func translatedInputCharacters(value any) (characters, images int) {
	switch typed := value.(type) {
	case nil:
		return len("null"), 0
	case string:
		if strings.HasPrefix(strings.ToLower(typed), "data:image/") {
			// The data URL is transport encoding, not prompt text. Use a bounded
			// vision allowance so a base64 payload cannot force immediate compaction.
			return len("data:image/"), 1
		}
		return utf8.RuneCountInString(typed) + 2, 0
	case []any:
		characters, images := 2, 0
		for _, item := range typed {
			itemCharacters, itemImages := translatedInputCharacters(item)
			characters += itemCharacters + 1
			images += itemImages
		}
		return characters, images
	case map[string]any:
		characters, images := 2, 0
		for key, item := range typed {
			itemCharacters, itemImages := translatedInputCharacters(item)
			characters += utf8.RuneCountInString(key) + itemCharacters + 3
			images += itemImages
		}
		return characters, images
	default:
		raw, err := json.Marshal(typed)
		if err != nil {
			return 0, 0
		}
		return utf8.RuneCount(raw), 0
	}
}
