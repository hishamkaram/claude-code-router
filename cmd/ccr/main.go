package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/hishamkaram/claude-code-router/internal/cli"
	"github.com/hishamkaram/claude-code-router/internal/secret"
)

const credentialUnavailableExitCode = 77

func main() {
	if err := cli.Execute(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(exitCode(err))
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return 1
	}
	if errors.Is(err, secret.ErrCredentialUnavailable) {
		return credentialUnavailableExitCode
	}
	return 1
}
