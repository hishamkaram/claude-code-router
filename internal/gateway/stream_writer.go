package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/observability"
)

const (
	streamStartupWindow   = 4 * time.Second
	streamHeartbeatWindow = 4 * time.Second
)

// streamFailure contains only a safe, client-visible provider failure. Provider
// workers must redact details before constructing it.
type streamFailure struct {
	statusCode int
	errorClass string
	message    string
}

type upstreamStreamEventKind uint8

const (
	upstreamStreamReady upstreamStreamEventKind = iota + 1
	upstreamStreamData
	upstreamStreamDone
	upstreamStreamFailed
)

type upstreamStreamEvent struct {
	kind     upstreamStreamEventKind
	status   int
	headers  http.Header
	streamID int
	sseEvent string
	data     []byte
	usage    *observability.TokenUsage
	failure  *streamFailure
}

type translatedStreamAdapter interface {
	Start(*anthropicSSEWriter) error
	Consume(*anthropicSSEWriter, upstreamStreamEvent) error
	Finish(*anthropicSSEWriter) (observability.TokenUsage, error)
}

type translatedStreamReadyAdapter interface {
	Ready(*anthropicSSEWriter, http.Header) error
}

type translatedStreamResult struct {
	Usage                  observability.TokenUsage
	Committed              bool
	HTTPStatus             int
	UpstreamHTTPStatus     int
	ErrorClass             string
	Message                string
	UpstreamHeadersMS      int64
	FirstUpstreamEventMS   int64
	FirstDownstreamEventMS int64
	UpstreamEventCount     int64
	EarlyStreamCommit      bool
	TerminalPhase          string
}

type anthropicSSEWriter struct {
	w            http.ResponseWriter
	started      bool
	terminal     bool
	eventWritten bool
	lastWrite    time.Time
}

func newAnthropicSSEWriter(w http.ResponseWriter) *anthropicSSEWriter {
	return &anthropicSSEWriter{w: w}
}

func (s *anthropicSSEWriter) Start() error {
	if s.started {
		return nil
	}
	s.w.Header().Set("Content-Type", "text/event-stream")
	s.w.Header().Set("Cache-Control", "no-cache")
	s.w.Header().Set("Connection", "keep-alive")
	s.w.WriteHeader(http.StatusOK)
	s.started = true
	return nil
}

func (s *anthropicSSEWriter) Event(event string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encoding SSE event %q: %w", event, err)
	}
	if err := s.writeFrame([]byte("event: " + event + "\ndata: " + string(data) + "\n\n")); err != nil {
		return err
	}
	s.eventWritten = true
	return nil
}

func (s *anthropicSSEWriter) RawEvent(event string, data []byte) error {
	event = strings.TrimSpace(event)
	if event == "" || strings.ContainsAny(event, "\r\n") {
		return fmt.Errorf("invalid SSE event name %q", event)
	}
	if len(data) == 0 {
		return fmt.Errorf("SSE event %q has no data", event)
	}
	var frame bytes.Buffer
	frame.WriteString("event: ")
	frame.WriteString(event)
	frame.WriteByte('\n')
	for _, line := range bytes.Split(data, []byte("\n")) {
		frame.WriteString("data: ")
		frame.Write(line)
		frame.WriteByte('\n')
	}
	frame.WriteByte('\n')
	if err := s.writeFrame(frame.Bytes()); err != nil {
		return err
	}
	s.eventWritten = true
	return nil
}

func (s *anthropicSSEWriter) Comment(comment string) error {
	return s.writeFrame([]byte(": " + comment + "\n\n"))
}

func (s *anthropicSSEWriter) Ping() error {
	return s.Event("ping", map[string]string{"type": "ping"})
}

func (s *anthropicSSEWriter) Error(message string) error {
	return s.ErrorWithType("api_error", message)
}

func (s *anthropicSSEWriter) ErrorWithType(errorType, message string) error {
	if s.terminal {
		return nil
	}
	s.terminal = true
	return s.Event("error", map[string]any{
		"type": "error",
		"error": map[string]string{
			"type":    errorType,
			"message": message,
		},
	})
}

func (s *anthropicSSEWriter) writeFrame(frame []byte) error {
	if !s.started {
		return errors.New("writing SSE frame before stream start")
	}
	if _, err := s.w.Write(frame); err != nil {
		return fmt.Errorf("writing SSE frame: %w", err)
	}
	if err := http.NewResponseController(s.w).Flush(); err != nil {
		return fmt.Errorf("flushing SSE frame: %w", err)
	}
	s.lastWrite = time.Now()
	return nil
}

