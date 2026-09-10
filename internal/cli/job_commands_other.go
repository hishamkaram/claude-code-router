//go:build !linux && !darwin

package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

func unsupportedJobs() error { return fmt.Errorf("detached jobs require Linux or macOS") }
func newJobCancelCommand(context.Context) *cobra.Command {
	return &cobra.Command{Use: "cancel <job_id>", Args: cobra.ExactArgs(1), RunE: func(*cobra.Command, []string) error { return unsupportedJobs() }}
}
func newJobOwnerCommand(context.Context, Dependencies) *cobra.Command {
	return &cobra.Command{Use: "_job-owner", Hidden: true, RunE: func(*cobra.Command, []string) error { return unsupportedJobs() }}
}
func newJobExecCommand() *cobra.Command {
	return &cobra.Command{Use: "_job-exec", Hidden: true, RunE: func(*cobra.Command, []string) error { return unsupportedJobs() }}
}
func runJobStatus(*cobra.Command, string, bool) error { return unsupportedJobs() }
func runPromptLaunch(context.Context, *cobra.Command, *options, Dependencies, launchInvocation, []string) error {
	return unsupportedJobs()
}
