package gateway

import (
	"strings"
	"testing"
)

func TestEstimateTranslatedInputTokensScalesWithPromptText(t *testing.T) {
	short := estimateTranslatedInputTokens(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "brief prompt"}},
	})
	long := estimateTranslatedInputTokens(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": strings.Repeat("detailed prompt ", 400)}},
	})
	if short < 1 {
		t.Fatalf("short prompt estimate = %d, want positive", short)
	}
	if long <= short {
		t.Fatalf("long prompt estimate = %d, want greater than short estimate %d", long, short)
	}
}

func TestEstimateTranslatedInputTokensBoundsInlineImageData(t *testing.T) {
	estimate := func(data string) int {
		return estimateTranslatedInputTokens(map[string]any{
			"input": []any{map[string]any{
				"type": "message",
				"content": []any{map[string]any{
					"type":      "input_image",
					"image_url": data,
				}},
			}},
		})
	}
	short := estimate("data:image/png;base64,a")
	long := estimate("data:image/png;base64," + strings.Repeat("a", 1<<20))
	if short != long {
		t.Fatalf("inline image estimates differ: short=%d long=%d", short, long)
	}
	if short < inlineImageInputTokenEstimate || short > inlineImageInputTokenEstimate+1024 {
		t.Fatalf("inline image estimate = %d, want bounded image allowance", short)
	}
}
