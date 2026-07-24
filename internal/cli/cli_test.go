package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

type fakeSecrets struct {
	values       map[string]string
	failStore    bool
	failResolve  bool
	resolveCount int
}

func (f *fakeSecrets) Available(ctx context.Context) error {
	return ctx.Err()
}

func (f *fakeSecrets) Store(ctx context.Context, ref string, value string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.failStore {
		return fmt.Errorf("fake keyring unavailable")
	}
	if f.values == nil {
		f.values = make(map[string]string)
	}
	f.values[ref] = value
	return nil
}

func (f *fakeSecrets) Resolve(ctx context.Context, ref string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f.resolveCount++
	if f.failResolve {
		return "", fmt.Errorf("fake resolve should not be called")
	}
	return f.values[ref], nil
}

func TestRootHelpExplainsRouterConcepts(t *testing.T) {
	t.Parallel()

	out, _, err := runCommand(t, "help")
	if err != nil {
		t.Fatalf("help error = %v", err)
	}
	for _, want := range []string{"fixed local gateway", "normal startup", "launch --model <alias>", "SQLite", "never silently fall back"} {
		if !strings.Contains(out, want) {
			t.Fatalf("help output missing %q:\n%s", want, out)
		}
	}
}

func TestVersionCommand(t *testing.T) {
	t.Parallel()

	out, _, err := runCommand(t, "version")
	if err != nil {
		t.Fatalf("version error = %v", err)
	}
	if !strings.Contains(out, "ccr dev") {
		t.Fatalf("version output = %q", out)
	}
}

func TestUnknownCommandReturnsSuggestion(t *testing.T) {
	t.Parallel()

	_, _, err := runCommand(t, "provder")
	if err == nil {
		t.Fatalf("expected unknown command error")
	}
	if !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("error = %v", err)
	}
}

func TestNoArgCommandsRejectStrayArgs(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	tests := [][]string{
		{"--db", dbPath, "init", "unexpected"},
		{"version", "unexpected"},
		{"--db", dbPath, "status", "unexpected"},
		{"--db", dbPath, "doctor", "unexpected"},
		{"sessions", "unexpected"},
		{"agents", "unexpected"},
	}
	for _, args := range tests {
		args := args
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			_, _, err := runCommand(t, args...)
			if err == nil {
				t.Fatalf("runCommand(%v) unexpectedly succeeded", args)
			}
		})
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("database path exists after invalid init: stat err=%v", err)
	}
}

func TestVisibleCommandsDoNotReturnNotImplemented(t *testing.T) {
	t.Parallel()

	server := newCLIConformanceOpenAIServer(t, "gpt-5")
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", server.URL, "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "litellm-gpt-5", "--provider", "litellm", "--model", "gpt-5"); err != nil {
		t.Fatalf("model add error = %v", err)
	}

	tests := []struct {
		name string
		deps Dependencies
		args []string
	}{
		{name: "provider_test", args: []string{"--db", dbPath, "provider", "test", "litellm"}},
		{name: "provider_update_missing_flags", args: []string{"--db", dbPath, "provider", "update", "litellm"}},
		{name: "provider_remove_confirm_required", args: []string{"--db", dbPath, "provider", "remove", "litellm"}},
		{name: "model_test", args: []string{"--db", dbPath, "model", "test", "litellm-gpt-5"}},
		{name: "model_update_missing_flags", args: []string{"--db", dbPath, "model", "update", "litellm-gpt-5"}},
		{name: "model_remove_confirm_required", args: []string{"--db", dbPath, "model", "remove", "litellm-gpt-5"}},
		{name: "conformance_run", args: []string{"--db", dbPath, "conformance", "run", "litellm-gpt-5"}},
		{name: "sessions", args: []string{"--db", dbPath, "sessions"}},
		{name: "agents", args: []string{"--db", dbPath, "agents"}},
		{name: "launch", deps: Dependencies{Launcher: &fakeLauncher{pid: os.Getpid()}}, args: []string{"--db", dbPath, "launch"}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			out, errOut, err := runCommandWithDeps(t, tt.deps, tt.args...)
			combined := out + errOut
			if err != nil {
				combined += err.Error()
			}
			if strings.Contains(strings.ToLower(combined), "not implemented yet") {
				t.Fatalf("command returned placeholder output/error:\nstdout=%s\nstderr=%s\nerr=%v", out, errOut, err)
			}
		})
	}
}

func TestDoctorUsesDatabaseAndReportsClaudeCode(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", "http://localhost:4000", "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	out, _, err := runCommand(t, "--db", dbPath, "doctor")
	if err != nil {
		t.Fatalf("doctor error = %v", err)
	}
	for _, want := range []string{"SQLite: ok", "Secrets: ok", "Claude Code:", "Providers: 1", "Provider litellm: protocol=openai-compatible mode=degraded token-count=provider"} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor output missing %q:\n%s", want, out)
		}
	}
}

