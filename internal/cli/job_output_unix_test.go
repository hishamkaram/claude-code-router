//go:build linux || darwin

package cli

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestPreparedOutputRejectsSpecialFilesWithoutTruncatingTargets(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(root, "fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{link, fifo, root} {
		if file, err := openJobOutput(path); err == nil {
			_ = file.Close()
			t.Fatalf("accepted special output %s", path)
		}
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "preserve" {
		t.Fatalf("changed target: %q %v", data, err)
	}
	file, err := openJobOutput(target)
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	info, err := os.Stat(target)
	if err != nil || info.Size() != 0 {
		t.Fatalf("regular prepared output not reset: %v", err)
	}
}
