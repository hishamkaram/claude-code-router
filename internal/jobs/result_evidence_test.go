package jobs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const resultFixture = `{"type":"system","subtype":"init","session_id":"session","model":"model"}
{"type":"result","subtype":"success","session_id":"session","is_error":false,"result":"yes"}
`

func TestCommittedOutputExcludesAppendsAndRejectsChangedOrTruncatedBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stdout.log")
	if err := os.WriteFile(path, []byte(resultFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	evidence, result, err := CommitOutput(t.Context(), path, "session", "model")
	if err != nil || result.Text != "yes" || !evidence.Successful {
		t.Fatalf("commit=%+v result=%+v err=%v", evidence, result, err)
	}
	for _, test := range []struct {
		name  string
		data  string
		valid bool
	}{
		{"unchanged", resultFixture, true},
		{"append", resultFixture + "not json and not part of the result", true},
		{"changed", strings.Replace(resultFixture, "yes", "bad", 1), false},
		{"truncated", resultFixture[:len(resultFixture)-2], false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if writeErr := os.WriteFile(path, []byte(test.data), 0o600); writeErr != nil {
				t.Fatal(writeErr)
			}
			got, readErr := ReadCommittedOutput(t.Context(), path, evidence)
			if (readErr == nil) != test.valid || (test.valid && got.Text != "yes") {
				t.Fatalf("committed read=%+v err=%v", got, readErr)
			}
		})
	}
}

func TestOutputValidationRequiresIdentityAndCompleteResult(t *testing.T) {
	for _, data := range []string{
		"", "not json\n", "{}\n", resultFixture + resultFixture,
		strings.Replace(resultFixture, `,"is_error":false`, "", 1),
		strings.Replace(resultFixture, `,"result":"yes"`, "", 1),
		strings.Replace(resultFixture, `"model":"model"`, `"model":"unexpected"`, 1),
		strings.Split(resultFixture, "\n")[1],
		strings.Repeat("x", maxOutputEventBytes+1),
	} {
		if _, err := parseOutput(t.Context(), strings.NewReader(data), "session", "model"); err == nil {
			t.Fatalf("accepted invalid stream of length %d", len(data))
		}
	}
	contradiction := resultFixture + `{"type":"assistant","session_id":"other"}` + "\n"
	_, err := parseOutput(t.Context(), strings.NewReader(contradiction), "session", "model")
	var outputErr *OutputError
	if !errors.As(err, &outputErr) || outputErr.ReasonCode != ReasonSessionIdentityMismatch || outputErr.ObservedSessionID != "other" {
		t.Fatalf("lost identity contradiction: %v", err)
	}
}
