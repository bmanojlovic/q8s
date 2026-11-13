package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"q8s/internal/podman"
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

// --- podmanContainerSyncState (single-container observation) ---

func TestPodmanContainerSyncStateTimes(t *testing.T) {
	started := time.Unix(1700000000, 0).UTC()
	exited := time.Unix(1700000300, 0).UTC()
	got := podmanContainerSyncState(podman.ContainerInfo{
		State:     "exited",
		ExitCode:  1,
		StartedAt: started.Unix(),
		ExitedAt:  exited.Unix(),
	})
	if !got.Seen {
		t.Error("Seen = false, want true for a listed container")
	}
	if got.Phase != corev1.PodFailed {
		t.Errorf("Phase = %q, want Failed", got.Phase)
	}
	if got.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", got.ExitCode)
	}
	if !got.StartedAt.Equal(started) {
		t.Errorf("StartedAt = %v, want %v", got.StartedAt, started)
	}
	if !got.ExitedAt.Equal(exited) {
		t.Errorf("ExitedAt = %v, want %v", got.ExitedAt, exited)
	}
}

func TestPodmanContainerSyncStateUnsetTimes(t *testing.T) {
	got := podmanContainerSyncState(podman.ContainerInfo{State: "running"})
	if !got.StartedAt.IsZero() || !got.ExitedAt.IsZero() {
		t.Errorf("times = %v/%v, want zero for unset podman values", got.StartedAt, got.ExitedAt)
	}
}

// --- podmanContainerStates (multi-container sync) ---

func TestPodmanContainerStatesMulti(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "healthy"},
			{Name: "starting"},
		}},
	}
	infos := []podman.ContainerInfo{
		{State: "running", Status: "Up 1 minute (healthy)", Labels: map[string]string{"io.kubernetes.container.name": "healthy"}},
		{State: "running", Status: "Up 1 minute (starting)", Labels: map[string]string{"io.kubernetes.container.name": "starting"}},
	}

	phase, states := podmanContainerStates(pod, infos)
	if phase != corev1.PodRunning {
		t.Fatalf("expected Phase Running when every container runs, got %q", phase)
	}
	if states["healthy"].Health != "healthy" || states["starting"].Health != "starting" {
		t.Fatalf("expected per-container health to be matched by label, got %+v", states)
	}
}

