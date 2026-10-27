//go:build linux || darwin

package cli

import (
	"slices"
	"strings"
	"testing"
)

func TestOwnedJobSessionArgumentsPreserveSourcePrefix(t *testing.T) {
	for _, flag := range []string{"--session-id", "--resume"} {
		original := []string{"--setting-sources", "project", "--setting-sources=user", "--verbose"}
		args := slices.Clone(original)
		got, err := ownedJobClaudeArgs(args, flag, "registered-session")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"--setting-sources", "project", "--setting-sources=user", flag, "registered-session", "--verbose"}
		if !slices.Equal(got, want) || !slices.Equal(args, original) {
			t.Fatal("job session insertion changed source controls, forwarded order, or admitted argv")
		}
		if validationErr := validateLaunchPassthroughArgs(got); validationErr != nil {
			t.Fatalf("owned job arguments lost the valid source prefix: %v", validationErr)
		}
		sources, err := parseNativeSettingsSources(got)
		if err != nil || !sources.UserEnabled {
			t.Fatal("owned job execution changed native user-settings selection")
		}
	}
}

func TestOwnedJobRejectsSavedArgumentsWithLateSourceControl(t *testing.T) {
	_, err := ownedJobClaudeArgs([]string{"--verbose", "--setting-sources=synthetic-private-marker"}, "--resume", "registered-session")
	if err == nil || !strings.Contains(err.Error(), "put --setting-sources before") {
		t.Fatal("saved job with ambiguous source control was not rejected explicitly")
	}
	if strings.Contains(err.Error(), "synthetic-private-marker") {
		t.Fatal("saved job error exposed a rejected argument value")
	}
}

func TestOwnedJobRejectsLiteralBoundaryBeforeSessionInsertion(t *testing.T) {
	for _, args := range [][]string{
		{"--", "--setting-sources=synthetic-private-marker"},
		{"--setting-sources=project", "--", "--setting-sources=synthetic-private-marker"},
	} {
		_, err := ownedJobClaudeArgs(args, "--resume", "registered-session")
		if err == nil || !strings.Contains(err.Error(), "standalone -- is not supported") {
			t.Fatal("owned job accepted a literal boundary that admission forbids")
		}
		if strings.Contains(err.Error(), "synthetic-private-marker") {
			t.Fatal("owned job boundary error exposed private prompt data")
		}
	}
}
