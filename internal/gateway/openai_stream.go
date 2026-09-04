package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"sort"
	"strings"
)

const maxOpenAIChatResponseBytes int64 = 8 << 20

type openAIChatStreamChunk struct {
	ID      string                   `json:"id"`
	Model   string                   `json:"model"`
	Choices []openAIChatStreamChoice `json:"choices"`
	Usage   *openAIChatUsage         `json:"usage"`
	Error   json.RawMessage          `json:"error"`
}

type openAIChatStreamChoice struct {
	Index        int                   `json:"index"`
	Delta        openAIChatStreamDelta `json:"delta"`
	FinishReason string                `json:"finish_reason"`
}

type openAIChatStreamDelta struct {
	Content      *string                    `json:"content"`
	ToolCalls    []openAIChatStreamToolCall `json:"tool_calls"`
	FunctionCall *openAIFunctionCall        `json:"function_call"`
}

type openAIChatStreamToolCall struct {
	Index    int                `json:"index"`
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function openAIFunctionCall `json:"function"`
}

type openAIChatStreamAccumulator struct {
	response openAIChatResponse
	choices  map[int]*openAIChatChoiceAccumulator
}

type openAIChatChoiceAccumulator struct {
	message           openAIChatResponseMessage
	content           strings.Builder
	contentObserved   bool
	functionArguments strings.Builder
	finishReason      string
	toolCalls         map[int]*openAIToolCallAccumulator
}

type openAIToolCallAccumulator struct {
	call      openAIToolCall
	arguments strings.Builder
}

type openAISSEScanner struct {
	scanner   *bufio.Scanner
	limited   *io.LimitedReader
	eventData bytes.Buffer
	maxBytes  int64
	finished  bool
}

func decodeOpenAIChatProviderResponse(resp *http.Response, streamRequested bool, apiKey string) (openAIChatResponse, error) {
	if streamRequested && isOpenAIEventStream(resp.Header.Get("Content-Type")) {
		decoded, err := decodeOpenAIChatStream(resp.Body, maxOpenAIChatResponseBytes, apiKey)
		if err != nil {
			return openAIChatResponse{}, fmt.Errorf("decoding streamed OpenAI-compatible provider response: %w", err)
		}
		return decoded, nil
	}

	var decoded openAIChatResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxOpenAIChatResponseBytes)).Decode(&decoded); err != nil {
		return openAIChatResponse{}, fmt.Errorf("decoding OpenAI-compatible provider response: %w", err)
	}
	return decoded, nil
}

func isOpenAIEventStream(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	return err == nil && strings.EqualFold(mediaType, "text/event-stream")
}

func decodeOpenAIChatStream(body io.Reader, maxBytes int64, apiKey string) (openAIChatResponse, error) {
	if maxBytes <= 0 {
		return openAIChatResponse{}, fmt.Errorf("stream response byte limit must be positive")
	}
	events := newOpenAISSEScanner(body, maxBytes)
	accumulator := openAIChatStreamAccumulator{choices: make(map[int]*openAIChatChoiceAccumulator)}
	sawDone := false
	for {
		data, ok, err := events.next()
		if err != nil {
			return openAIChatResponse{}, err
		}
		if !ok {
			break
		}
		if bytes.Equal(data, []byte("[DONE]")) {
			sawDone = true
			break
		}
		if err := accumulator.consume(data, apiKey); err != nil {
			return openAIChatResponse{}, err
		}
	}
	return accumulator.finalize(sawDone)
}

func newOpenAISSEScanner(body io.Reader, maxBytes int64) *openAISSEScanner {
	limited := &io.LimitedReader{R: body, N: maxBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 64<<10), int(maxBytes+1))
	return &openAISSEScanner{scanner: scanner, limited: limited, maxBytes: maxBytes}
}

func (s *openAISSEScanner) next() (data []byte, ok bool, err error) {
	if s.finished {
		return nil, false, nil
	}
	for s.scanner.Scan() {
		line := s.scanner.Bytes()
		if len(line) > 0 {
			s.appendDataLine(line)
			continue
		}
		if data := s.takeData(); len(data) > 0 {
			return data, true, nil
		}
	}
	s.finished = true
	if err := s.scanError(); err != nil {
		return nil, false, err
	}
	if data := s.takeData(); len(data) > 0 {
		return data, true, nil
	}
	return nil, false, nil
}

func (s *openAISSEScanner) appendDataLine(line []byte) {
	if line[0] == ':' || !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	data := bytes.TrimPrefix(line, []byte("data:"))
	if len(data) > 0 && data[0] == ' ' {
		data = data[1:]
	}
	if s.eventData.Len() > 0 {
		s.eventData.WriteByte('\n')
	}
	_, _ = s.eventData.Write(data)
}

