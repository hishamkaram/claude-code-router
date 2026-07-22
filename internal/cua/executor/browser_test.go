package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/cua"
)

func TestDockerBrowserCommandIsIsolated(t *testing.T) {
	t.Parallel()

	runner := newRecordingRunner(newFakeProcess(true), newFakeProcess(false))
	executor, err := NewDockerBrowser(context.Background(), DockerBrowserOptions{
		Runner:         runner,
		DockerPath:     "docker",
		Image:          "example/browser:latest",
		BrowserBinary:  "chromium",
		HostDebugPort:  41237,
		LaunchID:       "launch_1",
		CommandEnvBase: []string{"PATH=/usr/bin", "CCR_TOKEN=secret", "ANTHROPIC_API_KEY=secret"},
	})
	if err != nil {
		t.Fatalf("NewDockerBrowser() error = %v", err)
	}
	defer func() {
		if err := executor.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	specs := runner.Specs()
	if len(specs) != 1 {
		t.Fatalf("runner recorded %d specs, want 1", len(specs))
	}
	spec := specs[0]
	if spec.Path != "docker" {
		t.Fatalf("command path = %q", spec.Path)
	}
	assertOnlyPATHEnv(t, spec.Env)
	args := strings.Join(spec.Args, "\x00")
	for _, required := range []string{
		"run",
		"--rm",
		"--init",
		"--name\x00ccr-cua-launch_1",
		"--publish\x00127.0.0.1:41237:9222",
		"--cap-drop\x00ALL",
		"--security-opt\x00no-new-privileges",
		"--read-only",
		"--user\x0065532:65532",
		"--tmpfs\x00/home/chrome/profile:rw,nosuid,nodev,uid=65532,gid=65532,size=512m",
		"--no-sandbox",
		"--user-data-dir=/home/chrome/profile",
		"--remote-debugging-address=127.0.0.1",
		"--remote-debugging-port=9222",
	} {
		if !strings.Contains(args, required) {
			t.Fatalf("docker args missing %q in %q", required, args)
		}
	}
	for _, forbidden := range []string{"-v", "--volume", "--mount", "--env", "-e"} {
		if containsString(spec.Args, forbidden) {
			t.Fatalf("docker args contain forbidden flag %q in %#v", forbidden, spec.Args)
		}
	}
	for _, forbidden := range []string{"/var/run/docker.sock", "CCR_TOKEN", "ANTHROPIC_API_KEY"} {
		if strings.Contains(args, forbidden) {
			t.Fatalf("docker args contain forbidden value %q in %q", forbidden, args)
		}
	}
	if executor.DebugURL() != "http://127.0.0.1:41237/json/version" {
		t.Fatalf("DebugURL() = %q", executor.DebugURL())
	}
}

func TestDockerBrowserPreservesDockerConnectionEnvironment(t *testing.T) {
	t.Parallel()

	runner := newRecordingRunner(newFakeProcess(true))
	executor, err := NewDockerBrowser(context.Background(), DockerBrowserOptions{
		Runner:        runner,
		DockerPath:    "docker",
		Image:         "example/browser:latest",
		HostDebugPort: 41238,
		LaunchID:      "launch_1",
		CommandEnvBase: []string{
			"PATH=/usr/bin",
			"HOME=/home/test",
			"XDG_RUNTIME_DIR=/run/user/1000",
			"DOCKER_HOST=unix:///run/user/1000/docker.sock",
			"DOCKER_CONTEXT=rootless",
			"DOCKER_CONFIG=/home/test/.docker",
			"HTTPS_PROXY=http://proxy.example:8080",
			"NO_PROXY=127.0.0.1,localhost",
			"SSH_AUTH_SOCK=/run/user/1000/ssh-agent",
			"CCR_TOKEN=secret",
			"ANTHROPIC_API_KEY=secret",
		},
	})
	if err != nil {
		t.Fatalf("NewDockerBrowser() error = %v", err)
	}
	defer func() {
		_ = executor.Close()
	}()

	env := runner.Specs()[0].Env
	for _, required := range []string{
		"PATH=/usr/bin",
		"HOME=/home/test",
		"XDG_RUNTIME_DIR=/run/user/1000",
		"DOCKER_HOST=unix:///run/user/1000/docker.sock",
		"DOCKER_CONTEXT=rootless",
		"DOCKER_CONFIG=/home/test/.docker",
		"HTTPS_PROXY=http://proxy.example:8080",
		"NO_PROXY=127.0.0.1,localhost",
		"SSH_AUTH_SOCK=/run/user/1000/ssh-agent",
	} {
		if !containsString(env, required) {
			t.Fatalf("docker env missing %q in %#v", required, env)
		}
	}
	for _, forbidden := range []string{"CCR_TOKEN=secret", "ANTHROPIC_API_KEY=secret"} {
		if containsString(env, forbidden) {
			t.Fatalf("docker env contains secret-bearing value %q in %#v", forbidden, env)
		}
	}
}

func TestDefaultDockerBrowserImageMatchesReleaseArtifact(t *testing.T) {
	t.Parallel()

	const publishedImage = "ghcr.io/hishamkaram/claude-code-router/browser:latest"
	if defaultDockerBrowserImage != publishedImage {
		t.Fatalf("default Docker image = %q, want %q", defaultDockerBrowserImage, publishedImage)
	}
	prepared, err := prepareDockerBrowser(DockerBrowserOptions{HostDebugPort: 41238, LaunchID: "release-image"})
	if err != nil {
		t.Fatalf("prepareDockerBrowser() error = %v", err)
	}
	if !containsString(prepared.command.Args, publishedImage) {
		t.Fatalf("Docker command does not use published image: %#v", prepared.command.Args)
	}
}

func TestDockerBrowserCloseIgnoresAlreadyAutoRemovedContainer(t *testing.T) {
	t.Parallel()

	cleanupProcess := newFakeProcess(false)
	cleanupProcess.waitErr = errors.New("exit status 1")
	runner := &cleanupOutputRunner{
		recordingRunner: newRecordingRunner(newFakeProcess(false), cleanupProcess),
		cleanupStderr:   "Error response from daemon: No such container: ccr-cua-launch_1\n",
	}
	executor, err := NewDockerBrowser(context.Background(), DockerBrowserOptions{
		Runner:        runner,
		DockerPath:    "docker",
		Image:         "example/browser:latest",
		BrowserBinary: "chromium",
		HostDebugPort: 41239,
		LaunchID:      "launch_1",
	})
	if err != nil {
		t.Fatalf("NewDockerBrowser() error = %v", err)
	}
	if err := executor.Close(); err != nil {
		t.Fatalf("Close() error = %v, want nil for already auto-removed container", err)
	}
	if specs := runner.Specs(); len(specs) != 2 {
		t.Fatalf("runner recorded %d specs, want launch and cleanup", len(specs))
	}
}

func TestLocalBrowserUsesTemporaryProfileAndLoopbackDebugging(t *testing.T) {
	t.Parallel()

	profileRoot := t.TempDir()
	mainProcess := newFakeProcess(true)
	runner := newRecordingRunner(mainProcess)
	executor, err := NewLocalBrowser(context.Background(), LocalBrowserOptions{
		Runner:         runner,
		BrowserPath:    "/usr/bin/chromium",
		HostDebugPort:  45678,
		ProfileRoot:    profileRoot,
		CommandEnvBase: []string{"PATH=/bin", "CCR_TOKEN=secret", "OPENAI_API_KEY=secret"},
	})
	if err != nil {
		t.Fatalf("NewLocalBrowser() error = %v", err)
	}
	profileDir := executor.ProfileDir()
	if !strings.HasPrefix(profileDir, filepath.Clean(profileRoot)+string(os.PathSeparator)) {
		t.Fatalf("profile dir %q is not under root %q", profileDir, profileRoot)
	}
	if _, err := os.Stat(profileDir); err != nil {
		t.Fatalf("profile dir was not created: %v", err)
	}

	specs := runner.Specs()
	if len(specs) != 1 {
		t.Fatalf("runner recorded %d specs, want 1", len(specs))
	}
	spec := specs[0]
	if spec.Path != "/usr/bin/chromium" {
		t.Fatalf("command path = %q", spec.Path)
	}
	assertOnlyPATHEnv(t, spec.Env)
	args := strings.Join(spec.Args, "\x00")
	for _, required := range []string{
		"--headless=new",
		"--user-data-dir=" + filepath.Clean(profileDir),
		"--remote-debugging-address=127.0.0.1",
		"--remote-debugging-port=45678",
	} {
		if !strings.Contains(args, required) {
			t.Fatalf("local browser args missing %q in %q", required, args)
		}
	}
	if strings.Contains(args, "--remote-debugging-address=0.0.0.0") {
		t.Fatalf("local browser debug endpoint is not loopback-only: %q", args)
	}

	if err := executor.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if mainProcess.KillCount() != 1 || mainProcess.WaitCount() != 1 {
		t.Fatalf("process kill/wait = %d/%d, want 1/1", mainProcess.KillCount(), mainProcess.WaitCount())
	}
	if _, err := os.Stat(profileDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("profile dir still exists after Close(): %v", err)
	}
}

func TestLocalBrowserCancellationKillsProcess(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	mainProcess := newFakeProcess(true)
	runner := newRecordingRunner(mainProcess)
	executor, err := NewLocalBrowser(ctx, LocalBrowserOptions{
		Runner:         runner,
		BrowserPath:    "/usr/bin/chromium",
		HostDebugPort:  45679,
		ProfileRoot:    t.TempDir(),
		CommandEnvBase: []string{"PATH=/bin"},
	})
	if err != nil {
		t.Fatalf("NewLocalBrowser() error = %v", err)
	}
	cancel()
	waitFor(t, func() bool {
		return mainProcess.KillCount() == 1
	})

	err = executor.Close()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Close() error = %v, want context.Canceled", err)
	}
	if mainProcess.WaitCount() != 1 {
		t.Fatalf("WaitCount() = %d, want 1", mainProcess.WaitCount())
	}
}

