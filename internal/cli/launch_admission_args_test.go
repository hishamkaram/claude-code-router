package cli

import (
	"reflect"
	"strings"
	"testing"
)

func TestAdmissionOptionsRejectConflictsBeforeLaunch(t *testing.T) {
	for _, args := range [][]string{
		{"--submission-id=x"},
		{"--expected-parent-job=x"},
		{"--detach", "--submission-id="},
		{"--detach", "--submission-id=x", "--submission-id=x"},
		{"--detach", "--expected-parent-job=x"},
		{"--detach", "--resume="},
		{"--detach", "--resume=x", "--resume=y"},
	} {
		if _, err := parseLaunchInvocation(args); err == nil {
			t.Fatalf("accepted %q", args)
		}
	}
}

func TestDetachedLaunchRejectsNativeSessionControlsDuringParsing(t *testing.T) {
	const sessionID = "11111111-1111-4111-8111-111111111111"
	for _, args := range [][]string{
		{"--detach", "--session-id", sessionID},
		{"--detach", "-r", sessionID},
		{"--detach", "--continue"},
		{"--detach", "--fork-session"},
		{"--detach", "--no-session-persistence"},
		{"--detach", "--from-pr=123"},
		{"--detach", "--teleport"},
	} {
		if _, err := parseLaunchInvocation(args); err == nil || !strings.Contains(err.Error(), "detached session identity") {
			t.Fatalf("parseLaunchInvocation(%q) error = %v, want detached session identity refusal", args, err)
		}
	}
}

func TestDetachedResumeExtractsIdentityAndPreservesForwardedOrder(t *testing.T) {
	args := []string{"--detach", "--submission-id=attempt", "--expected-parent-job=parent", "--resume=session", "--output-format=stream-json", "--verbose", "--tools=Read"}
	invocation, err := parseLaunchInvocation(args)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--output-format=stream-json", "--verbose", "--tools=Read"}
	if invocation.submissionID != "attempt" || invocation.expectedParent != "parent" || invocation.resumeSession != "session" || !reflect.DeepEqual(invocation.claudeArgs, want) {
		t.Fatalf("wrong admission: %+v", invocation)
	}
	if err := validateResumeOutput(invocation.claudeArgs); err != nil {
		t.Fatal(err)
	}
}

func TestDetachedResumeDoesNotConsumeClaudeOptionValueAsCCRResume(t *testing.T) {
	args := []string{"--detach", "--system-prompt", "--resume", "--output-format=stream-json", "--verbose"}
	invocation, err := parseLaunchInvocation(args)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--system-prompt", "--resume", "--output-format=stream-json", "--verbose"}
	if invocation.resumeSession != "" || !reflect.DeepEqual(invocation.claudeArgs, want) {
		t.Fatalf("detached invocation = %+v; want literal Claude --resume value preserved", invocation)
	}
	if err := validateResumeOutput(invocation.claudeArgs); err != nil {
		t.Fatalf("validateResumeOutput() rejected valid forwarded options: %v", err)
	}
}

func TestDetachedResumeAfterForwardedSeparatorCannotTakeCCRIdentity(t *testing.T) {
	args := []string{"--detach", "--", "--resume=11111111-1111-4111-8111-111111111111"}
	if _, err := parseLaunchInvocation(args); err == nil || !strings.Contains(err.Error(), "cannot be passed after Claude Code's forwarded option separator") {
		t.Fatalf("parseLaunchInvocation(%q) error = %v, want forwarded session-control refusal", args, err)
	}
}

func TestForegroundResumeRemainsNative(t *testing.T) {
	args := []string{"--resume", "11111111-1111-4111-8111-111111111111"}
	invocation, err := parseLaunchInvocation(args)
	if err != nil || invocation.resumeSession != "" || !reflect.DeepEqual(invocation.claudeArgs, args) {
		t.Fatalf("invocation=%+v err=%v", invocation, err)
	}
}

func TestResumeOutputMustBeUnambiguousStreamJSON(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"--output-format=json", "--verbose"},
		{"--output-format=stream-json"},
		{"--output-format=stream-json", "--verbose=false"},
		{"--output-format=stream-json", "--output-format=text", "--verbose"},
		{"--verbose", "--output-format"},
	} {
		if err := validateResumeOutput(args); err == nil {
			t.Fatalf("accepted %q", args)
		}
	}
	if err := validateResumeOutput([]string{"--output-format", "stream-json", "--verbose"}); err != nil {
		t.Fatal(err)
	}
}
