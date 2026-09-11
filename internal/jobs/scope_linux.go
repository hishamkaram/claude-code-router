package jobs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

type scope struct {
	conn        *dbus.Conn
	name        string
	path        dbus.ObjectPath
	events      *os.File
	signals     *scopeSignals
	managerName string
}

type unitProperty struct {
	Name  string
	Value dbus.Variant
}
type auxiliaryUnit struct {
	Name       string
	Properties []unitProperty
}

// openScope detects the user manager before any workload is admitted.
func openScope(ctx context.Context, id string) (*scope, error) {
	if err := ValidateID(id); err != nil {
		return nil, err
	}
	signals := newScopeSignals(id + ".scope")
	conn, err := connectUserBus(ctx, signals)
	if err != nil {
		return nil, fmt.Errorf("connecting to systemd user bus: %w", err)
	}
	s := &scope{conn: conn, name: id + ".scope", signals: signals}
	if err := conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", 0, "org.freedesktop.systemd1").Store(&s.managerName); err != nil {
		s.Close(ctx)
		return nil, fmt.Errorf("resolving scope manager identity: %w", err)
	}
	signals.owner.Store(&s.managerName)
	if err := conn.AddMatchSignalContext(ctx, dbus.WithMatchSender(s.managerName), dbus.WithMatchInterface("org.freedesktop.systemd1.Manager"), dbus.WithMatchMember("JobRemoved"), dbus.WithMatchArg(2, s.name)); err != nil {
		s.Close(ctx)
		return nil, fmt.Errorf("subscribing to scope events: %w", err)
	}
	if err := s.manager().CallWithContext(ctx, "org.freedesktop.systemd1.Manager.Subscribe", 0).Err; err != nil {
		s.Close(ctx)
		return nil, fmt.Errorf("subscribing to user manager: %w", err)
	}
	return s, nil
}

func (s *scope) manager() dbus.BusObject {
	return s.conn.Object(s.managerName, "/org/freedesktop/systemd1")
}

func (s *scope) Admit(ctx context.Context, pid int) error {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return fmt.Errorf("opening owned child identity handle: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()
	properties := []unitProperty{
		{"PIDFDs", dbus.MakeVariant([]dbus.UnixFD{dbus.UnixFD(fd)})},
		{"Description", dbus.MakeVariant("CCR detached job")},
		{"KillMode", dbus.MakeVariant("control-group")},
		{"TimeoutStopUSec", dbus.MakeVariant(uint64(3 * time.Second / time.Microsecond))},
		{"CollectMode", dbus.MakeVariant("inactive-or-failed")},
	}
	call := s.manager().CallWithContext(ctx, "org.freedesktop.systemd1.Manager.StartTransientUnit", 0, s.name, "fail", properties, []auxiliaryUnit{})
	if waitErr := s.waitJob(ctx, call); waitErr != nil {
		return waitErr
	}
	if err = s.manager().CallWithContext(ctx, "org.freedesktop.systemd1.Manager.GetUnit", 0, s.name).Store(&s.path); err != nil {
		return fmt.Errorf("reading admitted scope: %w", err)
	}
	// Pin the unit until exit evidence has been consumed; collection is not history.
	if err = s.manager().CallWithContext(ctx, "org.freedesktop.systemd1.Manager.RefUnit", 0, s.name).Err; err != nil {
		return fmt.Errorf("pinning scope: %w", err)
	}
	value, err := s.property(ctx, "org.freedesktop.systemd1.Scope", "ControlGroup")
	if err != nil {
		return fmt.Errorf("reading scope cgroup: %w", err)
	}
	group, ok := value.Value().(string)
	if !ok || group == "" || group == "/" || !strings.HasPrefix(group, "/") {
		return fmt.Errorf("invalid scope cgroup returned by manager")
	}
	const cgroupRoot = "/sys/fs/cgroup"
	s.events, err = os.Open(filepath.Join(cgroupRoot, group, "cgroup.events"))
	if err != nil {
		return fmt.Errorf("opening scope observation: %w", err)
	}
	return nil
}

func connectUserBus(ctx context.Context, signals *scopeSignals) (*dbus.Conn, error) {
	address, err := systemdUserBusAddress()
	if err != nil {
		return nil, err
	}
	// Dial the resolved address directly. The convenience discovery API writes
	// DBUS_SESSION_BUS_ADDRESS into the owner's environment, changing the
	// prepared execution fingerprint during containment setup.
	conn, err := dbus.Dial(address, dbus.WithContext(context.WithoutCancel(ctx)), dbus.WithSignalHandler(signals))
	if err != nil {
		return nil, fmt.Errorf("opening user bus: %w", err)
	}
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	err = conn.Auth(nil)
	if err == nil {
		err = conn.Hello()
	}
	stopClose()
	if err == nil {
		err = ctx.Err()
	}
	if err == nil && !conn.SupportsUnixFDs() {
		err = fmt.Errorf("user bus does not support kernel identity handles")
	}
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("authenticating user bus: %w", err)
	}
	return conn, nil
}

