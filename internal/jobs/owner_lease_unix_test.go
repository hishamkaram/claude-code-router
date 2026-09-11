//go:build linux || darwin

package jobs

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestOwnerSocketRecoveryRequiresMatchingLease(t *testing.T) {
	s := Store{Root: t.TempDir()}
	record, err := s.Create()
	if err != nil {
		t.Fatal(err)
	}
	if writeErr := s.Write(record); writeErr != nil {
		t.Fatal(writeErr)
	}
	lock, acquired, err := s.Lock(record.JobID)
	if err != nil || !acquired {
		t.Fatalf("lease=%v %v", acquired, err)
	}
	defer func() { _ = lock.Close() }()
	path, err := s.socketPath(record.JobID)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if closeErr := listener.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	unrelated, err := os.Create(filepath.Join(t.TempDir(), "unrelated"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unrelated.Close() }()
	if wrongErr := s.PrepareControlSocket(record.JobID, unrelated); wrongErr == nil {
		t.Fatal("unrelated FD granted ownership")
	}
	if _, statErr := os.Lstat(path); statErr != nil {
		t.Fatal("unowned recovery removed socket")
	}
	if recoverErr := s.PrepareControlSocket(record.JobID, lock); recoverErr != nil {
		t.Fatal(recoverErr)
	}
	if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
		t.Fatalf("stale socket retained: %v", statErr)
	}
	if writeErr := os.WriteFile(path, []byte("not a socket"), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	if recoverErr := s.PrepareControlSocket(record.JobID, lock); recoverErr == nil {
		t.Fatal("removed non-socket control artifact")
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil || string(data) != "not a socket" {
		t.Fatalf("changed refused artifact: %q %v", data, readErr)
	}
}

func TestCanonicalControlAddressPreservesLegacyOwnerCompatibility(t *testing.T) {
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	record := Record{JobID: testAdmission().JobID, SchemaVersion: 2}
	original, err := (Store{Root: root}).controlSocketPath(record)
	if err != nil {
		t.Fatal(err)
	}
	alternate, err := (Store{Root: alias}).controlSocketPath(record)
	if err != nil {
		t.Fatal(err)
	}
	if original != alternate {
		t.Fatal("canonical namespace selected different control sockets")
	}
	record.SchemaVersion = 1
	legacy, err := (Store{Root: alias}).controlSocketPath(record)
	if err != nil {
		t.Fatal(err)
	}
	previous, err := (Store{Root: alias}).socketPath(record.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if legacy != previous || legacy == original {
		t.Fatal("legacy owner socket addressing changed")
	}
}
