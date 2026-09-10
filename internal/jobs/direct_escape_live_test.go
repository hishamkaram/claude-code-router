//go:build live && linux

package jobs

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

func TestLiveDirectChildMigrationFinishesWithoutExternalSignal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	file := filepath.Join(t.TempDir(), "heartbeat")
	unit := "ccr-test-direct-" + uuid.NewString() + ".scope"
	script := fmt.Sprintf("busctl --user call org.freedesktop.systemd1 /org/freedesktop/systemd1 org.freedesktop.systemd1.Manager StartTransientUnit 'ssa(sv)a(sa(sv))' %s fail 1 PIDs au 1 $$ 0 || exit 2; while :; do echo live >> '%s'; sleep 0.05; done", unit, file)
	cfg := testProcessConfig(t, script)
	cfg.Input = []byte(strings.Repeat("X", 1<<20))
	probe, err := openScope(ctx, cfg.JobID)
	if err != nil {
		t.Fatalf("live migration test requires a ready user manager: %v", err)
	}
	probe.Close(ctx)
	p, err := StartProcess(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	pid := p.PID()
	defer func() {
		cleanupCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer stop()
		out, stopErr := exec.CommandContext(cleanupCtx, "systemctl", "--user", "stop", unit).CombinedOutput()
		if stopErr != nil {
			t.Errorf("test-owned external scope cleanup: %v %s", stopErr, out)
		}
		_ = p.Stop()
		<-p.Done()
		var status unix.WaitStatus
		_, _ = unix.Wait4(pid, &status, unix.WNOHANG, nil)
	}()
	if p.Backend() != "systemd-scope" {
		t.Fatal("live migration test requires actual systemd containment")
	}
	if err := awaitHeartbeat(ctx, file); err != nil {
		t.Fatal(err)
	}
	_ = p.Stop()
	select {
	case <-p.Done():
	case <-time.After(7 * time.Second):
		t.Fatal("migrated direct child retained the owner")
	}
	cleanup, code := p.Result()
	if cleanup.Coverage != "unknown" || code != nil || len(cleanup.Survivors) != 1 || cleanup.Survivors[0].PID != pid {
		t.Fatalf("unconfirmed child not reported: %+v %v", cleanup, code)
	}
	assertHeartbeatContinues(t, file)
}
