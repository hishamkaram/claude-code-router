package buildinfo

import (
	"strings"
	"testing"
)

func TestStringIncludesBuildFields(t *testing.T) {
	t.Parallel()

	got := String()
	for _, want := range []string{"ccr ", Version, Commit, Date, BuiltBy} {
		if !strings.Contains(got, want) {
			t.Fatalf("String() = %q, want field %q", got, want)
		}
	}
}
