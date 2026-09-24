package gateway

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type agentChildResponseWriter func([]byte) (int, error)

func (writer agentChildResponseWriter) Write(data []byte) (int, error) {
	return writer(data)
}

type agentChildHTTPResponseWriter struct {
	header http.Header
	write  func([]byte) (int, error)
}

func (writer *agentChildHTTPResponseWriter) Header() http.Header {
	if writer.header == nil {
		writer.header = make(http.Header)
	}
	return writer.header
}

func (*agentChildHTTPResponseWriter) WriteHeader(int) {}

func (writer *agentChildHTTPResponseWriter) Write(data []byte) (int, error) {
	return writer.write(data)
}

func TestCopyJSONProviderResponseRegistersChildrenBeforeWriting(t *testing.T) {
	var registered bool
	var written bool
	var gotDescriptors []agentChildDescriptor
	var rolledBack bool

	raw := []byte(`{"model":"provider-model","content":[{"type":"tool_use","name":"Agent","input":{"prompt":"child task","description":"child task"}}]}`)
	_, err := copyJSONProviderResponseBody(
		agentChildResponseWriter(func(data []byte) (int, error) {
			written = true
			if !registered {
				return 0, errors.New("child reservation was not registered before response write")
			}
			if !bytes.Contains(data, []byte(`"model":"provider-model"`)) {
				return 0, errors.New("response model was unexpectedly rewritten")
			}
			return len(data), nil
		}),
		bytes.NewReader(raw),
		"provider-model",
		func(descriptors []agentChildDescriptor) (func(), error) {
			registered = true
			gotDescriptors = descriptors
			return func() { rolledBack = true }, nil
		},
	)
	if err != nil {
		t.Fatalf("copyJSONProviderResponseBody() error = %v", err)
	}
	if !registered || !written || rolledBack || len(gotDescriptors) != 1 {
		t.Fatalf("registration state = registered:%v written:%v rolledBack:%v descriptors:%#v", registered, written, rolledBack, gotDescriptors)
	}
}

func TestCopyJSONProviderResponseNormalizesAgentInputBeforeWriting(t *testing.T) {
	var got []byte
	_, err := copyJSONProviderResponseBody(
		agentChildResponseWriter(func(data []byte) (int, error) {
			got = append([]byte(nil), data...)
			return len(data), nil
		}),
		bytes.NewReader([]byte(`{"content":[{"type":"tool_use","name":"Agent","input":{"prompt":"research routing","agent_type":"general-purpose","model":"haiku","provider_only":"drop-me"}}]}`)),
		"provider-model",
		func(descriptors []agentChildDescriptor) (func(), error) {
			if len(descriptors) != 1 || descriptors[0].model != "haiku" || descriptors[0].description != "research routing" {
				t.Fatalf("normalized descriptors = %#v", descriptors)
			}
			return nil, nil
		},
	)
	if err != nil {
		t.Fatalf("copyJSONProviderResponseBody() error = %v", err)
	}
	for _, want := range []string{`"description":"research routing"`, `"subagent_type":"general-purpose"`, `"model":"haiku"`} {
		if !bytes.Contains(got, []byte(want)) {
			t.Fatalf("normalized response missing %s: %s", want, got)
		}
	}
	for _, forbidden := range []string{`"agent_type"`, `"provider_only"`} {
		if bytes.Contains(got, []byte(forbidden)) {
			t.Fatalf("normalized response retained %s: %s", forbidden, got)
		}
	}
}

func TestCopyJSONProviderResponsePreservesNativeChildInputWithoutCCRHandler(t *testing.T) {
	var got []byte
	raw := []byte(`{"model":"claude-sonnet","content":[{"type":"tool_use","name":"Agent","input":{"prompt":"resume work","description":"resume work","resume":"native-child-id","custom_native_option":true}}]}`)
	_, err := copyJSONProviderResponseBody(
		agentChildResponseWriter(func(data []byte) (int, error) {
			got = append([]byte(nil), data...)
			return len(data), nil
		}),
		bytes.NewReader(raw),
		"claude-sonnet",
		nil,
	)
	if err != nil {
		t.Fatalf("copyJSONProviderResponse() error = %v", err)
	}
	for _, want := range []string{`"resume":"native-child-id"`, `"custom_native_option":true`} {
		if !bytes.Contains(got, []byte(want)) {
			t.Fatalf("native child response lost %s: %s", want, got)
		}
	}
}

