package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"q8s/internal/quadlet"
	"q8s/internal/store"
)

// populateUninstallStore fills a store with every object shape uninstall must
// cover: a single-container Pod, a multi-container Pod, a Deployment with two
// replicas, a Job, a CronJob, a PVC and a second namespace.
func populateUninstallStore(t *testing.T, st *store.Store) {
	t.Helper()
	if _, err := st.CreateNamespace(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "work"},
	}); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	if _, err := st.CreatePod(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
	}); err != nil {
		t.Fatalf("CreatePod app: %v", err)
	}
	if _, err := st.CreatePod(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "multi",
			Namespace:   "work",
			Annotations: map[string]string{quadlet.AnnotationSocketActivation: "api=8080"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "one", Image: "nginx"},
			{Name: "two", Image: "busybox"},
		}},
	}); err != nil {
		t.Fatalf("CreatePod multi: %v", err)
	}
	replicas := int32(2)
	if _, err := st.CreateDeployment(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
			},
		},
	}); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	if _, err := st.CreateJob(&batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "batch", Namespace: "default"},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "batch", Image: "busybox"}}},
		}},
	}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if _, err := st.CreateCronJob(&batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "default"},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 3 * * *",
			JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "nightly", Image: "busybox"}}},
				},
			}},
		},
	}); err != nil {
		t.Fatalf("CreateCronJob: %v", err)
	}
	if _, err := st.CreatePVC(&corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "default"},
	}); err != nil {
		t.Fatalf("CreatePVC: %v", err)
	}
}

func newUninstallUnitStore(t *testing.T) *store.Store {
	t.Helper()
	st := store.New()
	populateUninstallStore(t, st)
	return st
}

func requireUnits(t *testing.T, group []string, want ...string) {
	t.Helper()
	have := make(map[string]bool, len(group))
	for _, unit := range group {
		if have[unit] {
			t.Errorf("duplicate unit %q in group %v", unit, group)
		}
		have[unit] = true
	}
	if len(group) != len(want) {
		t.Errorf("group has %d units, want %d:\n got %v\nwant %v", len(group), len(want), group, want)
	}
	for _, unit := range want {
		if !have[unit] {
			t.Errorf("group is missing %q: %v", unit, group)
		}
	}
}

func TestUninstallUnitsGroupsOrderAndNames(t *testing.T) {
	st := newUninstallUnitStore(t)
	groups := uninstallUnits(st)
	if len(groups) != 5 {
		t.Fatalf("uninstallUnits returned %d groups, want 5", len(groups))
	}

	requireUnits(t, groups[0],
		"default_nightly__cron.timer",
		"default-nightly-cron.timer")

	requireUnits(t, groups[1],
		// Single-container Pod.
		"default_app.service", "default-app.service",
		// Multi-container Pod: two member services and the pod unit.
		"work_multi_one.service", "work-multi-one.service",
		"work_multi_two.service", "work-multi-two.service",
		"work_multi-pod.service", "work-multi-pod.service",
		// Deployment instances.
		"default_web-0.service", "default-web-0.service",
		"default_web-1.service", "default-web-1.service",
		// Job and CronJob services.
		"default_batch__job.service", "default-batch-job.service",
		"default_nightly__cron.service", "default-nightly-cron.service")

	requireUnits(t, groups[2],
		"work_multi_api.socket", "work-multi-api.socket")

	requireUnits(t, groups[3],
		"default_data-volume.service", "default-data-volume.service")

	requireUnits(t, groups[4],
		"q8s-default-network.service", "q8s-work-network.service")
}

func TestUninstallUnitsDeduplicates(t *testing.T) {
	// The default namespace appears both in Namespaces() and in the explicit
	// q8s-default-network.service entry.
	st := store.New()
	groups := uninstallUnits(st)
	if len(groups[4]) != 1 || groups[4][0] != "q8s-default-network.service" {
		t.Fatalf("empty store network group = %v, want one default network unit", groups[4])
	}
}

