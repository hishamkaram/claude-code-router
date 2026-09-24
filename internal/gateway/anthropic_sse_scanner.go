package gateway

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strings"
)

type anthropicSSEFrame struct {
	event string
	data  []byte
}

type anthropicSSEScanner struct {
	scanner   *bufio.Scanner
	limited   *io.LimitedReader
	eventName string
	eventData bytes.Buffer
	maxBytes  int64
	finished  bool
}

func newAnthropicSSEScanner(body io.Reader) *anthropicSSEScanner {
	limited := &io.LimitedReader{R: body, N: maxAnthropicStreamBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 64<<10), int(maxAnthropicStreamBytes+1))
	return &anthropicSSEScanner{scanner: scanner, limited: limited, maxBytes: maxAnthropicStreamBytes}
}

func (s *anthropicSSEScanner) next() (anthropicSSEFrame, bool, error) {
	if s.finished {
		return anthropicSSEFrame{}, false, nil
	}
	for s.scanner.Scan() {
		line := trimSSELineTerminator(s.scanner.Bytes())
		if len(line) == 0 {
			if frame, ok := s.take(); ok {
				return frame, true, nil
			}
			continue
		}
		s.appendLine(line)
	}
	s.finished = true
	if s.limited.N <= 0 {
		return anthropicSSEFrame{}, false, fmt.Errorf("provider stream exceeds the %d byte limit", s.maxBytes)
	}
	if err := s.scanner.Err(); err != nil {
		return anthropicSSEFrame{}, false, fmt.Errorf("reading provider stream: %w", err)
	}
	if frame, ok := s.take(); ok {
		return frame, true, nil
	}
	return anthropicSSEFrame{}, false, nil
}

func (s *anthropicSSEScanner) appendLine(line []byte) {
	if bytes.HasPrefix(line, []byte("event:")) {
		s.eventName = strings.TrimSpace(string(bytes.TrimPrefix(line, []byte("event:"))))
		return
	}
	if !bytes.HasPrefix(line, []byte("data:")) {
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

func (s *anthropicSSEScanner) take() (anthropicSSEFrame, bool) {
	data := bytes.Clone(bytes.TrimSpace(s.eventData.Bytes()))
	event := s.eventName
	s.eventData.Reset()
	s.eventName = ""
	if len(data) == 0 {
		return anthropicSSEFrame{}, false
	}
	return anthropicSSEFrame{event: event, data: data}, true
}
