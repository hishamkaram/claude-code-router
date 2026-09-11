//go:build linux || darwin

package jobs

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestOwnedProcessGroupCancellationSettles(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ready := filepath.Join(t.TempDir(), "ready")
	script := fmt.Sprintf("sleep 120 & sleep 120 & echo ready > %q; wait", ready)
	p, err := startProcess(ctx, testProcessConfig(t, script), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop(); <-p.Done() })
	if p.Backend() != "process-group" {
		t.Fatalf("fallback not exercised: %s", p.Backend())
	}
	if err := awaitHeartbeat(ctx, ready); err != nil {
		t.Fatal(err)
	}
	before := observeGroup(ctx, p.PID())
	if before.Coverage != "partial" || len(before.Survivors) < 3 {
		t.Fatalf("workload group not ready: %+v", before)
	}
	if err := p.Stop(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Done():
	case <-ctx.Done():
		t.Fatal("process-group cancellation did not finish")
	}
	cleanup, code := p.Result()
	if cleanup.Coverage != "partial" || len(cleanup.Survivors) != 0 || code == nil || *code != -1 {
		t.Fatalf("clean cancellation reported survivors: %+v, exit=%v", cleanup, code)
	}
	if !slices.Contains(cleanup.Observed, "launch_pgid_members") || !slices.Contains(cleanup.Unobservable, "escaped_descendants") {
		t.Fatalf("missing cleanup evidence or escape limit: %+v", cleanup)
	}
}
