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

type Backend interface {
	Available(ctx context.Context) error
	Store(ctx context.Context, ref string, value string) error
	Resolve(ctx context.Context, ref string) (string, error)
}

type DefaultBackend struct{}

func (DefaultBackend) Available(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("secret.DefaultBackend.Available: context canceled: %w", err)
	}
	if _, err := keyring.Get(serviceName, "__availability_probe__"); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("secret.DefaultBackend.Available: OS keychain unavailable: %w; use --api-key-env <ENV> or --api-key-file <PATH> to store a secret reference instead", err)
	}
	return nil
}

func (DefaultBackend) Store(ctx context.Context, ref, value string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("secret.DefaultBackend.Store: context canceled: %w", err)
	}
	account, ok := strings.CutPrefix(ref, "keyring:")
	if !ok {
		return fmt.Errorf("secret.DefaultBackend.Store: unsupported writable secret ref %q", RedactRef(ref))
	}
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("secret.DefaultBackend.Store: empty secret for %q", RedactRef(ref))
	}
	if err := keyring.Set(serviceName, account, value); err != nil {
		return fmt.Errorf("secret.DefaultBackend.Store: storing %q in OS keychain: %w; use --api-key-env <ENV> or --api-key-file <PATH> to store a secret reference instead", RedactRef(ref), err)
	}
	return nil
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
		value, err := keyring.Get(serviceName, account)
		if err != nil {
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
	provider, suffix, found := strings.Cut(account, "/")
	if !found || provider != "provider" {
		return false
	}
	providerName, keyName, found := strings.Cut(suffix, "/")
	if !found || keyName != "api-key" || providerName == "" {
		return false
	}
	return !strings.ContainsAny(providerName, "\\/\t\r\n ")
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