func runTranslatedProviderStream(
	ctx context.Context,
	w http.ResponseWriter,
	producer func(context.Context, chan<- upstreamStreamEvent),
	adapter translatedStreamAdapter,
) translatedStreamResult {
	return runTranslatedProviderStreamWithTiming(ctx, w, producer, adapter, streamStartupWindow, streamHeartbeatWindow)
}

func runTranslatedProviderStreamWithTiming(
	ctx context.Context,
	w http.ResponseWriter,
	producer func(context.Context, chan<- upstreamStreamEvent),
	adapter translatedStreamAdapter,
	startupWindow, heartbeatWindow time.Duration,
) translatedStreamResult {
	streamCtx, cancel := context.WithCancel(ctx)
	events := make(chan upstreamStreamEvent, 16)
	var worker sync.WaitGroup
	worker.Add(1)
	go func() {
		defer worker.Done()
		defer close(events)
		producer(streamCtx, events)
	}()
	defer func() {
		cancel()
		worker.Wait()
	}()

	startup := time.NewTimer(startupWindow)
	defer startup.Stop()
	heartbeat := time.NewTicker(heartbeatWindow)
	defer heartbeat.Stop()
	coordinator := translatedStreamCoordinator{
		ctx:          ctx,
		writer:       newAnthropicSSEWriter(w),
		adapter:      adapter,
		startedAt:    time.Now(),
		readyStreams: make(map[int]struct{}),
	}
	return coordinator.run(events, startup.C, heartbeat.C, heartbeatWindow)
}

type translatedStreamCoordinator struct {
	ctx                          context.Context
	writer                       *anthropicSSEWriter
	adapter                      translatedStreamAdapter
	startedAt                    time.Time
	result                       translatedStreamResult
	upstreamReady                bool
	readyStreams                 map[int]struct{}
	upstreamHeadersRecorded      bool
	firstUpstreamEventRecorded   bool
	firstDownstreamEventRecorded bool
}

func (c *translatedStreamCoordinator) run(
	events <-chan upstreamStreamEvent,
	startup <-chan time.Time,
	heartbeat <-chan time.Time,
	heartbeatWindow time.Duration,
) translatedStreamResult {
	for {
		select {
		case <-c.ctx.Done():
			return c.canceled()
		case <-startup:
			if failure := c.startStream(); failure != nil {
				return c.fail(*failure)
			}
		case <-heartbeat:
			if failure := c.heartbeat(heartbeatWindow); failure != nil {
				return c.fail(*failure)
			}
		case event, open := <-events:
			complete, failure := c.handleEvent(event, open)
			if failure != nil {
				return c.fail(*failure)
			}
			if complete {
				return c.result
			}
		}
	}
}

func (c *translatedStreamCoordinator) canceled() translatedStreamResult {
	c.result.ErrorClass = "canceled"
	c.result.Message = c.ctx.Err().Error()
	c.result.TerminalPhase = "canceled"
	return c.result
}

func (c *translatedStreamCoordinator) heartbeat(window time.Duration) *streamFailure {
	if !c.writer.started || c.writer.terminal || (!c.writer.lastWrite.IsZero() && time.Since(c.writer.lastWrite) < window) {
		return nil
	}
	if !c.writer.eventWritten {
		if err := c.writer.Comment("ccr-keepalive"); err != nil {
			return downstreamDisconnect(err)
		}
		c.recordDownstreamEvent()
		return nil
	}
	if err := c.writer.Ping(); err != nil {
		return downstreamDisconnect(err)
	}
	c.recordDownstreamEvent()
	return nil
}

func (c *translatedStreamCoordinator) handleEvent(event upstreamStreamEvent, open bool) (bool, *streamFailure) {
	if !open {
		return false, &streamFailure{errorClass: "provider_stream", message: "provider stream ended before a completion event"}
	}
	switch event.kind {
	case upstreamStreamReady:
		return false, c.handleReady(event)
	case upstreamStreamData:
		return false, c.handleData(event)
	case upstreamStreamDone:
		return c.handleDone()
	case upstreamStreamFailed:
		if event.failure == nil {
			return false, &streamFailure{errorClass: "provider_stream", message: "provider stream failed"}
		}
		return false, event.failure
	default:
		return false, &streamFailure{errorClass: "provider_protocol", message: "provider emitted an unknown stream event"}
	}
}

