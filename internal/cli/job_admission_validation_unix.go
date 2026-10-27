//go:build linux || darwin

package cli

import (
	"fmt"

	"github.com/hishamkaram/claude-code-router/internal/jobs"
)

func validateDetachedClaudeBoundary(args []string) error {
	for _, arg := range args {
		if arg == "--" {
			return fmt.Errorf("standalone -- is not supported in detached Claude options; use --name=value for option values and --prompt-file for prompt text")
		}
	}
	return nil
}

func validateDetachedAdmission(invocation launchInvocation) error {
	if invocation.submissionID != "" {
		if err := jobs.ValidateSubmissionID(invocation.submissionID); err != nil {
			return err
		}
	}
	if invocation.expectedParent != "" {
		if err := jobs.ValidateID(invocation.expectedParent); err != nil {
			return err
		}
	}
	if invocation.resumeSession != "" {
		if err := jobs.ValidateSessionID(invocation.resumeSession); err != nil {
			return err
		}
		return validateResumeOutput(invocation.claudeArgs)
	}
	return nil
}
