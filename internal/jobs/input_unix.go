//go:build linux || darwin

package jobs

import (
	"fmt"
	"os"
)

type processInput struct {
	read  *os.File
	write *os.File
	done  chan struct{}
}

func newProcessInput(data []byte) (*processInput, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("opening workload input: %w", err)
	}
	p := &processInput{read: r, write: w, done: make(chan struct{})}
	go func() {
		defer close(p.done)
		defer func() { _ = w.Close() }()
		_, _ = w.Write(data)
	}()
	return p, nil
}

func (p *processInput) closeRead() { _ = p.read.Close() }

func (p *processInput) close() {
	_ = p.write.Close()
	p.closeRead()
	<-p.done
}
