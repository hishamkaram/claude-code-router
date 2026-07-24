//go:build live

package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type liveSubscriptionPTYLauncher struct {
	inner  livePickerLauncher
	starts chan *liveSubscriptionPTYStart

	mu        sync.Mutex
	allStarts []*liveSubscriptionPTYStart
}

func newLiveSubscriptionPTYLauncher() *liveSubscriptionPTYLauncher {
	return &liveSubscriptionPTYLauncher{
		inner:  livePickerLauncher{started: make(chan livePickerSession, 4)},
		starts: make(chan *liveSubscriptionPTYStart, 4),
	}
}

type liveSubscriptionPTYStart struct {
	Args       []string
	OAuthToken string
	PID        int
	Process    *liveSubscriptionRecordingProcess
	session    livePickerSession
	Transcript *synchronizedBuffer
	readDone   chan error
}

func (s *liveSubscriptionPTYStart) UsesToken(token string) bool {
	return s.OAuthToken == token
}

func (s *liveSubscriptionPTYStart) Write(t *testing.T, input string) {
	t.Helper()
	if _, err := s.session.pty.Write([]byte(input)); err != nil {
		t.Fatalf("writing to real Claude PTY: %v", err)
	}
}

func (s *liveSubscriptionPTYStart) Submit(t *testing.T, input string) {
	t.Helper()
	s.Write(t, input)
	time.Sleep(100 * time.Millisecond)
	s.Write(t, "\r")
}

func (s *liveSubscriptionPTYStart) WaitReady(t *testing.T, ctx context.Context, commandDone <-chan error) {
	t.Helper()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for s.Transcript.String() == "" {
		select {
		case err := <-commandDone:
			t.Fatalf("real Claude process exited before its PTY became ready: %v", err)
		case <-ctx.Done():
			t.Fatalf("waiting for real Claude PTY readiness: %v", ctx.Err())
		case <-ticker.C:
		}
	}
	time.Sleep(250 * time.Millisecond)
}

func (l *liveSubscriptionPTYLauncher) Start(
	ctx context.Context,
	args []string,
	env ClaudeEnvironment,
	in io.Reader,
	out, errOut io.Writer,
) (ClaudeProcess, error) {
	process, err := l.inner.Start(ctx, args, env, in, out, errOut)
	if err != nil {
		return nil, err
	}
	var session livePickerSession
	select {
	case session = <-l.inner.started:
	default:
		return nil, fmt.Errorf("real Claude PTY session was not recorded")
	}
	wrapped := newLiveSubscriptionRecordingProcess(process)
	start := &liveSubscriptionPTYStart{
		Args:       append([]string(nil), args...),
		OAuthToken: environmentEntries(env.Set)["CLAUDE_CODE_OAUTH_TOKEN"],
		PID:        process.PID(),
		Process:    wrapped,
		session:    session,
		Transcript: &synchronizedBuffer{},
		readDone:   make(chan error, 1),
	}
	go func() {
		_, copyErr := io.Copy(start.Transcript, session.pty)
		start.readDone <- copyErr
	}()
	l.mu.Lock()
	l.allStarts = append(l.allStarts, start)
	l.mu.Unlock()
	l.starts <- start
	return wrapped, nil
}

func (l *liveSubscriptionPTYLauncher) WaitStart(
	t *testing.T,
	ctx context.Context,
	commandDone <-chan error,
	commandOut, commandErr *bytes.Buffer,
) *liveSubscriptionPTYStart {
	t.Helper()
	select {
	case start := <-l.starts:
		return start
	case err := <-commandDone:
		t.Fatalf("ccr launch stopped before the expected real Claude start: %v\nstdout:\n%s\nstderr:\n%s\ntranscript:\n%s",
			err,
			redactLiveSubscriptionOutput(commandOut.String()),
			redactLiveSubscriptionOutput(commandErr.String()),
			redactLiveSubscriptionOutput(l.Transcript()))
	case <-ctx.Done():
		t.Fatalf("waiting for real Claude start: %v\nstdout:\n%s\nstderr:\n%s\ntranscript:\n%s",
			ctx.Err(),
			redactLiveSubscriptionOutput(commandOut.String()),
			redactLiveSubscriptionOutput(commandErr.String()),
			redactLiveSubscriptionOutput(l.Transcript()))
	}
	return nil
}

func (l *liveSubscriptionPTYLauncher) Transcript() string {
	l.mu.Lock()
	starts := append([]*liveSubscriptionPTYStart(nil), l.allStarts...)
	l.mu.Unlock()
	var transcript strings.Builder
	for _, start := range starts {
		transcript.WriteString(start.Transcript.String())
	}
	return transcript.String()
}

func (l *liveSubscriptionPTYLauncher) Close() {
	l.mu.Lock()
	starts := append([]*liveSubscriptionPTYStart(nil), l.allStarts...)
	l.mu.Unlock()
	for _, start := range starts {
		_ = start.Process.Stop()
		_ = start.session.pty.Close()
		select {
		case <-start.readDone:
		case <-time.After(time.Second):
		}
	}
}

type liveSubscriptionRecordingProcess struct {
	inner ClaudeProcess
	done  chan error

	mu           sync.Mutex
	stopped      bool
	doneObserved bool
}

func newLiveSubscriptionRecordingProcess(inner ClaudeProcess) *liveSubscriptionRecordingProcess {
	process := &liveSubscriptionRecordingProcess{inner: inner, done: make(chan error, 1)}
	go func() {
		err := <-inner.Done()
		process.mu.Lock()
		process.doneObserved = true
		process.mu.Unlock()
		process.done <- err
		close(process.done)
	}()
	return process
}

func (p *liveSubscriptionRecordingProcess) PID() int {
	return p.inner.PID()
}

func (p *liveSubscriptionRecordingProcess) Done() <-chan error {
	return p.done
}

func (p *liveSubscriptionRecordingProcess) Stop() error {
	p.mu.Lock()
	p.stopped = true
	p.mu.Unlock()
	return p.inner.Stop()
}

func (p *liveSubscriptionRecordingProcess) Stopped() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stopped
}

func (p *liveSubscriptionRecordingProcess) DoneObserved() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.doneObserved
}