func runCommand(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	return runCommandWithDeps(t, Dependencies{}, args...)
}

func runCommandWithDeps(t *testing.T, deps Dependencies, args ...string) (string, string, error) {
	t.Helper()
	var out bytes.Buffer
	var errOut bytes.Buffer
	if deps.In == nil {
		deps.In = strings.NewReader("")
	}
	deps.Out = &out
	deps.Err = &errOut
	if deps.Secrets == nil {
		deps.Secrets = &fakeSecrets{}
	}
	cmd := NewRootCommand(context.Background(), deps)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

func newModelsServer(t *testing.T, models []string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/model/info" {
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		if r.URL.Path != "/v1/models" {
			t.Fatalf("path = %q, want /v1/models or /model/info", r.URL.Path)
		}
		parts := make([]string, 0, len(models))
		for _, model := range models {
			parts = append(parts, fmt.Sprintf(`{"id":%q}`, model))
		}
		_, _ = fmt.Fprintf(w, `{"data":[%s]}`, strings.Join(parts, ","))
	}))
	t.Cleanup(server.Close)
	return server
}

func writeAPIKeyFile(t *testing.T, perm os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api.key")
	if err := os.WriteFile(path, []byte("sk-file-secret\n"), perm); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatalf("Chmod error = %v", err)
	}
	return path
}

type promptReader struct {
	reader *strings.Reader
}

func newPromptReader(input string) *promptReader {
	return &promptReader{reader: strings.NewReader(input)}
}

func (r *promptReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return r.reader.Read(p[:1])
}

type fakeLauncher struct {
	pid     int
	args    []string
	env     ClaudeEnvironment
	out     io.Writer
	errOut  io.Writer
	waitErr error
	starts  int
	process *fakeProcess
}

func (f *fakeLauncher) Start(ctx context.Context, args []string, env ClaudeEnvironment, in io.Reader, out, errOut io.Writer) (ClaudeProcess, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.starts++
	f.args = append([]string(nil), args...)
	f.env = ClaudeEnvironment{
		Set:   append([]string(nil), env.Set...),
		Unset: append([]string(nil), env.Unset...),
	}
	f.out = out
	f.errOut = errOut
	f.process = &fakeProcess{pid: f.pid, waitErr: f.waitErr}
	return f.process, nil
}

func (f *fakeLauncher) hasEnvPrefix(prefix string) bool {
	for _, item := range f.env.Set {
		if strings.HasPrefix(item, prefix) {
			return true
		}
	}
	return false
}

func (f *fakeLauncher) hasEnv(value string) bool {
	for _, item := range f.env.Set {
		if item == value {
			return true
		}
	}
	return false
}

func (f *fakeLauncher) envValue(name string) (string, bool) {
	prefix := name + "="
	for _, item := range f.env.Set {
		if strings.HasPrefix(item, prefix) {
			return strings.TrimPrefix(item, prefix), true
		}
	}
	return "", false
}

func (f *fakeLauncher) unsetsEnv(name string) bool {
	for _, item := range f.env.Unset {
		if item == name {
			return true
		}
	}
	return false
}

func claudeEnvironmentSummary(env ClaudeEnvironment) string {
	setNames := make([]string, 0, len(env.Set))
	for _, entry := range env.Set {
		name, _, found := strings.Cut(entry, "=")
		if found {
			setNames = append(setNames, name)
		}
	}
	unsetNames := append([]string(nil), env.Unset...)
	sort.Strings(setNames)
	sort.Strings(unsetNames)
	return fmt.Sprintf("set=%v unset=%v", setNames, unsetNames)
}

func (f *fakeLauncher) environmentSummary() string {
	return claudeEnvironmentSummary(f.env)
}

func (f *fakeLauncher) hasArg(value string) bool {
	for _, item := range f.args {
		if item == value {
			return true
		}
	}
	return false
}

func (f *fakeLauncher) settingsArgValue() (string, bool) {
	for index, item := range f.args {
		if item == "--settings" && index+1 < len(f.args) {
			return f.args[index+1], true
		}
	}
	return "", false
}

type fakeProcess struct {
	pid     int
	waitErr error
	stopped bool
	waited  bool
}

func (p *fakeProcess) PID() int {
	return p.pid
}

func (p *fakeProcess) Done() <-chan error {
	p.waited = true
	done := make(chan error, 1)
	done <- p.waitErr
	close(done)
	return done
}

func (p *fakeProcess) Stop() error {
	p.stopped = true
	return nil
}
