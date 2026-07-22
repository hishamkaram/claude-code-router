package executor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/cua"
)

const (
	defaultDockerPath             = "docker"
	defaultDockerBrowserImage     = "ghcr.io/hishamkaram/claude-code-router/browser:latest"
	defaultContainerBrowserBinary = "chromium"
	defaultLocalBrowserBinary     = "chromium"
	defaultBrowserHealthTimeout   = 5 * time.Second
	defaultDockerCleanupTimeout   = 10 * time.Second
	containerDebugPort            = 9222
	maxDiscardResponseBytes       = 64 << 10
)

// DockerBrowserOptions configures a launch-scoped Docker browser executor.
type DockerBrowserOptions struct {
	Runner         CommandRunner
	HTTPClient     *http.Client
	DockerPath     string
	Image          string
	BrowserBinary  string
	HostDebugPort  int
	LaunchID       string
	StartupURL     string
	CommandEnvBase []string
}

// DockerBrowser launches an isolated browser inside a Docker container.
type DockerBrowser struct {
	runner        CommandRunner
	process       *managedProcess
	httpClient    *http.Client
	debugURL      string
	commandSpec   CommandSpec
	cleanupSpec   CommandSpec
	containerName string
}

func NewDockerBrowser(ctx context.Context, opts DockerBrowserOptions) (*DockerBrowser, error) {
	runner := opts.Runner
	if runner == nil {
		runner = OSCommandRunner{}
	}
	prepared, err := prepareDockerBrowser(opts)
	if err != nil {
		return nil, err
	}
	process, err := startManagedProcess(ctx, runner, prepared.command)
	if err != nil {
		return nil, fmt.Errorf("starting Docker CUA browser: %w", err)
	}
	return &DockerBrowser{
		runner:        runner,
		process:       process,
		httpClient:    browserHTTPClient(opts.HTTPClient),
		debugURL:      prepared.debugURL,
		commandSpec:   prepared.command,
		cleanupSpec:   prepared.cleanup,
		containerName: prepared.containerName,
	}, nil
}

func (executor *DockerBrowser) Name() string {
	return string(cua.ExecutorDocker)
}

func (executor *DockerBrowser) Check(ctx context.Context) error {
	if err := checkBrowserDebugEndpoint(ctx, executor.httpClient, executor.debugURL); err != nil {
		return fmt.Errorf("checking Docker CUA browser health: %w", err)
	}
	return nil
}

func (executor *DockerBrowser) Execute(ctx context.Context, action cua.Action) (cua.Observation, error) {
	return executeBrowserCDPAction(ctx, executor.Name(), executor.httpClient, executor.debugURL, action)
}

func (executor *DockerBrowser) Close() error {
	if executor == nil {
		return nil
	}
	var closeErr error
	if executor.process != nil {
		closeErr = executor.process.Close()
	}
	if executor.cleanupSpec.Path == "" {
		return closeErr
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), defaultDockerCleanupTimeout)
	defer cancel()
	if err := runDockerCleanupCommand(cleanupCtx, executor.runner, executor.cleanupSpec, executor.containerName); err != nil {
		closeErr = errors.Join(closeErr, fmt.Errorf("removing Docker CUA browser container: %w", err))
	}
	return closeErr
}

func runDockerCleanupCommand(ctx context.Context, runner CommandRunner, spec CommandSpec, containerName string) error {
	var stderr strings.Builder
	cleanupSpec := spec.clone()
	cleanupSpec.Stderr = &stderr
	err := runCommandToExit(ctx, runner, cleanupSpec)
	if err != nil && dockerContainerAlreadyRemoved(containerName, stderr.String()) {
		return nil
	}
	return err
}

func dockerContainerAlreadyRemoved(containerName, stderr string) bool {
	containerName = strings.ToLower(strings.TrimSpace(containerName))
	stderr = strings.ToLower(stderr)
	return containerName != "" && strings.Contains(stderr, "no such container") && strings.Contains(stderr, containerName)
}

func (executor *DockerBrowser) DebugURL() string {
	if executor == nil {
		return ""
	}
	return executor.debugURL
}

func (executor *DockerBrowser) CommandSpec() CommandSpec {
	if executor == nil {
		return CommandSpec{}
	}
	return executor.commandSpec.clone()
}

func (executor *DockerBrowser) ContainerName() string {
	if executor == nil {
		return ""
	}
	return executor.containerName
}

// LocalBrowserOptions configures a launch-scoped local browser executor.
type LocalBrowserOptions struct {
	Runner         CommandRunner
	HTTPClient     *http.Client
	BrowserPath    string
	HostDebugPort  int
	ProfileRoot    string
	StartupURL     string
	CommandEnvBase []string
}

