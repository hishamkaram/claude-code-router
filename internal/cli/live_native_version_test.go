//go:build live

package cli

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

func liveNativeVersionAtLeast(t *testing.T, ctx context.Context, want [3]int) bool {
	t.Helper()
	output, err := exec.CommandContext(ctx, "claude", "--version").Output()
	if err != nil {
		t.Fatalf("read native Claude version for compatibility: %v", err)
	}
	fields := strings.Fields(string(output))
	if len(fields) == 0 {
		t.Fatal("native Claude version output was empty")
	}
	parts := strings.Split(fields[0], ".")
	if len(parts) != 3 {
		t.Fatal("native Claude version was not a three-component release")
	}
	var version [3]int
	for index, part := range parts {
		version[index], err = strconv.Atoi(part)
		if err != nil {
			t.Fatal("native Claude version contained an invalid release component")
		}
	}
	for index, component := range version {
		if component != want[index] {
			return component > want[index]
		}
	}
	return true
}
