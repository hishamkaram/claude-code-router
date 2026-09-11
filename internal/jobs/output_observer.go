package jobs

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// OutputObserver follows direct file output independently of lifecycle hooks.
// The owner must Stop it after workload cleanup, before committing output. Its
// single goroutine owns its reader and publishes state only through that barrier.
type OutputObserver struct {
	cancel context.CancelFunc
	done   chan struct{}
	result StreamResult
	err    error
}

func StartOutputObserver(ctx context.Context, path, session, model string, cancelWorkload context.CancelFunc) (*OutputObserver, error) {
	f, err := openOutputFile(path)
	if err != nil {
		return nil, fmt.Errorf("opening job output observer: %w", err)
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, &OutputError{ReasonCode: ReasonObservationFailed}
	}
	observeCtx, cancel := context.WithCancel(ctx)
	observer := &OutputObserver{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(observer.done)
		defer cancel()
		defer func() { _ = f.Close() }()
		parser := outputParser{expectedSession: session, expectedModel: model}
		observer.err = followOutput(observeCtx, f, &parser)
		observer.result = parser.result
		if observer.err != nil && cancelWorkload != nil {
			cancelWorkload()
		}
	}()
	return observer, nil
}

func followOutput(ctx context.Context, f *os.File, parser *outputParser) error {
	reader := bufio.NewReaderSize(f, 64<<10)
	var pending []byte
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return nil
		}
		part, err := reader.ReadSlice('\n')
		if len(pending)+len(part) > maxOutputEventBytes {
			return &OutputError{ReasonCode: ReasonObservationFailed}
		}
		pending = append(pending, part...)
		if err == nil {
			if parseErr := parser.consume(pending); parseErr != nil {
				return parseErr
			}
			pending = pending[:0]
			continue
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if !errors.Is(err, io.EOF) {
			return &OutputError{ReasonCode: ReasonObservationFailed}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Stop joins the observer. It cannot establish a successful result: the caller
// still has to validate and commit the final file prefix after cleanup.
func (o *OutputObserver) Stop() (StreamResult, error) {
	o.cancel()
	<-o.done
	return o.result, o.err
}
