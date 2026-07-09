package secret

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedactRef(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ref  string
		want string
	}{
		{name: "empty", ref: "", want: ""},
		{name: "env", ref: "env:OPENROUTER_API_KEY", want: "env:OPENROUTER_API_KEY"},
		{name: "keyring", ref: "keyring:provider/openrouter/api-key", want: "keyring:***"},
		{name: "file", ref: "file:/home/user/.config/ccr/litellm.key", want: "file:***"},
		{name: "opaque", ref: "plain-secret", want: "***"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := RedactRef(tt.ref); got != tt.want {
				t.Fatalf("RedactRef(%q) = %q, want %q", tt.ref, got, tt.want)
			}
		})
	}
}

func TestDefaultBackendResolveFileRef(t *testing.T) {
	t.Parallel()

	path := writeSecretFile(t, "sk-test\n", 0o600)
	ref, err := FileRefFromPath(path)
	if err != nil {
		t.Fatalf("FileRefFromPath error = %v", err)
	}
	wantRef := FileRef(path)
	if ref != wantRef {
		t.Fatalf("FileRefFromPath() = %q, want %q", ref, wantRef)
	}
	got, err := (DefaultBackend{}).Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve(file) error = %v", err)
	}
	if got != "sk-test" {
		t.Fatalf("Resolve(file) = %q, want sk-test", got)
	}
}

func TestDefaultBackendResolveFileRefRejectsInvalidFiles(t *testing.T) {
	t.Parallel()

	missingPath := filepath.Join(t.TempDir(), "missing.key")
	directoryPath := t.TempDir()
	emptyPath := writeSecretFile(t, "\n", 0o600)
	loosePath := writeSecretFile(t, "sk-test\n", 0o644)

	tests := []struct {
		name string
		ref  string
		want string
	}{
		{name: "missing", ref: FileRef(missingPath), want: "does not exist"},
		{name: "directory", ref: FileRef(directoryPath), want: "must be a regular file"},
		{name: "empty", ref: FileRef(emptyPath), want: "file is empty"},
		{name: "permissions", ref: FileRef(loosePath), want: "must have permissions 0600"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := (DefaultBackend{}).Resolve(context.Background(), tt.ref)
			if err == nil {
				t.Fatalf("Resolve(%s) unexpectedly succeeded", tt.name)
			}
			if strings.Contains(err.Error(), "sk-test") {
				t.Fatalf("Resolve(%s) leaked secret in error: %v", tt.name, err)
			}
			if path := strings.TrimPrefix(tt.ref, "file:"); path != "" && strings.Contains(err.Error(), path) {
				t.Fatalf("Resolve(%s) leaked file path in error: %v", tt.name, err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Resolve(%s) error = %v, want %q", tt.name, err, tt.want)
			}
		})
	}
}

func writeSecretFile(t *testing.T, value string, perm os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api.key")
	if err := os.WriteFile(path, []byte(value), perm); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatalf("Chmod error = %v", err)
	}
	return path
}