func TestCopyJSONProviderResponseIgnoresChildShapesInsideToolInput(t *testing.T) {
	var got []byte
	registered := -1
	raw := []byte(`{"content":[{"type":"tool_use","name":"Bash","input":{"command":"inspect","metadata":{"type":"tool_use","name":"Agent","input":{"prompt":"not a child"}}}}]}`)
	_, err := copyJSONProviderResponseBody(
		agentChildResponseWriter(func(data []byte) (int, error) {
			got = append([]byte(nil), data...)
			return len(data), nil
		}),
		bytes.NewReader(raw),
		"provider-model",
		func(descriptors []agentChildDescriptor) (func(), error) {
			registered = len(descriptors)
			return nil, nil
		},
	)
	if err != nil {
		t.Fatalf("copyJSONProviderResponseBody() error = %v", err)
	}
	if registered != 0 {
		t.Fatalf("nested child-shaped tool input created %d reservations", registered)
	}
	if !bytes.Contains(got, []byte(`"name":"Agent"`)) || !bytes.Contains(got, []byte(`"prompt":"not a child"`)) {
		t.Fatalf("provider tool input was unexpectedly rewritten: %s", got)
	}
}

func TestCopyJSONProviderResponseRollsBackChildrenWhenWritingFails(t *testing.T) {
	var rolledBack bool
	_, err := copyJSONProviderResponseBody(
		agentChildResponseWriter(func([]byte) (int, error) {
			return 0, errors.New("client disconnected")
		}),
		bytes.NewReader([]byte(`{"content":[{"type":"tool_use","name":"Task","input":{"prompt":"child task"}}]}`)),
		"provider-model",
		func(descriptors []agentChildDescriptor) (func(), error) {
			if len(descriptors) != 1 {
				t.Fatalf("registered descriptors = %#v, want one", descriptors)
			}
			return func() { rolledBack = true }, nil
		},
	)
	if err == nil {
		t.Fatal("copyJSONProviderResponseBody() error = nil, want write error")
	}
	if !rolledBack {
		t.Fatal("child reservation was not rolled back after response write failure")
	}
}

func TestCopyJSONProviderResponseConvertsMalformedChildToolToVisibleText(t *testing.T) {
	var got []byte
	var registered int
	_, err := copyJSONProviderResponseBody(
		agentChildResponseWriter(func(data []byte) (int, error) {
			got = append([]byte(nil), data...)
			return len(data), nil
		}),
		bytes.NewReader([]byte(`{"content":[{"type":"tool_use","name":"Task","input":{}}]}`)),
		"provider-model",
		func(descriptors []agentChildDescriptor) (func(), error) {
			registered = len(descriptors)
			return nil, nil
		},
	)
	if err != nil {
		t.Fatalf("copyJSONProviderResponseBody() error = %v", err)
	}
	if registered != 0 {
		t.Fatalf("registered malformed child descriptors = %d, want zero", registered)
	}
	if bytes.Contains(got, []byte(`"type":"tool_use"`)) || bytes.Contains(got, []byte(`"name":"Task"`)) {
		t.Fatalf("malformed child tool was exposed: %s", got)
	}
	if !bytes.Contains(got, []byte("invalid Task tool input")) || !bytes.Contains(got, []byte("subagent was not started")) {
		t.Fatalf("missing visible compatibility error: %s", got)
	}
	if !bytes.Contains(got, []byte(`"stop_reason":"end_turn"`)) {
		t.Fatalf("malformed child response retained tool-use stop reason: %s", got)
	}
}

