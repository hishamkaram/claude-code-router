package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/claudeaccount"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

const (
	maxProviderOnlyGuidanceAliases = 5
	maxClaudeAuthStatusBytes       = 64 << 10
	claudeAuthStatusTimeout        = 10 * time.Second
)

type claudeLaunchAuthDetector struct {
	goos               string
	getenv             func(string) string
	credentialsPresent func() (bool, error)
	cliStatus          func(context.Context) (bool, error)
}

type claudeAuthStatusDocument struct {
	LoggedIn *bool `json:"loggedIn"`
}

type cappedOutputBuffer struct {
	buffer   bytes.Buffer
	limit    int
	exceeded bool
}

func (b *cappedOutputBuffer) Write(value []byte) (int, error) {
	written := len(value)
	remaining := b.limit - b.buffer.Len()
	if remaining <= 0 {
		b.exceeded = b.exceeded || written > 0
		return written, nil
	}
	if written > remaining {
		value = value[:remaining]
		b.exceeded = true
	}
	_, _ = b.buffer.Write(value)
	return written, nil
}

func validateLaunchAuthMode(value string) error {
	switch value {
	case launchAuthModeAuto, launchAuthModePreserve, launchAuthModeProviderOnly, launchAuthModeGatewayToken, launchAuthModeSubscriptionPool:
		return nil
	default:
		return fmt.Errorf(
			"invalid launch auth mode %q; expected %s, %s, %s, %s, or %s",
			value, launchAuthModeAuto, launchAuthModePreserve, launchAuthModeProviderOnly, launchAuthModeSubscriptionPool, launchAuthModeGatewayToken,
		)
	}
}

func validateResolvedLaunchAuthMode(authMode, modelAlias string) error {
	if providerOnlyLaunchAuthMode(authMode) && modelAlias == "" {
		return fmt.Errorf("--auth-mode %s requires --model <alias>; use --auth-mode preserve for Claude Code default first-party routing", authMode)
	}
	return nil
}

func providerOnlyLaunchAuthMode(authMode string) bool {
	return authMode == launchAuthModeProviderOnly || authMode == launchAuthModeGatewayToken
}

func resolveLaunchAuthMode(
	ctx context.Context,
	deps Dependencies,
	s *store.Store,
	invocation launchInvocation,
	modelAlias string,
) (string, error) {
	if invocation.authMode != launchAuthModeAuto {
		return invocation.authMode, nil
	}
	if launchClaudeAuthPresentOrUnknown(ctx, deps) {
		return launchAuthModePreserve, nil
	}
	if modelAlias != "" {
		return launchAuthModeProviderOnly, nil
	}
	return "", missingClaudeAuthForAutoLaunchError(ctx, s)
}

func preflightAutoLaunchAuthBeforeStore(
	ctx context.Context,
	opts *options,
	deps Dependencies,
	invocation launchInvocation,
) (launchInvocation, error) {
	if invocation.authMode != launchAuthModeAuto || invocation.modelAlias != "" {
		return invocation, nil
	}
	if launchClaudeAuthPresentOrUnknown(ctx, deps) {
		invocation.authMode = launchAuthModePreserve
		return invocation, nil
	}
	dbPath, err := resolveDBPath(opts)
	if err != nil {
		return invocation, err
	}
	aliases, err := readOnlyRoutableModelAliases(ctx, dbPath)
	if err != nil {
		return invocation, missingClaudeAuthForAutoLaunchAliasReadError(err)
	}
	return invocation, missingClaudeAuthForAutoLaunchAliasesError(aliases)
}

func missingClaudeAuthForAutoLaunchError(ctx context.Context, s *store.Store) error {
	aliases, err := routableModelAliases(ctx, s, true)
	if err != nil {
		return missingClaudeAuthForAutoLaunchAliasReadError(err)
	}
	return missingClaudeAuthForAutoLaunchAliasesError(aliases)
}

func missingClaudeAuthForAutoLaunchAliasesError(aliases []string) error {
	if len(aliases) == 0 {
		return fmt.Errorf("claude subscription authentication was not found and no CCR startup model was selected; run claude /login or configure a CCR provider and launch with --model <alias>")
	}
	return fmt.Errorf(
		"claude subscription authentication was not found and no CCR startup model was selected; CCR will not choose a provider automatically. Rerun with --model <alias>, for example: ccr launch --model %s. Available provider aliases: %s",
		aliases[0],
		strings.Join(providerOnlyGuidanceAliases(aliases), ", "),
	)
}

func missingClaudeAuthForAutoLaunchAliasReadError(err error) error {
	return fmt.Errorf("claude subscription authentication was not found and no CCR startup model was selected; reading routable provider aliases: %w", err)
}

