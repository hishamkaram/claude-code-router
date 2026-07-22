package executor

import (
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/cua"
)

func TestParseTarget(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name         string
		value        string
		wantKind     cua.ExecutorKind
		wantExternal string
	}{
		{name: "docker", value: "docker", wantKind: cua.ExecutorDocker},
		{name: "local", value: " local-browser ", wantKind: cua.ExecutorLocalBrowser},
		{name: "macos", value: "macos-preview", wantKind: cua.ExecutorMacOSPreview},
		{name: "external", value: "external:browser_1.prod", wantKind: cua.ExecutorExternal, wantExternal: "browser_1.prod"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseTarget(test.value)
			if err != nil {
				t.Fatalf("ParseTarget(%q) error = %v", test.value, err)
			}
			if got.Kind != test.wantKind || got.ExternalName != test.wantExternal {
				t.Fatalf("ParseTarget(%q) = %#v", test.value, got)
			}
		})
	}
}

func TestParseTargetRejectsInvalidExternalName(t *testing.T) {
	t.Parallel()

	for _, value := range []string{
		"",
		"unknown",
		"external:",
		"external: bad",
		"external:-bad",
		"external:name/with/slash",
	} {
		t.Run(value, func(t *testing.T) {
			t.Parallel()

			if _, err := ParseTarget(value); err == nil {
				t.Fatalf("ParseTarget(%q) unexpectedly succeeded", value)
			}
		})
	}
}
