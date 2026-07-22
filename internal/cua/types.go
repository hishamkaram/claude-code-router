// Package cua defines the launch-scoped computer-use execution boundary.
package cua

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	DefaultMaxTurns   = 50
	DefaultMaxActions = 200
	DefaultTimeout    = 10 * time.Minute
	// MaxComputerScreenshotBytes leaves room for the data-URL encoding inside
	// CCR's 32 MiB gateway request limit.
	MaxComputerScreenshotBytes = ((32 << 20) - len("data:image/webp;base64,")) / 4 * 3
)

type Mode string

const (
	ModeClient  Mode = "client"
	ModeManaged Mode = "managed"
)

type ExecutorKind string

const (
	ExecutorDocker       ExecutorKind = "docker"
	ExecutorLocalBrowser ExecutorKind = "local-browser"
	ExecutorMacOSPreview ExecutorKind = "macos-preview"
	ExecutorExternal     ExecutorKind = "external"
)

type Config struct {
	Mode       Mode
	Executor   string
	MaxTurns   int
	MaxActions int
	Timeout    time.Duration
}

func (c Config) Normalize() (Config, error) {
	if c.Mode == "" {
		c.Mode = ModeClient
	}
	if c.Mode != ModeClient && c.Mode != ModeManaged {
		return Config{}, fmt.Errorf("invalid CUA mode %q; expected client or managed", c.Mode)
	}
	if c.MaxTurns == 0 {
		c.MaxTurns = DefaultMaxTurns
	}
	if c.MaxActions == 0 {
		c.MaxActions = DefaultMaxActions
	}
	if c.Timeout == 0 {
		c.Timeout = DefaultTimeout
	}
	if c.MaxTurns < 1 {
		return Config{}, fmt.Errorf("CUA max turns must be at least 1")
	}
	if c.MaxActions < 1 {
		return Config{}, fmt.Errorf("CUA max actions must be at least 1")
	}
	if c.Timeout < time.Second {
		return Config{}, fmt.Errorf("CUA timeout must be at least 1s")
	}
	if c.Mode == ModeClient && strings.TrimSpace(c.Executor) != "" {
		return Config{}, fmt.Errorf("CUA executor requires --ccr-cua-mode managed")
	}
	if c.Mode == ModeManaged && strings.TrimSpace(c.Executor) != "" {
		if _, err := ParseExecutor(c.Executor); err != nil {
			return Config{}, err
		}
	}
	return c, nil
}

func ParseExecutor(value string) (ExecutorKind, error) {
	value = strings.TrimSpace(value)
	switch value {
	case string(ExecutorDocker):
		return ExecutorDocker, nil
	case string(ExecutorLocalBrowser):
		return ExecutorLocalBrowser, nil
	case string(ExecutorMacOSPreview):
		return ExecutorMacOSPreview, nil
	}
	if name, found := strings.CutPrefix(value, "external:"); found && strings.TrimSpace(name) != "" {
		return ExecutorExternal, nil
	}
	return "", fmt.Errorf("invalid CUA executor %q; expected docker, local-browser, macos-preview, or external:<name>", value)
}

type ActionKind string

const (
	ActionScreenshot  ActionKind = "screenshot"
	ActionClick       ActionKind = "click"
	ActionDoubleClick ActionKind = "double_click"
	ActionDrag        ActionKind = "drag"
	ActionMove        ActionKind = "move"
	ActionType        ActionKind = "type"
	ActionKeypress    ActionKind = "keypress"
	ActionScroll      ActionKind = "scroll"
	ActionWait        ActionKind = "wait"
)

type Action struct {
	CallID string
	Kind   ActionKind
	X      int
	Y      int
	Text   string
	Keys   []string
	Raw    json.RawMessage
}

func (a Action) Validate() error {
	if strings.TrimSpace(a.CallID) == "" {
		return fmt.Errorf("computer-use action is missing call id")
	}
	switch a.Kind {
	case ActionScreenshot:
		return nil
	case ActionClick, ActionDoubleClick, ActionMove:
		if err := validateActionCoordinates(a.X, a.Y); err != nil {
			return err
		}
		_, err := validateResponseActionMouseKeys(a.Keys)
		return err
	case ActionDrag:
		_, err := validateResponseActionMouseKeys(a.Keys)
		return err
	case ActionType:
		if a.Text == "" || len(a.Text) > maxComputerActionTextBytes {
			return fmt.Errorf("computer-use type action text is invalid")
		}
		return nil
	case ActionKeypress:
		_, err := validateResponseActionKeys(a.Keys)
		return err
	case ActionScroll:
		_, err := validateResponseActionMouseKeys(a.Keys)
		return err
	case ActionWait:
		if len(a.Raw) == 0 {
			return nil
		}
		return validateActionRaw(a.Raw, validateResponseActionWait)
	default:
		return fmt.Errorf("unsupported computer-use action %q", a.Kind)
	}
}

func validateActionCoordinates(x, y int) error {
	if x < 0 || y < 0 || x > maxComputerActionCoordinate || y > maxComputerActionCoordinate {
		return fmt.Errorf("computer-use action coordinate is outside the allowed range")
	}
	return nil
}

func validateActionRaw(raw json.RawMessage, validate func(map[string]json.RawMessage) error) error {
	fields, err := responseActionFields(raw)
	if err != nil {
		return err
	}
	return validate(fields)
}

type Risk string

const (
	RiskLow  Risk = "low"
	RiskHigh Risk = "high"
)

func (a Action) Risk() Risk {
	switch a.Kind {
	case ActionScreenshot, ActionMove, ActionScroll, ActionWait:
		return RiskLow
	default:
		return RiskHigh
	}
}

type ApprovalRequest struct {
	ActionID string
	Kind     ActionKind
	Risk     Risk
	Executor string
}

type Decision string

const (
	DecisionApprove Decision = "approve"
	DecisionDeny    Decision = "deny"
)

type Approver interface {
	Approve(context.Context, ApprovalRequest) (Decision, error)
}

type Executor interface {
	Name() string
	Check(context.Context) error
	Execute(context.Context, Action) (Observation, error)
	Close() error
}

// Observation intentionally carries only in-memory execution output. Callers
// must not persist or log Screenshot, Text, or Raw fields.
type Observation struct {
	Screenshot  []byte
	ContentType string
	Text        string
	Raw         json.RawMessage
}

type AuditEvent struct {
	At       time.Time
	Executor string
	Action   ActionKind
	Risk     Risk
	Decision Decision
	Status   string
}

func UsesComputerTool(rawTools []json.RawMessage) bool {
	for _, raw := range rawTools {
		var tool struct {
			Type        string          `json:"type"`
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"input_schema"`
		}
		if json.Unmarshal(raw, &tool) != nil {
			continue
		}
		if IsNativeComputerTool(tool.Type, tool.Name, tool.InputSchema) {
			return true
		}
	}
	return false
}

// IsNativeComputerTool reports whether an Anthropic tool declaration is a
// native computer-use tool rather than a function coincidentally named
// "computer".
func IsNativeComputerTool(toolType, name string, inputSchema json.RawMessage) bool {
	toolType = strings.ToLower(strings.TrimSpace(toolType))
	if toolType == "computer" || strings.HasPrefix(toolType, "computer_") {
		return true
	}
	return toolType == "" &&
		len(inputSchema) == 0 &&
		strings.EqualFold(strings.TrimSpace(name), "computer")
}
