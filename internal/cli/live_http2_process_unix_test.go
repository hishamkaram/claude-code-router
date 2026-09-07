//go:build live && (darwin || linux)

package cli

import (
	"os/exec"
	"syscall"
	"time"
)

// Only this test's new process group is killed on a deadline. This also covers
// startup failures before CCR has persisted its Claude child's PID.
func protectHTTP2TestProcess(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 10 * time.Second
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	return nil
}
