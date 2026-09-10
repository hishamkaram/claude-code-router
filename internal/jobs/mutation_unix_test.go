//go:build linux || darwin

package jobs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

func scopeWorkload(dir, workload string) string {
	heartbeat := filepath.Join(dir, "heartbeat")
	child := "while :; do echo alive >> '" + heartbeat + "'; sleep 0.05; done"
	if workload == "forking" {
		child = "while :; do (sleep 0.1) & echo alive >> '" + heartbeat + "'; sleep 0.02; done"
	}
	prefix := ""
	if workload == "stdin" {
		prefix = "exec 3<&0; "
	}
	return prefix + "setsid /bin/sh -c \"(" + child + ") &\" </dev/null >/dev/null 2>&1 & echo ready > '" + filepath.Join(dir, "ready") + "'; sleep 120"
}

func awaitHeartbeat(ctx context.Context, path string) error {
	for {
		data, _ := os.ReadFile(path)
		if len(data) > 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func negativeContainmentProof(dir, workload string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	output, err := os.Create(filepath.Join(dir, "negative-output"))
	if err != nil {
		return err
	}
	defer output.Close()
	cfg := ProcessConfig{JobID: "ccr-" + uuid.NewString(), Executable: executable, Path: "/bin/sh", Args: []string{"-c", scopeWorkload(dir, workload)}, Env: os.Environ(), Out: output, Err: output}
	if workload == "stdin" {
		cfg.Input = []byte(strings.Repeat("X", 1<<20))
	}
	p, err := startProcess(ctx, cfg, false)
	if err != nil {
		return err
	}
	heartbeat := filepath.Join(dir, "heartbeat")
	if heartbeatErr := awaitHeartbeat(ctx, heartbeat); heartbeatErr != nil {
		_ = p.Stop()
		<-p.Done()
		return heartbeatErr
	}
	stopping := time.Now()
	_ = p.Stop()
	<-p.Done()
	if time.Since(stopping) > 3*time.Second {
		return fmt.Errorf("escaped stdin holder delayed cancellation")
	}
	before, err := os.ReadFile(heartbeat)
	if err != nil {
		return err
	}
	time.Sleep(200 * time.Millisecond)
	after, err := os.ReadFile(heartbeat)
	if err != nil {
		return err
	}
	if len(after) <= len(before) {
		return fmt.Errorf("invalid mutation control: cleanup assertion passed with containment disabled")
	}
	fmt.Println("MUTATION_DETECTED: escaped descendant remains active")
	return nil
}
