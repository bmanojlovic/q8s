package systemd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	godbus "github.com/godbus/dbus/v5"
)

// The single match rule q8s subscribes to. Every value is a constant so the
// rule handed to the bus and the rule the test asserts cannot drift apart.
const (
	propertiesInterface = "org.freedesktop.DBus.Properties"
	propertiesChanged   = "PropertiesChanged"
	unitPathNamespace   = "/org/freedesktop/systemd1/unit"
	unitInterface       = "org.freedesktop.systemd1.Unit"

	managerBusName    = "org.freedesktop.systemd1"
	managerObjectPath = "/org/freedesktop/systemd1"
	managerInterface  = "org.freedesktop.systemd1.Manager"

	// alreadySubscribedName is what systemd replies to a second Subscribe
	// on one connection; q8s treats it as success.
	alreadySubscribedName = "org.freedesktop.systemd1.AlreadySubscribed"

	// unitSignalBuffer absorbs a burst of unit changes (a container start
	// touches several properties) without ever stalling the bus reader.
	unitSignalBuffer = 64
)

// unitMatchOptions builds the narrow match rule q8s installs: a signal, from
// the properties interface, named PropertiesChanged, on a path under the
// unit namespace, with org.freedesktop.systemd1.Unit as arg0 (the interface
// whose properties changed). The bus daemon applies this, so session-scope
// traffic and non-unit property changes never reach q8s.
func unitMatchOptions() []godbus.MatchOption {
	return []godbus.MatchOption{
		godbus.WithMatchInterface(propertiesInterface),
		godbus.WithMatchMember(propertiesChanged),
		godbus.WithMatchPathNamespace(unitPathNamespace),
		godbus.WithMatchArg(0, unitInterface),
	}
}

// unitMatchRule renders unitMatchOptions in the textual form the bus daemon
// matches. AddMatchSignal prepends type='signal', hence the same key here.
// It exists so a test can pin the rule down.
func unitMatchRule() string {
	return "type='signal'" +
		",interface='" + propertiesInterface + "'" +
		",member='" + propertiesChanged + "'" +
		",path_namespace='" + unitPathNamespace + "'" +
		",arg0='" + unitInterface + "'"
}

// unitNameFromPath reverses systemd's D-Bus path escaping for a unit object
// path. Every byte that is not an ASCII letter or digit is written as _xx
// with the lowercase hex value, so /org/freedesktop/systemd1/unit/
// demo_2dapp_2eservice is demo-app.service. It returns "" for anything that
// is not a unit object path or is not validly escaped; the caller ignores
// those instead of guessing.
func unitNameFromPath(path string) string {
	escaped, ok := strings.CutPrefix(path, unitPathNamespace+"/")
	if !ok || escaped == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(escaped))
	for i := 0; i < len(escaped); i++ {
		c := escaped[i]
		if c == '/' {
			// Unit paths are flat; a deeper path is not a unit object.
			return ""
		}
		if c != '_' {
			b.WriteByte(c)
			continue
		}
		if i+2 >= len(escaped) {
			return ""
		}
		v, err := strconv.ParseUint(escaped[i+1:i+3], 16, 8)
		if err != nil {
			return ""
		}
		b.WriteByte(byte(v))
		i += 2
	}
	return b.String()
}

// userBusPath returns the address of the rootless user bus. This is the one
// place that knows where it lives; both the Manager connection and the
// signal subscription use it.
func userBusPath() (string, error) {
	xdgRuntime := os.Getenv("XDG_RUNTIME_DIR")
	if xdgRuntime == "" {
		return "", fmt.Errorf("XDG_RUNTIME_DIR not set; cannot locate user D-Bus socket")
	}
	return filepath.Join(xdgRuntime, "bus"), nil
}

// dialBus opens a private, authenticated connection to the bus that serves
// mode. The signal subscription must not share the Manager's go-systemd
// connection: it is long lived, reconnectable, and carries no method calls.
func dialBus(mode Mode) (*godbus.Conn, error) {
	var conn *godbus.Conn
	var err error
	if mode == ModeRootful {
		conn, err = godbus.SystemBusPrivate()
	} else {
		var busPath string
		busPath, err = userBusPath()
		if err != nil {
			return nil, err
		}
		conn, err = godbus.Dial("unix:path=" + busPath)
	}
	if err != nil {
		return nil, err
	}
	if err := conn.Auth(nil); err != nil {
		conn.Close()
		return nil, err
	}
	if err := conn.Hello(); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// WatchUnits calls fn (from one goroutine, never concurrently) with the unit
// name each time systemd reports a property change of a unit for which
// match(unitName) is true. It blocks until ctx is done, and reconnects with
// backoff (1 s doubling to 30 s) when the bus connection fails.
//
// The subscription is to systemd itself, so no child process is started and
// the journal is never read. A unit property change is only a trigger: the
// caller still reads authoritative state on the reconcile pass it starts.
func WatchUnits(ctx context.Context, mode Mode, match func(unit string) bool, fn func(unit string)) {
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := watchUnitsOnce(ctx, mode, match, fn)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			fmt.Printf("systemd watch: %v\n", err)
		}
		// A connection that stayed up is not a crash loop: start again at
		// the short delay instead of continuing to back off.
		if time.Since(started) >= 30*time.Second {
			backoff = time.Second
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
}

// watchUnitsOnce owns one bus connection: dial, subscribe, deliver unit names
// until the connection breaks or ctx is cancelled. Every signal costs one
// string unescape and one match call; no D-Bus round trip happens per event,
// which matters on hosts where unrelated session-scope units change often.
func watchUnitsOnce(ctx context.Context, mode Mode, match func(unit string) bool, fn func(unit string)) error {
	conn, err := dialBus(mode)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close()

	if err := conn.AddMatchSignal(unitMatchOptions()...); err != nil {
		return fmt.Errorf("add match rule: %w", err)
	}

	signals := make(chan *godbus.Signal, unitSignalBuffer)
	conn.Signal(signals)

	// godbus has no context-aware receive. Closing the connection is what
	// unblocks the receive loop (it also closes the signal channel).
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()

	if err := subscribe(conn); err != nil {
		return err
	}

	for {
		select {
		case sig, ok := <-signals:
			if !ok {
				return errors.New("D-Bus connection closed")
			}
			unit := unitNameFromPath(string(sig.Path))
			if unit == "" || !match(unit) {
				continue
			}
			fn(unit)
		case <-ctx.Done():
			return nil
		}
	}
}

// subscribe asks systemd to start announcing unit changes to this connection.
// systemd only emits these signals while at least one client is subscribed,
// and the subscription lasts as long as the connection.
func subscribe(conn *godbus.Conn) error {
	obj := conn.Object(managerBusName, godbus.ObjectPath(managerObjectPath))
	call := obj.Call(managerInterface+".Subscribe", 0)
	if call.Err == nil || isAlreadySubscribed(call.Err) {
		return nil
	}
	return fmt.Errorf("subscribe to systemd signals: %w", call.Err)
}

// isAlreadySubscribed reports whether err is systemd's AlreadySubscribed.
// godbus returns D-Bus errors as either Error or *Error, hence both cases.
func isAlreadySubscribed(err error) bool {
	var e godbus.Error
	if errors.As(err, &e) {
		return e.Name == alreadySubscribedName
	}
	var ep *godbus.Error
	if errors.As(err, &ep) && ep != nil {
		return ep.Name == alreadySubscribedName
	}
	return false
}
