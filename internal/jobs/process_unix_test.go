//go:build linux || darwin

package jobs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 4 && os.Args[1] == "_job-negative-proof" {
		if err := negativeContainmentProof(os.Args[2], os.Args[3]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) == 2 && os.Args[1] == "_job-exec" {
		if err := ExecGated(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func testProcessConfig(t *testing.T, script string) ProcessConfig {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(t.TempDir(), "output"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return ProcessConfig{JobID: "ccr-" + uuid.NewString(), Executable: executable, Path: "/bin/sh", Args: []string{"-c", script}, Env: os.Environ(), Out: f, Err: f}
}

func TestOwnedProcessImmediateExit(t *testing.T) {
	for _, code := range []int{0, 7} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		p, err := startProcess(ctx, testProcessConfig(t, fmt.Sprintf("exit %d", code)), false)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		err = <-p.Done()
		cancel()
		_, actual := p.Result()
		if actual == nil || *actual != code || (err == nil) != (code == 0) {
			t.Fatalf("exit %d: %v, %+v", code, err, actual)
		}
		if err := p.Stop(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOwnedProcessCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, err := startProcess(ctx, testProcessConfig(t, "sleep 120"), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Stop(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Done():
	case <-ctx.Done():
		t.Fatal("owned process cancellation did not finish")
	}
	cleanup, code := p.Result()
	if cleanup.Coverage == "complete" || code == nil {
		t.Fatalf("incorrect cancellation result: %+v %v", cleanup, code)
	}
}
