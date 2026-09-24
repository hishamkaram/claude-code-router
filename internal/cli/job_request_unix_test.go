//go:build linux || darwin

package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
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

func TestRequestFingerprintPreservesLegacyEncodingWithoutForwardedSeparator(t *testing.T) {
	invocation, err := parseLaunchInvocation([]string{"--detach", "-p", "--model=alias", "--prompt-file=/prompt"})
	if err != nil {
		t.Fatal(err)
	}
	execution := admissionContext{Directory: "/work", Database: "/db", JobRoot: "/jobs", User: 42, Profile: map[string]string{"HOME": "/home"}}
	got, err := requestFingerprint(invocation, []byte("prompt"), execution)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := admissionDigest(struct {
		Version          int
		Prompt           []byte
		Options          admissionOptions
		Forwarded        []string
		RequestedSession string
		ExpectedParent   string
		Context          admissionContext
	}{
		Version: 1, Prompt: []byte("prompt"), Context: execution,
		Forwarded: invocation.claudeArgs,
		Options: admissionOptions{
			Model: invocation.modelAlias, Print: invocation.printMode,
			AuthMode: normalizedAdmissionAuthMode(invocation.authMode), ClaudeAccount: invocation.claudeAccount,
			PermissionMode: invocation.permissionMode, NoHistory: invocation.noHistory,
			NoLifecycle: invocation.noLifecycle, NoStatusline: invocation.noStatusline,
			CUA: invocation.cuaConfig, CUAExternalURL: invocation.cuaExternalURL, CUATokenEnv: invocation.cuaTokenEnv,
			CUAExplicit: [5]bool{invocation.cuaModeSet, invocation.cuaExecutorSet, invocation.cuaLimitsSet, invocation.cuaURLSet, invocation.cuaTokenEnvSet},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != legacy {
		t.Fatalf("request fingerprint changed legacy encoding: got %q, legacy %q", got, legacy)
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

func TestDetachedClaudeProfileDestinationUsesSessionIdentity(t *testing.T) {
	root := "/var/lib/ccr/jobs"
	sessionID := "11111111-1111-4111-8111-111111111111"
	got := detachedClaudeProfileDestination(root, sessionID)
	want := "/var/lib/ccr/jobs/claude-profiles/11111111-1111-4111-8111-111111111111"
	if got != want {
		t.Fatalf("detachedClaudeProfileDestination() = %q, want %q", got, want)
	}
}

func TestDetachedClaudeProfileDestinationAvoidsBroadNativeSource(t *testing.T) {
	source := t.TempDir()
	root := filepath.Join(source, "jobs")
	sessionID := "11111111-1111-4111-8111-111111111111"
	got, err := detachedClaudeProfileDestinationForSource(root, source, sessionID)
	if err != nil {
		t.Fatalf("detachedClaudeProfileDestinationForSource() error = %v", err)
	}
	if claudePathContains(source, got) {
		t.Fatalf("detached profile %q is inside native source %q", got, source)
	}
	if got == detachedClaudeProfileDestination(root, sessionID) {
		t.Fatalf("broad native source reused unsafe job-root profile %q", got)
	}
}

func TestDetachedClaudeProfileDestinationMigratesPersistedProfileWhenSourceBroadens(t *testing.T) {
	root := t.TempDir()
	sessionID := uuid.NewString()
	persisted := detachedClaudeProfileDestination(root, sessionID)
	if err := os.MkdirAll(persisted, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(persisted, ".ccr-profile-ready"), []byte("ccr private Claude profile\nsource=previous\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(persisted, "projects.jsonl"), []byte("session history"), 0o600); err != nil {
		t.Fatal(err)
	}
	broadSource := filepath.Dir(root)
	got, err := resolveDetachedClaudeProfileDestination(root, broadSource, sessionID, persisted, true)
	if err != nil {
		t.Fatalf("resolveDetachedClaudeProfileDestination() error = %v", err)
	}
	if got == persisted || claudePathContains(broadSource, got) {
		t.Fatalf("migrated profile = %q; want destination outside broad source %q", got, broadSource)
	}
	if _, statErr := os.Stat(persisted); !os.IsNotExist(statErr) {
		t.Fatalf("persisted profile source still exists: %v", statErr)
	}
	history, err := os.ReadFile(filepath.Join(got, "projects.jsonl"))
	if err != nil || string(history) != "session history" {
		t.Fatalf("migrated session history = %q, %v", history, err)
	}
}

func TestDetachedClaudeProfileDestinationRejectsUnownedPersistedProfile(t *testing.T) {
	root := t.TempDir()
	sessionID := uuid.NewString()
	persisted := detachedClaudeProfileDestination(root, sessionID)
	if err := os.MkdirAll(persisted, 0o700); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(persisted, "settings.json")
	if err := os.WriteFile(settings, []byte(`{"model":"native-model"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	broadSource := filepath.Dir(root)
	if _, err := resolveDetachedClaudeProfileDestination(root, broadSource, sessionID, persisted, true); err == nil {
		t.Fatal("resolveDetachedClaudeProfileDestination() accepted an unowned persisted directory")
	}
	if got, err := os.ReadFile(settings); err != nil || string(got) != `{"model":"native-model"}` {
		t.Fatalf("unowned persisted profile changed after rejection: %q, %v", got, err)
	}
}

func TestDetachedClaudeProfileDestinationRecoversCommittedMigration(t *testing.T) {
	root := t.TempDir()
	sessionID := uuid.NewString()
	persisted := detachedClaudeProfileDestination(root, sessionID)
	broadSource := filepath.Dir(root)
	candidate, err := detachedClaudeProfileDestinationForSource(root, broadSource, sessionID)
	if err != nil {
		t.Fatalf("detachedClaudeProfileDestinationForSource() error = %v", err)
	}
	if candidate == persisted || claudePathContains(broadSource, candidate) {
		t.Fatalf("recovery candidate = %q; want outside source %q and distinct from %q", candidate, broadSource, persisted)
	}
	if mkdirErr := os.MkdirAll(candidate, 0o700); mkdirErr != nil {
		t.Fatal(mkdirErr)
	}
	if writeErr := os.WriteFile(filepath.Join(candidate, ".ccr-profile-ready"), []byte("ccr private Claude profile\nsource=previous\n"), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	if writeErr := os.WriteFile(filepath.Join(candidate, "projects.jsonl"), []byte("committed session history"), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}

	got, err := resolveDetachedClaudeProfileDestination(root, broadSource, sessionID, persisted, true)
	if err != nil {
		t.Fatalf("resolveDetachedClaudeProfileDestination() recovery error = %v", err)
	}
	if got != candidate {
		t.Fatalf("recovered profile = %q, want %q", got, candidate)
	}
	history, err := os.ReadFile(filepath.Join(got, "projects.jsonl"))
	if err != nil || string(history) != "committed session history" {
		t.Fatalf("recovered session history = %q, %v", history, err)
	}
}

func TestDetachedClaudeProfileDestinationFinishesInterruptedMigration(t *testing.T) {
	root := t.TempDir()
	sessionID := uuid.NewString()
	persisted := detachedClaudeProfileDestination(root, sessionID)
	broadSource := filepath.Dir(root)
	candidate, err := detachedClaudeProfileDestinationForSource(root, broadSource, sessionID)
	if err != nil {
		t.Fatalf("detachedClaudeProfileDestinationForSource() error = %v", err)
	}
	for _, directory := range []string{persisted, candidate} {
		if mkdirErr := os.MkdirAll(directory, 0o700); mkdirErr != nil {
			t.Fatal(mkdirErr)
		}
		if writeErr := os.WriteFile(filepath.Join(directory, ".ccr-profile-ready"), []byte("ccr private Claude profile\nsource=previous\n"), 0o600); writeErr != nil {
			t.Fatal(writeErr)
		}
	}

	got, err := resolveDetachedClaudeProfileDestination(root, broadSource, sessionID, persisted, true)
	if err != nil {
		t.Fatalf("resolveDetachedClaudeProfileDestination() interrupted migration error = %v", err)
	}
	if got != candidate {
		t.Fatalf("finished migration profile = %q, want %q", got, candidate)
	}
	if _, err := os.Stat(persisted); !os.IsNotExist(err) {
		t.Fatalf("interrupted migration source still exists: %v", err)
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
