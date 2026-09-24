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
		return validateForegroundAdmissionOptions(*invocation)
	}
	if option := detachedForwardedSessionOption(invocation.claudeArgs, invocation.claudeArgsSeparated, invocation.claudeArgsSeparator); option != "" {
		return fmt.Errorf("%s cannot be passed after Claude Code's forwarded option separator because it conflicts with CCR's detached session identity", option)
	}
	forwarded, resumeSession, err := normalizeDetachedResumeArguments(
		invocation.claudeArgs, invocation.resumeSession, invocation.claudeArgsSeparated, invocation.claudeArgsSeparator,
	)
	if err != nil {
		return err
	}
	invocation.claudeArgs = forwarded
	invocation.resumeSession = resumeSession
	if option := detachedNativeSessionOption(forwarded); option != "" {
		return fmt.Errorf("%s conflicts with CCR's detached session identity", option)
	}
	if invocation.expectedParent != "" && invocation.resumeSession == "" {
		return fmt.Errorf("--expected-parent-job requires --resume")
	}
	return nil
}

func detachedForwardedSessionOption(args []string, separated bool, separator int) string {
	if !separated || separator < 0 || separator > len(args) {
		return ""
	}
	return detachedNativeSessionOption(args[separator:])
}

func validateForegroundAdmissionOptions(invocation launchInvocation) error {
	if invocation.submissionID != "" || invocation.expectedParent != "" {
		return fmt.Errorf("--submission-id and --expected-parent-job require --detach")
	}
	return nil // Preserve native foreground --resume semantics.
}

func normalizeDetachedResumeArguments(args []string, existingResume string, separated bool, separator int) (forwarded []string, resumeSession string, err error) {
	resumeSession = existingResume
	prefix, suffix := args, []string(nil)
	if separated && separator >= 0 && separator <= len(args) {
		// parseLaunchArguments has already consumed CCR's separator. Keep its
		// boundary for this second parse so a Claude-native --resume after -- is
		// never mistaken for CCR's detached session identity.
		prefix, suffix = args[:separator], args[separator:]
	}
	forwarded = make([]string, 0, len(args))
	for index := 0; index < len(prefix); index++ {
		if prefix[index] == "--" {
			forwarded = append(forwarded, prefix[index:]...)
			break
		}
		if claudePassthroughOptionConsumesNextValue(prefix, index) {
			forwarded = append(forwarded, prefix[index], prefix[index+1])
			index++
			continue
		}
		option, value, inline := strings.Cut(prefix[index], "=")
		if option != "--resume" {
			forwarded = append(forwarded, prefix[index])
			continue
		}
		if resumeSession != "" {
			return nil, "", fmt.Errorf("--resume must occur exactly once")
		}
		if !inline {
			value, index, err = launchOptionValue(prefix, index, option)
			if err != nil {
				return nil, "", err
			}
		}
		if value == "" {
			return nil, "", fmt.Errorf("detached --resume requires a session ID")
		}
		resumeSession = value
	}
	forwarded = append(forwarded, suffix...)
	return forwarded, resumeSession, nil
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
		if option != "--output-format" && claudePassthroughOptionConsumesNextValue(args, i) {
			i++
		}
	}
	if formats != 1 || format != "stream-json" || !verbose {
		return fmt.Errorf("detached resume requires exactly one --output-format=stream-json and --verbose")
	}
	return nil
}
