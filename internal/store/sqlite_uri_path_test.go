package store

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSQLiteURIPathAlwaysStartsWithSlash(t *testing.T) {
	// A Windows absolute path starts with a drive letter. Without a leading
	// slash url.URL renders file://C:/... and SQLite reads "C:" as the URI
	// authority ("invalid uri authority: C:"). POSIX paths must pass through
	// unchanged.
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "posix", in: "/home/user/.local/share/ccr.db", want: "/home/user/.local/share/ccr.db"},
		{name: "windows drive", in: `C:\Users\user\ccr.db`, want: "/" + filepath.ToSlash(`C:\Users\user\ccr.db`)},
		{name: "windows forward slashes", in: "D:/data/ccr.db", want: "/D:/data/ccr.db"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sqliteURIPath(tc.in)
			if got != tc.want {
				t.Fatalf("sqliteURIPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if !strings.HasPrefix(got, "/") {
				t.Fatalf("sqliteURIPath(%q) = %q does not start with a slash", tc.in, got)
			}
		})
	}
}

func TestOpenWorksWithAbsoluteTempPath(t *testing.T) {
	// filepath.Abs(t.TempDir()) is drive-letter based on Windows and
	// slash-based elsewhere; Open must succeed on both.
	path := filepath.Join(t.TempDir(), "uri", "ccr.db")
	s, err := Open(t.Context(), path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