func (s *openAISSEScanner) takeData() []byte {
	data := bytes.Clone(bytes.TrimSpace(s.eventData.Bytes()))
	s.eventData.Reset()
	return data
}

func (s *openAISSEScanner) scanError() error {
	if s.limited.N <= 0 {
		return fmt.Errorf("provider stream exceeds the %d byte limit", s.maxBytes)
	}
	if err := s.scanner.Err(); err != nil {
		return fmt.Errorf("reading provider stream: %w", err)
	}
	return nil
}

func (a *openAIChatStreamAccumulator) consume(data []byte, apiKey string) error {
	var chunk openAIChatStreamChunk
	if err := json.Unmarshal(data, &chunk); err != nil {
		return fmt.Errorf("decoding provider stream event: %w", err)
	}
	if raw := bytes.TrimSpace(chunk.Error); len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
		detail := sanitizeProviderErrorDetail(raw, apiKey)
		if detail == "" {
			return fmt.Errorf("provider stream reported an error")
		}
		return fmt.Errorf("provider stream reported an error: %s", detail)
	}
	if a.response.ID == "" {
		a.response.ID = chunk.ID
	}
	if a.response.Model == "" {
		a.response.Model = chunk.Model
	}
	if chunk.Usage != nil {
		a.response.Usage = *chunk.Usage
		a.response.usageObserved = true
	}
	for _, choice := range chunk.Choices {
		a.choice(choice.Index).consume(choice)
	}
	return nil
}

func (a *openAIChatStreamAccumulator) choice(index int) *openAIChatChoiceAccumulator {
	choice := a.choices[index]
	if choice == nil {
		choice = &openAIChatChoiceAccumulator{toolCalls: make(map[int]*openAIToolCallAccumulator)}
		a.choices[index] = choice
	}
	return choice
}

func (a *openAIChatChoiceAccumulator) consume(chunk openAIChatStreamChoice) {
	if chunk.Delta.Content != nil {
		a.contentObserved = true
		a.content.WriteString(*chunk.Delta.Content)
	}
	for _, streamed := range chunk.Delta.ToolCalls {
		toolCall := a.toolCalls[streamed.Index]
		if toolCall == nil {
			toolCall = &openAIToolCallAccumulator{}
			a.toolCalls[streamed.Index] = toolCall
		}
		if toolCall.call.ID == "" {
			toolCall.call.ID = streamed.ID
		}
		if toolCall.call.Type == "" {
			toolCall.call.Type = streamed.Type
		}
		toolCall.call.Function.Name = appendStableStreamValue(toolCall.call.Function.Name, streamed.Function.Name)
		toolCall.arguments.WriteString(streamed.Function.Arguments)
	}
	if streamed := chunk.Delta.FunctionCall; streamed != nil {
		if a.message.FunctionCall == nil {
			a.message.FunctionCall = &openAIFunctionCall{}
		}
		a.message.FunctionCall.Name = appendStableStreamValue(a.message.FunctionCall.Name, streamed.Name)
		a.functionArguments.WriteString(streamed.Arguments)
	}
	if chunk.FinishReason != "" {
		a.finishReason = chunk.FinishReason
	}
}

func appendStableStreamValue(current, fragment string) string {
	if fragment == "" || fragment == current || strings.HasSuffix(current, fragment) {
		return current
	}
	if strings.HasPrefix(fragment, current) {
		return fragment
	}
	return current + fragment
}

func (a *openAIChatStreamAccumulator) finalize(sawDone bool) (openAIChatResponse, error) {
	if len(a.choices) == 0 {
		return openAIChatResponse{}, fmt.Errorf("provider stream returned no choices")
	}
	indexes := make([]int, 0, len(a.choices))
	for index := range a.choices {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	allChoicesFinished := true
	for _, index := range indexes {
		choice := a.choices[index]
		if choice.contentObserved {
			content := choice.content.String()
			choice.message.Content = &content
		}
		if choice.message.FunctionCall != nil {
			choice.message.FunctionCall.Arguments = choice.functionArguments.String()
		}
		toolIndexes := make([]int, 0, len(choice.toolCalls))
		for toolIndex := range choice.toolCalls {
			toolIndexes = append(toolIndexes, toolIndex)
		}
		sort.Ints(toolIndexes)
		for _, toolIndex := range toolIndexes {
			toolCall := choice.toolCalls[toolIndex]
			toolCall.call.Function.Arguments = toolCall.arguments.String()
			choice.message.ToolCalls = append(choice.message.ToolCalls, toolCall.call)
		}
		a.response.Choices = append(a.response.Choices, openAIChatChoice{
			Message:      choice.message,
			FinishReason: choice.finishReason,
		})
		allChoicesFinished = allChoicesFinished && choice.finishReason != ""
	}
	if !sawDone && !allChoicesFinished {
		return openAIChatResponse{}, fmt.Errorf("provider stream ended before a completion event")
	}
	return a.response, nil
}
