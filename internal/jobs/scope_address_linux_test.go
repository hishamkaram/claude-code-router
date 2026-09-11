package jobs

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestUserBusDiscoveryDoesNotMutateExecutionEnvironment(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	t.Setenv("XDG_RUNTIME_DIR", directory)
	listener, err := net.Listen("unix", filepath.Join(directory, "bus"))
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		connection, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- struct{}{}
			_ = connection.Close()
		}
	}()
	defer func() { _ = listener.Close(); <-done }()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	connection, connectErr := connectUserBus(ctx, newScopeSignals("fixture.scope"))
	if connection != nil {
		_ = connection.Close()
	}
	if connectErr == nil {
		t.Fatal("unauthenticated fixture unexpectedly accepted")
	}
	select {
	case <-accepted:
	default:
		t.Fatal("discovery did not reach the populated runtime socket")
	}
	if got := os.Getenv("DBUS_SESSION_BUS_ADDRESS"); got != "" {
		t.Fatalf("containment discovery changed execution environment: %q", got)
	}
}

func TestUserBusAddressRejectsNonSocketAndRemoteTransport(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "tcp:host=127.0.0.1,port=1")
	if _, err := systemdUserBusAddress(); err == nil {
		t.Fatal("accepted remote user bus transport")
	}
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	directory := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", directory)
	if err := os.WriteFile(filepath.Join(directory, "bus"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := systemdUserBusAddress(); err == nil {
		t.Fatal("accepted a regular file as the user bus")
	}
}
