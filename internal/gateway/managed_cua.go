package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strings"

	"github.com/hishamkaram/claude-code-router/internal/cua"
	openairesponses "github.com/hishamkaram/claude-code-router/internal/responses"
)

const maxManagedComputerScreenshotBytes = cua.MaxComputerScreenshotBytes

func (h *handler) createResponses(ctx context.Context, client *openairesponses.Client, request *openairesponses.Request, usesComputer bool) (*openairesponses.Response, error) {
	if !usesComputer {
		response, err := client.Create(ctx, request)
		if err != nil {
			return nil, err
		}
		if statusErr := openairesponses.ValidateStatus(response); statusErr != nil {
			return nil, statusErr
		}
		if calls, _ := computerCalls(response.Output); len(calls) > 0 {
			return nil, responsesComputerUseRequiresManagedExecutorError()
		}
		return response, nil
	}
	if managedErr := h.responsesComputerUseAvailabilityError(usesComputer); managedErr != nil {
		return nil, managedErr
	}
	return h.createManagedComputerResponses(ctx, client, request)
}

func (h *handler) responsesComputerUseAvailabilityError(usesComputer bool) *managedCUAError {
	if !usesComputer {
		return nil
	}
	if h.cfg.ManagedCUA == nil {
		return responsesComputerUseRequiresManagedExecutorError()
	}
	if strings.TrimSpace(h.cfg.ManagedCUAProject) == "" {
		return newManagedCUAError(http.StatusBadGateway, "managed computer-use launch is missing its project identity")
	}
	return nil
}

func (h *handler) createManagedComputerResponses(ctx context.Context, client *openairesponses.Client, request *openairesponses.Request) (*openairesponses.Response, error) {
	current := request
	var totalUsage openairesponses.Usage
	for {
		response, err := client.Create(ctx, current)
		if err != nil {
			return nil, err
		}
		if statusErr := openairesponses.ValidateStatus(response); statusErr != nil {
			return nil, statusErr
		}
		totalUsage.InputTokens += response.Usage.InputTokens
		totalUsage.OutputTokens += response.Usage.OutputTokens
		calls, hasFunction := computerCalls(response.Output)
		if hasFunction && len(calls) > 0 {
			return nil, newManagedCUAError(http.StatusNotImplemented, "managed computer-use response mixed computer and host function calls; no action was executed")
		}
		if len(calls) == 0 {
			response.Usage = totalUsage
			return response, nil
		}
		if strings.TrimSpace(response.ID) == "" {
			return nil, newManagedCUAError(http.StatusBadGateway, "managed computer-use provider response did not include a response id")
		}
		if beginErr := h.cfg.ManagedCUA.BeginTurn(ctx); beginErr != nil {
			return nil, managedCUATurnError(beginErr)
		}
		outputs, err := h.managedComputerOutputs(ctx, calls)
		if err != nil {
			return nil, err
		}
		current = managedComputerFollowUp(request, response.ID, outputs)
	}
}

func computerCalls(items []openairesponses.OutputItem) ([]openairesponses.OutputItem, bool) {
	calls := make([]openairesponses.OutputItem, 0)
	hasFunction := false
	for index := range items {
		switch items[index].Type {
		case "computer_call":
			calls = append(calls, items[index])
		case "function_call":
			hasFunction = true
		}
	}
	return calls, hasFunction
}

func managedComputerFollowUp(initial *openairesponses.Request, previousResponseID string, outputs []openairesponses.InputItem) *openairesponses.Request {
	next := *initial
	next.PreviousResponseID = previousResponseID
	next.Input = outputs
	next.ToolChoice = nil
	return &next
}

func (h *handler) managedComputerOutputs(ctx context.Context, calls []openairesponses.OutputItem) ([]openairesponses.InputItem, error) {
	outputs := make([]openairesponses.InputItem, 0, len(calls))
	for index := range calls {
		output, err := h.managedComputerOutput(ctx, calls[index])
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, output)
	}
	return outputs, nil
}

func (h *handler) managedComputerOutput(ctx context.Context, call openairesponses.OutputItem) (openairesponses.InputItem, error) {
	if strings.TrimSpace(call.CallID) == "" || len(call.Actions) == 0 {
		return openairesponses.InputItem{}, newManagedCUAError(http.StatusBadGateway, "managed computer-use provider response has an invalid computer call")
	}
	hasSafetyChecks, err := pendingSafetyChecksPresent(call.PendingSafetyChecks)
	if err != nil {
		return openairesponses.InputItem{}, newManagedCUAError(http.StatusBadGateway, "managed computer-use provider response has invalid pending safety checks")
	}
	if hasSafetyChecks {
		return openairesponses.InputItem{}, newManagedCUAError(http.StatusNotImplemented, "managed computer-use provider returned pending safety checks that require explicit user approval; no action was executed")
	}
	actions := make([]cua.Action, 0, len(call.Actions)+1)
	for _, raw := range call.Actions {
		action, actionErr := cua.ActionFromResponse(call.CallID, raw)
		if actionErr != nil {
			return openairesponses.InputItem{}, newManagedCUAError(http.StatusNotImplemented, "managed computer-use provider action is unsupported; no fallback was attempted")
		}
		actions = append(actions, action)
	}
	if actions[len(actions)-1].Kind != cua.ActionScreenshot {
		actions = append(actions, cua.Action{
			CallID: call.CallID,
			Kind:   cua.ActionScreenshot,
		})
	}
	observations, err := h.cfg.ManagedCUA.ExecuteActions(ctx, h.cfg.ManagedCUAProject, actions)
	if err != nil {
		failed := len(observations)
		if failed >= len(actions) {
			failed = len(actions) - 1
		}
		return openairesponses.InputItem{}, managedCUAExecutionError(actions[failed].Kind, err)
	}
	observation := observations[len(observations)-1]
	screenshot, err := managedComputerScreenshot(observation)
	if err != nil {
		return openairesponses.InputItem{}, newManagedCUAError(http.StatusBadGateway, "managed computer-use executor did not return a usable screenshot; no fallback was attempted")
	}
	return openairesponses.InputItem{
		Type: "computer_call_output", CallID: call.CallID, Output: screenshot,
	}, nil
}

