package cli

import (
	"fmt"
	"strings"
)

func parseAdmissionOption(invocation *launchInvocation, args []string, index *int) (bool, error) {
	option, value, inline := strings.Cut(args[*index], "=")
	var target *string
	switch option {
	case "--submission-id":
		target = &invocation.submissionID
	case "--expected-parent-job":
		target = &invocation.expectedParent
	default:
		return false, nil
	}
	if *target != "" {
		return true, fmt.Errorf("%s must occur exactly once", option)
	}
	if !inline {
		var err error
		value, *index, err = launchOptionValue(args, *index, option)
		if err != nil {
			return true, err
		}
	}
	if value == "" {
		return true, fmt.Errorf("%s requires a nonempty value", option)
	}
	*target = value
	return true, nil
}

func normalizeAdmissionOptions(invocation *launchInvocation) error {
	if !invocation.detach {
		if invocation.submissionID != "" || invocation.expectedParent != "" {
			return fmt.Errorf("--submission-id and --expected-parent-job require --detach")
		}
		return nil // Preserve native foreground --resume semantics.
	}
	args := invocation.claudeArgs
	forwarded := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			forwarded = append(forwarded, args[i:]...)
			break
		}
		option, value, inline := strings.Cut(args[i], "=")
		if option != "--resume" {
			forwarded = append(forwarded, args[i])
			continue
		}
		if invocation.resumeSession != "" {
			return fmt.Errorf("--resume must occur exactly once")
		}
		if !inline {
			var err error
			value, i, err = launchOptionValue(args, i, option)
			if err != nil {
				return err
			}
		}
		if value == "" {
			return fmt.Errorf("detached --resume requires a session ID")
		}
		invocation.resumeSession = value
	}
	invocation.claudeArgs = forwarded
	if invocation.expectedParent != "" && invocation.resumeSession == "" {
		return fmt.Errorf("--expected-parent-job requires --resume")
	}
	return nil
}

func validateResumeOutput(args []string) error {
	format, formats, verbose := "", 0, false
	for i := 0; i < len(args); i++ {
		option, value, inline := strings.Cut(args[i], "=")
		switch option {
		case "--output-format":
			if !inline {
				var err error
				value, i, err = launchOptionValue(args, i, option)
				if err != nil {
					return err
				}
			}
			format, formats = value, formats+1
		case "--verbose":
			if inline && value != "true" {
				return fmt.Errorf("detached resume requires --verbose")
			}
			verbose = true
		}
	}
	if formats != 1 || format != "stream-json" || !verbose {
		return fmt.Errorf("detached resume requires exactly one --output-format=stream-json and --verbose")
	}
	return nil
}