func readOnlyRoutableModelAliases(ctx context.Context, dbPath string) ([]string, error) {
	s, err := store.OpenReadOnly(ctx, dbPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer closeStore(s)
	return routableModelAliases(ctx, s, true)
}

func launchClaudeAuthPresentOrUnknown(ctx context.Context, deps Dependencies) bool {
	hasAuth, err := deps.DetectClaudeAuth(ctx)
	// Unknown stays preserve so auto never disables auth that could not be inspected.
	return err != nil || hasAuth
}

func providerOnlyGuidanceAliases(aliases []string) []string {
	if len(aliases) <= maxProviderOnlyGuidanceAliases {
		return aliases
	}
	result := append([]string(nil), aliases[:maxProviderOnlyGuidanceAliases]...)
	result = append(result, "...")
	return result
}

func detectClaudeLaunchAuth(ctx context.Context) (bool, error) {
	detector := claudeLaunchAuthDetector{
		goos:               runtime.GOOS,
		getenv:             os.Getenv,
		credentialsPresent: currentClaudeCredentialsPresent,
		cliStatus:          currentClaudeCLIAuthStatus,
	}
	return detector.Detect(ctx)
}

func (d claudeLaunchAuthDetector) Detect(ctx context.Context) (bool, error) {
	if claudeLaunchAuthEnvPresent(d.getenv) {
		return true, nil
	}
	if d.goos == "darwin" {
		return d.cliStatus(ctx)
	}
	return d.credentialsPresent()
}

func claudeLaunchAuthEnvPresent(getenv func(string) string) bool {
	for _, name := range []string{
		"ANTHROPIC_API_KEY",
		"CLAUDE_CODE_OAUTH_TOKEN",
		"CLAUDE_CODE_OAUTH_REFRESH_TOKEN",
	} {
		if strings.TrimSpace(getenv(name)) != "" {
			return true
		}
	}
	return anthropicCustomHeadersContainAuth(getenv("ANTHROPIC_CUSTOM_HEADERS"))
}

func anthropicCustomHeadersContainAuth(value string) bool {
	for _, line := range strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		name, authValue, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(authValue) == "" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "authorization", "x-api-key":
			return true
		}
	}
	return false
}

func currentClaudeCredentialsPresent() (bool, error) {
	path, err := claudeaccount.CurrentCredentialsPath(runtime.GOOS, os.Getenv("CLAUDE_CONFIG_DIR"), "")
	if err != nil {
		return false, fmt.Errorf("checking current Claude credentials: %w", err)
	}
	if _, statErr := os.Lstat(path); statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("checking current Claude credentials: %w", statErr)
	}
	credentials, readErr := claudeaccount.ReadCurrentCredentials()
	if errors.Is(readErr, claudeaccount.ErrCurrentCredentialsNoAccessToken) {
		return false, nil
	}
	if readErr != nil {
		return false, fmt.Errorf("checking current Claude credentials: %w", readErr)
	}
	return strings.TrimSpace(credentials.AccessToken) != "", nil
}

func currentClaudeCLIAuthStatus(ctx context.Context) (bool, error) {
	statusCtx, cancel := context.WithTimeout(ctx, claudeAuthStatusTimeout)
	defer cancel()

	stdout := cappedOutputBuffer{limit: maxClaudeAuthStatusBytes}
	command := exec.CommandContext(statusCtx, "claude", "auth", "status", "--json")
	command.Env = applyClaudeEnvironment(os.Environ(), ClaudeEnvironment{Unset: []string{
		"ANTHROPIC_API_KEY",
		"ANTHROPIC_AUTH_TOKEN",
		"ANTHROPIC_BASE_URL",
		"ANTHROPIC_CUSTOM_HEADERS",
		"CLAUDE_CODE_OAUTH_TOKEN",
		"CLAUDE_CODE_OAUTH_REFRESH_TOKEN",
		"CLAUDE_CODE_OAUTH_SCOPES",
	}})
	command.Stdout = &stdout
	command.Stderr = io.Discard
	runErr := command.Run()
	if stdout.exceeded {
		return false, fmt.Errorf("checking Claude auth status: output exceeds %d bytes", maxClaudeAuthStatusBytes)
	}
	if statusCtx.Err() != nil {
		return false, fmt.Errorf("checking Claude auth status: %w", statusCtx.Err())
	}
	return evaluateClaudeAuthStatus(stdout.buffer.Bytes(), runErr)
}

func evaluateClaudeAuthStatus(raw []byte, runErr error) (bool, error) {
	hasAuth, parseErr := parseClaudeAuthStatus(raw)
	if parseErr == nil {
		// Claude exits nonzero when signed out but still emits the authoritative JSON status.
		return hasAuth, nil
	}
	if runErr != nil {
		return false, fmt.Errorf("checking Claude auth status: %w", runErr)
	}
	return false, parseErr
}

func parseClaudeAuthStatus(raw []byte) (bool, error) {
	var document claudeAuthStatusDocument
	if err := json.Unmarshal(raw, &document); err != nil {
		return false, fmt.Errorf("parsing Claude auth status: %w", err)
	}
	if document.LoggedIn == nil {
		return false, fmt.Errorf("parsing Claude auth status: loggedIn field is missing")
	}
	return *document.LoggedIn, nil
}
