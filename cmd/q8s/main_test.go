package main

import (
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"q8s/internal/store"
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


// --- restoreSecretFiles (tic-b31a) ---

// TestRestoreSecretFilesWritesContent proves the restart path rewrites secret
// file CONTENT from the stored Data, not just empty placeholders. This is the
// second half of the tic-b31a fix: handler.normalizeSecret guarantees content
// lives in Data, and restoreSecretFiles must actually write it out.
func TestRestoreSecretFilesWritesContent(t *testing.T) {
	dir := t.TempDir()
	st := store.New()
	if _, err := st.CreateSecret(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "tls", Namespace: "default"},
		Data: map[string][]byte{
			"cert.pem": []byte("PEMDATA"),
			"key.pem":  []byte("KEYDATA"),
		},
	}); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}

	restoreSecretFiles(dir, st)

	for name, want := range map[string]string{"cert.pem": "PEMDATA", "key.pem": "KEYDATA"} {
		got, err := os.ReadFile(filepath.Join(dir, "default", "tls", name))
		if err != nil {
			t.Fatalf("reading restored %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("restored %s = %q, want %q", name, got, want)
		}
	}
}

// TestRestoreLegacySecretSelfHealsOnLoad is the tic-c627 end-to-end check: a
// secret persisted before the tic-b31a fix keeps its content only in
// stringData. A bare restart (store.Load -> restoreSecretFiles) must write
// real file content, not the empty placeholders that crash-looped the pod on
// the mozakq8s VM. Load folds stringData into Data, so restore writes it out.
func TestRestoreLegacySecretSelfHealsOnLoad(t *testing.T) {
	tmp := t.TempDir()
	storeFile := filepath.Join(tmp, "store.json")
	legacy := `{
	  "secrets": [
	    {
	      "apiVersion": "v1", "kind": "Secret",
	      "metadata": {"name": "mozak-brain-tls", "namespace": "default"},
	      "stringData": {"cert.pem": "PEMDATA", "key.pem": "KEYDATA"}
	    }
	  ]
	}`
	if err := os.WriteFile(storeFile, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}

	st, err := store.Load(storeFile)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	secretDir := filepath.Join(tmp, "secrets")
	restoreSecretFiles(secretDir, st)

	for name, want := range map[string]string{"cert.pem": "PEMDATA", "key.pem": "KEYDATA"} {
		got, err := os.ReadFile(filepath.Join(secretDir, "default", "mozak-brain-tls", name))
		if err != nil {
			t.Fatalf("reading restored %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("legacy secret %s = %q, want %q — restart did not self-heal", name, got, want)
		}
	}
}
