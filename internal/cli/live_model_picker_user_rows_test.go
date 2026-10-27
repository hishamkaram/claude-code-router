//go:build live

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/gateway"
	"github.com/hishamkaram/claude-code-router/internal/liveclaude"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

const (
	liveUserRowID          = "anthropic.ccr.picker-ordinary"
	liveUserRowPaddedID    = " \t" + liveUserRowID + "\t "
	liveUserRowLabel       = "  User padded catalog model  "
	liveUserRowDescription = "Synthetic user picker metadata"
	liveUserRowBehavesAs   = "claude-sonnet-4-6"
)

func TestLiveFixtureModelPickerPaddedUserRowSelectionAndResume(t *testing.T) {
	ctx, cancel := context.WithTimeout(liveCompactionContext(t), 90*time.Second)
	defer cancel()
	t.Chdir(t.TempDir())
	modern := livePickerCatalogSupported(t, ctx)
	writeLivePaddedUserRow(t)
	fixture := newLivePickerCatalogFixture(t, "picker-ordinary-model")
	database := fixture.configure(t, ctx)
	s, model := openLiveUserRowStore(t, ctx, database)
	capture := make(chan liveUserRowCapture, 1)
	var launchID int64
	deps := liveUserRowDependencies(fixture, &launchID, ExecClaudeLauncher{}, capture)
	run := startLiveCompactionCommand(t, ctx, deps, []string{
		"--db", database, "launch", "--model", "picker-seed", "--print", "--auth-mode", "provider-only", "--no-statusline",
		"--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
	})
	assertLiveUserRowCapture(t, ctx, capture)
	selection, err := liveRealTurn(ctx, run, liveStreamInput(t, "/model "+liveUserRowID))
	if err != nil || !strings.HasPrefix(selection, "Set model to ") {
		t.Fatalf("padded user model selection failed: error=%t; %s", err != nil, liveRealSelectionSummary(selection, liveUserRowID))
	}
	fixture.assertSelectionCalls(t, modern)
	boundary := liveRealRouteBoundary(t, ctx, s, launchID)
	fixture.turnStarted.Store(true)
	result, err := liveRealTurn(ctx, run, liveStreamInput(t, "Reply with the synthetic catalog fixture response."))
	if err != nil || strings.TrimSpace(result) != livePickerCatalogResponse {
		t.Fatal("selected padded user model did not complete its actual response")
	}
	fixture.assertRealRoute(t)
	assertLiveRealMarkerRoute(t, ctx, s, launchID, boundary, &model)
	if _, _, err := run.finish(ctx); err != nil {
		t.Fatal("finish padded user-row launch")
	}
	_, session, route := readLiveResumeRouteEvidence(t, ctx, database)
	if route.ModelAlias != model.Alias || route.ProviderName != model.ProviderName || route.ProviderModel != model.ProviderModel {
		t.Fatal("padded user-row turn did not record its selected route")
	}
	// Resume exercises the real pinned settings branch, where exact untrimmed ID
	// comparisons would drop this user row and its presentation metadata.
	fixture.actualCalls.Store(0)
	resumeCapture := make(chan liveUserRowCapture, 1)
	var resumedID int64
	resumed := startLiveCompactionCommand(t, ctx, liveUserRowDependencies(fixture, &resumedID, ExecClaudeLauncher{}, resumeCapture), []string{
		"--db", database, "launch", "--resume-route", "--print", "--no-statusline", "--resume", session.ClaudeSessionID,
		"--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
	})
	assertLiveUserRowCapture(t, ctx, resumeCapture)
	boundary = liveRealRouteBoundary(t, ctx, s, resumedID)
	result, err = liveRealTurn(ctx, resumed, liveStreamInput(t, "Reply with the synthetic catalog fixture response after resuming."))
	if err != nil || strings.TrimSpace(result) != livePickerCatalogResponse {
		t.Fatal("pinned padded user model did not complete its actual response")
	}
	fixture.assertRealRoute(t)
	assertLiveRealMarkerRoute(t, ctx, s, resumedID, boundary, &model)
	if _, _, err := resumed.finish(ctx); err != nil {
		t.Fatal("finish pinned padded user-row launch")
	}
	t.Logf("padded_user_metadata_preserved=true pinned_metadata_preserved=true selected_route_proved=true catalog_supported=%t", modern)
}