func TestPodmanContainerStatesFailed(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
	}
	phase, _ := podmanContainerStates(pod, []podman.ContainerInfo{
		{State: "exited", ExitCode: 1, Labels: map[string]string{"io.kubernetes.container.name": "app"}},
	})
	if phase != corev1.PodFailed {
		t.Fatalf("expected Phase Failed, got %q", phase)
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
// the test VM. Load folds stringData into Data, so restore writes it out.
func TestRestoreLegacySecretSelfHealsOnLoad(t *testing.T) {
	tmp := t.TempDir()
	storeFile := filepath.Join(tmp, "store.json")
	legacy := `{
	  "secrets": [
	    {
	      "apiVersion": "v1", "kind": "Secret",
	      "metadata": {"name": "demo-app-tls", "namespace": "default"},
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
		got, err := os.ReadFile(filepath.Join(secretDir, "default", "demo-app-tls", name))
		if err != nil {
			t.Fatalf("reading restored %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("legacy secret %s = %q, want %q — restart did not self-heal", name, got, want)
		}
	}
}

// --- isQ8sUnit ---

func TestIsQ8sUnit(t *testing.T) {
	dir := t.TempDir()
	// One quadlet of every shape q8s writes. Only the file names matter;
	// isQ8sUnit never reads them.
	for _, name := range []string{
		"demo-app.container",
		"default-demo-0.container",
		"demo-multi.pod",
		"demo-multi-web.container",
		"default-demo-cron.container",
		"q8s-default.network",
		"demo-data.volume",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("[Container]\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		unit string
		want bool
	}{
		{"demo-app.service", true},
		{"default-demo-0.service", true},
		{"demo-multi-web.service", true},
		{"demo-multi-pod.service", true},    // from demo-multi.pod
		{"default-demo-cron.service", true}, // CronJob container
		{"other.service", false},            // no quadlet source
		{"demo-multi.service", false},       // pod unit is <base>-pod.service
		{"demo-app.container", false},       // not a service
		{"demo-multi-pod.timer", false},
		{"demo-app.socket", false},
		{"q8s-default.service", false}, // .network has no service
		{"demo-data.service", false},   // .volume has no service
		{"-pod.service", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := isQ8sUnit(dir, tc.unit); got != tc.want {
			t.Errorf("isQ8sUnit(%q) = %v, want %v", tc.unit, got, tc.want)
		}
	}
}

func TestIsQ8sUnitMissingQuadletDir(t *testing.T) {
	if isQ8sUnit(filepath.Join(t.TempDir(), "does-not-exist"), "demo-app.service") {
		t.Error("isQ8sUnit() = true for a unit with no quadlet source")
	}
}

func TestSyncUnitMatcher(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "demo-app.container"), []byte("[Container]\n"), 0644); err != nil {
		t.Fatal(err)
	}

	watched := strings.Repeat("a", 64)
	unwatched := strings.Repeat("b", 64)
	ids := map[string]struct{}{watched: {}}
	var current atomic.Pointer[map[string]struct{}]
	current.Store(&ids)
	match := syncUnitMatcher(dir, &current)

	tests := []struct {
		unit string
		want bool
	}{
		{"demo-app.service", true},
		{watched + "-2bc0f6a9870abb7b.service", true},
		{unwatched + "-2bc0f6a9870abb7b.service", false},
		{unwatched + "-2bc0f6a9870abb7b.timer", false},
		{"demo-app.container", false},
	}
	for _, tc := range tests {
		if got := match(tc.unit); got != tc.want {
			t.Errorf("syncUnitMatcher(%q) = %v, want %v", tc.unit, got, tc.want)
		}
	}

	// An empty set means no container is inside its probe window. Health
	// check units then cost nothing, while q8s units keep matching.
	empty := map[string]struct{}{}
	current.Store(&empty)
	if match(watched + "-2bc0f6a9870abb7b.service") {
		t.Error("health check unit matched after the watch set was emptied")
	}
	if !match("demo-app.service") {
		t.Error("q8s unit stopped matching after the watch set was emptied")
	}
}

func TestChangedNamespacesForBackends(t *testing.T) {
	pod := func(ns, name string, phase corev1.PodPhase) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Status: corev1.PodStatus{Phase: phase}}
	}
	// At boot the store still has the phases written at shutdown.
	before := servablePods([]*corev1.Pod{
		pod("mail", "stalwart-0", corev1.PodFailed),
		pod("default", "traefik-0", corev1.PodRunning),
		pod("web", "app-0", corev1.PodRunning),
	})
	after := servablePods([]*corev1.Pod{
		pod("mail", "stalwart-0", corev1.PodRunning),
		pod("default", "traefik-0", corev1.PodRunning),
		pod("web", "app-0", corev1.PodSucceeded),
	})
	got := changedNamespaces(before, after)
	if len(got) != 2 || got[0] != "mail" || got[1] != "web" {
		t.Fatalf("changedNamespaces = %v, want [mail web]", got)
	}
	if got := changedNamespaces(after, after); len(got) != 0 {
		t.Fatalf("unchanged state reported %v", got)
	}
}

// A unit that systemd has not started yet looks exactly like one that exited
// 0 (inactive, Result=success). A Pending pod with no container stays Pending.
func TestUnitPhaseNotStartedYetStaysPending(t *testing.T) {
	notStarted := &systemd.UnitState{Active: "inactive", Sub: "dead", Result: "success"}
	pending := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending}}
	if got := unitPhase(pending, notStarted, nil); got != corev1.PodPending {
		t.Fatalf("pending pod, no container: %s, want Pending", got)
	}
	running := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	if got := unitPhase(running, notStarted, nil); got != corev1.PodSucceeded {
		t.Fatalf("pod that ran: %s, want Succeeded", got)
	}
	exited := []podman.ContainerInfo{{State: "exited"}}
	if got := unitPhase(pending, notStarted, exited); got != corev1.PodSucceeded {
		t.Fatalf("pending pod with an exited container: %s, want Succeeded", got)
	}
}
