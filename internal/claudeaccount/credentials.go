package claudeaccount

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	maxCredentialsBytes = 1 << 20
	maxTokenBytes       = 64 << 10
)

var ErrCurrentCredentialsUnsupported = errors.New("current Claude credentials cannot be imported on this platform")

type Credentials struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    string
	ScopesJSON   string
}

type credentialDocument struct {
	ClaudeAIOAuth oauthCredentials `json:"claudeAiOauth"`
}

type oauthCredentials struct {
	AccessToken  string   `json:"accessToken"`
	RefreshToken string   `json:"refreshToken"`
	ExpiresAt    int64    `json:"expiresAt"`
	Scopes       []string `json:"scopes"`
}

func ReadCurrentCredentials() (Credentials, error) {
	path, err := CurrentCredentialsPath(runtime.GOOS, os.Getenv("CLAUDE_CONFIG_DIR"), "")
	if err != nil {
		return Credentials{}, err
	}
	return readCredentialsFile(path, runtime.GOOS)
}

func CurrentCredentialsPath(goos, configDir, homeDir string) (string, error) {
	if goos == "darwin" {
		return "", fmt.Errorf("%w: macOS stores Claude login credentials in Keychain; use claude setup-token and ccr claude-account import --oauth-token-stdin", ErrCurrentCredentialsUnsupported)
	}
	if goos != "linux" && goos != "windows" {
		return "", fmt.Errorf("%w: unsupported operating system %q", ErrCurrentCredentialsUnsupported, goos)
	}
	base := strings.TrimSpace(configDir)
	if base == "" {
		if homeDir == "" {
			var err error
			homeDir, err = os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("locating the current Claude credentials: %w", err)
			}
		}
		base = filepath.Join(homeDir, ".claude")
	}
	absolute, err := filepath.Abs(base)
	if err != nil {
		return "", fmt.Errorf("resolving the current Claude configuration directory: %w", err)
	}
	return filepath.Join(absolute, ".credentials.json"), nil
}

func CredentialsFromToken(reader io.Reader) (Credentials, error) {
	if reader == nil {
		return Credentials{}, fmt.Errorf("reading Claude OAuth token: input is required")
	}
	raw, err := io.ReadAll(io.LimitReader(reader, maxTokenBytes+1))
	if err != nil {
		return Credentials{}, fmt.Errorf("reading Claude OAuth token: %w", err)
	}
	if len(raw) > maxTokenBytes {
		return Credentials{}, fmt.Errorf("reading Claude OAuth token: token exceeds %d bytes", maxTokenBytes)
	}
	token, err := ValidateToken(string(raw))
	if err != nil {
		return Credentials{}, err
	}
	return Credentials{AccessToken: token, ScopesJSON: "[]"}, nil
}

func ValidateToken(value string) (string, error) {
	token := strings.TrimSpace(value)
	if token == "" {
		return "", fmt.Errorf("claude OAuth token is empty")
	}
	if len(token) > maxTokenBytes {
		return "", fmt.Errorf("claude OAuth token exceeds %d bytes", maxTokenBytes)
	}
	for _, character := range token {
		if character < 0x21 || character == 0x7f {
			return "", fmt.Errorf("claude OAuth token contains whitespace or control characters")
		}
	}
	return token, nil
}

func readCredentialsFile(path, goos string) (Credentials, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Credentials{}, fmt.Errorf("current Claude credentials were not found; run claude auth login first")
		}
		return Credentials{}, fmt.Errorf("inspecting current Claude credentials: %w", err)
	}
	if !info.Mode().IsRegular() {
		return Credentials{}, fmt.Errorf("current Claude credentials must be a regular file")
	}
	if goos != "windows" && info.Mode().Perm() != 0o600 {
		return Credentials{}, fmt.Errorf("current Claude credentials must have permissions 0600")
	}
	file, err := os.Open(path)
	if err != nil {
		return Credentials{}, fmt.Errorf("opening current Claude credentials: %w", err)
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(io.LimitReader(file, maxCredentialsBytes+1))
	if err != nil {
		return Credentials{}, fmt.Errorf("reading current Claude credentials: %w", err)
	}
	if len(raw) > maxCredentialsBytes {
		return Credentials{}, fmt.Errorf("current Claude credentials exceed %d bytes", maxCredentialsBytes)
	}
	return ParseCredentials(raw)
}

func ParseCredentials(raw []byte) (Credentials, error) {
	var document credentialDocument
	if err := json.Unmarshal(raw, &document); err != nil {
		return Credentials{}, fmt.Errorf("current Claude credentials contain invalid JSON")
	}
	accessToken, err := ValidateToken(document.ClaudeAIOAuth.AccessToken)
	if err != nil {
		return Credentials{}, fmt.Errorf("current Claude credentials do not contain a usable access token")
	}
	refreshToken := ""
	if document.ClaudeAIOAuth.RefreshToken != "" {
		refreshToken, err = ValidateToken(document.ClaudeAIOAuth.RefreshToken)
		if err != nil {
			return Credentials{}, fmt.Errorf("current Claude credentials contain an invalid refresh token")
		}
	}
	scopes, err := json.Marshal(document.ClaudeAIOAuth.Scopes)
	if err != nil {
		return Credentials{}, fmt.Errorf("encoding Claude OAuth scopes: %w", err)
	}
	expiresAt := ""
	if document.ClaudeAIOAuth.ExpiresAt > 0 {
		expiresAt = time.UnixMilli(document.ClaudeAIOAuth.ExpiresAt).UTC().Format(time.RFC3339)
	}
	return Credentials{
		AccessToken: accessToken, RefreshToken: refreshToken,
		ExpiresAt: expiresAt, ScopesJSON: string(scopes),
	}, nil
}