// LocalBrowser launches a local browser with an isolated temporary profile.
type LocalBrowser struct {
	process     *managedProcess
	httpClient  *http.Client
	debugURL    string
	profileDir  string
	commandSpec CommandSpec
}

func NewLocalBrowser(ctx context.Context, opts LocalBrowserOptions) (*LocalBrowser, error) {
	runner := opts.Runner
	if runner == nil {
		runner = OSCommandRunner{}
	}
	prepared, err := prepareLocalBrowser(opts)
	if err != nil {
		return nil, err
	}
	process, err := startManagedProcess(ctx, runner, prepared.command)
	if err != nil {
		_ = os.RemoveAll(prepared.profileDir)
		return nil, fmt.Errorf("starting local CUA browser: %w", err)
	}
	return &LocalBrowser{
		process:     process,
		httpClient:  browserHTTPClient(opts.HTTPClient),
		debugURL:    prepared.debugURL,
		profileDir:  prepared.profileDir,
		commandSpec: prepared.command,
	}, nil
}

func (executor *LocalBrowser) Name() string {
	return string(cua.ExecutorLocalBrowser)
}

func (executor *LocalBrowser) Check(ctx context.Context) error {
	if err := checkBrowserDebugEndpoint(ctx, executor.httpClient, executor.debugURL); err != nil {
		return fmt.Errorf("checking local CUA browser health: %w", err)
	}
	return nil
}

func (executor *LocalBrowser) Execute(ctx context.Context, action cua.Action) (cua.Observation, error) {
	return executeBrowserCDPAction(ctx, executor.Name(), executor.httpClient, executor.debugURL, action)
}

func (executor *LocalBrowser) Close() error {
	if executor == nil {
		return nil
	}
	var closeErr error
	if executor.process != nil {
		closeErr = executor.process.Close()
	}
	if executor.profileDir != "" {
		closeErr = errors.Join(closeErr, os.RemoveAll(executor.profileDir))
	}
	return closeErr
}

func (executor *LocalBrowser) DebugURL() string {
	if executor == nil {
		return ""
	}
	return executor.debugURL
}

func (executor *LocalBrowser) ProfileDir() string {
	if executor == nil {
		return ""
	}
	return executor.profileDir
}

func (executor *LocalBrowser) CommandSpec() CommandSpec {
	if executor == nil {
		return CommandSpec{}
	}
	return executor.commandSpec.clone()
}

type preparedDockerBrowser struct {
	command       CommandSpec
	cleanup       CommandSpec
	debugURL      string
	containerName string
}

func prepareDockerBrowser(opts DockerBrowserOptions) (preparedDockerBrowser, error) {
	dockerPath := valueOrDefault(opts.DockerPath, defaultDockerPath)
	image := valueOrDefault(opts.Image, defaultDockerBrowserImage)
	browserBinary := valueOrDefault(opts.BrowserBinary, defaultContainerBrowserBinary)
	startupURL := valueOrDefault(opts.StartupURL, "about:blank")
	port, err := normalizePort(opts.HostDebugPort)
	if err != nil {
		return preparedDockerBrowser{}, err
	}
	launchID := opts.LaunchID
	if launchID == "" {
		launchID, err = randomLaunchID()
		if err != nil {
			return preparedDockerBrowser{}, err
		}
	}
	if err := validateLaunchID(launchID); err != nil {
		return preparedDockerBrowser{}, err
	}
	containerName := "ccr-cua-" + launchID
	envBase := opts.CommandEnvBase
	if envBase == nil {
		envBase = os.Environ()
	}
	args := dockerBrowserArgs(containerName, image, browserBinary, port, startupURL)
	return preparedDockerBrowser{
		command: CommandSpec{
			Path: dockerPath,
			Args: args,
			Env:  dockerCommandEnv(envBase),
		},
		cleanup: CommandSpec{
			Path: dockerPath,
			Args: []string{"rm", "--force", "--volumes", containerName},
			Env:  dockerCommandEnv(envBase),
		},
		debugURL:      browserDebugURL(port),
		containerName: containerName,
	}, nil
}

