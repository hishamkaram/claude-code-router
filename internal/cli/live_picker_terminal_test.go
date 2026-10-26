package cli

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/parser"
	"github.com/hinshun/vt10x"
)

const (
	livePickerCols = 140
	livePickerRows = 60
)

type livePickerSnapshot struct {
	stream    string
	screen    string
	cursorRow int
}

// The PTY reader owns the byte stream; this owner updates its derived screen
// under the same lock so snapshots cannot observe half-applied redraws.
type livePickerTerminal struct {
	mu       sync.Mutex
	raw      bytes.Buffer
	terminal vt10x.Terminal
	parser   *ansi.Parser
	pending  []byte
}

func newLivePickerTerminal() *livePickerTerminal {
	return &livePickerTerminal{
		terminal: vt10x.New(vt10x.WithSize(livePickerCols, livePickerRows)),
		parser:   ansi.NewParser(),
	}
}

func (t *livePickerTerminal) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, err := t.raw.Write(p); err != nil {
		return 0, fmt.Errorf("recording picker output: %w", err)
	}
	for i, b := range p {
		t.pending = append(t.pending, b)
		t.parser.Advance(b)
		if t.parser.State() != parser.GroundState {
			continue
		}
		if !isPickerKeyboardProtocol(t.pending) {
			if _, err := t.terminal.Write(t.pending); err != nil {
				return i, fmt.Errorf("rendering picker output: %w", err)
			}
		}
		t.pending = t.pending[:0]
	}
	return len(p), nil
}

func isPickerKeyboardProtocol(sequence []byte) bool {
	// Kitty keyboard negotiation does not paint cells. vt10x predates it and
	// otherwise interprets private CSI ... u commands as cursor restoration.
	return len(sequence) >= 4 && sequence[0] == '\x1b' && sequence[1] == '[' &&
		strings.ContainsRune("<=>?", rune(sequence[2])) && sequence[len(sequence)-1] == 'u'
}

func (t *livePickerTerminal) Snapshot() livePickerSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	return livePickerSnapshot{stream: t.raw.String(), screen: t.terminal.String(), cursorRow: t.terminal.Cursor().Y}
}

func (s livePickerSnapshot) inputLine() string {
	lines := strings.Split(s.screen, "\n")
	if s.cursorRow < 0 || s.cursorRow >= len(lines) {
		return ""
	}
	return normalizeLiveClaudeText(lines[s.cursorRow])
}

func livePickerPromptReady(view livePickerSnapshot) bool {
	screen := normalizeLiveClaudeText(view.screen)
	return strings.HasPrefix(view.inputLine(), "❯") &&
		!strings.Contains(screen, "Selectmodel") && !strings.Contains(screen, "Esctocancel")
}

func livePickerInputEchoed(view livePickerSnapshot, input string) bool {
	return view.inputLine() == "❯"+normalizeLiveClaudeText(input)
}

func waitForPickerState(ctx context.Context, snapshot func() livePickerSnapshot, ready func(livePickerSnapshot) bool) error {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if ready(snapshot()) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (t *livePickerTerminal) String() string {
	return t.Snapshot().stream
}

func (s livePickerSnapshot) diagnostics() string {
	raw := s.stream
	if len(raw) > 8192 {
		raw = raw[len(raw)-8192:]
	}
	return fmt.Sprintf("rendered screen:\n%s\nraw transcript tail: %q", s.screen, raw)
}

func livePickerTextMissing(view livePickerSnapshot, wants []string) []string {
	stream := ansi.Strip(view.stream)
	compactStream := normalizeLiveClaudeText(stream)
	compactScreen := normalizeLiveClaudeText(view.screen)
	var missing []string
	for _, want := range wants {
		compactWant := normalizeLiveClaudeText(want)
		if !strings.Contains(stream, want) && !strings.Contains(compactStream, compactWant) &&
			!strings.Contains(view.screen, want) && !strings.Contains(compactScreen, compactWant) {
			missing = append(missing, want)
		}
	}
	return missing
}

func normalizeLiveClaudeText(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || r == '`' {
			return -1
		}
		return r
	}, ansi.Strip(value))
}

func TestLivePickerTextAppliesCursorRedraws(t *testing.T) {
	t.Parallel()
	terminal := newLivePickerTerminal()
	input := "\x1b[1;1H7. anthropic.ccr.gpt\x1b[1;5Hnthropic.ccr.h%61iku\x1b[K"
	if _, err := terminal.Write([]byte(input)); err != nil {
		t.Fatal(err)
	}
	want := "anthropic.ccr.h%61iku"
	view := terminal.Snapshot()
	if strings.Contains(normalizeLiveClaudeText(view.stream), want) {
		t.Fatal("fixture did not reproduce the raw-stream matching defect")
	}
	if missing := livePickerTextMissing(view, []string{want}); len(missing) != 0 {
		t.Fatalf("rendered alias missing: %v\n%s", missing, view.diagnostics())
	}
	if missing := livePickerTextMissing(view, []string{want, "never-rendered-alias"}); len(missing) != 1 || missing[0] != "never-rendered-alias" {
		t.Fatalf("missing alias was accepted: %v", missing)
	}
}

func TestLivePickerTextPreservesSplitUTF8AndControlSequences(t *testing.T) {
	t.Parallel()
	input := "❯ Select\x1b[1Cmodel\x1b[2;1Hkeep\x1b[>1u!\x1b[?2026h\x1b[?2026l"
	whole, split := newLivePickerTerminal(), newLivePickerTerminal()
	if _, err := whole.Write([]byte(input)); err != nil {
		t.Fatal(err)
	}
	for _, b := range []byte(input) {
		if _, err := split.Write([]byte{b}); err != nil {
			t.Fatal(err)
		}
	}
	view := split.Snapshot()
	if view.screen != whole.Snapshot().screen {
		t.Fatalf("fragmented PTY output changed the screen:\n%s", view.diagnostics())
	}
	if missing := livePickerTextMissing(view, []string{"❯ Select model", "keep!"}); len(missing) != 0 {
		t.Fatalf("terminal text missing: %v\n%s", missing, view.diagnostics())
	}
}

func TestLivePickerReadinessUsesCurrentPromptAndWaitsForModalExit(t *testing.T) {
	t.Parallel()
	frames := []livePickerSnapshot{
		{screen: "❯ Default\nSelect model\nEsc to cancel"},
		{screen: "❯ \nRunning PreModelSwitch hooks… (Esc to cancel)"},
		{screen: "❯\u00a0"},
	}
	index := 0
	snapshot := func() livePickerSnapshot {
		view := frames[min(index, len(frames)-1)]
		index++
		return view
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := waitForPickerState(ctx, snapshot, livePickerPromptReady); err != nil || index != len(frames) {
		t.Fatalf("prompt readiness returned before modal exit: index=%d error=%v", index, err)
	}
	input := "/model anthropic.ccr.gpt"
	old := livePickerSnapshot{stream: "❯ " + input, screen: "❯ " + input + "\n❯ ", cursorRow: 1}
	if livePickerInputEchoed(old, input) {
		t.Fatal("historical command output was treated as current input echo")
	}
	current := livePickerSnapshot{screen: "❯\u00a0" + input}
	if !livePickerInputEchoed(current, input) {
		t.Fatal("current prompt input was not recognized")
	}
	canceled, stop := context.WithCancel(context.Background())
	stop()
	if err := waitForPickerState(canceled, func() livePickerSnapshot { return frames[1] }, livePickerPromptReady); err == nil {
		t.Fatal("busy prompt ignored cancellation")
	}
}
