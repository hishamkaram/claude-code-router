package executor

import (
	"fmt"
	"strings"

	"github.com/hishamkaram/claude-code-router/internal/cua"
)

// Target is the parsed launch-scoped executor selector.
type Target struct {
	Kind         cua.ExecutorKind
	ExternalName string
}

// ParseTarget parses the managed CUA executor selector.
func ParseTarget(value string) (Target, error) {
	value = strings.TrimSpace(value)
	switch value {
	case string(cua.ExecutorDocker):
		return Target{Kind: cua.ExecutorDocker}, nil
	case string(cua.ExecutorLocalBrowser):
		return Target{Kind: cua.ExecutorLocalBrowser}, nil
	case string(cua.ExecutorMacOSPreview):
		return Target{Kind: cua.ExecutorMacOSPreview}, nil
	}
	name, found := strings.CutPrefix(value, string(cua.ExecutorExternal)+":")
	if !found {
		return Target{}, fmt.Errorf("invalid CUA executor %q; expected docker, local-browser, macos-preview, or external:<name>", value)
	}
	if name != strings.TrimSpace(name) {
		return Target{}, fmt.Errorf("external CUA executor name %q must not include whitespace", name)
	}
	if err := validateExternalName(name); err != nil {
		return Target{}, err
	}
	return Target{Kind: cua.ExecutorExternal, ExternalName: name}, nil
}

func validateExternalName(name string) error {
	if name == "" {
		return fmt.Errorf("external CUA executor name is required")
	}
	if len(name) > 63 {
		return fmt.Errorf("external CUA executor name %q is too long", name)
	}
	for index, char := range name {
		if isASCIILetter(char) || isASCIIDigit(char) {
			continue
		}
		if index > 0 && (char == '.' || char == '_' || char == '-') {
			continue
		}
		return fmt.Errorf("external CUA executor name %q must use ASCII letters, digits, '.', '_', or '-'", name)
	}
	return nil
}

func isASCIILetter(char rune) bool {
	return (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z')
}

func isASCIIDigit(char rune) bool {
	return char >= '0' && char <= '9'
}
