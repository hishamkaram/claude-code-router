//go:build linux || darwin

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestJobAdmissionPreservesMaximumPrompt(t *testing.T) {
	prompt := bytes.Repeat([]byte("<>&\x00"), 4<<20)
	admission := jobAdmission{ID: "fixture", Prompt: prompt, Args: []string{"-p"}}
	raw, err := json.Marshal(admission)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > 32<<20 {
		t.Fatal("maximum advertised prompt exceeds admission frame limit")
	}
	var got jobAdmission
	if err := json.NewDecoder(io.LimitReader(bytes.NewReader(raw), 32<<20)).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(prompt, got.Prompt) {
		t.Fatal("prompt bytes changed during admission")
	}
}

func TestJobLaunchArguments(t *testing.T) {
	args := []string{"--model", "fixture", "--detach", "-p", "--prompt-file=x", "--", "--output-format", "stream-json"}
	inv, err := parseLaunchInvocation(args)
	if err != nil {
		t.Fatal(err)
	}
	if !inv.detach || !inv.printMode || inv.promptFile != "x" {
		t.Fatalf("incorrect parser state: %+v", inv)
	}
	want := []string{"--model", "fixture", "-p", "--", "--output-format", "stream-json"}
	if got := foregroundJobArgs(args); !reflect.DeepEqual(got, want) {
		t.Fatalf("forwarding: %v", got)
	}
	for _, arg := range []string{"--detach", "--prompt-file"} {
		if err := validateLaunchPassthroughArgs([]string{arg, "x"}); err == nil {
			t.Fatalf("CCR option bypass: %s", arg)
		}
	}
}

func TestJobLaunchValidationBeforeAdmission(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	for _, args := range [][]string{
		{"launch", "--detach"},
		{"launch", "--detach", "-p"},
		{"launch", "-p", "--prompt-file="},
		{"launch", "--detach", "-p", "--prompt-file", "/does/not/exist"},
		{"launch", "--detach", "-p", "--prompt-file", "x", "--session-id", "x"},
		{"cancel", "0"},
		{"cancel", "-1"},
		{"status", "../x", "--json"},
	} {
		cmd := NewRootCommand(context.Background(), Dependencies{})
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Errorf("accepted invalid args: %v", args)
		}
	}
	s, err := jobStore()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Root); !os.IsNotExist(err) {
		t.Fatal("invalid arguments created job storage")
	}
}

func TestJobPromptSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prompt")
	if err := os.WriteFile(path, []byte("snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	prompt, err := readJobPrompt(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if prompt != "snapshot" {
		t.Fatal("prompt snapshot lost")
	}
	if _, err := readJobPrompt(filepath.Dir(path)); err == nil {
		t.Fatal("accepted directory prompt")
	}
}

func TestDetachedSessionOptions(t *testing.T) {
	for _, arg := range []string{"-r", "-rfixture", "-r=fixture", "-c", "-pc", "-prfixture", "--resume=fixture", "--from-pr=123", "--teleport"} {
		if got := detachedSessionOption([]string{arg}); got == "" {
			t.Errorf("missed session option %q", arg)
		}
	}
	for _, arg := range []string{"-p", "-dtrace", "-nccr", "-wccr", "-pdtrace", "--system-prompt=ccr", "prompt"} {
		if got := detachedSessionOption([]string{arg}); got != "" {
			t.Errorf("mistook %q for session option %q", arg, got)
		}
	}
	if got := detachedSessionOption([]string{"--", "-rfixture", "--resume"}); got != "" {
		t.Fatalf("interpreted literal prompt as option: %q", got)
	}
}

func TestDetachedSessionConflictBeforeAdmission(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	prompt := filepath.Join(t.TempDir(), "prompt")
	if err := os.WriteFile(prompt, []byte("PONG"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, arg := range []string{"-rfixture", "-pc", "-prfixture", "--session-id=fixture", "--from-pr=123"} {
		cmd := NewRootCommand(context.Background(), Dependencies{})
		cmd.SetArgs([]string{"launch", "--detach", "-p", "--prompt-file", prompt, "--", arg})
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "conflicts with CCR's detached session identity") {
			t.Errorf("expected pre-admission session conflict for %q, got %v", arg, err)
		}
	}
	s, err := jobStore()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Root); !os.IsNotExist(err) {
		t.Fatal("conflicting session arguments created job storage")
	}
}

func TestDetachedRejectsAmbiguousClaudeBoundary(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	prompt := filepath.Join(t.TempDir(), "prompt")
	if err := os.WriteFile(prompt, []byte("PONG"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tail := range [][]string{{"--"}, {"--system-prompt", "--", "--resume", "fixture"}, {"--system-prompt", "--", "--session-id", "fixture"}, {"--system-prompt", "--", "--no-session-persistence"}} {
		cmd := NewRootCommand(context.Background(), Dependencies{})
		cmd.SetArgs(append([]string{"launch", "--detach", "-p", "--prompt-file", prompt, "--"}, tail...))
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "standalone -- is not supported") {
			t.Errorf("expected boundary validation for %v, got %v", tail, err)
		}
	}
	s, err := jobStore()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Root); !os.IsNotExist(err) {
		t.Fatal("ambiguous arguments created job storage")
	}
}

func TestJobPromptRejectsFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fifo")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { _, err := readJobPrompt(path); result <- err }()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("accepted FIFO prompt")
		}
	case <-time.After(time.Second):
		// Unblock a regressed reader before failing, so the test owns its goroutine.
		fd, err := unix.Open(path, unix.O_WRONLY|unix.O_NONBLOCK, 0)
		if err == nil {
			_ = unix.Close(fd)
		}
		<-result
		t.Fatal("FIFO prompt blocked admission")
	}
}