func dockerBrowserArgs(containerName, image, browserBinary string, hostPort int, startupURL string) []string {
	// The non-root, no-new-privileges container cannot use Chromium's setuid
	// sandbox. Docker remains the isolation boundary through the restrictions
	// below, so this flag is required for the published browser image to start.
	return []string{
		"run",
		"--rm",
		"--init",
		"--name", containerName,
		"--network", "bridge",
		"--publish", "127.0.0.1:" + strconv.Itoa(hostPort) + ":" + strconv.Itoa(containerDebugPort),
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--pids-limit", "512",
		"--memory", "2g",
		"--shm-size", "1g",
		"--read-only",
		"--user", "65532:65532",
		"--tmpfs", "/tmp:rw,nosuid,nodev,size=256m",
		"--tmpfs", "/home/chrome/profile:rw,nosuid,nodev,uid=65532,gid=65532,size=512m",
		image,
		browserBinary,
		"--headless=new",
		"--no-sandbox",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-background-networking",
		"--disable-component-update",
		"--disable-extensions",
		"--disable-sync",
		"--user-data-dir=/home/chrome/profile",
		"--remote-debugging-address=127.0.0.1",
		"--remote-debugging-port=" + strconv.Itoa(containerDebugPort),
		startupURL,
	}
}

type preparedLocalBrowser struct {
	command    CommandSpec
	debugURL   string
	profileDir string
}

func prepareLocalBrowser(opts LocalBrowserOptions) (preparedLocalBrowser, error) {
	browserPath := valueOrDefault(opts.BrowserPath, defaultLocalBrowserBinary)
	startupURL := valueOrDefault(opts.StartupURL, "about:blank")
	port, err := normalizePort(opts.HostDebugPort)
	if err != nil {
		return preparedLocalBrowser{}, err
	}
	profileRoot := opts.ProfileRoot
	if profileRoot == "" {
		profileRoot = os.TempDir()
	}
	profileDir, err := os.MkdirTemp(profileRoot, "ccr-cua-browser-*")
	if err != nil {
		return preparedLocalBrowser{}, fmt.Errorf("creating local browser profile: %w", err)
	}
	envBase := opts.CommandEnvBase
	if envBase == nil {
		envBase = os.Environ()
	}
	command := CommandSpec{
		Path: browserPath,
		Args: localBrowserArgs(profileDir, port, startupURL),
		Env:  sanitizedCommandEnv(envBase),
	}
	return preparedLocalBrowser{
		command:    command,
		debugURL:   browserDebugURL(port),
		profileDir: profileDir,
	}, nil
}

func localBrowserArgs(profileDir string, port int, startupURL string) []string {
	return []string{
		"--headless=new",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-background-networking",
		"--disable-component-update",
		"--disable-extensions",
		"--disable-sync",
		"--user-data-dir=" + filepath.Clean(profileDir),
		"--remote-debugging-address=127.0.0.1",
		"--remote-debugging-port=" + strconv.Itoa(port),
		startupURL,
	}
}

func browserHTTPClient(client *http.Client) *http.Client {
	if client != nil {
		return client
	}
	return &http.Client{Timeout: defaultBrowserHealthTimeout}
}

func checkBrowserDebugEndpoint(ctx context.Context, client *http.Client, endpoint string) error {
	if ctx == nil {
		return fmt.Errorf("browser health check context is required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return fmt.Errorf("creating browser health request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("requesting browser health endpoint: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDiscardResponseBytes))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("browser health endpoint returned HTTP %d", resp.StatusCode)
	}
	return nil
}

func normalizePort(port int) (int, error) {
	if port == 0 {
		return chooseLoopbackPort()
	}
	if port < 1 || port > 65535 {
		return 0, fmt.Errorf("debug port must be between 1 and 65535")
	}
	return port, nil
}

func chooseLoopbackPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("choosing loopback debug port: %w", err)
	}
	defer func() {
		_ = listener.Close()
	}()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		return 0, fmt.Errorf("reading loopback debug port: %w", err)
	}
	parsed, err := strconv.Atoi(port)
	if err != nil {
		return 0, fmt.Errorf("parsing loopback debug port: %w", err)
	}
	return parsed, nil
}

func browserDebugURL(port int) string {
	return "http://127.0.0.1:" + strconv.Itoa(port) + "/json/version"
}

func randomLaunchID() (string, error) {
	var data [8]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", fmt.Errorf("generating Docker CUA launch id: %w", err)
	}
	return hex.EncodeToString(data[:]), nil
}

func validateLaunchID(value string) error {
	if value == "" {
		return fmt.Errorf("launch id is required")
	}
	if len(value) > 48 {
		return fmt.Errorf("launch id %q is too long", value)
	}
	for _, char := range value {
		if isASCIILetter(char) || isASCIIDigit(char) || char == '-' || char == '_' {
			continue
		}
		return fmt.Errorf("launch id %q must use ASCII letters, digits, '-' or '_'", value)
	}
	return nil
}

func valueOrDefault(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}
