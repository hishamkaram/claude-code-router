//go:build linux || darwin

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/hishamkaram/claude-code-router/internal/config"
	"github.com/hishamkaram/claude-code-router/internal/jobs"
)

func jobStore() (jobs.Store, error) {
	root, err := config.DefaultDataDir()
	if err != nil {
		return jobs.Store{}, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return jobs.Store{}, fmt.Errorf("resolving job storage: %w", err)
	}
	return jobs.Store{Root: filepath.Join(root, "jobs")}, nil
}

func runJobStatus(cmd *cobra.Command, id string, jsonOutput bool) error {
	s, err := jobStore()
	if err != nil {
		return err
	}
	r, err := s.Status(id)
	if err != nil {
		return err
	}
	return writeJobStatus(cmd, r, jsonOutput)
}

func writeJobStatus(cmd *cobra.Command, r jobs.Record, jsonOutput bool) error {
	r.Cleanup.Normalize()
	if jsonOutput {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(r)
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s status=%s containment=%s cancel_requested=%t\nSession: %s\nLog: %s\nCleanup coverage: %s\nReason: %s\n", r.JobID, r.Status, r.Containment, r.CancelRequested, r.SessionID, r.Log, r.Cleanup.Coverage, r.Cleanup.Reason)
	return err
}

func newJobCancelCommand(ctx context.Context) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{Use: "cancel <job_id>", Short: "Request cancellation from the live job owner", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		s, err := jobStore()
		if err != nil {
			return err
		}
		r, err := s.Cancel(ctx, args[0])
		if err != nil {
			return err
		}
		return writeJobStatus(cmd, r, jsonOutput)
	}}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit job state; cancellation may still be in progress")
	return cmd
}

func runPromptLaunch(ctx context.Context, cmd *cobra.Command, opts *options, deps Dependencies, invocation launchInvocation, args []string) error {
	if !invocation.printMode {
		return fmt.Errorf("--detach and --prompt-file require --print/-p")
	}
	if invocation.promptFile == "" {
		return fmt.Errorf("--detach requires --prompt-file")
	}
	if err := validateLaunchInputs(invocation.modelAlias, invocation.authMode, invocation.claudeAccount, invocation.permissionMode); err != nil {
		return err
	}
	if err := validateLaunchPassthroughArgs(invocation.claudeArgs); err != nil {
		return err
	}
	if invocation.detach {
		for _, arg := range invocation.claudeArgs {
			if arg == "--" {
				return fmt.Errorf("standalone -- is not supported in detached Claude options; use --name=value for option values and --prompt-file for prompt text")
			}
		}
		if option := detachedSessionOption(invocation.claudeArgs); option != "" {
			return fmt.Errorf("%s conflicts with CCR's detached session identity", option)
		}
	}
	prompt, err := readJobPrompt(invocation.promptFile)
	if err != nil {
		return err
	}
	if invocation.detach {
		return launchDetached(ctx, cmd, opts, deps, args, prompt)
	}
	cmd.SetIn(strings.NewReader(prompt))
	return runLaunch(ctx, cmd, opts, deps, invocation)
}

func detachedSessionOption(args []string) string {
	if option := findLaunchOption(args, "--session-id", "--resume", "--continue", "--fork-session", "--no-session-persistence", "--from-pr", "--teleport"); option != "" {
		return option
	}
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if option := detachedShortSessionOption(arg); option != "" {
			return option
		}
	}
	return ""
}

func detachedShortSessionOption(arg string) string {
	if !strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "--") {
		return ""
	}
	for _, option := range arg[1:] {
		switch option {
		case 'c', 'r':
			return "-" + string(option)
		case 'p', 'h', 'v':
			// Claude bundles boolean short options; value-taking options consume the tail.
		default:
			return ""
		}
	}
	return ""
}

func readJobPrompt(path string) (string, error) {
	// A FIFO must be rejected by Stat rather than blocking before validation.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", fmt.Errorf("opening prompt file: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("checking prompt file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("prompt file must be a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, (16<<20)+1))
	if err != nil {
		return "", fmt.Errorf("reading prompt file: %w", err)
	}
	if len(data) > 16<<20 {
		return "", fmt.Errorf("prompt file exceeds 16 MiB")
	}
	if strings.TrimSpace(string(data)) == "" {
		return "", fmt.Errorf("prompt file is empty")
	}
	return string(data), nil
}

func foregroundJobArgs(args []string) []string {
	result := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			return append(result, args[i:]...)
		}
		option, _, inline := strings.Cut(args[i], "=")
		if option == "--detach" {
			continue
		}
		if option == "--prompt-file" {
			if !inline {
				i++
			}
			continue
		}
		result = append(result, args[i])
	}
	return result
}

func newJobExecCommand() *cobra.Command {
	return &cobra.Command{Use: "_job-exec", Hidden: true, Args: cobra.NoArgs, RunE: func(_ *cobra.Command, _ []string) error { return jobs.ExecGated() }}
}
