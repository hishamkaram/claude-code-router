//go:build live

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Metadata deliberately excludes result bodies, command text and UUID values.
type liveRealTurnMetadata struct {
	InputMatched        bool
	ModelCommand        bool
	UnrelatedResults    int
	UnattributedResults int
	UnattributedError   bool
	ProtocolViolation   bool
	OutputIDDistinct    bool
	LastUnmatchedBytes  int
	LastUnmatchedModel  bool
}

type liveRealResultReader struct {
	offset     int
	inputUUID  string
	correlated bool
	observed   liveRealTurnMetadata
}

func (reader *liveRealResultReader) metadata() liveRealTurnMetadata {
	return reader.observed
}

func (reader *liveRealResultReader) next(output string) (result string, failed, complete bool) {
	return reader.read(output, false)
}

func (reader *liveRealResultReader) read(output string, final bool) (result string, failed, complete bool) {
	for reader.offset < len(output) {
		line, _, terminated := strings.Cut(output[reader.offset:], "\n")
		if !terminated && !final && (reader.correlated || !json.Valid([]byte(line))) {
			return "", false, false
		}
		reader.offset += len(line)
		if terminated {
			reader.offset++
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		var header struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(line), &header) != nil {
			if reader.correlated {
				reader.observed.ProtocolViolation = true
				return "", true, true
			}
			continue
		}
		if header.Type == "" && reader.correlated {
			reader.observed.ProtocolViolation = true
			return "", true, true
		}
		if header.Type != "result" {
			continue
		}
		var event struct {
			Type             string   `json:"type"`
			Subtype          string   `json:"subtype"`
			Result           string   `json:"result"`
			IsError          bool     `json:"is_error"`
			LocalCommand     string   `json:"local_command"`
			UserMessageUUID  string   `json:"user_message_uuid"`
			UserMessageUUIDs []string `json:"user_message_uuids"`
			UUID             string   `json:"uuid"`
		}
		if json.Unmarshal([]byte(line), &event) != nil {
			if reader.correlated {
				reader.observed.ProtocolViolation = true
				return "", true, true
			}
			continue
		}
		matched := event.UserMessageUUID == reader.inputUUID || slices.Contains(event.UserMessageUUIDs, reader.inputUUID)
		if reader.correlated && (reader.inputUUID == "" || !matched) {
			reader.observed.LastUnmatchedBytes = len(event.Result)
			reader.observed.LastUnmatchedModel = event.LocalCommand == "model"
			if event.IsError || event.Subtype != "success" {
				reader.observed.UnattributedError = true
				return "", true, true
			}
			if event.UserMessageUUID == "" && len(event.UserMessageUUIDs) == 0 {
				reader.observed.UnattributedResults++
			} else {
				reader.observed.UnrelatedResults++
			}
			continue
		}
		reader.observed.InputMatched = reader.inputUUID != "" && matched
		reader.observed.ModelCommand = event.LocalCommand == "model"
		reader.observed.OutputIDDistinct = reader.inputUUID != "" && event.UUID != "" && event.UUID != reader.inputUUID
		failed = event.IsError || reader.correlated && event.Subtype != "success"
		reader.inputUUID = ""
		return event.Result, failed, true
	}
	return "", false, false
}

func liveRealResultFailure(metadata liveRealTurnMetadata) error {
	if metadata.ProtocolViolation {
		return errors.New("native stream-json protocol violation (output withheld)")
	}
	if metadata.UnattributedError {
		return errors.New("unattributed native error result (body withheld)")
	}
	return errors.New("Claude Code returned an error result (body withheld)")
}

func liveRealCorrelatedDrain(run *liveCompactionRun) (liveRealTurnMetadata, error) {
	if run.correlatedResults == nil {
		return liveRealTurnMetadata{}, nil
	}
	reader := run.correlatedResults
	if _, failed, _ := reader.read(run.out.String(), true); failed {
		return reader.metadata(), liveRealResultFailure(reader.metadata())
	}
	return reader.metadata(), nil
}

func liveRealCorrelatedTurn(ctx context.Context, run *liveCompactionRun, input string) (string, liveRealTurnMetadata, error) {
	frame, inputUUID, err := liveRealCorrelatedInput(input)
	if err != nil {
		return "", liveRealTurnMetadata{}, err
	}
	return liveRealTurnWithCorrelation(ctx, run, frame, inputUUID)
}