func TestParseUninstallArgs(t *testing.T) {
	tests := []struct {
		args string
		want uninstallOptions
		err  bool
	}{
		{"", uninstallOptions{}, false},
		{"--yes", uninstallOptions{yes: true}, false},
		{"--purge", uninstallOptions{purge: true}, false},
		{"--yes --purge", uninstallOptions{yes: true, purge: true}, false},
		{"-h", uninstallOptions{help: true}, false},
		{"--help", uninstallOptions{help: true}, false},
		{"--bogus", uninstallOptions{}, true},
	}
	for _, tc := range tests {
		var args []string
		if tc.args != "" {
			args = strings.Fields(tc.args)
		}
		got, err := parseUninstallArgs(args)
		if tc.err {
			if err == nil {
				t.Errorf("parseUninstallArgs(%v) succeeded, want an error", args)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseUninstallArgs(%v): %v", args, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseUninstallArgs(%v) = %+v, want %+v", args, got, tc.want)
		}
	}
}

func TestUninstallConfirmation(t *testing.T) {
	tests := []struct {
		name  string
		yes   bool
		tty   bool
		input string
		want  error
	}{
		{"yes flag", true, false, "", nil},
		{"non-terminal without yes", false, false, "", errUninstallNeedsYes},
		{"terminal yes", false, true, "yes\n", nil},
		{"terminal no", false, true, "no\n", errUninstallAborted},
		{"terminal empty", false, true, "", errUninstallAborted},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := confirmUninstall(tc.yes, tc.tty, strings.NewReader(tc.input), &out, false)
			if !errors.Is(err, tc.want) {
				t.Fatalf("confirmUninstall() = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestUninstallWithoutYesOnNonTerminalRunsNothing(t *testing.T) {
	fake := fakeUninstallCommands(t)
	var out, errOut bytes.Buffer
	code := runUninstallCommand(nil, strings.NewReader(""), false, &out, &errOut)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "refusing to uninstall without --yes (stdin is not a terminal)") {
		t.Errorf("refusal message missing: %q", errOut.String())
	}
	if len(fake.calls) != 0 {
		t.Fatalf("refused uninstall ran commands: %v", fake.calls)
	}
}

type fakeUninstall struct {
	calls              []string
	timerPath          string
	timerPresentAtStop bool
	absent             map[string]bool
}

func fakeUninstallCommands(t *testing.T) *fakeUninstall {
	t.Helper()
	f := &fakeUninstall{}
	oldRun, oldOutput := runCmd, runCmdOutput
	runCmd = func(name string, args ...string) error {
		f.calls = append(f.calls, commandLine(name, args))
		if name == "systemctl" && containsArg(args, "stop") &&
			containsArg(args, "default_nightly__cron.timer") && f.timerPath != "" {
			if _, err := os.Stat(f.timerPath); err == nil {
				f.timerPresentAtStop = true
			}
		}
		return nil
	}
	runCmdOutput = func(name string, args ...string) ([]byte, error) {
		f.calls = append(f.calls, commandLine(name, args))
		if name == "systemctl" && containsArg(args, "list-units") {
			// Every listed unit counts as loaded, except the ones a test
			// marks as absent.
			var b strings.Builder
			for i, a := range args {
				if i > 0 && !strings.HasPrefix(a, "-") && a != "list-units" && !f.absent[a] {
					b.WriteString(a + " loaded inactive dead x\n")
				}
			}
			return []byte(b.String()), nil
		}
		if name == "podman" && len(args) >= 2 && args[0] == "volume" && args[1] == "inspect" {
			return []byte("default_data\n"), nil
		}
		return nil, nil
	}
	t.Cleanup(func() {
		runCmd = oldRun
		runCmdOutput = oldOutput
	})
	return f
}

func commandLine(name string, args []string) string {
	return strings.Join(append([]string{name}, args...), " ")
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func callContaining(calls []string, parts ...string) string {
	for _, call := range calls {
		ok := true
		for _, part := range parts {
			if !strings.Contains(call, part) {
				ok = false
				break
			}
		}
		if ok {
			return call
		}
	}
	return ""
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func writeTestLink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0755); err != nil {
		t.Fatalf("mkdir for %s: %v", link, err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink %s -> %s: %v", link, target, err)
	}
}

func newUninstallTestDirs(t *testing.T) dirs {
	t.Helper()
	root := t.TempDir()
	d := dirs{
		dataDir:    filepath.Join(root, "data"),
		quadletDir: filepath.Join(root, "quadlets"),
		systemdDir: filepath.Join(root, "systemd"),
		configDir:  filepath.Join(root, "run", "q8s", "configmaps"),
		secretDir:  filepath.Join(root, "run", "q8s", "secrets"),
	}
	for _, dir := range []string{d.dataDir, d.quadletDir, d.systemdDir, d.configDir, d.secretDir, filepath.Join(root, "data", "snapshots")} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	return d
}

func TestUninstallStopsUnitsBeforeRemovingFilesAndCleansUp(t *testing.T) {
	d := newUninstallTestDirs(t)
	st, err := store.Load(filepath.Join(d.dataDir, "store.json"))
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	populateUninstallStore(t, st)
	st.Close()

	timerPath := filepath.Join(d.systemdDir, "default_nightly__cron.timer")
	legacyTimerPath := filepath.Join(d.systemdDir, "default-nightly-cron.timer")
	socketPath := filepath.Join(d.systemdDir, "work_multi_api.socket")
	files := []string{
		filepath.Join(d.quadletDir, "default_app.container"),
		filepath.Join(d.quadletDir, "default-app.container"),
		filepath.Join(d.quadletDir, "default_nightly__cron.container"),
		filepath.Join(d.quadletDir, "default-nightly-cron.container"),
		filepath.Join(d.quadletDir, "default_data.volume"),
		filepath.Join(d.quadletDir, "default-data.volume"),
		filepath.Join(d.quadletDir, "q8s-default.network"),
		filepath.Join(d.quadletDir, "q8s-work.network"),
		timerPath,
		legacyTimerPath,
		filepath.Join(d.systemdDir, "q8s.socket"),
		filepath.Join(d.systemdDir, "q8s-api.service"),
		filepath.Join(d.systemdDir, "valid.timer"),
	}
	for _, path := range files {
		writeTestFile(t, path, "[Unit]\n")
	}
	writeTestFile(t, socketPath, "[Socket]\nX-Q8s-Namespace=work\n")
	writeTestFile(t, filepath.Join(d.systemdDir, "work-multi-api.socket"), "[Socket]\nX-Q8s-Namespace=work\n")

	wantsTimers := filepath.Join(d.systemdDir, "timers.target.wants")
	writeTestLink(t, "../default_nightly__cron.timer", filepath.Join(wantsTimers, "default_nightly__cron.timer"))
	writeTestLink(t, "../default-nightly-cron.timer", filepath.Join(wantsTimers, "default-nightly-cron.timer"))
	writeTestLink(t, "../missing.timer", filepath.Join(wantsTimers, "unrelated.timer"))
	writeTestLink(t, "../valid.timer", filepath.Join(wantsTimers, "valid.timer"))
	wantsSockets := filepath.Join(d.systemdDir, "sockets.target.wants")
	writeTestLink(t, "../work_multi_api.socket", filepath.Join(wantsSockets, "work_multi_api.socket"))
	writeTestLink(t, "../work-multi-api.socket", filepath.Join(wantsSockets, "work-multi-api.socket"))

	stampDir := t.TempDir()
	oldStampDir := uninstallTimerStampDir
	uninstallTimerStampDir = func() string { return stampDir }
	t.Cleanup(func() { uninstallTimerStampDir = oldStampDir })
	for _, name := range []string{
		"stamp-default_nightly__cron.timer",
		"stamp-default-nightly-cron.timer",
		"stamp-unrelated.timer",
	} {
		writeTestFile(t, filepath.Join(stampDir, name), "stamp")
	}

	// The parts of the data dir today's code removes, plus files it keeps.
	writeTestFile(t, filepath.Join(d.dataDir, "certs", "ca.key"), "key")
	writeTestFile(t, filepath.Join(d.dataDir, "traefik", "dynamic.yaml"), "x")
	writeTestFile(t, filepath.Join(d.dataDir, "quadlets", "old"), "x")
	writeTestFile(t, filepath.Join(d.dataDir, "snapshots", "1.gz"), "x")
	writeTestFile(t, filepath.Join(d.dataDir, "config.json"), "{}")
	writeTestFile(t, filepath.Join(d.dataDir, "workload-bin", "q8s"), "x")
	writeTestFile(t, filepath.Join(d.dataDir, "keep.txt"), "keep")

	leakedTemp := filepath.Join(d.quadletDir, ".default_app.container.tmp-123")
	writeTestFile(t, leakedTemp, "partial")
	unrelatedTemp := filepath.Join(d.quadletDir, ".other.container.tmp-123")
	writeTestFile(t, unrelatedTemp, "partial")

	fake := fakeUninstallCommands(t)
	fake.timerPath = timerPath
	var out, errOut bytes.Buffer
	runUninstall(false, d, false, &out, &errOut)

	if _, err := os.Stat(leakedTemp); !os.IsNotExist(err) {
		t.Errorf("leaked atomic-write temp of a removed file survived: %v", err)
	}
	if _, err := os.Stat(unrelatedTemp); err != nil {
		t.Errorf("unrelated temp file was removed: %v", err)
	}
	if !fake.timerPresentAtStop {
		t.Error("timer file was removed before the timer was stopped")
	}
	if _, err := os.Stat(timerPath); !os.IsNotExist(err) {
		t.Errorf("new timer file still exists: %v", err)
	}
	if _, err := os.Stat(legacyTimerPath); !os.IsNotExist(err) {
		t.Errorf("legacy timer file still exists: %v", err)
	}

	timerStop := callContaining(fake.calls, "systemctl --user stop", "default_nightly__cron.timer", "default-nightly-cron.timer")
	if timerStop == "" {
		t.Errorf("timers were not stopped in one call: %v", fake.calls)
	}
	if callContaining(fake.calls, "systemctl --user disable", "default_nightly__cron.timer") == "" {
		t.Errorf("timers were not disabled: %v", fake.calls)
	}
	if callContaining(fake.calls, "systemctl --user stop", "work_multi_api.socket", "work-multi-api.socket") == "" {
		t.Errorf("pod sockets were not stopped in one call: %v", fake.calls)
	}
	if callContaining(fake.calls, "systemctl --user stop", "default_data-volume.service", "default-data-volume.service") == "" {
		t.Errorf("volume units were not stopped in one call: %v", fake.calls)
	}
	if callContaining(fake.calls, "systemctl --user stop", "q8s-default-network.service", "q8s-work-network.service") == "" {
		t.Errorf("network units were not stopped in one call: %v", fake.calls)
	}
	if callContaining(fake.calls, "podman rm -f --filter label=app.kubernetes.io/managed-by=q8s") == "" {
		t.Errorf("q8s containers were not removed by label: %v", fake.calls)
	}
	if callContaining(fake.calls, "podman pod rm -f --ignore", "work_multi", "work-multi") == "" {
		t.Errorf("generated pod names were not removed: %v", fake.calls)
	}
	if callContaining(fake.calls, "podman network rm", "q8s-default", "q8s-work") == "" {
		t.Errorf("q8s networks were not removed: %v", fake.calls)
	}
	for _, call := range fake.calls {
		if strings.Contains(call, "volume rm") {
			t.Errorf("uninstall removed a Podman volume: %s", call)
		}
	}

	// Dangling links to removed units go; unrelated links stay.
	for _, link := range []string{
		filepath.Join(wantsTimers, "default_nightly__cron.timer"),
		filepath.Join(wantsTimers, "default-nightly-cron.timer"),
		filepath.Join(wantsSockets, "work_multi_api.socket"),
		filepath.Join(wantsSockets, "work-multi-api.socket"),
	} {
		if _, err := os.Lstat(link); !os.IsNotExist(err) {
			t.Errorf("dangling link %s was not removed: %v", link, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(wantsTimers, "unrelated.timer")); err != nil {
		t.Errorf("unrelated dangling link was removed: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(wantsTimers, "valid.timer")); err != nil {
		t.Errorf("unrelated valid link was removed: %v", err)
	}
	if _, err := os.Stat(wantsSockets); !os.IsNotExist(err) {
		t.Errorf("empty sockets.target.wants was not removed: %v", err)
	}

	for _, name := range []string{
		"stamp-default_nightly__cron.timer",
		"stamp-default-nightly-cron.timer",
	} {
		// Without --purge the store is kept for a restore, so the stamps
		// stay: a restored Persistent= timer must not fire at once.
		if _, err := os.Stat(filepath.Join(stampDir, name)); err != nil {
			t.Errorf("timer stamp %s was removed without --purge: %v", name, err)
		}
	}

	if _, err := os.Stat(filepath.Join(d.configDir, "..")); !os.IsNotExist(err) {
		t.Errorf("runtime parent directory still exists: %v", err)
	}

	if _, err := os.Stat(filepath.Join(d.dataDir, "store.json")); err != nil {
		t.Errorf("store.json was removed without --purge: %v", err)
	}
	if _, err := os.Stat(filepath.Join(d.dataDir, "store.json.bak")); err != nil {
		t.Errorf("store.json.bak was not kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(d.dataDir, "keep.txt")); err != nil {
		t.Errorf("unrelated data file was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(d.dataDir, "workload-bin", "q8s")); err != nil {
		t.Errorf("workload-bin was removed, but today's code keeps it: %v", err)
	}
	for _, path := range []string{
		filepath.Join(d.dataDir, "certs"),
		filepath.Join(d.dataDir, "traefik"),
		filepath.Join(d.dataDir, "quadlets"),
		filepath.Join(d.dataDir, "snapshots"),
		filepath.Join(d.dataDir, "config.json"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s was not removed: %v", path, err)
		}
	}

	for _, want := range []string{
		"q8s uninstalled",
		"rm -rf " + d.dataDir,
		"podman volume rm default_data",
		"were NOT deleted",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("summary does not contain %q:\n%s", want, out.String())
		}
	}
}

func TestUninstallPurgeRemovesDataDir(t *testing.T) {
	d := newUninstallTestDirs(t)
	st, err := store.Load(filepath.Join(d.dataDir, "store.json"))
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	populateUninstallStore(t, st)
	st.Close()
	writeTestFile(t, filepath.Join(d.dataDir, "keep.txt"), "keep")
	stampDir := t.TempDir()
	oldStampDir := uninstallTimerStampDir
	uninstallTimerStampDir = func() string { return stampDir }
	t.Cleanup(func() { uninstallTimerStampDir = oldStampDir })
	for _, name := range []string{"stamp-default_nightly__cron.timer", "stamp-default-nightly-cron.timer", "stamp-unrelated.timer"} {
		writeTestFile(t, filepath.Join(stampDir, name), "stamp")
	}

	fake := fakeUninstallCommands(t)
	var out, errOut bytes.Buffer
	runUninstall(false, d, true, &out, &errOut)

	for _, name := range []string{"stamp-default_nightly__cron.timer", "stamp-default-nightly-cron.timer"} {
		if _, err := os.Stat(filepath.Join(stampDir, name)); !os.IsNotExist(err) {
			t.Errorf("timer stamp %s survived --purge: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(stampDir, "stamp-unrelated.timer")); err != nil {
		t.Errorf("unrelated timer stamp was removed: %v", err)
	}

	if _, err := os.Stat(d.dataDir); !os.IsNotExist(err) {
		t.Fatalf("data dir survived --purge: %v", err)
	}
	if !strings.Contains(out.String(), "Removed the q8s data directory (--purge).") {
		t.Errorf("summary does not report the purge:\n%s", out.String())
	}
	for _, call := range fake.calls {
		if strings.Contains(call, "volume rm") {
			t.Errorf("--purge removed a Podman volume: %s", call)
		}
	}
}

func TestLoadedUnitsSkipsUnitsSystemdDoesNotKnow(t *testing.T) {
	fake := fakeUninstallCommands(t)
	fake.absent = map[string]bool{"default-nightly-cron.timer": true}
	got := loadedUnits([]string{"--user"}, []string{"default_nightly__cron.timer", "default-nightly-cron.timer"})
	if len(got) != 1 || got[0] != "default_nightly__cron.timer" {
		t.Fatalf("loadedUnits = %v, want only the loaded new-name timer", got)
	}
	if got := loadedUnits(nil, nil); len(got) != 0 {
		t.Fatalf("loadedUnits(nil) = %v", got)
	}
}
