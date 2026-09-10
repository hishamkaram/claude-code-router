//go:build live && linux

package jobs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestLiveScopeContainment(t *testing.T) {
	for _, workload := range []string{"escape", "forking", "stdin"} {
		for _, enabled := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/containment=%t", workload, enabled), func(t *testing.T) { proveScopeCancellation(t, workload, enabled) })
		}
	}
}

func proveScopeCancellation(t *testing.T, workload string, enabled bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	dir := t.TempDir()
	heartbeat := filepath.Join(dir, "heartbeat")
	if !enabled {
		proveMutation(t, ctx, dir, workload)
		return
	}
	cfg := testProcessConfig(t, scopeWorkload(dir, workload))
	if workload == "stdin" {
		cfg.Input = []byte(strings.Repeat("X", 1<<20))
	}
	p, err := startProcess(ctx, cfg, enabled)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop()
	if p.Backend() != "systemd-scope" {
		t.Fatal("live test requires actual systemd containment")
	}
	if heartbeatErr := awaitHeartbeat(ctx, heartbeat); heartbeatErr != nil {
		t.Fatal("workload not demonstrably alive:", heartbeatErr)
	}
	if stopErr := p.Stop(); stopErr != nil {
		t.Fatal(stopErr)
	}
	select {
	case <-p.Done():
	case <-ctx.Done():
		t.Fatal("scope cancellation timed out")
	}
	cleanup, _ := p.Result()
	if cleanup.Coverage != "partial" || len(cleanup.Observed) == 0 {
		t.Fatalf("scope not observed empty: %+v", cleanup)
	}
	before, err := os.ReadFile(heartbeat)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	after, err := os.ReadFile(heartbeat)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatal("descendant survived scope cancellation")
	}
}

func proveMutation(t *testing.T, ctx context.Context, dir, workload string) {
	t.Helper()
	guardianName := "ccr-test-" + uuid.NewString() + ".scope"
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if out, stopErr := exec.CommandContext(cleanupCtx, "systemctl", "--user", "stop", guardianName).CombinedOutput(); stopErr != nil {
			t.Errorf("guardian cleanup failed: %v %s", stopErr, out)
		}
	}()
	command := exec.CommandContext(ctx, "systemd-run", "--user", "--scope", "--unit="+guardianName, "--property=TasksMax=128", "--property=TimeoutStopSec=2s", "--", executable, "_job-negative-proof", dir, workload)
	out, err := command.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "MUTATION_DETECTED") {
		t.Fatalf("negative control failed: %v %s", err, out)
	}
}
