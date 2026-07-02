package main

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"q8s/internal/systemd"
)

// --- resolveDirs ---

func TestResolveDirsRootful(t *testing.T) {
	d := resolveDirs(true)
	checks := []struct{ field, got, want string }{
		{"dataDir", d.dataDir, "/etc/q8s"},
		{"quadletDir", d.quadletDir, "/etc/containers/systemd"},
		{"configDir", d.configDir, "/run/q8s/configmaps"},
		{"systemdDir", d.systemdDir, "/etc/systemd/system"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: got %q, want %q", c.field, c.got, c.want)
		}
	}
}

func TestResolveDirsRootless(t *testing.T) {
	t.Setenv("HOME", "/home/testuser")
	t.Setenv("XDG_CONFIG_HOME", "/home/testuser/.xdgconfig")
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/9999")

	d := resolveDirs(false)
	checks := []struct{ field, got, want string }{
		{"dataDir", d.dataDir, "/home/testuser/.local/share/q8s"},
		{"quadletDir", d.quadletDir, "/home/testuser/.xdgconfig/containers/systemd"},
		{"configDir", d.configDir, "/run/user/9999/q8s/configmaps"},
		{"systemdDir", d.systemdDir, "/home/testuser/.xdgconfig/systemd/user"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: got %q, want %q", c.field, c.got, c.want)
		}
	}
}

func TestResolveDirsRootlessXDGDefaults(t *testing.T) {
	t.Setenv("HOME", "/home/bob")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_RUNTIME_DIR", "")

	d := resolveDirs(false)
	if d.quadletDir != "/home/bob/.config/containers/systemd" {
		t.Errorf("quadletDir: got %q, want /home/bob/.config/containers/systemd", d.quadletDir)
	}
	if d.systemdDir != "/home/bob/.config/systemd/user" {
		t.Errorf("systemdDir: got %q, want /home/bob/.config/systemd/user", d.systemdDir)
	}
}

// --- systemctlFlags ---

func TestSystemctlFlagsRootful(t *testing.T) {
	flags := systemctlFlags(true)
	if len(flags) != 0 {
		t.Fatalf("expected no flags for rootful, got %v", flags)
	}
}

func TestSystemctlFlagsRootless(t *testing.T) {
	flags := systemctlFlags(false)
	if len(flags) != 1 || flags[0] != "--user" {
		t.Fatalf("expected [--user], got %v", flags)
	}
}

// --- podmanStateToPhase ---

func TestPodmanStateToPhase(t *testing.T) {
	cases := []struct {
		state    string
		exitCode int
		want     corev1.PodPhase
	}{
		{"running", 0, corev1.PodRunning},
		{"exited", 0, corev1.PodSucceeded},
		{"exited", 1, corev1.PodFailed},
		{"stopped", 0, corev1.PodSucceeded},
		{"stopped", 2, corev1.PodFailed},
		{"created", 0, corev1.PodPending},
		{"unknown", 0, corev1.PodPending},
	}
	for _, tc := range cases {
		got := podmanStateToPhase(tc.state, tc.exitCode)
		if got != tc.want {
			t.Errorf("podmanStateToPhase(%q, %d) = %q, want %q", tc.state, tc.exitCode, got, tc.want)
		}
	}
}

// --- unitStateToPhase ---

func TestUnitStateToPhase(t *testing.T) {
	cases := []struct {
		active, result string
		want           corev1.PodPhase
	}{
		{"active", "", corev1.PodRunning},
		{"failed", "", corev1.PodFailed},
		{"inactive", "success", corev1.PodSucceeded},
		{"inactive", "exit-code", corev1.PodFailed},
		{"inactive", "signal", corev1.PodFailed},
		{"inactive", "core-dump", corev1.PodFailed},
		{"inactive", "watchdog", corev1.PodFailed},
		{"inactive", "timeout", corev1.PodFailed},
		{"inactive", "", corev1.PodPending},
		{"activating", "", corev1.PodPending},
		{"deactivating", "", corev1.PodPending},
	}
	for _, tc := range cases {
		s := &systemd.UnitState{Active: tc.active, Result: tc.result}
		got := unitStateToPhase(s)
		if got != tc.want {
			t.Errorf("unitStateToPhase({Active:%q, Result:%q}) = %q, want %q",
				tc.active, tc.result, got, tc.want)
		}
	}
}
