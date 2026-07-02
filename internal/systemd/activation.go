package systemd

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

// ListenersFromSystemd creates net.Listeners from systemd socket activation
// file descriptors (LISTEN_FDS environment variable).
// File descriptors start at 3 (stdin=0, stdout=1, stderr=2).
func ListenersFromSystemd() ([]net.Listener, error) {
	fdsStr := os.Getenv("LISTEN_FDS")
	if fdsStr == "" {
		return nil, nil // not socket-activated
	}

	count, err := strconv.Atoi(fdsStr)
	if err != nil {
		return nil, fmt.Errorf("invalid LISTEN_FDS value %q: %w", fdsStr, err)
	}

	fdNamesStr := os.Getenv("LISTEN_FDNAMES")
	fdNames := strings.Split(fdNamesStr, ":")

	var listeners []net.Listener
	for i := uint(3); i < 3+uint(count); i++ {
		f := os.NewFile(uintptr(i), "")
		if f == nil {
			return nil, fmt.Errorf("could not open fd %d", i)
		}

		// Get the listener name for this fd
		var name string
		if int(i-3) < len(fdNames) {
			name = fdNames[i-3]
		}

		listener, err := net.FileListener(f)
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("failed to create listener from fd %d (name=%q): %w", i, name, err)
		}
		listeners = append(listeners, listener)
	}

	return listeners, nil
}

// DefaultListeners returns listeners for the default addresses.
// Used when not socket-activated (e.g., during development).
func DefaultListeners() ([]net.Listener, error) {
	var listeners []net.Listener

	tcp, err := net.Listen("tcp", ":6443")
	if err != nil {
		return nil, fmt.Errorf("failed to listen on TCP :6443: %w", err)
	}
	listeners = append(listeners, tcp)

	// Unix socket
	xdgRuntime := os.Getenv("XDG_RUNTIME_DIR")
	if xdgRuntime == "" {
		xdgRuntime = "/run"
	}
	unixPath := xdgRuntime + "/q8s/api.sock"
	if err := os.MkdirAll(xdgRuntime+"/q8s", 0755); err != nil {
		tcp.Close()
		return nil, fmt.Errorf("failed to create socket directory: %w", err)
	}
	unix, err := net.Listen("unix", unixPath)
	if err != nil {
		tcp.Close()
		return nil, fmt.Errorf("failed to listen on Unix socket %s: %w", unixPath, err)
	}
	listeners = append(listeners, unix)

	return listeners, nil
}
