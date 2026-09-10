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

func TestLiveScopeSiblingUntouched(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	file := filepath.Join(t.TempDir(), "sibling-heartbeat")
	sibling, err := StartProcess(ctx, testProcessConfig(t, "while :; do echo live >> '"+file+"'; sleep 0.05; done"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sibling.Stop(); <-sibling.Done() }()
	if heartbeatErr := awaitHeartbeat(ctx, file); heartbeatErr != nil {
		t.Fatal(heartbeatErr)
	}
	job, err := StartProcess(ctx, testProcessConfig(t, "sleep 120"))
	if err != nil {
		t.Fatal(err)
	}
	_ = job.Stop()
	<-job.Done()
	assertHeartbeatContinues(t, file)
	select {
	case err := <-sibling.Done():
		t.Fatalf("unrelated sibling stopped: %v", err)
	default:
	}
}

func TestLiveScopeExternalUnitEscapeIsPartial(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	file := filepath.Join(t.TempDir(), "external-heartbeat")
	unit := "ccr-test-escape-" + uuid.NewString() + ".service"
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cleanupCancel()
		if out, err := exec.CommandContext(cleanupCtx, "systemctl", "--user", "stop", unit).CombinedOutput(); err != nil {
			t.Errorf("external test unit cleanup: %v %s", err, out)
		}
	}()
	script := "systemd-run --user --unit=" + unit + " --collect -- /bin/sh -c 'while :; do echo live >> \"" + file + "\"; sleep 0.05; done'; sleep 120"
	p, err := StartProcess(ctx, testProcessConfig(t, script))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop()
	if err := awaitHeartbeat(ctx, file); err != nil {
		t.Fatal(err)
	}
	_ = p.Stop()
	<-p.Done()
	cleanup, _ := p.Result()
	if cleanup.Coverage != "partial" || !strings.Contains(cleanup.Reason, "D-Bus") {
		t.Fatalf("dishonest escape report: %+v", cleanup)
	}
	assertHeartbeatContinues(t, file)
}

func assertHeartbeatContinues(t *testing.T, file string) {
	t.Helper()
	before, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	after, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) <= len(before) {
		t.Fatal("independent process did not remain active")
	}
}

func TestLiveMissingScopeCancellationDoesNotSignal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := openScope(ctx, "ccr-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(ctx)
	if err := s.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if got := s.Observe(ctx); got.Coverage != "unknown" {
		t.Fatalf("absence was treated as cleanup evidence: %+v", got)
	}
}

func TestLiveScopeAdmissionCancellationRace(t *testing.T) {
	for i := 0; i < 20; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			file := filepath.Join(t.TempDir(), "executed")
			cfg := testProcessConfig(t, "echo executed > '"+file+"'; sleep 120")
			goDone := make(chan struct{})
			go func() { defer close(goDone); time.Sleep(time.Duration(i%4) * 30 * time.Millisecond); cancel() }()
			p, err := StartProcess(ctx, cfg)
			<-goDone
			if err != nil {
				if _, statErr := os.Stat(file); statErr == nil {
					t.Fatal("refused launch executed workload")
				}
				return
			}
			select {
			case <-p.Done():
			case <-time.After(10 * time.Second):
				t.Fatal("admission race leaked child")
			}
			cleanup, code := p.Result()
			if code == nil || cleanup.Coverage == "complete" {
				t.Fatalf("incorrect raced result: %+v %v", cleanup, code)
			}
		})
	}
}