func TestCopyJSONProviderResponseConvertsMalformedWorkflowToVisibleText(t *testing.T) {
	var got []byte
	var registered int
	_, err := copyJSONProviderResponseBody(
		agentChildResponseWriter(func(data []byte) (int, error) {
			got = append([]byte(nil), data...)
			return len(data), nil
		}),
		bytes.NewReader([]byte(`{"content":[{"type":"tool_use","name":"Workflow","input":{}}]}`)),
		"provider-model",
		func(descriptors []agentChildDescriptor) (func(), error) {
			registered = len(descriptors)
			return nil, nil
		},
	)
	if err != nil {
		t.Fatalf("copyJSONProviderResponseBody() error = %v", err)
	}
	if registered != 0 {
		t.Fatalf("registered malformed Workflow descriptors = %d, want zero", registered)
	}
	if bytes.Contains(got, []byte(`"type":"tool_use"`)) || bytes.Contains(got, []byte(`"name":"Workflow"`)) {
		t.Fatalf("malformed Workflow tool was exposed: %s", got)
	}
	if !bytes.Contains(got, []byte("invalid Workflow tool input")) || !bytes.Contains(got, []byte("workflow was not started")) {
		t.Fatalf("missing visible Workflow compatibility error: %s", got)
	}
	if !bytes.Contains(got, []byte(`"stop_reason":"end_turn"`)) {
		t.Fatalf("malformed Workflow response retained tool-use stop reason: %s", got)
	}
}

func TestCopyJSONProviderResponseRejectsUncorrelatedChildOutput(t *testing.T) {
	selection := newActiveModelSelection("active")
	var got []byte
	_, err := copyJSONProviderResponseBody(
		agentChildResponseWriter(func(data []byte) (int, error) {
			got = append([]byte(nil), data...)
			return len(data), nil
		}),
		bytes.NewReader([]byte(`{"id":"msg-uncorrelated","content":[{"type":"tool_use","name":"Agent","input":{"prompt":"child task","description":"child task"}}]}`)),
		"provider-model",
		func(descriptors []agentChildDescriptor) (func(), error) {
			return selection.registerAgentChildDescriptorsAtAliasChecked("", "active", descriptors)
		},
	)
	if err != nil {
		t.Fatalf("copyJSONProviderResponseBody() error = %v", err)
	}
	if bytes.Contains(got, []byte(`"type":"tool_use"`)) || bytes.Contains(got, []byte(`"name":"Agent"`)) {
		t.Fatalf("uncorrelated child tool was exposed: %s", got)
	}
	if !bytes.Contains(got, []byte("session correlation")) || !bytes.Contains(got, []byte("child was not started")) {
		t.Fatalf("missing visible correlation error: %s", got)
	}
	if bytes.Contains(got, []byte(`"stop_reason":"tool_use"`)) {
		t.Fatalf("uncorrelated child response retained tool-use stop reason: %s", got)
	}
}

func TestCopyJSONProviderResponseRejectsUncorrelatedChildOutputInArray(t *testing.T) {
	selection := newActiveModelSelection("active")
	var got []byte
	_, err := copyJSONProviderResponseBody(
		agentChildResponseWriter(func(data []byte) (int, error) {
			got = append([]byte(nil), data...)
			return len(data), nil
		}),
		bytes.NewReader([]byte(`[{"type":"tool_use","name":"Task","input":{"prompt":"child task","description":"child task"}}]`)),
		"provider-model",
		func(descriptors []agentChildDescriptor) (func(), error) {
			return selection.registerAgentChildDescriptorsAtAliasChecked("", "active", descriptors)
		},
	)
	if err != nil {
		t.Fatalf("copyJSONProviderResponseBody() error = %v", err)
	}
	if bytes.Contains(got, []byte(`"type":"tool_use"`)) || bytes.Contains(got, []byte(`"name":"Task"`)) {
		t.Fatalf("uncorrelated child tool was exposed from array: %s", got)
	}
	if !bytes.Contains(got, []byte("session correlation")) {
		t.Fatalf("missing visible correlation refusal: %s", got)
	}
}

func TestCopyProviderResponseRegistersChildrenFromUnexpectedJSONContentType(t *testing.T) {
	var registered bool
	var written bool
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/plain"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(`{"content":[{"type":"tool_use","name":"Task","input":{"prompt":"child task"}}]}`))),
	}
	_, err := copyProviderResponseBody(
		&agentChildHTTPResponseWriter{write: func(data []byte) (int, error) {
			written = true
			if !registered {
				return 0, errors.New("child reservation was not registered before unexpected-content-type response write")
			}
			if !bytes.Contains(data, []byte(`"name":"Task"`)) {
				return 0, errors.New("child tool was not forwarded")
			}
			return len(data), nil
		}},
		response,
		"provider-model",
		func(descriptors []agentChildDescriptor) (func(), error) {
			registered = len(descriptors) == 1 && descriptors[0].prompt == "child task"
			return nil, nil
		},
		false,
	)
	if err != nil {
		t.Fatalf("copyProviderResponseBody() error = %v", err)
	}
	if !registered || !written {
		t.Fatalf("registration state = registered:%v written:%v", registered, written)
	}
}