func liveRealCorrelatedInput(input string) (frame, inputUUID string, resultErr error) {
	line := strings.TrimSuffix(strings.TrimSuffix(input, "\n"), "\r")
	if strings.ContainsAny(line, "\r\n") {
		return "", "", errors.New("correlated native turn requires exactly one user JSON frame")
	}
	var envelope map[string]json.RawMessage
	// Input may contain user data. JSON decoding details must not reach errors.
	if json.Unmarshal([]byte(line), &envelope) != nil || envelope == nil {
		return "", "", errors.New("correlated native turn requires exactly one user JSON frame")
	}
	var kind string
	if json.Unmarshal(envelope["type"], &kind) != nil || kind != "user" {
		return "", "", errors.New("correlated native turn requires a user frame")
	}
	if _, exists := envelope["uuid"]; exists {
		return "", "", errors.New("correlated turn helper owns the input UUID; omit an existing uuid field")
	}
	// This turn owns its outbound client identity; never reuse an earlier ID.
	inputUUID = uuid.NewString()
	encodedUUID, err := json.Marshal(inputUUID)
	if err != nil {
		return "", "", errors.New("encoding native turn identity failed")
	}
	envelope["uuid"] = encodedUUID
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return "", "", errors.New("encoding correlated native input failed")
	}
	return string(encoded) + "\n", inputUUID, nil
}

func TestLiveRealResultReaderRequiresInputIdentity(t *testing.T) {
	const requested = "requested-client-uuid"
	for _, test := range []struct {
		name, output string
		failed       bool
	}{
		{"single join", `{"type":"result","subtype":"success","result":"ACK","user_message_uuid":"requested-client-uuid"}`, false},
		{"batch join", `{"type":"result","subtype":"success","result":"ACK","user_message_uuid":"other","user_message_uuids":["other","requested-client-uuid"]}`, false},
		{"matching error", `{"type":"result","subtype":"error_during_execution","result":"private body","is_error":true,"user_message_uuid":"requested-client-uuid"}`, true},
		{"non-success subtype", `{"type":"result","subtype":"error_max_turns","user_message_uuid":"requested-client-uuid"}`, true},
		{"missing subtype", `{"type":"result","result":"ACK","user_message_uuid":"requested-client-uuid"}`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := liveRealResultReader{inputUUID: requested, correlated: true}
			prefix := "{\"type\":\"result\",\"subtype\":\"success\",\"result\":\"unrelated\",\"user_message_uuid\":\"other\"}\n" +
				"{\"type\":\"result\",\"subtype\":\"success\",\"result\":\"unattributed\",\"uuid\":\"requested-client-uuid\"}\n"
			_, failed, complete := reader.next(prefix + test.output + "\n")
			if !complete || failed != test.failed || !reader.metadata().InputMatched {
				t.Fatal("reader accepted the wrong result or lost matching error status")
			}
			if reader.metadata().UnrelatedResults != 1 || reader.metadata().UnattributedResults != 1 {
				t.Fatal("reader did not classify unrelated results without accepting them")
			}
		})
	}
}

func TestLiveRealResultReaderNeverInfersModernJoinFromMissingFields(t *testing.T) {
	output := `{"type":"result","subtype":"success","result":"unattributed"}` + "\n"
	modern := liveRealResultReader{inputUUID: "requested", correlated: true}
	if _, _, complete := modern.next(output); complete {
		t.Fatal("modern reader silently fell back to serial completion")
	}
	legacy := liveRealResultReader{}
	result, failed, complete := legacy.next(output)
	if !complete || failed || result != "unattributed" || legacy.metadata().InputMatched {
		t.Fatal("explicit legacy strategy changed or claimed UUID attribution")
	}
}

func TestLiveRealResultReaderRetainsFragmentsAndConsumesEventsOnce(t *testing.T) {
	reader := liveRealResultReader{inputUUID: "requested", correlated: true}
	prefix := "{\"type\":\"result\",\"subtype\":\"success\",\"result\":\"background\"}\n"
	partial := prefix + `{"type":"result","subtype":"success","result":"ACK","user_message_uu`
	for range 2 {
		if _, _, complete := reader.next(partial); complete {
			t.Fatal("partial or unattributed result completed the turn")
		}
	}
	result, failed, complete := reader.next(partial + `id":"requested","local_command":"model"}` + "\n")
	if !complete || failed || result != "ACK" || !reader.metadata().ModelCommand || reader.metadata().UnattributedResults != 1 {
		t.Fatal("fragmented matching result or event cursor was handled incorrectly")
	}
}

