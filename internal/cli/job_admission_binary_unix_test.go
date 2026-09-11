//go:build linux || darwin

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/hishamkaram/claude-code-router/internal/jobs"
)

// This helper runs only as the isolated fake Claude executable of the binary
// test. It models history explicitly; it is not a real Claude acceptance proof.
func TestAdmissionBinaryClaudeHelper(t *testing.T) {
	if os.Getenv("CCR_ADMISSION_CLAUDE_HELPER") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		os.Exit(2)
	}
	args = args[1:]
	option := func(name string) string {
		for i, arg := range args {
			if arg == name && i+1 < len(args) {
				return args[i+1]
			}
			if value, ok := strings.CutPrefix(arg, name+"="); ok {
				return value
			}
		}
		return ""
	}
	session := option("--session-id")
	resumed := option("--resume")
	if session != "" && resumed != "" {
		os.Exit(3)
	}
	if resumed != "" {
		session = resumed
	}
	root := os.Getenv("CCR_ADMISSION_HISTORY")
	history := filepath.Join(root, session)
	previous, historyErr := os.ReadFile(history)
	if resumed != "" && historyErr != nil {
		os.Exit(1)
	}
	var prompt bytes.Buffer
	if _, err := prompt.ReadFrom(os.Stdin); err != nil {
		os.Exit(4)
	}
	text := string(previous) + prompt.String()
	if err := os.WriteFile(history, []byte(text), 0o600); err != nil {
		os.Exit(5)
	}
	count, err := os.OpenFile(filepath.Join(root, "executions"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		os.Exit(6)
	}
	if _, err := fmt.Fprintln(count, session); err != nil {
		os.Exit(7)
	}
	_ = count.Close()
	encoder := json.NewEncoder(os.Stdout)
	_ = encoder.Encode(map[string]any{"type": "system", "subtype": "init", "session_id": session, "model": option("--model")})
	_ = encoder.Encode(map[string]any{"type": "result", "subtype": "success", "session_id": session, "is_error": false, "result": text})
	os.Exit(0)
}

type admissionBinaryHarness struct {
	t          *testing.T
	executable string
	root       string
	env        []string
	prompt     string
}

