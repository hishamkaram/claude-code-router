package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

type ClaudeEnvironment struct {
	Set   []string
	Unset []string
}

type ClaudeLauncher interface {
	Start(ctx context.Context, args []string, env ClaudeEnvironment, in io.Reader, out, errOut io.Writer) (ClaudeProcess, error)
}

type ClaudeProcess interface {
	PID() int
	Wait() error
	Stop() error
}

type ExecClaudeLauncher struct{}

func (ExecClaudeLauncher) Start(ctx context.Context, args []string, env ClaudeEnvironment, in io.Reader, out, errOut io.Writer) (ClaudeProcess, error) {
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Env = applyClaudeEnvironment(os.Environ(), env)
	cmd.Stdin = readerOrDefault(in, os.Stdin)
	cmd.Stdout = writerOrDefault(out, os.Stdout)
	cmd.Stderr = writerOrDefault(errOut, os.Stderr)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting Claude Code: %w", err)
	}
	return execClaudeProcess{cmd: cmd}, nil
}

func applyClaudeEnvironment(base []string, overlay ClaudeEnvironment) []string {
	replaced := make(map[string]struct{}, len(overlay.Set)+len(overlay.Unset))
	for _, name := range overlay.Unset {
		replaced[name] = struct{}{}
	}
	for _, entry := range overlay.Set {
		name, _, ok := strings.Cut(entry, "=")
		if ok {
			replaced[name] = struct{}{}
		}
	}

	merged := make([]string, 0, len(base)+len(overlay.Set))
	for _, entry := range base {
		name, _, ok := strings.Cut(entry, "=")
		if ok {
			if _, skip := replaced[name]; skip {
				continue
			}
		}
		merged = append(merged, entry)
	}
	return append(merged, overlay.Set...)
}

type execClaudeProcess struct {
	cmd *exec.Cmd
}

func (p execClaudeProcess) PID() int {
	if p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

func (p execClaudeProcess) Wait() error {
	if p.cmd == nil {
		return nil
	}
	if err := p.cmd.Wait(); err != nil {
		return fmt.Errorf("waiting for Claude Code: %w", err)
	}
	return nil
}

func (p execClaudeProcess) Stop() error {
	if p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	if err := p.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("stopping Claude Code: %w", err)
	}
	return nil
}
