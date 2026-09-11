package cli

import (
	"reflect"
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

func TestForegroundResumeRemainsNative(t *testing.T) {
	args := []string{"--resume", "native-session"}
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
