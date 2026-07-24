package secret

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	keyring "github.com/zalando/go-keyring"
)

const (
	serviceName                = "claude-code-router"
	fileSecretPerm os.FileMode = 0o600
)

var ErrNotFound = errors.New("secret not found")

type Backend interface {
	Available(ctx context.Context) error
	Store(ctx context.Context, ref string, value string) error
	Resolve(ctx context.Context, ref string) (string, error)
}

// Deleter is the optional secret-backend capability used when durable metadata
// is removed. Keeping it separate preserves compatibility with read/store test
// backends and external integrations.
type Deleter interface {
	Delete(ctx context.Context, ref string) error
}

type DefaultBackend struct{}

func (DefaultBackend) Available(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("secret.DefaultBackend.Available: context canceled: %w", err)
	}
	if _, err := keyring.Get(serviceName, "__availability_probe__"); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("secret.DefaultBackend.Available: OS keychain unavailable: %w", err)
	}
	return nil
}

func (DefaultBackend) Store(ctx context.Context, ref, value string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("secret.DefaultBackend.Store: context canceled: %w", err)
	}
	account, ok := strings.CutPrefix(ref, "keyring:")
	if !ok || !validWritableKeyringAccount(account) {
		return fmt.Errorf("secret.DefaultBackend.Store: unsupported writable secret ref %q", RedactRef(ref))
	}
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("secret.DefaultBackend.Store: empty secret for %q", RedactRef(ref))
	}
	if err := keyring.Set(serviceName, account, value); err != nil {
		return keyringStoreError(ref, account, err)
	}
	return nil
}

func keyringStoreError(ref, account string, err error) error {
	if validClaudeAccountKeyringAccount(account) {
		return fmt.Errorf(
			"secret.DefaultBackend.Store: storing %q in OS keychain: %w; configure or unlock the keychain and retry because Claude account OAuth credentials cannot use API-key environment or file references",
			RedactRef(ref),
			err,
		)
	}
	return fmt.Errorf(
		"secret.DefaultBackend.Store: storing %q in OS keychain: %w; use --api-key-env <ENV> or --api-key-file <PATH> to store a provider secret reference instead",
		RedactRef(ref),
		err,
	)
}

func (DefaultBackend) Resolve(ctx context.Context, ref string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("secret.DefaultBackend.Resolve: context canceled: %w", err)
	}
	if name, ok := strings.CutPrefix(ref, "env:"); ok {
		value := os.Getenv(name)
		if value == "" {
			return "", fmt.Errorf("secret.DefaultBackend.Resolve: environment variable %q is empty or unset", name)
		}
		return value, nil
	}
	if account, ok := strings.CutPrefix(ref, "keyring:"); ok {
		if !validWritableKeyringAccount(account) {
			return "", fmt.Errorf("secret.DefaultBackend.Resolve: unsupported secret ref %q", RedactRef(ref))
		}
		value, err := keyring.Get(serviceName, account)
		if err != nil {
			if errors.Is(err, keyring.ErrNotFound) {
				return "", fmt.Errorf(
					"secret.DefaultBackend.Resolve: reading %q from OS keychain: %w",
					RedactRef(ref),
					ErrNotFound,
				)
			}
			return "", fmt.Errorf("secret.DefaultBackend.Resolve: reading %q from OS keychain: %w", RedactRef(ref), err)
		}
		return value, nil
	}
	if path, ok := strings.CutPrefix(ref, "file:"); ok {
		value, err := readFileSecret(path)
		if err != nil {
			return "", fmt.Errorf("secret.DefaultBackend.Resolve: reading %q: %w", RedactRef(ref), err)
		}
		return value, nil
	}
	return "", fmt.Errorf("secret.DefaultBackend.Resolve: unsupported secret ref %q", RedactRef(ref))
}

func (DefaultBackend) Delete(ctx context.Context, ref string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("secret.DefaultBackend.Delete: context canceled: %w", err)
	}
	account, ok := strings.CutPrefix(ref, "keyring:")
	if !ok || !validWritableKeyringAccount(account) {
		return fmt.Errorf("secret.DefaultBackend.Delete: unsupported secret ref %q", RedactRef(ref))
	}
	if err := keyring.Delete(serviceName, account); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("secret.DefaultBackend.Delete: deleting %q from OS keychain: %w", RedactRef(ref), err)
	}
	return nil
}

