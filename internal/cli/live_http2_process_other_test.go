//go:build live && !(darwin || linux)

package cli

import (
	"fmt"
	"os/exec"
)

func protectHTTP2TestProcess(_ *exec.Cmd) error {
	return fmt.Errorf("executable HTTP/2 live acceptance requires macOS or Linux process-group ownership")
}
