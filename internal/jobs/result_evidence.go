package jobs

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

const maxOutputEventBytes = 16 << 20

// StreamResult contains only the result validated from one bounded log prefix.
type StreamResult struct {
	SessionID   string
	Model       string
	Text        string
	Successful  bool
	Initialized bool
}

type outputEvent struct {
	Type      string  `json:"type"`
	Subtype   string  `json:"subtype"`
	SessionID string  `json:"session_id"`
	Model     string  `json:"model"`
	Result    *string `json:"result"`
	IsError   *bool   `json:"is_error"`
	NumTurns  *int    `json:"num_turns"`
}

// OutputError deliberately excludes untrusted output text from diagnostics.
type OutputError struct {
	Incomplete        bool
	ReasonCode        string
	ObservedSessionID string
}

func (e *OutputError) Error() string { return "job output evidence: " + e.ReasonCode }

type outputParser struct {
	expectedSession string
	expectedModel   string
	result          StreamResult
	resultSeen      bool
}

func (p *outputParser) consume(line []byte) error {
	var event outputEvent
	if err := json.Unmarshal(line, &event); err != nil || event.Type == "" {
		return &OutputError{ReasonCode: ReasonObservationFailed}
	}
	if event.SessionID != "" && event.SessionID != p.expectedSession {
		return &OutputError{ReasonCode: ReasonSessionIdentityMismatch, ObservedSessionID: event.SessionID}
	}
	if event.Type == "system" && event.Subtype == "init" {
		return p.consumeInit(event)
	}
	if event.Type == "result" {
		return p.consumeResult(event)
	}
	return nil
}

func (p *outputParser) consumeInit(event outputEvent) error {
	if p.result.Initialized || p.resultSeen || event.SessionID == "" || event.Model == "" {
		return &OutputError{ReasonCode: ReasonObservationFailed}
	}
	if p.expectedModel != "" && event.Model != p.expectedModel {
		return &OutputError{ReasonCode: ReasonObservationFailed}
	}
	p.result.Initialized, p.result.SessionID, p.result.Model = true, event.SessionID, event.Model
	return nil
}

func (p *outputParser) consumeResult(event outputEvent) error {
	if p.resultSeen || event.SessionID == "" || event.IsError == nil || event.Subtype == "" {
		return &OutputError{ReasonCode: ReasonObservationFailed}
	}
	if !p.result.Initialized {
		// Claude emits this structured zero-turn failure before initialization
		// when resume setup fails. Preserve it as incomplete failure evidence;
		// never read its error text or accept it as a successful result.
		if event.Subtype != "error_during_execution" || !*event.IsError || event.NumTurns == nil || *event.NumTurns != 0 {
			return &OutputError{ReasonCode: ReasonObservationFailed}
		}
		p.resultSeen, p.result.SessionID = true, event.SessionID
		return nil
	}
	if event.Subtype == "success" && event.Result == nil {
		return &OutputError{ReasonCode: ReasonObservationFailed}
	}
	p.resultSeen = true
	if event.Result != nil {
		p.result.Text = *event.Result
	}
	p.result.Successful = event.Subtype == "success" && !*event.IsError
	return nil
}

func parseOutput(ctx context.Context, reader io.Reader, session, model string) (StreamResult, error) {
	p := outputParser{expectedSession: session, expectedModel: model}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), maxOutputEventBytes)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return p.result, fmt.Errorf("observing output: %w", err)
		}
		if err := p.consume(scanner.Bytes()); err != nil {
			return p.result, err
		}
	}
	if scanner.Err() != nil {
		return p.result, &OutputError{ReasonCode: ReasonObservationFailed}
	}
	if !p.result.Initialized || !p.resultSeen {
		return p.result, &OutputError{ReasonCode: ReasonObservationFailed, Incomplete: true}
	}
	return p.result, nil
}

// CommitOutput freezes a validated prefix without redirecting workload stdout
// through a pipe. The caller must first finish process cleanup and observation.
func CommitOutput(ctx context.Context, path, session, model string) (ResultEvidence, StreamResult, error) {
	f, err := openOutputFile(path)
	if err != nil {
		return ResultEvidence{}, StreamResult{}, fmt.Errorf("opening terminal output: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return ResultEvidence{}, StreamResult{}, &OutputError{ReasonCode: ReasonObservationFailed}
	}
	evidence := ResultEvidence{Boundary: info.Size(), SessionID: session, Model: model}
	result, digest, err := readOutputPrefix(ctx, f, evidence)
	if err != nil {
		return ResultEvidence{}, result, err
	}
	evidence.SHA256, evidence.Model, evidence.Successful = digest, result.Model, result.Successful
	return evidence, result, nil
}

func readOutputPrefix(ctx context.Context, f *os.File, evidence ResultEvidence) (StreamResult, string, error) {
	if evidence.Boundary < 0 {
		return StreamResult{}, "", &OutputError{ReasonCode: ReasonObservationFailed}
	}
	hash := sha256.New()
	limited := &io.LimitedReader{R: f, N: evidence.Boundary}
	result, err := parseOutput(ctx, io.TeeReader(limited, hash), evidence.SessionID, evidence.Model)
	if err != nil {
		return result, "", err
	}
	if limited.N != 0 {
		return result, "", &OutputError{ReasonCode: ReasonObservationFailed}
	}
	return result, hex.EncodeToString(hash.Sum(nil)), nil
}

// ReadCommittedOutput parses and hashes the very same bytes. It never validates
// a file and then reopens changing bytes for interpretation. Appends are ignored.
func ReadCommittedOutput(ctx context.Context, path string, evidence ResultEvidence) (StreamResult, error) {
	f, err := openOutputFile(path)
	if err != nil {
		return StreamResult{}, fmt.Errorf("opening committed output: %w", err)
	}
	defer func() { _ = f.Close() }()
	result, digest, err := readOutputPrefix(ctx, f, evidence)
	if err != nil {
		return StreamResult{}, err
	}
	if digest != evidence.SHA256 || result.Successful != evidence.Successful {
		return StreamResult{}, &OutputError{ReasonCode: ReasonObservationFailed}
	}
	return result, nil
}
