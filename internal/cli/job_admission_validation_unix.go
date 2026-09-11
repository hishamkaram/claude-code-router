//go:build linux || darwin

package cli

import "github.com/hishamkaram/claude-code-router/internal/jobs"

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
