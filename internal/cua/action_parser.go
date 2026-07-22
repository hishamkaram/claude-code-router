package cua

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ActionFromResponse converts one native Responses computer action into the
// internal, in-memory-only action representation. Raw is deliberately retained
// only for the selected executor and must never be persisted or logged.
func ActionFromResponse(callID string, raw json.RawMessage) (Action, error) {
	if strings.TrimSpace(callID) == "" {
		return Action{}, fmt.Errorf("computer-use action is missing call id")
	}
	fields, err := responseActionFields(raw)
	if err != nil {
		return Action{}, err
	}
	kind, err := responseActionKind(fields)
	if err != nil {
		return Action{}, err
	}
	action := Action{
		CallID: callID,
		Kind:   kind,
		Raw:    append(json.RawMessage(nil), raw...),
	}
	if err := populateResponseAction(&action, fields); err != nil {
		return Action{}, err
	}
	if err := action.Validate(); err != nil {
		return Action{}, err
	}
	return action, nil
}
