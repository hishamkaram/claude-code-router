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
	Root            string
	ID              string
	DB              string
	Args            []string
	Prompt          []byte
	SubmissionID    string
	ExecutionDigest string
}

type jobReceipt struct {
	SubmissionID string `json:"submission_id,omitempty"`
	JobID        string `json:"job_id"`
	SessionID    string `json:"session_id"`
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
	invocation, err := parseLaunchInvocation(args)
	if err != nil {
		return err
	}
	database, err := resolveDBPath(opts)
	if err != nil {
		return err
	}
	execution, err := canonicalAdmissionContext(database, s.Root)
	if err != nil {
		return err
	}
	s.Root = execution.JobRoot
	registry, err := jobs.OpenRegistry(ctx, s.Root)
	if err != nil {
		return err
	}
	defer func() { _ = registry.Close() }()
	digest, err := requestFingerprint(invocation, []byte(prompt), execution)
	if err != nil {
		return err
	}
	bound, lease, err := prepareDetachedAdmission(ctx, registry, &options{dbPath: execution.Database}, invocation, digest)
	if err != nil {
		return err
	}
	receipt := jobReceipt{JobID: bound.JobID, SessionID: bound.SessionID, SubmissionID: bound.SubmissionID}
	if lease == nil {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(receipt)
	}
	defer func() { _ = lease.Close() }()
	record, lock, err := registry.MaterializePrepared(ctx, lease, bound.SubmissionID)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	admission := jobAdmission{
		Root: s.Root, ID: record.JobID, DB: execution.Database, Args: foregroundJobArgs(args), Prompt: []byte(prompt),
		SubmissionID: bound.SubmissionID, ExecutionDigest: bound.ExecutionDigest,
	}
	if err := startJobOwner(ctx, executable, lock, lease, record, admission); err != nil {
		return fmt.Errorf("admitting job %s (query this ID or submission before retrying): %w", record.JobID, err)
	}
	return json.NewEncoder(cmd.OutOrStdout()).Encode(receipt)
}

func startJobOwner(ctx context.Context, executable string, lock *os.File, lease *jobs.SessionLease, r jobs.Record, admission jobAdmission) error {
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
	out, err := openJobOutput(r.Log)
	if err != nil {
		return fmt.Errorf("opening job output: %w", err)
	}
	defer func() { _ = out.Close() }()
	errOut, err := openJobOutput(r.ErrorLog)
	if err != nil {
		return fmt.Errorf("opening job error output: %w", err)
	}
	defer func() { _ = errOut.Close() }()
	child := exec.Command(executable, "_job-owner")
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	child.Stdout, child.Stderr = out, errOut
	child.ExtraFiles = []*os.File{input, ackWriter, lock, lease.File()}
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
