package jobs

import (
	"context"
	"fmt"
)

type scope struct{}

func openScope(context.Context, string) (*scope, error) {
	return nil, fmt.Errorf("systemd scopes unavailable on macOS")
}

func (*scope) Admit(context.Context, int) error {
	return fmt.Errorf("systemd scopes unavailable on macOS")
}
func (*scope) Stop(context.Context) error { return fmt.Errorf("systemd scopes unavailable on macOS") }
func (*scope) Observe(context.Context) Cleanup {
	return Cleanup{Coverage: "unknown", Reason: "systemd scopes unavailable on macOS"}
}
func (*scope) Close(context.Context) {}
