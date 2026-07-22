package executor

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/cua"
)

var ErrUnsupportedAction = errors.New("unsupported computer-use executor action")

// UnsupportedActionError reports a CUA action the executor cannot perform.
type UnsupportedActionError struct {
	Executor string
	Action   cua.ActionKind
	Reason   string
}

func (err *UnsupportedActionError) Error() string {
	if err == nil {
		return ErrUnsupportedAction.Error()
	}
	if err.Reason == "" {
		return fmt.Sprintf("%s does not support computer-use action %q", err.Executor, err.Action)
	}
	return fmt.Sprintf("%s does not support computer-use action %q: %s", err.Executor, err.Action, err.Reason)
}

func (err *UnsupportedActionError) Unwrap() error {
	return ErrUnsupportedAction
}

func unsupportedAction(executorName string, action cua.ActionKind, reason string) error {
	return &UnsupportedActionError{Executor: executorName, Action: action, Reason: reason}
}

func waitDuration(raw json.RawMessage) time.Duration {
	if len(raw) == 0 {
		return time.Second
	}
	var payload struct {
		DurationMS   int `json:"duration_ms"`
		TimeoutMS    int `json:"timeout_ms"`
		Milliseconds int `json:"milliseconds"`
		MS           int `json:"ms"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return time.Second
	}
	milliseconds := payload.DurationMS
	if milliseconds == 0 {
		milliseconds = payload.TimeoutMS
	}
	if milliseconds == 0 {
		milliseconds = payload.Milliseconds
	}
	if milliseconds == 0 {
		milliseconds = payload.MS
	}
	if milliseconds <= 0 {
		return time.Second
	}
	duration := time.Duration(milliseconds) * time.Millisecond
	if duration > 30*time.Second {
		return 30 * time.Second
	}
	return duration
}
