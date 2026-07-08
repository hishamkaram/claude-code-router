//go:build live

package liveclaude

import (
	"context"
	"testing"
)

func TestClaudeCodeAvailable(t *testing.T) {
	t.Parallel()

	availability, err := Check(context.Background())
	if err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}
	if availability.Path == "" {
		t.Fatalf("Check() returned empty path")
	}
}