func TestBrowserExecutorExecutesClickViaCDP(t *testing.T) {
	t.Parallel()

	server := newFakeCDPServer(t)
	runner := newRecordingRunner(newFakeProcess(true))
	executor, err := NewLocalBrowser(context.Background(), LocalBrowserOptions{
		Runner:        runner,
		BrowserPath:   "/usr/bin/chromium",
		HostDebugPort: server.port(),
		ProfileRoot:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewLocalBrowser() error = %v", err)
	}
	defer func() {
		_ = executor.Close()
	}()

	if _, err := executor.Execute(context.Background(), cua.Action{CallID: "call_1", Kind: cua.ActionClick, X: 1, Y: 2}); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := server.mouseEventTypes(); strings.Join(got, ",") != "mouseMoved,mousePressed,mouseReleased" {
		t.Fatalf("mouse event types = %#v", got)
	}
}

func TestBrowserHealthCheckUsesInjectedHTTPClient(t *testing.T) {
	t.Parallel()

	runner := newRecordingRunner(newFakeProcess(true))
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "http://127.0.0.1:45681/json/version" {
			t.Fatalf("health URL = %q", req.URL.String())
		}
		return textResponse(http.StatusOK, "application/json", `{}`), nil
	})}
	executor, err := NewLocalBrowser(context.Background(), LocalBrowserOptions{
		Runner:        runner,
		HTTPClient:    client,
		BrowserPath:   "/usr/bin/chromium",
		HostDebugPort: 45681,
		ProfileRoot:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewLocalBrowser() error = %v", err)
	}
	defer func() {
		_ = executor.Close()
	}()
	if err := executor.Check(context.Background()); err != nil {
		t.Fatalf("Check() error = %v", err)
	}
}

