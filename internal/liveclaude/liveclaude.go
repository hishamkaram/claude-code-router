package liveclaude

import (
	"context"
	"fmt"
	"os/exec"
)

type Availability struct {
	Path string
}

func Check(ctx context.Context) (Availability, error) {
	if err := ctx.Err(); err != nil {
		return Availability{}, fmt.Errorf("liveclaude.Check: context canceled: %w", err)
	}
	path, err := exec.LookPath("claude")
	if err != nil {
		return Availability{}, fmt.Errorf("liveclaude.Check: claude binary not found in PATH: %w", err)
	}
	return Availability{Path: path}, nil
}