func (s *scope) waitJob(ctx context.Context, call *dbus.Call) error {
	var job dbus.ObjectPath
	if err := call.Store(&job); err != nil {
		return fmt.Errorf("requesting scope operation: %w", err)
	}
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for scope operation: %w", ctx.Err())
		case <-s.signals.closed:
			return fmt.Errorf("scope event connection closed")
		case <-s.signals.overflow:
			return fmt.Errorf("scope event observation overflowed")
		case signal := <-s.signals.events:
			if signal == nil || len(signal.Body) != 4 || signal.Body[1] != job {
				continue
			}
			if signal.Body[3] != "done" {
				return fmt.Errorf("scope operation did not complete successfully")
			}
			return nil
		}
	}
}

func (s *scope) Stop(ctx context.Context) error {
	call := s.manager().CallWithContext(ctx, "org.freedesktop.systemd1.Manager.StopUnit", 0, s.name, "replace")
	var busErr dbus.Error
	if errors.As(call.Err, &busErr) && busErr.Name == "org.freedesktop.systemd1.NoSuchUnit" {
		return nil
	}
	return s.waitJob(ctx, call)
}

func (s *scope) Observe(ctx context.Context) Cleanup {
	c := Cleanup{Coverage: "unknown", Reason: "scope observation unavailable"}
	if s.events == nil {
		c.Normalize()
		return c
	}
	observed := "scope_subtree_empty"
	if err := waitScopeEmpty(ctx, s.events); err != nil {
		// systemd can remove the empty cgroup before this reader wakes. Require
		// positive evidence from the pinned unit, never infer it from disappearance.
		if !errors.Is(err, unix.ENODEV) || !s.confirmStopped(ctx) {
			c.Reason = err.Error()
			c.Normalize()
			return c
		}
		observed = "systemd_scope_stop_confirmed"
	}
	c = Cleanup{Coverage: "partial", Observed: []string{observed}, Unobservable: []string{"external_user_manager_units"}, Reason: "same-user D-Bus access permits launching outside the job scope; external units cannot be exhaustively attributed"}
	c.Normalize()
	return c
}

func (s *scope) property(ctx context.Context, iface, name string) (dbus.Variant, error) {
	var value dbus.Variant
	err := s.conn.Object(s.managerName, s.path).CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, iface, name).Store(&value)
	return value, err
}

func (s *scope) confirmStopped(ctx context.Context) bool {
	state, stateErr := s.property(ctx, "org.freedesktop.systemd1.Unit", "ActiveState")
	result, resultErr := s.property(ctx, "org.freedesktop.systemd1.Scope", "Result")
	return stateErr == nil && resultErr == nil && state.Value() == "inactive" && result.Value() == "success"
}

func waitScopeEmpty(ctx context.Context, file *os.File) error {
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("waiting for scope emptiness: %w", err)
		}
		if _, err := file.Seek(0, 0); err != nil {
			return fmt.Errorf("seeking scope events: %w", err)
		}
		data := make([]byte, 4096)
		n, err := file.Read(data)
		if err != nil {
			return fmt.Errorf("reading scope events: %w", err)
		}
		if n == len(data) {
			return fmt.Errorf("truncated scope events")
		}
		populated, err := parsePopulated(string(data[:n]))
		if err != nil {
			return err
		}
		if !populated {
			return nil
		}
		if err := waitScopeEvent(ctx, file); err != nil {
			return err
		}
	}
}

func waitScopeEvent(ctx context.Context, file *os.File) error {
	fds := []unix.PollFd{{Fd: int32(file.Fd()), Events: unix.POLLPRI | unix.POLLERR}}
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("waiting for scope event: %w", err)
		}
		n, err := unix.Poll(fds, 100)
		if err != nil && !errors.Is(err, unix.EINTR) {
			return fmt.Errorf("polling scope events: %w", err)
		}
		if n > 0 {
			return nil
		}
	}
}

func parsePopulated(data string) (bool, error) {
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "populated" {
			n, err := strconv.Atoi(fields[1])
			if err == nil && (n == 0 || n == 1) {
				return n == 1, nil
			}
		}
	}
	return false, fmt.Errorf("missing or malformed populated state")
}

func (s *scope) Close(ctx context.Context) {
	if s.events != nil {
		_ = s.events.Close()
	}
	if s.path != "" {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		_ = s.manager().CallWithContext(cleanupCtx, "org.freedesktop.systemd1.Manager.UnrefUnit", 0, s.name).Err
	}
	_ = s.conn.Close()
}

func systemdUserBusAddress() (string, error) {
	address := os.Getenv("DBUS_SESSION_BUS_ADDRESS")
	if address == "" || address == "autolaunch:" {
		runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
		if runtimeDir == "" {
			runtimeDir = fmt.Sprintf("/run/user/%d", os.Geteuid())
		}
		path := filepath.Join(runtimeDir, "bus")
		info, err := os.Stat(path)
		if err != nil {
			return "", fmt.Errorf("locating systemd user bus: %w", err)
		}
		if info.Mode()&os.ModeSocket == 0 {
			return "", fmt.Errorf("systemd user bus path is not a socket")
		}
		address = "unix:path=" + dbus.EscapeBusAddressValue(path)
	}
	for _, candidate := range strings.Split(address, ";") {
		if candidate == "" || !strings.HasPrefix(candidate, "unix:") {
			return "", fmt.Errorf("systemd job containment requires a local Unix user bus")
		}
	}
	return address, nil
}