func pendingSafetyChecksPresent(raw json.RawMessage) (bool, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return false, nil
	}
	var checks []json.RawMessage
	if err := json.Unmarshal(raw, &checks); err != nil {
		return false, err
	}
	if len(checks) == 0 {
		return false, nil
	}
	for _, check := range checks {
		var value map[string]json.RawMessage
		if err := json.Unmarshal(check, &value); err != nil || value == nil {
			return false, fmt.Errorf("safety check must be an object")
		}
	}
	return true, nil
}

type managedCUAError struct {
	status  int
	message string
}

func (err *managedCUAError) Error() string {
	if err == nil {
		return ""
	}
	return err.message
}

func newManagedCUAError(status int, message string) *managedCUAError {
	return &managedCUAError{status: status, message: message}
}

func responsesComputerUseRequiresManagedExecutorError() *managedCUAError {
	return newManagedCUAError(
		http.StatusNotImplemented,
		"OpenAI Responses computer use requires a CCR managed CUA executor; relaunch with --ccr-cua-mode managed and --ccr-cua-executor <executor>. No action was executed.",
	)
}

func managedCUATurnError(err error) error {
	switch {
	case errors.Is(err, cua.ErrTurnLimit):
		return newManagedCUAError(http.StatusTooManyRequests, "managed computer-use exceeded the launch turn safety limit; no fallback was attempted")
	case errors.Is(err, context.DeadlineExceeded):
		return newManagedCUAError(http.StatusGatewayTimeout, "managed computer-use turn timed out before execution; no fallback was attempted")
	case errors.Is(err, context.Canceled), errors.Is(err, cua.ErrManagedClosed):
		return newManagedCUAError(http.StatusServiceUnavailable, "managed computer-use turn stopped before execution; no fallback was attempted")
	default:
		return newManagedCUAError(http.StatusBadGateway, "managed computer-use turn could not be started; no fallback was attempted")
	}
}

func managedCUAExecutionError(action cua.ActionKind, err error) error {
	switch {
	case errors.Is(err, cua.ErrApprovalDeny):
		return newManagedCUAError(http.StatusForbidden, fmt.Sprintf("managed computer-use action %q was denied; no fallback was attempted", action))
	case errors.Is(err, cua.ErrActionLimit), errors.Is(err, cua.ErrTurnLimit):
		return newManagedCUAError(http.StatusTooManyRequests, fmt.Sprintf("managed computer-use action %q exceeded the launch safety limit; no fallback was attempted", action))
	case errors.Is(err, context.DeadlineExceeded):
		return newManagedCUAError(http.StatusGatewayTimeout, fmt.Sprintf("managed computer-use action %q timed out; no fallback was attempted", action))
	case errors.Is(err, context.Canceled), errors.Is(err, cua.ErrManagedClosed):
		return newManagedCUAError(http.StatusServiceUnavailable, fmt.Sprintf("managed computer-use action %q stopped before completion; no fallback was attempted", action))
	default:
		return newManagedCUAError(http.StatusBadGateway, fmt.Sprintf("managed computer-use executor could not complete action %q; no fallback was attempted", action))
	}
}

func managedComputerScreenshot(observation cua.Observation) (openairesponses.ComputerScreenshot, error) {
	if len(observation.Screenshot) == 0 {
		return openairesponses.ComputerScreenshot{}, fmt.Errorf("managed computer-use executor returned no screenshot")
	}
	if len(observation.Screenshot) > maxManagedComputerScreenshotBytes {
		return openairesponses.ComputerScreenshot{}, fmt.Errorf("managed computer-use screenshot exceeds the base64-safe 32 MiB gateway limit")
	}
	mediaType, _, err := mime.ParseMediaType(observation.ContentType)
	if err != nil || !supportedImageMediaType(mediaType) {
		return openairesponses.ComputerScreenshot{}, fmt.Errorf("managed computer-use executor returned an unsupported screenshot content type")
	}
	return openairesponses.ComputerScreenshot{
		Type:     "computer_screenshot",
		ImageURL: "data:" + normalizeImageMediaType(mediaType) + ";base64," + base64.StdEncoding.EncodeToString(observation.Screenshot),
		Detail:   "original",
	}, nil
}
