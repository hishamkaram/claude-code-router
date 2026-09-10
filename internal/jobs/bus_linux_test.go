package jobs

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestUserBusAuthenticationIsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bus")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path="+path)
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- conn
		}
		close(accepted)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	conn, err := connectUserBus(ctx, newScopeSignals("fixture.scope"))
	if conn != nil {
		conn.Close()
	}
	if err == nil || time.Since(started) > time.Second {
		t.Fatalf("unbounded or successful fake authentication: %v", err)
	}
	listener.Close()
	for peer := range accepted {
		peer.Close()
	}
}

func TestUserBusRejectsNonLocalTransport(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "tcp:host=192.0.2.1,port=1234")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if conn, err := connectUserBus(ctx, newScopeSignals("fixture.scope")); err == nil {
		conn.Close()
		t.Fatal("accepted transport without Unix identity handles")
	}
}
