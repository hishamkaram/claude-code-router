package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

const claudeProcessStopTimeout = 5 * time.Second

func waitForClaudeProcess(
	ctx context.Context,
	process ClaudeProcess,
	notices <-chan string,
	noticeOut io.Writer,
) (waitErr, stopErr error) {
	done := process.Done()
	if notices == nil {
		return <-done, nil
	}
	for {
		select {
		case notice := <-notices:
			if notice != "" {
				fmt.Fprintln(noticeOut, notice)
			}
		case waitErr = <-done:
			drainSubscriptionPoolNotices(notices, noticeOut)
			return waitErr, nil
		case <-ctx.Done():
			stopErr = stopClaudeProcessAndWait(process, done, claudeProcessStopTimeout)
			return ctx.Err(), stopErr
		}
	}
}

func drainSubscriptionPoolNotices(notices <-chan string, out io.Writer) {
	for {
		select {
		case notice := <-notices:
			if notice != "" {
				fmt.Fprintln(out, notice)
			}
		default:
			return
		}
	}
}

func stopClaudeProcessAndWait(process ClaudeProcess, done <-chan error, timeout time.Duration) error {
	stopErr := process.Stop()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return stopErr
	case <-timer.C:
		return errors.Join(stopErr, fmt.Errorf("timed out after %s waiting for Claude Code to stop", timeout))
	}
}