func TestLiveRealCorrelatedInputPreservesUserFrameAndOwnsIdentity(t *testing.T) {
	input := `{"type":"user","message":{"role":"user","content":"synthetic-private-marker"},"parent_tool_use_id":null}`
	frame, identifier, err := liveRealCorrelatedInput(input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := uuid.Parse(identifier); err != nil || identifier == "old" {
		t.Fatal("correlated turn did not generate a fresh valid UUID")
	}
	var payload struct {
		Type    string `json:"type"`
		UUID    string `json:"uuid"`
		Message struct {
			Role, Content string
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(frame), &payload); err != nil || payload.Type != "user" || payload.UUID != identifier || payload.Message.Role != "user" || payload.Message.Content != "synthetic-private-marker" {
		t.Fatal("correlation changed the user frame")
	}
	for _, invalid := range []string{`synthetic-private-marker`, `{"type":"assistant"}`, input + "\n" + input, `{"type":"user","uuid":"synthetic-private-marker"}`, "\n"} {
		_, _, err := liveRealCorrelatedInput(invalid)
		if err == nil || strings.Contains(err.Error(), "synthetic-private-marker") {
			t.Fatal("invalid correlated frame was accepted or disclosed")
		}
	}
}

func TestLiveFixtureModelPickerInputUUIDCompletion(t *testing.T) {
	for _, failActual := range []bool{false, true} {
		name := "success"
		if failActual {
			name = "error"
		}
		t.Run(name, func(t *testing.T) {
			ctx := liveCompactionContext(t)
			t.Chdir(t.TempDir())
			correlated := liveNativeVersionAtLeast(t, ctx, [3]int{2, 1, 310})
			fixture := newLivePickerCatalogFixture(t, "picker-ordinary-model")
			database := fixture.configure(t, ctx)
			run := startLivePickerCatalogRun(t, ctx, database, fixture)
			turn := func(input string) (string, liveRealTurnMetadata, error) {
				if correlated {
					return liveRealCorrelatedTurn(ctx, run, input)
				}
				result, err := liveRealTurn(ctx, run, input)
				return result, liveRealTurnMetadata{}, err
			}
			selection, metadata, err := turn(liveStreamInput(t, "/model anthropic.ccr.picker-ordinary"))
			if err != nil || !strings.HasPrefix(selection, "Set model to ") {
				t.Fatal("native model command did not complete the requested input")
			}
			if correlated && (!metadata.InputMatched || !metadata.ModelCommand || !metadata.OutputIDDistinct) {
				t.Fatal("native slash-command result lacked the verified input UUID join")
			}
			fixture.assertSelectionCalls(t, livePickerCatalogSupported(t, ctx))
			fixture.turnStarted.Store(true)
			fixture.failActual.Store(failActual)
			result, metadata, err := turn(liveStreamInput(t, "Reply with the synthetic catalog fixture response."))
			if failActual && (err == nil || result != "") || !failActual && (err != nil || strings.TrimSpace(result) != livePickerCatalogResponse) {
				t.Fatal("native actual turn lost its response, error, or body-withholding invariant")
			}
			if correlated && (!metadata.InputMatched || !metadata.OutputIDDistinct) {
				t.Fatal("native normal-turn result lacked the verified input UUID join")
			}
			fixture.assertRealRoute(t)
			if !failActual {
				if _, _, err := run.finish(ctx); err != nil {
					t.Fatalf("finish native input-join fixture: %v", err)
				}
				if _, err := liveRealCorrelatedDrain(run); err != nil {
					t.Fatalf("drain native input-join fixture: %v", err)
				}
			}
		})
	}
}

func TestLiveRealResultReaderDoesNotHideErrorsOrProtocolViolations(t *testing.T) {
	for _, output := range []string{
		`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"synthetic-private-marker"}`,
		`{"type":"result","subtype":"error_max_turns","user_message_uuid":"other","result":"synthetic-private-marker"}`,
		`synthetic-private-marker`,
	} {
		reader := liveRealResultReader{inputUUID: "requested", correlated: true}
		result, failed, complete := reader.next(output + "\n")
		if !complete || !failed || result != "" {
			t.Fatal("modern reader hid a native error or protocol violation")
		}
		if strings.Contains(liveRealResultFailure(reader.metadata()).Error(), "synthetic-private-marker") {
			t.Fatal("native result diagnostic exposed output data")
		}
	}
}

func TestLiveRealResultReaderRetainsLateResultsAcrossTurns(t *testing.T) {
	reader := liveRealResultReader{inputUUID: "first", correlated: true}
	first := `{"type":"result","subtype":"success","result":"first","user_message_uuid":"first"}` + "\n"
	if result, failed, complete := reader.next(first); !complete || failed || result != "first" {
		t.Fatal("first correlated result was lost")
	}
	reader.inputUUID = "second"
	reader.observed = liveRealTurnMetadata{}
	late := `{"type":"result","subtype":"success","result":"retained old IDs","user_message_uuid":"first"}` + "\n"
	second := `{"type":"result","subtype":"success","result":"second","user_message_uuids":["first","second"]}` + "\n"
	if result, failed, complete := reader.next(first + late + second); !complete || failed || result != "second" || reader.metadata().UnrelatedResults != 1 {
		t.Fatal("session cursor dropped late results or rejected legitimate retained IDs")
	}
	errorTail := `{"type":"result","subtype":"error_during_execution","is_error":true}` + "\n"
	if _, failed, complete := reader.read(first+late+second+errorTail, true); !complete || !failed || !reader.metadata().UnattributedError {
		t.Fatal("final drain hid a late native error")
	}
}
