package responses

import (
	"bytes"
	"encoding/json"
)

// Request is the supported subset of a POST /v1/responses request.
type Request struct {
	Model              string            `json:"model"`
	Input              []InputItem       `json:"input"`
	PreviousResponseID string            `json:"previous_response_id,omitempty"`
	Instructions       string            `json:"instructions,omitempty"`
	MaxOutputTokens    int               `json:"max_output_tokens,omitempty"`
	Temperature        *float64          `json:"temperature,omitempty"`
	Stop               []string          `json:"stop,omitempty"`
	Tools              []Tool            `json:"tools,omitempty"`
	ToolChoice         any               `json:"tool_choice,omitempty"`
	ParallelToolCalls  *bool             `json:"parallel_tool_calls,omitempty"`
	Metadata           map[string]string `json:"metadata,omitempty"`
	Reasoning          *Reasoning        `json:"reasoning,omitempty"`
	Text               *Text             `json:"text,omitempty"`
}

// Reasoning configures the Responses reasoning effort when the provider
// supports it.
type Reasoning struct {
	Effort string `json:"effort,omitempty"`
}

// Text configures text output formatting for the Responses API.
type Text struct {
	Format *TextFormat `json:"format,omitempty"`
}

// TextFormat is the supported JSON-schema text format shape.
type TextFormat struct {
	Type   string          `json:"type"`
	Name   string          `json:"name"`
	Schema json.RawMessage `json:"schema"`
	Strict bool            `json:"strict"`
}

// InputItem is a typed Responses input item. The fields used depend on Type:
// message, function_call, function_call_output, computer_call, or
// computer_call_output.
type InputItem struct {
	Type                     string            `json:"type"`
	Role                     string            `json:"role,omitempty"`
	Content                  []Content         `json:"content,omitempty"`
	CallID                   string            `json:"call_id,omitempty"`
	Name                     string            `json:"name,omitempty"`
	Arguments                string            `json:"arguments,omitempty"`
	Output                   any               `json:"output,omitempty"`
	Status                   string            `json:"status,omitempty"`
	Actions                  []json.RawMessage `json:"actions,omitempty"`
	AcknowledgedSafetyChecks json.RawMessage   `json:"acknowledged_safety_checks,omitempty"`
}

// Content is a Responses message content part.
type Content struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Refusal  string `json:"refusal,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	FileID   string `json:"file_id,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// Tool is a Responses tool definition. Native GA computer tools use Type only;
// preview display metadata is accepted by the adapter for validation but is not
// forwarded. Function tools use Type, Name, Description, Parameters, and optional
// Strict.
type Tool struct {
	Type          string          `json:"type"`
	Name          string          `json:"name,omitempty"`
	Description   string          `json:"description,omitempty"`
	Parameters    json.RawMessage `json:"parameters,omitempty"`
	Strict        *bool           `json:"strict,omitempty"`
	DisplayWidth  int             `json:"display_width,omitempty"`
	DisplayHeight int             `json:"display_height,omitempty"`
	Environment   string          `json:"environment,omitempty"`
}

// ComputerScreenshot is the output object for a computer_call_output item.
type ComputerScreenshot struct {
	Type     string `json:"type"`
	ImageURL string `json:"image_url"`
	Detail   string `json:"detail,omitempty"`
}

// Response is the supported subset of a non-stream Responses API response.
type Response struct {
	ID                string             `json:"id"`
	Model             string             `json:"model"`
	Status            string             `json:"status,omitempty"`
	IncompleteDetails *IncompleteDetails `json:"incomplete_details,omitempty"`
	Output            []OutputItem       `json:"output"`
	OutputText        string             `json:"output_text,omitempty"`
	Usage             Usage              `json:"usage"`
	UsageObserved     bool               `json:"-"`
}

func (r *Response) UnmarshalJSON(data []byte) error {
	type wire Response
	var decoded wire
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var presence struct {
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(data, &presence); err != nil {
		return err
	}
	*r = Response(decoded)
	trimmed := bytes.TrimSpace(presence.Usage)
	r.UsageObserved = len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
	return nil
}

// IncompleteDetails describes why a Responses result did not complete.
type IncompleteDetails struct {
	Reason string `json:"reason,omitempty"`
}

// OutputItem is a typed Responses output item used by the reverse adapter.
type OutputItem struct {
	Type                string            `json:"type"`
	ID                  string            `json:"id,omitempty"`
	Role                string            `json:"role,omitempty"`
	Content             []Content         `json:"content,omitempty"`
	CallID              string            `json:"call_id,omitempty"`
	Name                string            `json:"name,omitempty"`
	Arguments           string            `json:"arguments,omitempty"`
	Status              string            `json:"status,omitempty"`
	Action              json.RawMessage   `json:"action,omitempty"`
	Actions             []json.RawMessage `json:"actions,omitempty"`
	PendingSafetyChecks json.RawMessage   `json:"pending_safety_checks,omitempty"`
	Raw                 json.RawMessage   `json:"-"`
}

// UnmarshalJSON keeps the original output item bytes for callers that need to
// inspect an output type newer than this adapter.
func (o *OutputItem) UnmarshalJSON(data []byte) error {
	type wire OutputItem
	var decoded wire
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*o = OutputItem(decoded)
	if len(o.Actions) == 0 && len(o.Action) != 0 {
		o.Actions = []json.RawMessage{append(json.RawMessage(nil), o.Action...)}
	}
	o.Raw = append(o.Raw[:0], data...)
	return nil
}

// Usage carries token counts shared by Responses and Anthropic message shapes.
type Usage struct {
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
}

// AnthropicResponse is the Anthropic-compatible non-stream message returned by
// AnthropicResponseFromResponses.
type AnthropicResponse struct {
	ID           string                  `json:"id"`
	Type         string                  `json:"type"`
	Role         string                  `json:"role"`
	Model        string                  `json:"model"`
	Content      []AnthropicContentBlock `json:"content"`
	StopReason   string                  `json:"stop_reason"`
	StopSequence *string                 `json:"stop_sequence"`
	Usage        Usage                   `json:"usage"`
}

// AnthropicContentBlock is a text or tool_use block in an Anthropic-compatible
// response.
type AnthropicContentBlock struct {
	Type  string `json:"type"`
	Text  string `json:"text,omitempty"`
	ID    string `json:"id,omitempty"`
	Name  string `json:"name,omitempty"`
	Input any    `json:"input,omitempty"`
}

func (block AnthropicContentBlock) MarshalJSON() ([]byte, error) {
	if block.Type == "text" {
		return json.Marshal(struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{
			Type: block.Type,
			Text: block.Text,
		})
	}
	type alias AnthropicContentBlock
	return json.Marshal(alias(block))
}