func TestLiveFixtureModelPickerPaddedUserRowRenderedOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Fatal("required native Claude unavailable for user-row picker proof")
	}
	// Build before changing HOME so Go can reuse its existing build cache.
	executable := buildLiveCCRExecutable(t, ctx)
	project, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal("resolve isolated user-row project")
	}
	t.Chdir(project)
	configureIsolatedLivePickerClaude(t)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL", "1")
	t.Setenv("CLAUDE_CODE_DISABLE_AUTO_MEMORY", "1")
	modern := livePickerCatalogSupported(t, ctx)
	writeLivePaddedUserRow(t)
	fixture := newLivePickerCatalogFixture(t, "picker-ordinary-model")
	database := fixture.configure(t, ctx)
	run := startLiveUserRowUI(t, ctx, database, executable, fixture)
	run.waitForText(t, ctx, "Detected a custom API key")
	run.write(t, "\x1b[A\r", "accepting isolated placeholder API key")
	run.waitForText(t, ctx, "Claude Code v")
	run.submit(t, ctx, "/model", "opening user-row picker")
	ready := func(view livePickerSnapshot) bool {
		screen := normalizeLiveClaudeText(view.screen)
		return strings.Contains(screen, "Selectmodel") && strings.Contains(screen, "Esctocancel")
	}
	if err := waitForPickerState(ctx, run.transcript.Snapshot, ready); err != nil {
		view := run.transcript.Snapshot()
		options := livePickerVisibleOptions(view.screen)
		userRows, startupRows, rawTargetRows := liveUserRowVisibleCounts(options)
		t.Fatalf("native user-row picker did not render: picker_heading=%t visible_options=%d user_rows=%d startup_rows=%d raw_target_rows=%d", strings.Contains(normalizeLiveClaudeText(view.screen), "Selectmodel"), len(options), userRows, startupRows, rawTargetRows)
	}
	userRows, startupRows, rawTargetRows := liveUserRowVisibleCounts(livePickerVisibleOptions(run.transcript.Snapshot().screen))
	if startupRows != 1 {
		t.Fatalf("native startup row count=%d, want=1", startupRows)
	}
	// With replaceBuiltInOptions=false native picker precedence may keep the
	// discovery row's label. Count the selected identity once under either label;
	// exact user metadata remains independently checked in the injected settings.
	if userRows+rawTargetRows != 1 {
		t.Fatalf("rendered user rows=%d raw target rows=%d, want one selected identity", userRows, rawTargetRows)
	}
	if !modern && userRows != 0 {
		t.Fatal("legacy picker unexpectedly claimed the modern user label")
	}
	run.write(t, "\x1b", "closing user-row picker")
	run.waitForText(t, ctx, "Kept model as")
	run.submit(t, ctx, "/exit", "exiting user-row picker")
	run.waitForExit(t, ctx)
	t.Logf("startup_row_once=true selected_model_row_once=true user_label_used=%t catalog_supported=%t", userRows == 1, modern)
}

type liveUserRowCapture struct {
	count     int
	preserved bool
}

type liveUserRowLauncher struct {
	delegate ClaudeLauncher
	capture  chan<- liveUserRowCapture
}

