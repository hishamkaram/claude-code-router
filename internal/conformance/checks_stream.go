package conformance

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// The text probe must complete with visible text, not just well-framed SSE.
func requireTextStream(body []byte) error {
	var probe textStreamProbe
	var data strings.Builder
	consume := func() error {
		if data.Len() == 0 {
			return nil
		}
		err := probe.consume(data.String())
		data.Reset()
		return err
	}
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 4096), 8<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := consume(); err != nil {
				return err
			}
		} else if value, ok := strings.CutPrefix(line, "data:"); ok {
			data.WriteString(strings.TrimPrefix(value, " "))
			data.WriteByte('\n')
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading Anthropic probe stream: %w", err)
	}
	if err := consume(); err != nil {
		return err
	}
	if !probe.started || !probe.stopped || !probe.text {
		return fmt.Errorf("anthropic stream omitted a complete non-empty text response")
	}
	return nil
}

type textStreamProbe struct {
	started, stopped, text bool
}

func (p *textStreamProbe) consume(data string) error {
	var event struct {
		Type         string `json:"type"`
		ContentBlock struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content_block"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	}
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		return fmt.Errorf("invalid Anthropic stream event JSON")
	}
	switch event.Type {
	case "error":
		return fmt.Errorf("anthropic stream reported an error")
	case "message_start":
		if p.started {
			return fmt.Errorf("unexpected Anthropic message_start")
		}
		p.started = true
	case "content_block_start", "content_block_delta":
		if !p.started || p.stopped {
			return fmt.Errorf("anthropic stream content outside message")
		}
		p.text = p.text || (event.ContentBlock.Type == "text" && strings.TrimSpace(event.ContentBlock.Text) != "") ||
			(event.Delta.Type == "text_delta" && strings.TrimSpace(event.Delta.Text) != "")
	case "message_stop":
		if !p.started || p.stopped {
			return fmt.Errorf("unexpected Anthropic message_stop")
		}
		p.stopped = true
	}
	return nil
}
