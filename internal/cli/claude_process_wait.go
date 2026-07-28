package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/gateway"
)

const claudeProcessStopTimeout = 5 * time.Second

func waitForClaudeProcess(
	ctx context.Context,
	process ClaudeProcess,
	exhaustion subscriptionExhaustionControl,
	noticeOut io.Writer,
) (waitErr error, exhausted *gateway.AnthropicSubscriptionExhaustionEvent, stopErr error) {
	done := process.Done()
	if exhaustion.events == nil {
		return <-done, nil, nil
	}
	shouldRotate := func(event gateway.AnthropicSubscriptionExhaustionEvent) bool {
		return exhaustion.handle == nil || exhaustion.handle(noticeOut, event)
	}
	for {
		select {
		case event := <-exhaustion.events:
			if !shouldRotate(event) {
				continue
			}
			stopErr = stopClaudeProcessAndWait(process, done, claudeProcessStopTimeout)
			return nil, &event, stopErr
		case waitErr = <-done:
			select {
			case event := <-exhaustion.events:
				if shouldRotate(event) {
					return nil, &event, nil
				}
			default:
			}
			return waitErr, nil, nil
		case <-ctx.Done():
			stopErr = stopClaudeProcessAndWait(process, done, claudeProcessStopTimeout)
			return ctx.Err(), nil, stopErr
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
