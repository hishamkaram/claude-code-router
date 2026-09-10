//go:build live && linux

package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

func TestLiveLostBusDoesNotSignalMigratedChild(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	file := filepath.Join(t.TempDir(), "heartbeat")
	unit := "ccr-test-lost-bus-" + uuid.NewString() + ".scope"
	script := fmt.Sprintf("busctl --user call org.freedesktop.systemd1 /org/freedesktop/systemd1 org.freedesktop.systemd1.Manager StartTransientUnit 'ssa(sv)a(sa(sv))' %s fail 1 PIDs au 1 $$ 0 || exit 2; while :; do echo live >> '%s'; sleep 0.05; done", unit, file)
	p, s := admittedTestProcess(t, ctx, testProcessConfig(t, script))
	pid := p.PID()
	defer func() {
		cleanupCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer stop()
		if out, err := exec.CommandContext(cleanupCtx, "systemctl", "--user", "stop", unit).CombinedOutput(); err != nil {
			t.Errorf("external test scope cleanup: %v %s", err, out)
		}
		_ = p.Stop()
		<-p.Done()
		var status unix.WaitStatus
		_, _ = unix.Wait4(pid, &status, unix.WNOHANG, nil)
	}()
	if err := awaitHeartbeat(ctx, file); err != nil {
		t.Fatal(err)
	}
	// Break only this owner's connection, never the shared user manager.
	_ = s.conn.Close()
	_ = p.Stop()
	select {
	case <-p.Done():
	case <-time.After(7 * time.Second):
		t.Fatal("lost bus retained the owner")
	}
	cleanup, code := p.Result()
	if cleanup.Coverage != "unknown" || code != nil || len(cleanup.Survivors) != 1 || cleanup.Survivors[0].PID != pid {
		t.Fatalf("lost-bus cleanup: %+v %v", cleanup, code)
	}
	assertHeartbeatContinues(t, file)
}

func admittedTestProcess(t *testing.T, ctx context.Context, cfg ProcessConfig) (*Process, *scope) {
	t.Helper()
	s, err := openScope(ctx, cfg.JobID)
	if err != nil {
		t.Fatal(err)
	}
	gate, send, err := os.Pipe()
	if err != nil {
		s.Close(ctx)
		t.Fatal(err)
	}
	defer func() { _ = gate.Close(); _ = send.Close() }()
	input, err := newProcessInput(cfg.Input)
	if err != nil {
		s.Close(ctx)
		t.Fatal(err)
	}
	defer input.closeRead()
	cmd := exec.Command(cfg.Executable, "_job-exec")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = input.read, cfg.Out, cfg.Err
	cmd.ExtraFiles = []*os.File{gate}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		input.close()
		s.Close(ctx)
		t.Fatal(err)
	}
	if err := s.Admit(ctx, cmd.Process.Pid); err != nil {
		input.close()
		t.Fatal(abortGated(ctx, cmd, s, err))
	}
	if err := json.NewEncoder(send).Encode(ExecRequest{Path: cfg.Path, Args: cfg.Args}); err != nil {
		input.close()
		t.Fatal(abortGated(ctx, cmd, s, err))
	}
	runCtx, cancel := context.WithCancel(ctx)
	p := &Process{cmd: cmd, cancel: cancel, done: make(chan error, 1), input: input, backend: "systemd-scope"}
	go p.run(runCtx, s, nil)
	return p, s
}