func newAdmissionBinaryHarness(t *testing.T) *admissionBinaryHarness {
	t.Helper()
	root := t.TempDir()
	binary := filepath.Join(root, "ccr")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "../../cmd/ccr")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build binary: %v\n%s", err, output)
	}
	for _, name := range []string{"home", "profile", "bin", "history"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	helper, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nexec \"$CCR_ADMISSION_TEST_BINARY\" -test.run '^TestAdmissionBinaryClaudeHelper$' -- \"$@\"\n"
	if err := os.WriteFile(filepath.Join(root, "bin", "claude"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	env := []string{}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if name == "HOME" || name == "XDG_DATA_HOME" || name == "CLAUDE_CONFIG_DIR" || name == "PATH" || strings.HasPrefix(name, "ANTHROPIC_") || strings.HasPrefix(name, "CLAUDE_CODE_OAUTH") || strings.HasPrefix(name, "CCR_") {
			continue
		}
		env = append(env, entry)
	}
	env = append(env, "HOME="+filepath.Join(root, "home"), "XDG_DATA_HOME="+filepath.Join(root, "data"), "CLAUDE_CONFIG_DIR="+filepath.Join(root, "profile"), "PATH="+filepath.Join(root, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"), "CCR_ADMISSION_CLAUDE_HELPER=1", "CCR_ADMISSION_TEST_BINARY="+helper, "CCR_ADMISSION_HISTORY="+filepath.Join(root, "history"))
	h := &admissionBinaryHarness{t: t, executable: binary, root: root, env: env, prompt: filepath.Join(root, "prompt")}
	t.Cleanup(h.cleanup)
	h.run("provider", "add", "fixture-provider", "--type=anthropic-compatible", "--base-url=http://127.0.0.1:1", "--no-api-key")
	h.run("model", "add", "fixture", "--provider=fixture-provider", "--model=fixture-model", "--compat=full")
	return h
}

func (h *admissionBinaryHarness) command(args ...string) *exec.Cmd {
	cmd := exec.CommandContext(h.t.Context(), h.executable, args...)
	cmd.Dir, cmd.Env = h.root, h.env
	return cmd
}

func (h *admissionBinaryHarness) run(args ...string) []byte {
	h.t.Helper()
	output, err := h.command(args...).CombinedOutput()
	if err != nil {
		h.t.Fatalf("ccr %q: %v\n%s", args, err, output)
	}
	return output
}

func (h *admissionBinaryHarness) args(submission string) []string {
	return []string{"launch", "--detach", "-p", "--model=fixture", "--auth-mode=provider-only", "--no-history", "--no-lifecycle", "--no-statusline", "--output-format=stream-json", "--verbose", "--prompt-file=" + h.prompt, "--submission-id=" + submission}
}

func (h *admissionBinaryHarness) receipt(args []string) jobReceipt {
	h.t.Helper()
	var receipt jobReceipt
	if err := json.Unmarshal(h.run(args...), &receipt); err != nil {
		h.t.Fatal(err)
	}
	return receipt
}

func (h *admissionBinaryHarness) terminal(id string) jobs.Record {
	h.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var record jobs.Record
		if err := json.Unmarshal(h.run("status", id, "--json"), &record); err != nil {
			h.t.Fatal(err)
		}
		if record.Terminal() {
			if record.Status != "completed" {
				diagnostic, _ := os.ReadFile(record.ErrorLog)
				h.t.Logf("fixture job %s ended %s: %s", id, record.Status, diagnostic)
			}
			return record
		}
		time.Sleep(25 * time.Millisecond)
	}
	h.t.Fatal("job did not finalize")
	return jobs.Record{}
}

func TestAdmissionBuiltBinaryReplayResumeAndConcurrentSubmission(t *testing.T) {
	h := newAdmissionBinaryHarness(t)
	marker := "original-" + uuid.NewString()
	if err := os.WriteFile(h.prompt, []byte(marker), 0o600); err != nil {
		t.Fatal(err)
	}
	first := h.receipt(h.args("original"))
	one := h.terminal(first.JobID)
	if one.Status != "completed" || !one.Stopped() || one.ResultEvidence == nil {
		t.Fatalf("first result: %+v", one)
	}
	if replay := h.receipt(h.args("original")); replay != first {
		t.Fatalf("replay changed admission: %+v", replay)
	}
	if err := os.WriteFile(h.prompt, []byte("second-round"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := h.command(h.args("original")...).CombinedOutput(); err == nil {
		t.Fatalf("conflicting replay admitted: %s", output)
	}
	resume := append(h.args("continuation"), "--resume="+first.SessionID, "--expected-parent-job="+first.JobID)
	second := h.receipt(resume)
	two := h.terminal(second.JobID)
	if two.Status != "completed" || two.ResumedFrom != first.JobID || two.SessionID != first.SessionID || two.JobID == first.JobID {
		t.Fatalf("wrong continuation: %+v", two)
	}
	result, err := jobs.ReadCommittedOutput(t.Context(), two.Log, *two.ResultEvidence)
	if err != nil || result.Text != marker+"second-round" {
		t.Fatalf("history lost: %+v %v", result, err)
	}
	stale := append(h.args("stale"), "--resume="+first.SessionID, "--expected-parent-job="+first.JobID)
	if output, err := h.command(stale...).CombinedOutput(); err == nil {
		t.Fatalf("stale parent admitted: %s", output)
	}
	var head jobs.Record
	if err := json.Unmarshal(h.run("status", "--session-id="+first.SessionID, "--json"), &head); err != nil || head.JobID != second.JobID {
		t.Fatalf("wrong authoritative head: %+v %v", head, err)
	}
	t.Run("concurrent-identical-resume", func(t *testing.T) {
		args := append(h.args("concurrent-resume"), "--resume="+second.SessionID, "--expected-parent-job="+second.JobID)
		testConcurrentAdmission(t, h, args, 3)
	})
	t.Run("concurrent-identical-submissions", func(t *testing.T) { testConcurrentAdmission(t, h, h.args("concurrent"), 4) })
	t.Run("lost-receipt", func(t *testing.T) { testLostAdmissionReceipt(t, h) })
}

func testConcurrentAdmission(t *testing.T, h *admissionBinaryHarness, args []string, expectedCount int) {
	t.Helper()
	type response struct {
		data []byte
		err  error
	}
	responses := make(chan response, 8)
	var group sync.WaitGroup
	for range 8 {
		group.Go(func() {
			data, err := h.command(args...).CombinedOutput()
			// A contender can observe the nonblocking session lease before
			// its peer commits any submission binding. Retry the same request
			// only for that explicit busy refusal; never invent a fresh ID.
			deadline := time.Now().Add(3 * time.Second)
			for err != nil && strings.TrimSpace(string(data)) == jobs.ErrSessionBusy.Error() && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
				data, err = h.command(args...).CombinedOutput()
			}
			responses <- response{data, err}
		})
	}
	group.Wait()
	close(responses)
	id := ""
	for response := range responses {
		if response.err != nil {
			t.Fatalf("identical concurrent submission failed: %v %s", response.err, response.data)
		}
		var receipt jobReceipt
		if err := json.Unmarshal(response.data, &receipt); err != nil {
			t.Fatal(err)
		}
		if id != "" && id != receipt.JobID {
			t.Fatal("duplicate admission")
		}
		id = receipt.JobID
	}
	if final := h.terminal(id); final.Status != "completed" {
		t.Fatalf("concurrent job: %+v", final)
	}
	executions, err := os.ReadFile(filepath.Join(h.root, "history", "executions"))
	if err != nil || len(strings.Fields(string(executions))) != expectedCount {
		t.Fatalf("wrong execution count: %q %v", executions, err)
	}
}

func (h *admissionBinaryHarness) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	root := filepath.Join(h.root, "data", "claude-code-router", "jobs")
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		h.t.Errorf("reading cleanup jobs: %v", err)
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || jobs.ValidateID(entry.Name()) != nil {
			continue
		}
		command := exec.CommandContext(ctx, h.executable, "cancel", entry.Name())
		command.Dir, command.Env = h.root, h.env
		_ = command.Run()
		for ctx.Err() == nil {
			lease, acquired, lockErr := (jobs.Store{Root: root}).Lock(entry.Name())
			if lockErr != nil {
				h.t.Errorf("checking cleanup ownership: %v", lockErr)
				break
			}
			if acquired {
				_ = lease.Close()
				break
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
	if ctx.Err() != nil {
		h.t.Error("detached fixture owner did not settle before cleanup")
	}
}

func testLostAdmissionReceipt(t *testing.T, h *admissionBinaryHarness) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr := reader.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	command := h.command(h.args("lost-receipt")...)
	command.Stdout = writer
	var diagnostic bytes.Buffer
	command.Stderr = &diagnostic
	startErr := command.Start()
	_ = writer.Close()
	if startErr != nil {
		t.Fatal(startErr)
	}
	if waitErr := command.Wait(); waitErr == nil {
		t.Fatal("receipt unexpectedly delivered to closed reader")
	}
	var admitted jobs.Record
	if decodeErr := json.Unmarshal(h.run("status", "--submission-id=lost-receipt", "--json"), &admitted); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	replay := h.receipt(h.args("lost-receipt"))
	if replay.JobID != admitted.JobID {
		t.Fatal("lost receipt caused duplicate admission")
	}
	if final := h.terminal(replay.JobID); final.Status != "completed" {
		t.Fatalf("caller loss revoked owner: %+v; %s", final, diagnostic.String())
	}
	executions, err := os.ReadFile(filepath.Join(h.root, "history", "executions"))
	if err != nil || len(strings.Fields(string(executions))) != 5 {
		t.Fatalf("lost receipt duplicated execution: %q %v", executions, err)
	}
}