func TestCopyProviderResponseRegistersChildrenFromSSEBeforeWriting(t *testing.T) {
	var registered bool
	var written bool
	raw := []byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"name\":\"Agent\",\"input\":{}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"prompt\\\":\\\"child task\\\",\\\"description\\\":\\\"child task\\\"}\"}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(bytes.NewReader(raw)),
	}
	_, err := copyProviderResponseBody(
		&agentChildHTTPResponseWriter{write: func(data []byte) (int, error) {
			written = true
			if !registered {
				return 0, errors.New("child reservation was not registered before SSE response write")
			}
			if !bytes.Contains(data, []byte(`event: message_stop`)) {
				return 0, errors.New("SSE response was not forwarded")
			}
			return len(data), nil
		}},
		response,
		"provider-model",
		func(descriptors []agentChildDescriptor) (func(), error) {
			registered = len(descriptors) == 1 && descriptors[0].prompt == "child task"
			return nil, nil
		},
		false,
	)
	if err != nil {
		t.Fatalf("copyProviderResponseBody() error = %v", err)
	}
	if !registered || !written {
		t.Fatalf("registration state = registered:%v written:%v", registered, written)
	}
}

func TestCopyProviderResponseNormalizesAgentInputFromSSEBeforeWriting(t *testing.T) {
	var got []byte
	raw := []byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"name\":\"Agent\",\"input\":{}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"prompt\\\":\\\"research routing\\\",\\\"agent_type\\\":\\\"general-purpose\\\",\\\"model\\\":\\\"haiku\\\",\\\"provider_only\\\":\\\"drop-me\\\"}\"}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(bytes.NewReader(raw)),
	}
	_, err := copyProviderResponseBody(
		&agentChildHTTPResponseWriter{write: func(data []byte) (int, error) {
			got = append([]byte(nil), data...)
			return len(data), nil
		}},
		response,
		"provider-model",
		func(descriptors []agentChildDescriptor) (func(), error) {
			if len(descriptors) != 1 || descriptors[0].model != "haiku" || descriptors[0].description != "research routing" {
				t.Fatalf("normalized SSE descriptors = %#v", descriptors)
			}
			return nil, nil
		},
		false,
	)
	if err != nil {
		t.Fatalf("copyProviderResponseBody() error = %v", err)
	}
	for _, want := range []string{`\"description\":\"research routing\"`, `\"subagent_type\":\"general-purpose\"`, `\"model\":\"haiku\"`} {
		if !bytes.Contains(got, []byte(want)) {
			t.Fatalf("normalized SSE response missing %s: %s", want, got)
		}
	}
	for _, forbidden := range []string{`\"agent_type\"`, `\"provider_only\"`} {
		if bytes.Contains(got, []byte(forbidden)) {
			t.Fatalf("normalized SSE response retained %s: %s", forbidden, got)
		}
	}
}

func TestCopyProviderResponseSanitizesMalformedChildFromSSE(t *testing.T) {
	var got []byte
	raw := []byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"name\":\"Task\",\"input\":{}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"model\\\":\\\"haiku\\\"}\"}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(bytes.NewReader(raw)),
	}
	registered := -1
	_, err := copyProviderResponseBody(
		&agentChildHTTPResponseWriter{write: func(data []byte) (int, error) {
			got = append([]byte(nil), data...)
			return len(data), nil
		}},
		response,
		"provider-model",
		func(descriptors []agentChildDescriptor) (func(), error) {
			registered = len(descriptors)
			return nil, nil
		},
		false,
	)
	if err != nil {
		t.Fatalf("copyProviderResponseBody() error = %v", err)
	}
	if registered != 0 {
		t.Fatalf("registered malformed SSE child descriptors = %d, want zero", registered)
	}
	if bytes.Contains(got, []byte(`\"type\":\"tool_use\"`)) || bytes.Contains(got, []byte(`\"name\":\"Task\"`)) {
		t.Fatalf("malformed SSE child tool was exposed: %s", got)
	}
	if !bytes.Contains(got, []byte("invalid Task tool input")) || !bytes.Contains(got, []byte("subagent was not started")) {
		t.Fatalf("missing visible malformed-child compatibility error: %s", got)
	}
}

