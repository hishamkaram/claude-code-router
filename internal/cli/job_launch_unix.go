//go:build linux || darwin

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/hishamkaram/claude-code-router/internal/jobs"
)

type jobAdmission struct {
	Root   string
	ID     string
	DB     string
	Args   []string
	Prompt []byte
}

type jobReceipt struct {
	JobID     string `json:"job_id"`
	SessionID string `json:"session_id"`
}

func launchDetached(ctx context.Context, cmd *cobra.Command, opts *options, deps Dependencies, args []string, prompt string) error {
	s, err := jobStore()
	if err != nil {
		return err
	}
	executable := deps.ExecutablePath
	if executable == "" {
		executable, err = os.Executable()
	}
	if err != nil {
		return fmt.Errorf("resolving CCR executable: %w", err)
	}
	r, err := s.Create()
	if err != nil {
		return err
	}
	lock, acquired, err := s.Lock(r.JobID)
	if err != nil {
		return err
	}
	if !acquired {
		return fmt.Errorf("new job ownership already claimed")
	}
	defer func() { _ = lock.Close() }()
	if err := s.Write(r); err != nil {
		return err
	}
	admission := jobAdmission{Root: s.Root, ID: r.JobID, DB: opts.dbPath, Args: foregroundJobArgs(args), Prompt: []byte(prompt)}
	if err := startJobOwner(ctx, executable, lock, r, admission); err != nil {
		return fmt.Errorf("admitting job %s (query this ID before retrying): %w", r.JobID, err)
	}
	return json.NewEncoder(cmd.OutOrStdout()).Encode(jobReceipt{JobID: r.JobID, SessionID: r.SessionID})
}

func startJobOwner(ctx context.Context, executable string, lock *os.File, r jobs.Record, admission jobAdmission) error {
	input, inputWriter, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("opening owner input: %w", err)
	}
	defer func() { _ = input.Close() }()
	defer func() { _ = inputWriter.Close() }()
	ack, ackWriter, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("opening owner acknowledgment: %w", err)
	}
	defer func() { _ = ack.Close() }()
	defer func() { _ = ackWriter.Close() }()
	out, err := os.OpenFile(r.Log, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("opening job output: %w", err)
	}
	defer func() { _ = out.Close() }()
	errOut, err := os.OpenFile(r.ErrorLog, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("opening job error output: %w", err)
	}
	defer func() { _ = errOut.Close() }()
	child := exec.Command(executable, "_job-owner")
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	child.Stdout, child.Stderr = out, errOut
	child.ExtraFiles = []*os.File{input, ackWriter, lock}
	if err := child.Start(); err != nil {
		return fmt.Errorf("starting job owner: %w", err)
	}
	// The durable owner intentionally outlives this CLI; no pipe-copy goroutines exist.
	if err := child.Process.Release(); err != nil {
		return fmt.Errorf("releasing detached owner handle: %w", err)
	}
	_ = input.Close()
	_ = ackWriter.Close()
	deadline := time.Now().Add(15 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := inputWriter.SetWriteDeadline(deadline); err != nil {
		return fmt.Errorf("setting admission deadline: %w", err)
	}
	if err := json.NewEncoder(inputWriter).Encode(admission); err != nil {
		return fmt.Errorf("sending job admission: %w", err)
	}
	_ = inputWriter.Close()
	if err := ack.SetReadDeadline(deadline); err != nil {
		return fmt.Errorf("setting acknowledgment deadline: %w", err)
	}
	var receipt jobReceipt
	if err := json.NewDecoder(ack).Decode(&receipt); err != nil {
		return fmt.Errorf("reading durable acknowledgment: %w", err)
	}
	if receipt.JobID != r.JobID || receipt.SessionID != r.SessionID {
		return fmt.Errorf("owner acknowledgment identity mismatch")
	}
	return nil
}
