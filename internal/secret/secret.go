package secret

import (
	"context"
	"fmt"
	"os"
	"strings"

	keyring "github.com/zalando/go-keyring"
)

const serviceName = "claude-code-router"

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
		return fmt.Errorf("secret.DefaultBackend.Store: storing %q in OS keychain: %w", RedactRef(ref), err)
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
	return "", fmt.Errorf("secret.DefaultBackend.Resolve: unsupported secret ref %q", RedactRef(ref))
}

func EnvRef(name string) string {
	return "env:" + name
}

func KeyringRef(providerName string) string {
	return "keyring:provider/" + providerName + "/api-key"
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