func (c *translatedStreamCoordinator) handleReady(event upstreamStreamEvent) *streamFailure {
	if _, exists := c.readyStreams[event.streamID]; exists {
		return &streamFailure{errorClass: "provider_protocol", message: "provider emitted duplicate response headers"}
	}
	c.readyStreams[event.streamID] = struct{}{}
	c.upstreamReady = true
	c.recordUpstreamHeaders(event.status)
	if event.status < http.StatusOK || event.status >= http.StatusMultipleChoices {
		return &streamFailure{statusCode: event.status, errorClass: "provider_http", message: "provider returned an unsuccessful response"}
	}
	if !c.writer.started {
		if readyAdapter, ok := c.adapter.(translatedStreamReadyAdapter); ok {
			if err := readyAdapter.Ready(c.writer, event.headers); err != nil {
				return &streamFailure{errorClass: "provider_protocol", message: err.Error()}
			}
		}
	}
	return c.startStream()
}

func (c *translatedStreamCoordinator) handleData(event upstreamStreamEvent) *streamFailure {
	if _, ready := c.readyStreams[event.streamID]; !ready {
		return &streamFailure{errorClass: "provider_protocol", message: "provider emitted stream data before response headers"}
	}
	c.recordUpstreamEvent()
	if failure := c.startStream(); failure != nil {
		return failure
	}
	if err := c.adapter.Consume(c.writer, event); err != nil {
		return &streamFailure{errorClass: "provider_protocol", message: err.Error()}
	}
	c.recordDownstreamEvent()
	return nil
}

func (c *translatedStreamCoordinator) handleDone() (bool, *streamFailure) {
	if !c.upstreamReady {
		return false, &streamFailure{errorClass: "provider_stream", message: "provider stream ended before response headers"}
	}
	if failure := c.startStream(); failure != nil {
		return false, failure
	}
	usage, err := c.adapter.Finish(c.writer)
	if err != nil {
		return false, &streamFailure{errorClass: "provider_protocol", message: err.Error()}
	}
	c.result.Usage = usage
	c.result.TerminalPhase = "completed"
	return true, nil
}

func (c *translatedStreamCoordinator) startStream() *streamFailure {
	if err := c.start(); err != nil {
		return downstreamDisconnect(err)
	}
	return nil
}

func (c *translatedStreamCoordinator) start() error {
	if c.writer.started {
		return nil
	}
	if err := c.writer.Start(); err != nil {
		return err
	}
	c.result.EarlyStreamCommit = !c.upstreamReady
	// WriteHeader commits the HTTP response even when the first protocol
	// event cannot be written. Callers must never attempt a second response.
	c.result.Committed = true
	c.result.HTTPStatus = http.StatusOK
	if err := c.adapter.Start(c.writer); err != nil {
		return err
	}
	c.recordDownstreamEvent()
	return nil
}

func (c *translatedStreamCoordinator) recordUpstreamHeaders(status int) {
	if !c.upstreamHeadersRecorded {
		c.upstreamHeadersRecorded = true
		c.result.UpstreamHeadersMS = time.Since(c.startedAt).Milliseconds()
	}
	c.result.UpstreamHTTPStatus = status
}

func (c *translatedStreamCoordinator) recordUpstreamEvent() {
	c.result.UpstreamEventCount++
	if c.firstUpstreamEventRecorded {
		return
	}
	c.firstUpstreamEventRecorded = true
	c.result.FirstUpstreamEventMS = time.Since(c.startedAt).Milliseconds()
}

func (c *translatedStreamCoordinator) recordDownstreamEvent() {
	if c.firstDownstreamEventRecorded || c.writer.lastWrite.IsZero() {
		return
	}
	c.firstDownstreamEventRecorded = true
	c.result.FirstDownstreamEventMS = time.Since(c.startedAt).Milliseconds()
}

func (c *translatedStreamCoordinator) fail(failure streamFailure) translatedStreamResult {
	c.result.ErrorClass = failure.errorClass
	c.result.Message = failure.message
	if failure.statusCode != 0 {
		c.result.UpstreamHTTPStatus = failure.statusCode
	}
	if !c.writer.started {
		c.result.HTTPStatus = failure.statusCode
		c.result.TerminalPhase = "failed"
		return c.result
	}
	if err := c.writer.Error(failure.message); err != nil {
		c.result.ErrorClass = "downstream_disconnect"
		c.result.Message = err.Error()
		c.result.TerminalPhase = "downstream_disconnect"
		return c.result
	}
	c.recordDownstreamEvent()
	c.result.TerminalPhase = "failed"
	return c.result
}

func downstreamDisconnect(err error) *streamFailure {
	return &streamFailure{errorClass: "downstream_disconnect", message: err.Error()}
}

func sendUpstreamStreamEvent(ctx context.Context, events chan<- upstreamStreamEvent, event upstreamStreamEvent) bool {
	select {
	case events <- event:
		return true
	case <-ctx.Done():
		return false
	}
}
