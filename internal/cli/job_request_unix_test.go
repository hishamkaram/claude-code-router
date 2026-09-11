//go:build linux || darwin

package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRequestFingerprintBindsPayloadAndExcludesLocatorsAndGeneratedIDs(t *testing.T) {
	args := []string{"--detach", "-p", "--model=alias", "--prompt-file=/first", "--submission-id=one", "--output-format=stream-json", "--verbose"}
	invocation, err := parseLaunchInvocation(args)
	if err != nil {
		t.Fatal(err)
	}
	execution := admissionContext{Directory: "/work", Database: "/db", JobRoot: "/jobs", User: 42, Profile: map[string]string{"HOME": "/home"}}
	original, err := requestFingerprint(invocation, []byte("exact\nprompt"), execution)
	if err != nil {
		t.Fatal(err)
	}
	same := invocation
	same.promptFile, same.submissionID = "/second", "two"
	replay, err := requestFingerprint(same, []byte("exact\nprompt"), execution)
	if err != nil || replay != original {
		t.Fatalf("locator or generated identity changed fingerprint: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*launchInvocation, *[]byte, *admissionContext)
	}{
		{"prompt", func(_ *launchInvocation, p *[]byte, _ *admissionContext) { *p = []byte("exact prompt") }},
		{"model", func(i *launchInvocation, _ *[]byte, _ *admissionContext) { i.modelAlias = "other" }},
		{"argv-order", func(i *launchInvocation, _ *[]byte, _ *admissionContext) {
			i.claudeArgs = []string{"--verbose", "--output-format=stream-json"}
		}},
		{"requested-session", func(i *launchInvocation, _ *[]byte, _ *admissionContext) { i.resumeSession = "session" }},
		{"parent", func(i *launchInvocation, _ *[]byte, _ *admissionContext) { i.expectedParent = "parent" }},
		{"directory", func(_ *launchInvocation, _ *[]byte, c *admissionContext) { c.Directory = "/elsewhere" }},
		{"database", func(_ *launchInvocation, _ *[]byte, c *admissionContext) { c.Database = "/other-db" }},
		{"namespace", func(_ *launchInvocation, _ *[]byte, c *admissionContext) { c.JobRoot = "/other-jobs" }},
		{"user", func(_ *launchInvocation, _ *[]byte, c *admissionContext) { c.User = 43 }},
		{"profile", func(_ *launchInvocation, _ *[]byte, c *admissionContext) {
			c.Profile = map[string]string{"HOME": "/other"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			i, p, c := invocation, []byte("exact\nprompt"), execution
			test.mutate(&i, &p, &c)
			got, err := requestFingerprint(i, p, c)
			if err != nil || got == original {
				t.Fatalf("changed request accepted: %v", err)
			}
		})
	}
}

func TestAdmissionCanonicalPathResolvesMissingChildrenThroughSymlink(t *testing.T) {
	root := t.TempDir()
	actual := filepath.Join(root, "actual")
	if err := os.Mkdir(actual, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(actual, alias); err != nil {
		t.Fatal(err)
	}
	a, err := canonicalAdmissionPath(filepath.Join(alias, "missing", "db"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := canonicalAdmissionPath(filepath.Join(actual, "missing", "db"))
	if err != nil || a != b {
		t.Fatalf("canonical paths differ: %q %q %v", a, b, err)
	}
}

func TestAdmissionContextPreservesExplicitEmptyProfile(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	root := t.TempDir()
	got, err := canonicalAdmissionContext(filepath.Join(root, "db"), filepath.Join(root, "jobs"))
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := got.Profile["CLAUDE_CONFIG_DIR"]; !ok || value != "" {
		t.Fatalf("lost explicit empty profile: %+v", got)
	}
	if got.User != os.Geteuid() || !filepath.IsAbs(got.Directory) {
		t.Fatalf("incorrect effective execution context: %+v", got)
	}
}

func TestRequestFingerprintNormalizesOwnedOptionSpellings(t *testing.T) {
	for _, pair := range [][2][]string{
		{{"--detach", "-p", "--model=x"}, {"--detach=true", "--print=true", "--model", "x", "--auth-mode=auto"}},
		{{"--detach", "-p", "--auth-mode=gateway-token"}, {"--detach", "-p", "--auth-mode=provider-only"}},
	} {
		first, err := parseLaunchInvocation(pair[0])
		if err != nil {
			t.Fatal(err)
		}
		second, err := parseLaunchInvocation(pair[1])
		if err != nil {
			t.Fatal(err)
		}
		a, err := requestFingerprint(first, []byte("prompt"), admissionContext{})
		if err != nil {
			t.Fatal(err)
		}
		b, err := requestFingerprint(second, []byte("prompt"), admissionContext{})
		if err != nil || a != b {
			t.Fatalf("equivalent owned options differ: %q %q %v", pair[0], pair[1], err)
		}
	}
}
