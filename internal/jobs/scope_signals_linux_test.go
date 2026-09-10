package jobs

import (
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestScopeSignalsIgnoreUnrelatedUnitsAndSenders(t *testing.T) {
	s := newScopeSignals("owned.scope")
	owner := ":1.1"
	s.owner.Store(&owner)
	for i := 0; i < 10000; i++ {
		s.DeliverSignal("org.freedesktop.systemd1.Manager", "JobRemoved", &dbus.Signal{Sender: owner, Body: []any{uint32(i), dbus.ObjectPath("/job/1"), "unrelated.scope", "done"}})
		s.DeliverSignal("org.freedesktop.systemd1.Manager", "JobRemoved", &dbus.Signal{Sender: ":1.2", Body: []any{uint32(i), dbus.ObjectPath("/job/1"), "owned.scope", "done"}})
	}
	if len(s.events) != 0 || len(s.overflow) != 0 {
		t.Fatal("unrelated activity reached the owner's signal queue")
	}
	s.DeliverSignal("org.freedesktop.systemd1.Manager", "JobRemoved", &dbus.Signal{Sender: owner, Body: []any{uint32(1), dbus.ObjectPath("/job/1"), "owned.scope", "done"}})
	if len(s.events) != 1 {
		t.Fatal("owned event was lost")
	}
}

func TestScopeSignalOverflowIsExplicit(t *testing.T) {
	s := newScopeSignals("owned.scope")
	owner := ":1.1"
	s.owner.Store(&owner)
	for i := 0; i < 100; i++ {
		s.DeliverSignal("org.freedesktop.systemd1.Manager", "JobRemoved", &dbus.Signal{Sender: owner, Body: []any{uint32(i), dbus.ObjectPath("/job/1"), "owned.scope", "done"}})
	}
	if len(s.events) != 16 || len(s.overflow) != 1 {
		t.Fatal("overflow was unbounded or silent")
	}
	s.Terminate()
	s.Terminate()
}