func (launcher liveUserRowLauncher) Start(ctx context.Context, args []string, env ClaudeEnvironment, in io.Reader, out, errOut io.Writer) (ClaudeProcess, error) {
	var path string
	for index, arg := range args {
		if arg == "--settings" && index+1 < len(args) {
			path = args[index+1]
			break
		}
	}
	if path == "" {
		return nil, errors.New("user-row fixture missing launch settings")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("read synthetic user-row launch settings")
	}
	var settings struct {
		Picker claudeModelPicker `json:"modelPicker"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, errors.New("decode synthetic user-row launch settings")
	}
	captured := liveUserRowCapture{}
	for _, option := range settings.Picker.Options {
		if strings.TrimSpace(option.Model) != liveUserRowID {
			continue
		}
		captured.count++
		captured.preserved = option.Model == liveUserRowPaddedID && option.Label == liveUserRowLabel && option.Description == liveUserRowDescription && option.BehavesAs == liveUserRowBehavesAs && settings.Picker.ReplaceBuiltInOptions != nil && !*settings.Picker.ReplaceBuiltInOptions
	}
	select {
	case launcher.capture <- captured:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return launcher.delegate.Start(ctx, args, env, in, out, errOut)
}

func liveUserRowDependencies(fixture *livePickerCatalogFixture, launchID *int64, delegate ClaudeLauncher, capture chan<- liveUserRowCapture) Dependencies {
	starter := liveRealGatewayStarter(launchID)
	return Dependencies{Launcher: liveUserRowLauncher{delegate: delegate, capture: capture}, StartGateway: func(ctx context.Context, cfg gateway.Config) (*gateway.Server, error) {
		cfg.AnthropicBaseURL = fixture.firstParty.URL
		return starter(ctx, cfg)
	}}
}

func assertLiveUserRowCapture(t *testing.T, ctx context.Context, capture <-chan liveUserRowCapture) {
	t.Helper()
	select {
	case actual := <-capture:
		if actual.count != 1 || !actual.preserved {
			t.Fatalf("padded user-row count=%d metadata_preserved=%t, want=1/true", actual.count, actual.preserved)
		}
	case <-ctx.Done():
		t.Fatal("user-row launch settings were not captured")
	}
}

func writeLivePaddedUserRow(t *testing.T) {
	t.Helper()
	configDir := os.Getenv("CLAUDE_CONFIG_DIR")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal("create isolated user-row config directory")
	}
	replace := false
	settings := struct {
		Theme  string            `json:"theme"`
		Picker claudeModelPicker `json:"modelPicker"`
	}{Theme: "dark", Picker: claudeModelPicker{
		ReplaceBuiltInOptions: &replace, Options: []claudeModelPickerOption{{Model: liveUserRowPaddedID, Label: liveUserRowLabel, Description: liveUserRowDescription, BehavesAs: liveUserRowBehavesAs}},
	}}
	writeLivePickerJSON(t, filepath.Join(configDir, "settings.json"), settings)
}

func openLiveUserRowStore(t *testing.T, ctx context.Context, database string) (*store.Store, store.Model) {
	t.Helper()
	s, err := store.Open(ctx, database)
	if err != nil {
		t.Fatal("open synthetic user-row route store")
	}
	t.Cleanup(func() { _ = s.Close() })
	model, err := s.GetModel(ctx, "picker-ordinary")
	if err != nil {
		t.Fatal("read synthetic user-row model")
	}
	return s, model
}

func liveUserRowVisibleCounts(options []string) (userRows, startupRows, rawTargetRows int) {
	for _, option := range options {
		text := normalizeLiveClaudeText(option)
		hasUserLabel := strings.Contains(text, normalizeLiveClaudeText(liveUserRowLabel))
		if hasUserLabel {
			userRows++
		}
		if strings.HasPrefix(text, "anthropic.ccr.picker-seed") || strings.HasPrefix(text, "CCRpicker-seed") {
			startupRows++
		}
		if !hasUserLabel && (strings.HasPrefix(text, liveUserRowID) || strings.HasPrefix(text, "CCRpicker-ordinary")) {
			rawTargetRows++
		}
	}
	return userRows, startupRows, rawTargetRows
}

// This fixture owns both command and PTY-reader goroutines; cleanup cancels the
// command, closes its PTY and waits for both, including assertion-failure paths.
func startLiveUserRowUI(t *testing.T, ctx context.Context, database, executable string, fixture *livePickerCatalogFixture) *livePickerRun {
	t.Helper()
	runCtx, cancel := context.WithCancel(ctx)
	launcher := &livePickerLauncher{started: make(chan livePickerSession, 1), statuslineExecutable: executable}
	capture := make(chan liveUserRowCapture, 1)
	commandOut, commandErr := &bytes.Buffer{}, &bytes.Buffer{}
	commandDone := make(chan error, 1)
	deps := Dependencies{Launcher: liveUserRowLauncher{delegate: launcher, capture: capture}, ExecutablePath: executable, StartGateway: func(ctx context.Context, cfg gateway.Config) (*gateway.Server, error) {
		cfg.AnthropicBaseURL = fixture.firstParty.URL
		return gateway.Start(ctx, cfg)
	}}
	go func() {
		defer close(commandDone)
		deps.In, deps.Out, deps.Err = strings.NewReader(""), commandOut, commandErr
		cmd := NewRootCommand(runCtx, deps)
		cmd.SetArgs([]string{"--db", database, "launch", "--model", "picker-seed", "--bare"})
		commandDone <- cmd.Execute()
	}()
	var session livePickerSession
	select {
	case session = <-launcher.started:
	case <-commandDone:
		cancel()
		t.Fatal("synthetic user-row UI exited before native startup")
	case <-ctx.Done():
		cancel()
		<-commandDone
		t.Fatal("synthetic user-row UI startup timed out")
	}
	terminal := newLivePickerTerminal()
	readDone := make(chan error, 1)
	go func() { defer close(readDone); _, err := io.Copy(terminal, session.pty); readDone <- err }()
	run := &livePickerRun{session: session, commandDone: commandDone, commandOut: commandOut, commandErr: commandErr, transcript: terminal, readDone: readDone}
	t.Cleanup(func() { cancel(); run.close(); <-commandDone; <-readDone })
	assertLiveUserRowCapture(t, ctx, capture)
	return run
}
