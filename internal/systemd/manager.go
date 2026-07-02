package systemd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	godbus "github.com/godbus/dbus/v5"
	"github.com/coreos/go-systemd/v22/dbus"
)

// UnitState holds the relevant systemd unit state for a container.
type UnitState struct {
	Active string // "active", "inactive", "failed", "activating", "deactivating"
	Sub    string // "running", "dead", "start", "stop", ...
	Result string // "success", "exit-code", "signal", "" (empty when active)
}

// UnitState queries the unit's ActiveState, SubState, and Result via D-Bus.
// Returns nil if the unit is unknown to systemd (never loaded).
func (m *Manager) UnitState(name string) (*UnitState, error) {
	// systemctl show is the most reliable path across loaded/unloaded units.
	args := []string{"show", name, "--property=ActiveState,SubState,Result", "--no-pager"}
	out, err := m.RunSystemctl(args...)
	if err != nil {
		return nil, err
	}
	s := &UnitState{}
	for _, line := range strings.Split(out, "\n") {
		kv := strings.SplitN(strings.TrimSpace(line), "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "ActiveState":
			s.Active = kv[1]
		case "SubState":
			s.Sub = kv[1]
		case "Result":
			s.Result = kv[1]
		}
	}
	if s.Active == "" {
		return nil, nil // unit not known
	}
	return s, nil
}

// RestartUnit restarts a systemd unit.
func (m *Manager) RestartUnit(name string) error {
	ctx := context.Background()
	_, err := m.conn.RestartUnitContext(ctx, name, "replace", nil)
	return err
}

// Mode represents rootful or rootless systemd mode.
type Mode string

const (
	ModeRootful Mode = "rootful"
	ModeRootless Mode = "rootless"
)

// Manager handles systemd operations.
type Manager struct {
	mode Mode
	conn *dbus.Conn
}

// NewManager creates a new systemd Manager.
func NewManager(mode Mode) (*Manager, error) {
	var conn *dbus.Conn
	var err error

	if mode == ModeRootful {
		conn, err = dbus.NewSystemConnectionContext(context.Background())
	} else {
		// Build a proper unix:path= address so dbus can connect regardless of env format.
		busPath := ""
		if xdgRuntime := os.Getenv("XDG_RUNTIME_DIR"); xdgRuntime != "" {
			busPath = filepath.Join(xdgRuntime, "bus")
		}
		if busPath == "" {
			return nil, fmt.Errorf("XDG_RUNTIME_DIR not set; cannot locate user D-Bus socket")
		}
		conn, err = dbus.NewConnection(func() (*godbus.Conn, error) {
			c, dialErr := godbus.Dial("unix:path=" + busPath)
			if dialErr != nil {
				return nil, dialErr
			}
			if authErr := c.Auth(nil); authErr != nil {
				c.Close()
				return nil, authErr
			}
			if helloErr := c.Hello(); helloErr != nil {
				c.Close()
				return nil, helloErr
			}
			return c, nil
		})
	}
	if err != nil {
		return nil, fmt.Errorf("failed to connect to systemd: %w", err)
	}

	return &Manager{mode: mode, conn: conn}, nil
}

// Close closes the D-Bus connection.
func (m *Manager) Close() {
	if m.conn != nil {
		m.conn.Close()
	}
}

// DaemonReload runs systemctl daemon-reload.
func (m *Manager) DaemonReload() error {
	ctx := context.Background()
	if m.mode == ModeRootful {
		return m.conn.ReloadContext(ctx)
	}
	return m.conn.ReloadContext(ctx)
}

// StartUnit starts a systemd unit.
func (m *Manager) StartUnit(name string) error {
	ctx := context.Background()
	_, err := m.conn.StartUnitContext(ctx, name, "replace", nil)
	return err
}

// StopUnit stops a systemd unit.
func (m *Manager) StopUnit(name string) error {
	ctx := context.Background()
	_, err := m.conn.StopUnitContext(ctx, name, "replace", nil)
	return err
}

// EnableUnit enables a systemd unit (creates symlinks in WantedBy).
func (m *Manager) EnableUnit(name string) error {
	ctx := context.Background()
	_, _, err := m.conn.EnableUnitFilesContext(ctx, []string{name}, false, false)
	return err
}

// DisableUnit disables a systemd unit.
func (m *Manager) DisableUnit(name string) error {
	ctx := context.Background()
	_, err := m.conn.DisableUnitFilesContext(ctx, []string{name}, false)
	return err
}

// LinkUnitFiles copies unit files to the systemd directory.
func (m *Manager) LinkUnitFiles(paths []string) error {
	ctx := context.Background()
	_, err := m.conn.LinkUnitFilesContext(ctx, paths, false, false)
	return err
}

// ListUnits returns all loaded units.
func (m *Manager) ListUnits() ([]dbus.UnitStatus, error) {
	ctx := context.Background()
	return m.conn.ListUnitsContext(ctx)
}

// UnitExists checks if a unit is loaded.
func (m *Manager) UnitExists(name string) (bool, error) {
	ctx := context.Background()
	units, err := m.conn.ListUnitsContext(ctx)
	if err != nil {
		return false, err
	}
	for _, u := range units {
		if u.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// WriteUnitFile writes a systemd unit file to the appropriate directory.
func (m *Manager) WriteUnitFile(name, content string) error {
	var dir string
	if m.mode == ModeRootful {
		dir = "/etc/systemd/system"
	} else {
		xdgConfig := os.Getenv("XDG_CONFIG_HOME")
		if xdgConfig == "" {
			home := os.Getenv("HOME")
			if home == "" {
				home = os.Getenv("USERPROFILE")
			}
			if home == "" {
				return fmt.Errorf("HOME not set")
			}
			xdgConfig = filepath.Join(home, ".config")
		}
		dir = filepath.Join(xdgConfig, "systemd", "user")
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return fmt.Errorf("failed to write unit file: %w", err)
	}
	return nil
}

// ReadUnitFile reads a systemd unit file.
func (m *Manager) ReadUnitFile(name string) (string, error) {
	var dir string
	if m.mode == ModeRootful {
		dir = "/etc/systemd/system"
	} else {
		xdgConfig := os.Getenv("XDG_CONFIG_HOME")
		if xdgConfig == "" {
			xdgConfig = filepath.Join(os.Getenv("HOME"), ".config")
		}
		dir = filepath.Join(xdgConfig, "systemd", "user")
	}
	path := filepath.Join(dir, name)
	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("failed to read unit file %s: %w", name, err)
	}
	return string(content), nil
}

// RunSystemctl runs a systemctl command and returns output.
func (m *Manager) RunSystemctl(args ...string) (string, error) {
	cmdArgs := append([]string{"--no-pager"}, args...)
	if m.mode == ModeRootless {
		cmdArgs = append([]string{"--user"}, cmdArgs...)
	}
	cmd := exec.Command("systemctl", cmdArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("systemctl %s: %w: %s", args, err, string(output))
	}
	return string(output), nil
}
