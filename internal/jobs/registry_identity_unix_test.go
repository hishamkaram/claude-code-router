//go:build linux || darwin

package jobs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMissingEstablishedRegistryCannotBeRecreated(t *testing.T) {
	root := t.TempDir()
	registry, err := OpenRegistry(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if closeErr := registry.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	path := filepath.Join(root, "admissions.sqlite")
	if removeErr := os.Remove(path); removeErr != nil {
		t.Fatal(removeErr)
	}
	if reopened, openErr := OpenRegistry(t.Context(), root); openErr == nil {
		_ = reopened.Close()
		t.Fatal("recreated lost session authority")
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("missing authority was rewritten: %v", statErr)
	}
}

func TestCorruptRegistryIdentityFailsClosed(t *testing.T) {
	root := t.TempDir()
	registry, err := OpenRegistry(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if closeErr := registry.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if writeErr := os.WriteFile(filepath.Join(root, "admissions.identity"), []byte("corrupt"), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	if reopened, openErr := OpenRegistry(t.Context(), root); openErr == nil {
		_ = reopened.Close()
		t.Fatal("accepted corrupt authority marker")
	}
}

func TestTruncatedEstablishedRegistryCannotResetAuthority(t *testing.T) {
	root := t.TempDir()
	registry, err := OpenRegistry(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "admissions.sqlite")
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	if reopened, err := OpenRegistry(t.Context(), root); err == nil {
		_ = reopened.Close()
		t.Fatal("reinitialized a truncated established registry")
	}
}

func TestUnestablishedRegistryRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "outside")
	original := []byte("unrelated data")
	if err := os.WriteFile(target, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "admissions.sqlite")); err != nil {
		t.Fatal(err)
	}
	if registry, err := OpenRegistry(t.Context(), root); err == nil {
		_ = registry.Close()
		t.Fatal("followed database symlink")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != string(original) {
		t.Fatalf("changed unrelated target: %q %v", data, err)
	}
}