func EnvRef(name string) string {
	return "env:" + name
}

func FileRef(path string) string {
	return "file:" + path
}

func FileRefFromPath(path string) (string, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return "", fmt.Errorf("API key file path is required")
	}
	absolute, err := filepath.Abs(trimmed)
	if err != nil {
		return "", fmt.Errorf("resolving API key file path: %w", err)
	}
	if _, err := readFileSecret(absolute); err != nil {
		return "", err
	}
	return FileRef(absolute), nil
}

func KeyringRef(providerName string) string {
	return "keyring:provider/" + providerName + "/api-key"
}

func ClaudeAccountAccessTokenRef(accountName string) string {
	return "keyring:claude-account/" + accountName + "/access-token"
}

func ClaudeAccountRefreshTokenRef(accountName string) string {
	return "keyring:claude-account/" + accountName + "/refresh-token"
}

// ValidateRef accepts only the durable secret-reference formats CCR supports.
// File existence and permissions are checked when resolving the reference so a
// portable configuration can be imported before its local credential exists.
func ValidateRef(ref string) error {
	if ref == "" {
		return nil
	}
	if strings.TrimSpace(ref) != ref {
		return fmt.Errorf("secret reference must not contain surrounding whitespace")
	}
	if name, ok := strings.CutPrefix(ref, "env:"); ok {
		if !validEnvName(name) {
			return fmt.Errorf("invalid environment secret reference %q", RedactRef(ref))
		}
		return nil
	}
	if path, ok := strings.CutPrefix(ref, "file:"); ok {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("file secret reference must use an absolute path")
		}
		return nil
	}
	if account, ok := strings.CutPrefix(ref, "keyring:"); ok {
		if !validKeyringAccount(account) {
			return fmt.Errorf("invalid keyring secret reference %q", RedactRef(ref))
		}
		return nil
	}
	return fmt.Errorf("unsupported secret reference %q; expected env:, file:, or keyring", RedactRef(ref))
}

func RedactRef(ref string) string {
	if ref == "" {
		return ""
	}
	if strings.HasPrefix(ref, "env:") {
		return ref
	}
	if prefix, _, ok := strings.Cut(ref, ":"); ok {
		return prefix + ":***"
	}
	return "***"
}

func validEnvName(value string) bool {
	if value == "" {
		return false
	}
	for index, r := range value {
		if (r >= 'A' && r <= 'Z') || r == '_' {
			continue
		}
		if index > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return true
}

func validKeyringAccount(account string) bool {
	parts := strings.Split(account, "/")
	return len(parts) == 3 &&
		parts[0] == "provider" &&
		validKeyringName(parts[1]) &&
		parts[2] == "api-key"
}

func validWritableKeyringAccount(account string) bool {
	return validKeyringAccount(account) || validClaudeAccountKeyringAccount(account)
}

func validClaudeAccountKeyringAccount(account string) bool {
	parts := strings.Split(account, "/")
	return len(parts) == 3 &&
		parts[0] == "claude-account" &&
		validKeyringName(parts[1]) &&
		(parts[2] == "access-token" || parts[2] == "refresh-token")
}

func validKeyringName(value string) bool {
	return value != "" && !strings.ContainsAny(value, "\\/\t\r\n ")
}

func readFileSecret(path string) (string, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return "", fmt.Errorf("API key file path is required")
	}
	info, err := os.Lstat(trimmed)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("API key file does not exist")
		}
		if errors.Is(err, os.ErrPermission) {
			return "", fmt.Errorf("API key file cannot be inspected: permission denied")
		}
		return "", fmt.Errorf("API key file cannot be inspected")
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("API key file must be a regular file")
	}
	if perm := info.Mode().Perm(); perm != fileSecretPerm {
		return "", fmt.Errorf("API key file must have permissions 0600 (got %04o)", perm)
	}
	raw, err := os.ReadFile(trimmed)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return "", fmt.Errorf("API key file cannot be read: permission denied")
		}
		return "", fmt.Errorf("API key file cannot be read")
	}
	value := strings.TrimSpace(string(raw))
	if value == "" {
		return "", fmt.Errorf("API key file is empty")
	}
	return value, nil
}
