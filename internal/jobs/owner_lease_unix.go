//go:build linux || darwin

package jobs

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// ValidateOwnerLease adopts a correctly inherited descriptor. An arbitrary FD
// must not grant ownership simply because the owner command received it at FD5.
func (s Store) ValidateOwnerLease(id string, lease *os.File) error {
	directory, err := s.Directory(id)
	if err != nil {
		return err
	}
	if lease == nil {
		return fmt.Errorf("job owner lease is missing")
	}
	actual, err := lease.Stat()
	if err != nil {
		return fmt.Errorf("reading inherited owner lease: %w", err)
	}
	expected, err := os.Stat(filepath.Join(directory, "owner.lock"))
	if err != nil || !os.SameFile(actual, expected) {
		return fmt.Errorf("inherited owner lease identity differs from job")
	}
	if err := unix.Flock(int(lease.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fmt.Errorf("adopting job owner lease: %w", err)
	}
	unix.CloseOnExec(int(lease.Fd()))
	return nil
}

// PrepareControlSocket removes a stale socket only while this job's ownership
// lease is held. A crash can leave the socket pathname after its listener dies.
func (s Store) PrepareControlSocket(id string, lease *os.File) error {
	if err := s.ValidateOwnerLease(id, lease); err != nil {
		return err
	}
	record, err := s.Read(id)
	if err != nil {
		return err
	}
	path, err := s.controlSocketPath(record)
	if err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking prior owner socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("prior owner control path is not a socket")
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("removing stale owner socket: %w", err)
	}
	return nil
}