func assertOnlyPATHEnv(t *testing.T, env []string) {
	t.Helper()
	if len(env) != 1 || env[0] != "PATH=/bin" && env[0] != "PATH=/usr/bin" {
		t.Fatalf("env = %#v, want only PATH", env)
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

type recordingRunner struct {
	mu        sync.Mutex
	specs     []CommandSpec
	processes []*fakeProcess
}

func newRecordingRunner(processes ...*fakeProcess) *recordingRunner {
	return &recordingRunner{processes: processes}
}

func (runner *recordingRunner) Start(_ context.Context, spec CommandSpec) (Process, error) {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	runner.specs = append(runner.specs, spec.clone())
	if len(runner.processes) == 0 {
		return newFakeProcess(false), nil
	}
	process := runner.processes[0]
	runner.processes = runner.processes[1:]
	return process, nil
}

func (runner *recordingRunner) Specs() []CommandSpec {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	specs := make([]CommandSpec, len(runner.specs))
	for index := range runner.specs {
		specs[index] = runner.specs[index].clone()
	}
	return specs
}

type cleanupOutputRunner struct {
	*recordingRunner
	cleanupStderr string
}

func (runner *cleanupOutputRunner) Start(ctx context.Context, spec CommandSpec) (Process, error) {
	process, err := runner.recordingRunner.Start(ctx, spec)
	if err != nil {
		return nil, err
	}
	if len(runner.Specs()) >= 2 && spec.Stderr != nil {
		_, _ = io.WriteString(spec.Stderr, runner.cleanupStderr)
	}
	return process, nil
}

type fakeProcess struct {
	done      chan struct{}
	closeDone sync.Once

	mu        sync.Mutex
	killCount int
	waitCount int
	waitErr   error
	killErr   error
}

func newFakeProcess(block bool) *fakeProcess {
	process := &fakeProcess{done: make(chan struct{})}
	if !block {
		process.closeDone.Do(func() {
			close(process.done)
		})
	}
	return process
}

func (process *fakeProcess) PID() int {
	return 1234
}

func (process *fakeProcess) Wait() error {
	process.mu.Lock()
	process.waitCount++
	process.mu.Unlock()
	<-process.done
	process.mu.Lock()
	defer process.mu.Unlock()
	return process.waitErr
}

func (process *fakeProcess) Kill() error {
	process.mu.Lock()
	process.killCount++
	killErr := process.killErr
	process.mu.Unlock()
	process.closeDone.Do(func() {
		close(process.done)
	})
	return killErr
}

func (process *fakeProcess) KillCount() int {
	process.mu.Lock()
	defer process.mu.Unlock()
	return process.killCount
}

func (process *fakeProcess) WaitCount() int {
	process.mu.Lock()
	defer process.mu.Unlock()
	return process.waitCount
}

const testWaitTimeout = time.Second

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(testWaitTimeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition was not met within %s", testWaitTimeout)
}