func TestCopyProviderResponseDoesNotRegisterDiscardedMixedSSEChildren(t *testing.T) {
	raw := []byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"name\":\"Agent\",\"input\":{}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"prompt\\\":\\\"valid child\\\",\\\"description\\\":\\\"valid child\\\"}\"}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"name\":\"Task\",\"input\":{}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"model\\\":\\\"haiku\\\"}\"}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(bytes.NewReader(raw)),
	}
	registered := -1
	var got []byte
	_, err := copyProviderResponseBody(
		&agentChildHTTPResponseWriter{write: func(data []byte) (int, error) {
			got = append(got, data...)
			return len(data), nil
		}},
		response,
		"provider-model",
		func(descriptors []agentChildDescriptor) (func(), error) {
			registered = len(descriptors)
			return nil, nil
		},
		false,
	)
	if err != nil {
		t.Fatalf("copyProviderResponse() error = %v", err)
	}
	if registered != 0 {
		t.Fatalf("registered discarded mixed child descriptors = %d, want zero", registered)
	}
	if bytes.Contains(got, []byte(`\"type\":\"tool_use\"`)) || bytes.Contains(got, []byte(`\"name\":\"Agent\"`)) {
		t.Fatalf("discarded mixed child tools were exposed: %s", got)
	}
	if !bytes.Contains(got, []byte("invalid Task tool input")) {
		t.Fatalf("missing visible mixed-child compatibility error: %s", got)
	}
}

func TestCopyProviderResponseBoundsBufferedSSE(t *testing.T) {
	tooLarge := strings.NewReader(strings.Repeat("x", int(maxAnthropicStreamBytes)+1))
	_, err := copySSEProviderResponseBody(io.Discard, tooLarge, "provider-model", nil, false)
	if err == nil || !strings.Contains(err.Error(), "provider response exceeds") {
		t.Fatalf("copySSEProviderResponseBody() error = %v, want bounded response error", err)
	}
}

func TestCopyProviderResponseSurfacesBufferedSSEFailures(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{
			name: "provider error event",
			raw:  "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}\n\n",
		},
		{
			name: "missing message stop",
			raw:  "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_incomplete\"}}\n\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var got []byte
			response := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(test.raw)),
			}
			_, err := copyProviderResponseBody(
				&agentChildHTTPResponseWriter{write: func(data []byte) (int, error) {
					got = append(got, data...)
					return len(data), nil
				}},
				response,
				"provider-model",
				nil,
				false,
			)
			if err == nil {
				t.Fatal("copyProviderResponseBody() error = nil, want visible provider failure")
			}
			if !bytes.Contains(got, []byte("No successful turn was exposed")) ||
				!bytes.Contains(got, []byte("event: message_stop")) {
				t.Fatalf("buffered failure response was not visible and complete: %s", got)
			}
		})
	}
}

type failingProviderBody struct{}

func (failingProviderBody) Read([]byte) (int, error) {
	return 0, errors.New("provider body read failed")
}

func TestCopyProviderResponseSurfacesBufferedJSONReadFailure(t *testing.T) {
	var got []byte
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(failingProviderBody{}),
	}
	_, err := copyProviderResponseBody(
		&agentChildHTTPResponseWriter{write: func(data []byte) (int, error) {
			got = append(got, data...)
			return len(data), nil
		}},
		response,
		"provider-model",
		nil,
		false,
	)
	if err == nil || !strings.Contains(err.Error(), "provider body read failed") {
		t.Fatalf("copyProviderResponseBody() error = %v, want provider read failure", err)
	}
	if !bytes.Contains(got, []byte(`"type":"message"`)) ||
		!bytes.Contains(got, []byte("No successful turn was exposed")) {
		t.Fatalf("buffered JSON read failure was not visible as a complete response: %s", got)
	}
}
