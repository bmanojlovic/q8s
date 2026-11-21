package server

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"q8s/internal/names"
	"q8s/internal/quadlet"
	"q8s/internal/store"
)

type nameMigrationHost struct {
	srv        *Server
	st         *store.Store
	quadletDir string
	systemdDir string
	configDir  string
	dataDir    string
	secretDir  string

	stops     []string
	starts    []string
	enables   []string
	disables  []string
	reloads   int
	podman    []string
	legacyPVC string
}

func (h *nameMigrationHost) clearEvents() {
	h.stops = nil
	h.starts = nil
	h.enables = nil
	h.disables = nil
	h.reloads = 0
	h.podman = nil
}

func (h *nameMigrationHost) file(parts ...string) string {
	return filepath.Join(append([]string{h.quadletDir}, parts...)...)
}

func (h *nameMigrationHost) systemdFile(name string) string {
	return filepath.Join(h.systemdDir, name)
}

func (h *nameMigrationHost) envFile(name string) string {
	return filepath.Join(h.secretDir, "demo", "_env", name)
}

func newNameMigrationHost(t *testing.T) *nameMigrationHost {
	t.Helper()
	h := &nameMigrationHost{
		st:         store.New(),
		quadletDir: t.TempDir(),
		systemdDir: t.TempDir(),
		configDir:  t.TempDir(),
		dataDir:    t.TempDir(),
	}
	h.secretDir = filepath.Join(filepath.Dir(h.configDir), "secrets")
	if _, err := h.st.CreateNamespace(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}

	replicas := int32(2)
	objects := []interface{}{
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "app", Namespace: "demo",
				Annotations: map[string]string{quadlet.AnnotationSocketActivation: "http=18080"},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "scratch"}}},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "duo", Namespace: "demo"},
			Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Name: "web", Image: "scratch"},
				{Name: "side", Image: "scratch"},
			}},
		},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "demo"},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "web", Image: "scratch"}},
				}},
			},
		},
		&batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "jobrun", Namespace: "demo"},
			Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "jobrun", Image: "scratch"}},
			}}},
		},
		&batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "jobdone", Namespace: "demo"},
			Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "jobdone", Image: "scratch"}},
			}}},
			Status: batchv1.JobStatus{Succeeded: 1},
		},
		&batchv1.CronJob{
			ObjectMeta: metav1.ObjectMeta{Name: "cronactive", Namespace: "demo"},
			Spec: batchv1.CronJobSpec{
				Schedule: "0 3 * * *",
				JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "cronactive", Image: "scratch"}},
					}},
				}},
			},
		},
	}
	suspended := true
	objects = append(objects, &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "cronsus", Namespace: "demo"},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 4 * * *",
			Suspend:  &suspended,
			JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{
				Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "cronsus", Image: "scratch"}},
				}},
			}},
		},
	})
	for _, obj := range objects {
		var err error
		switch o := obj.(type) {
		case *corev1.Pod:
			_, err = h.st.CreatePod(o)
		case *appsv1.Deployment:
			_, err = h.st.CreateDeployment(o)
		case *batchv1.Job:
			_, err = h.st.CreateJob(o)
		case *batchv1.CronJob:
			_, err = h.st.CreateCronJob(o)
		}
		if err != nil {
			t.Fatalf("create stored object: %v", err)
		}
	}

	pvc, err := h.st.CreatePVC(&corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "demo"},
	})
	if err != nil {
		t.Fatalf("CreatePVC: %v", err)
	}
	pvc.Spec.VolumeName = names.LegacyHost(names.KindPVCVolume, "demo", "data")
	if _, err := h.st.UpdatePVC(pvc); err != nil {
		t.Fatalf("UpdatePVC: %v", err)
	}
	h.legacyPVC = names.LegacyHost(names.KindPVCFile, "demo", "data")

	var starts, stops, enables, disables []string
	h.srv = &Server{config: Config{
		Store:      h.st,
		QuadletDir: h.quadletDir,
		SystemdDir: h.systemdDir,
		ConfigDir:  h.configDir,
		DataDir:    h.dataDir,
	}}
	h.srv.stopUnitHook = func(name string) { h.stops = append(h.stops, name) }
	h.srv.startUnitHook = func(name string) { h.starts = append(h.starts, name) }
	h.srv.enableUnitHook = func(name string) error {
		h.enables = append(h.enables, name)
		return nil
	}
	h.srv.disableUnitHook = func(name string) error {
		h.disables = append(h.disables, name)
		return nil
	}
	h.srv.daemonReloadHook = func() error {
		h.reloads++
		return nil
	}
	_ = starts
	_ = stops
	_ = enables
	_ = disables

	old := podmanCommand
	podmanCommand = func(ctx context.Context, args ...string) ([]byte, error) {
		h.podman = append(h.podman, strings.Join(args, " "))
		return nil, nil
	}
	t.Cleanup(func() { podmanCommand = old })
	return h
}

func (h *nameMigrationHost) writeFile(path, content string) {
	path = filepath.Clean(path)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		panic(err)
	}
}

func (h *nameMigrationHost) writeLegacyFiles(t *testing.T) {
	t.Helper()
	legacy := func(kind names.Kind, name string, extra ...string) string {
		return names.LegacyHost(kind, "demo", name, extra...)
	}
	h.writeFile(h.file(legacy(names.KindPodContainerFile, "app", "")), "[Container]\n")
	h.writeFile(h.systemdFile(legacy(names.KindSocketFile, "app", "http")), "[Socket]\n")

	h.writeFile(h.file(legacy(names.KindPodFile, "duo")), "[Pod]\n")
	h.writeFile(h.file(legacy(names.KindPodContainerFile, "duo", "web")), "[Container]\n")
	h.writeFile(h.file(legacy(names.KindPodContainerFile, "duo", "side")), "[Container]\n")

	for i := 0; i < 2; i++ {
		instance := quadlet.DeploymentInstanceName("web", int32(i))
		h.writeFile(h.file(legacy(names.KindPodContainerFile, instance, "")), "[Container]\n")
	}

	h.writeFile(h.file(legacy(names.KindJobFile, "jobrun")), "[Container]\n")
	h.writeFile(h.file(legacy(names.KindJobFile, "jobdone")), "[Container]\n")
	h.writeFile(h.envFile(names.LegacyHost(names.KindEnvJob, "", "jobrun")), "A=1\n")
	h.writeFile(h.envFile(names.LegacyHost(names.KindEnvJob, "", "jobdone")), "A=1\n")

	h.writeFile(h.file(legacy(names.KindCronJobFile, "cronactive")), "[Container]\n")
	h.writeFile(h.systemdFile(legacy(names.KindCronJobTimer, "cronactive")), "[Timer]\n")
	h.writeFile(h.envFile(names.LegacyHost(names.KindEnvCronJob, "", "cronactive")), "A=1\n")
	h.writeFile(h.file(legacy(names.KindCronJobFile, "cronsus")), "[Container]\n")
	h.writeFile(h.systemdFile(legacy(names.KindCronJobTimer, "cronsus")), "[Timer]\n")
	h.writeFile(h.envFile(names.LegacyHost(names.KindEnvCronJob, "", "cronsus")), "A=1\n")

	h.writeFile(h.file(h.legacyPVC+".volume"), "[Volume]\nVolumeName="+h.legacyPVC+"\n")
}

func (h *nameMigrationHost) writeNewFiles(t *testing.T) {
	t.Helper()
	host := func(kind names.Kind, name string, extra ...string) string {
		return names.Host(kind, "demo", name, extra...)
	}
	single := func(name string, containers []corev1.Container) {
		if len(containers) > 1 {
			h.writeFile(h.file(host(names.KindPodFile, name)), "[Pod]\n")
			for _, c := range containers {
				h.writeFile(h.file(host(names.KindPodContainerFile, name, c.Name)), "[Container]\n")
			}
			return
		}
		h.writeFile(h.file(host(names.KindPodContainerFile, name, "")), "[Container]\n")
	}
	single("app", []corev1.Container{{Name: "app"}})
	h.writeFile(h.systemdFile(host(names.KindSocketFile, "app", "http")), "[Socket]\n")
	single("duo", []corev1.Container{{Name: "web"}, {Name: "side"}})
	single("web-0", []corev1.Container{{Name: "web"}})
	single("web-1", []corev1.Container{{Name: "web"}})
	h.writeFile(h.file(host(names.KindJobFile, "jobrun")), "[Container]\n")
	h.writeFile(h.file(host(names.KindJobFile, "jobdone")), "[Container]\n")
	h.writeFile(h.file(host(names.KindCronJobFile, "cronactive")), "[Container]\n")
	h.writeFile(h.systemdFile(host(names.KindCronJobTimer, "cronactive")), "[Timer]\n")
	h.writeFile(h.file(host(names.KindCronJobFile, "cronsus")), "[Container]\n")
	h.writeFile(h.systemdFile(host(names.KindCronJobTimer, "cronsus")), "[Timer]\n")
}

func hasName(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func nameIndex(items []string, want string) int {
	for i, item := range items {
		if item == want {
			return i
		}
	}
	return -1
}

func assertNoFile(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s still exists (stat err %v)", path, err)
	}
}

func assertFile(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("%s is missing: %v", path, err)
	}
}

func TestMigrateLegacyNamesStopsRemovesAndReconcileStartsNewUnits(t *testing.T) {
	h := newNameMigrationHost(t)
	h.writeLegacyFiles(t)

	if got := h.srv.MigrateLegacyNames(); got != 7 {
		t.Fatalf("migrated objects = %d, want 7", got)
	}
	h.srv.ReconcileQuadlets()
	h.srv.EnsureWorkloadsStarted()
	if err := h.srv.WriteNamesMigrationMarker(); err != nil {
		t.Fatalf("WriteNamesMigrationMarker: %v", err)
	}

	legacy := func(kind names.Kind, name string, extra ...string) string {
		return names.LegacyHost(kind, "demo", name, extra...)
	}
	for _, path := range []string{
		h.file(legacy(names.KindPodContainerFile, "app", "")),
		h.systemdFile(legacy(names.KindSocketFile, "app", "http")),
		h.file(legacy(names.KindPodFile, "duo")),
		h.file(legacy(names.KindPodContainerFile, "duo", "web")),
		h.file(legacy(names.KindPodContainerFile, "duo", "side")),
		h.file(legacy(names.KindPodContainerFile, "web-0", "")),
		h.file(legacy(names.KindPodContainerFile, "web-1", "")),
		h.file(legacy(names.KindJobFile, "jobrun")),
		h.file(legacy(names.KindJobFile, "jobdone")),
		h.file(legacy(names.KindCronJobFile, "cronactive")),
		h.systemdFile(legacy(names.KindCronJobTimer, "cronactive")),
		h.file(legacy(names.KindCronJobFile, "cronsus")),
		h.systemdFile(legacy(names.KindCronJobTimer, "cronsus")),
		h.envFile(names.LegacyHost(names.KindEnvJob, "", "jobrun")),
		h.envFile(names.LegacyHost(names.KindEnvJob, "", "jobdone")),
		h.envFile(names.LegacyHost(names.KindEnvCronJob, "", "cronactive")),
		h.envFile(names.LegacyHost(names.KindEnvCronJob, "", "cronsus")),
	} {
		assertNoFile(t, path)
	}
	assertFile(t, h.file(h.legacyPVC+".volume"))

	host := func(kind names.Kind, name string, extra ...string) string {
		return names.Host(kind, "demo", name, extra...)
	}
	for _, path := range []string{
		h.file(host(names.KindPodContainerFile, "app", "")),
		h.systemdFile(host(names.KindSocketFile, "app", "http")),
		h.file(host(names.KindPodFile, "duo")),
		h.file(host(names.KindPodContainerFile, "duo", "web")),
		h.file(host(names.KindPodContainerFile, "duo", "side")),
		h.file(host(names.KindPodContainerFile, "web-0", "")),
		h.file(host(names.KindPodContainerFile, "web-1", "")),
		h.file(host(names.KindJobFile, "jobrun")),
		h.file(host(names.KindJobFile, "jobdone")),
		h.file(host(names.KindCronJobFile, "cronactive")),
		h.systemdFile(host(names.KindCronJobTimer, "cronactive")),
		h.file(host(names.KindCronJobFile, "cronsus")),
		h.systemdFile(host(names.KindCronJobTimer, "cronsus")),
	} {
		assertFile(t, path)
	}
	assertFile(t, filepath.Join(h.dataDir, namesMarkerName))

	for _, unit := range []string{
		legacy(names.KindPodService, "app"),
		legacy(names.KindSocketFile, "app", "http"),
		legacy(names.KindPodUnit, "duo"),
		legacy(names.KindPodContainerService, "duo", "web"),
		legacy(names.KindPodContainerService, "duo", "side"),
		legacy(names.KindPodService, "web-0"),
		legacy(names.KindPodService, "web-1"),
		legacy(names.KindJobService, "jobrun"),
		legacy(names.KindJobService, "jobdone"),
		legacy(names.KindCronJobService, "cronactive"),
		legacy(names.KindCronJobTimer, "cronactive"),
		legacy(names.KindCronJobService, "cronsus"),
		legacy(names.KindCronJobTimer, "cronsus"),
	} {
		if !hasName(h.stops, unit) {
			t.Errorf("legacy unit %s was not stopped; stops: %v", unit, h.stops)
		}
	}
	for _, name := range []string{
		legacy(names.KindPodContainer, "app", ""),
		legacy(names.KindPodContainer, "web-0", ""),
		legacy(names.KindPodContainer, "web-1", ""),
		legacy(names.KindJob, "jobrun"),
		legacy(names.KindJob, "jobdone"),
		legacy(names.KindCronJob, "cronactive"),
		legacy(names.KindCronJob, "cronsus"),
	} {
		if !hasName(h.podman, "rm -f "+name) {
			t.Errorf("legacy container %s was not removed; podman calls: %v", name, h.podman)
		}
	}
	if !hasName(h.podman, "pod rm -f --ignore "+legacy(names.KindPod, "duo")) {
		t.Errorf("legacy Podman pod duo was not removed; podman calls: %v", h.podman)
	}

	for _, unit := range []string{
		host(names.KindPodService, "app"),
		host(names.KindPodContainerService, "duo", "web"),
		host(names.KindPodContainerService, "duo", "side"),
		host(names.KindPodService, "web-0"),
		host(names.KindPodService, "web-1"),
		host(names.KindJobService, "jobrun"),
		host(names.KindCronJobTimer, "cronactive"),
	} {
		if !hasName(h.starts, unit) {
			t.Errorf("new unit %s was not started; starts: %v", unit, h.starts)
		}
	}
	if hasName(h.starts, host(names.KindJobService, "jobdone")) {
		t.Errorf("completed Job was started again: %v", h.starts)
	}
	if hasName(h.starts, host(names.KindCronJobTimer, "cronsus")) {
		t.Errorf("suspended CronJob timer was started: %v", h.starts)
	}
	if hasName(h.enables, host(names.KindCronJobTimer, "cronsus")) {
		t.Errorf("suspended CronJob timer was enabled: %v", h.enables)
	}
	if !hasName(h.enables, host(names.KindCronJobTimer, "cronactive")) {
		t.Errorf("active CronJob timer was not enabled: %v", h.enables)
	}
	socketStart := nameIndex(h.starts, host(names.KindSocketFile, "app", "http"))
	serviceStart := nameIndex(h.starts, host(names.KindPodService, "app"))
	if socketStart < 0 || serviceStart < 0 || socketStart > serviceStart {
		t.Errorf("socket must start before its service (socket %d, service %d): %v", socketStart, serviceStart, h.starts)
	}
	if h.reloads == 0 {
		t.Error("migration did not daemon-reload")
	}
}

func TestMigrateLegacyNamesWithMarkerDoesNothing(t *testing.T) {
	h := newNameMigrationHost(t)
	if err := h.srv.WriteNamesMigrationMarker(); err != nil {
		t.Fatalf("WriteNamesMigrationMarker: %v", err)
	}
	h.clearEvents()
	if got := h.srv.MigrateLegacyNames(); got != 0 {
		t.Fatalf("migrated objects = %d, want 0", got)
	}
	h.srv.EnsureWorkloadsStarted()
	if len(h.podman) != 0 {
		t.Errorf("podman was called with the marker present: %v", h.podman)
	}
	if len(h.stops) != 0 || h.reloads != 0 {
		t.Errorf("startup migration acted with the marker present: stops %v reloads %d", h.stops, h.reloads)
	}
}

func TestNamesMigrationCrashBeforeNewFilesConverges(t *testing.T) {
	h := newNameMigrationHost(t)
	// The legacy files are already gone, no new files and no marker: the
	// state after a crash between removing the old files and writing new ones.
	if got := h.srv.MigrateLegacyNames(); got != 0 {
		t.Fatalf("migrated objects = %d, want 0", got)
	}
	h.srv.ReconcileQuadlets()
	h.srv.EnsureWorkloadsStarted()

	assertFile(t, filepath.Join(h.quadletDir, names.Host(names.KindPodContainerFile, "demo", "app", "")))
	assertFile(t, filepath.Join(h.quadletDir, names.Host(names.KindJobFile, "demo", "jobrun")))
	if !hasName(h.starts, names.Host(names.KindPodService, "demo", "app")) {
		t.Errorf("pod was not started after the crash: %v", h.starts)
	}
	if !hasName(h.starts, names.Host(names.KindJobService, "demo", "jobrun")) {
		t.Errorf("running Job was not started after the crash: %v", h.starts)
	}
	if hasName(h.starts, names.Host(names.KindJobService, "demo", "jobdone")) {
		t.Errorf("completed Job was started after the crash: %v", h.starts)
	}
}

func TestNamesMigrationEnsureStartsAfterFilesWereWritten(t *testing.T) {
	h := newNameMigrationHost(t)
	h.writeNewFiles(t)
	if got := h.srv.MigrateLegacyNames(); got != 0 {
		t.Fatalf("migrated objects = %d, want 0", got)
	}
	h.srv.ReconcileQuadlets()
	// Reconcile only starts files that were missing. Simulate the crash
	// window where every file exists but the units were never started.
	h.clearEvents()
	h.srv.EnsureWorkloadsStarted()

	// demo/app is socket-activated: its socket starts, its service waits for
	// the first connection, as the normal deploy path leaves it.
	if !hasName(h.starts, names.Host(names.KindSocketFile, "demo", "app", "http")) {
		t.Errorf("ensure pass did not start the demo/app socket; starts: %v", h.starts)
	}
	if hasName(h.starts, names.Host(names.KindPodService, "demo", "app")) {
		t.Errorf("ensure pass started the socket-activated service eagerly: %v", h.starts)
	}
	for _, unit := range []string{
		names.Host(names.KindPodContainerService, "demo", "duo", "web"),
		names.Host(names.KindPodContainerService, "demo", "duo", "side"),
		names.Host(names.KindPodService, "demo", "web-0"),
		names.Host(names.KindPodService, "demo", "web-1"),
		names.Host(names.KindJobService, "demo", "jobrun"),
		names.Host(names.KindCronJobTimer, "demo", "cronactive"),
	} {
		if !hasName(h.starts, unit) {
			t.Errorf("ensure pass did not start %s; starts: %v", unit, h.starts)
		}
	}
	if hasName(h.starts, names.Host(names.KindJobService, "demo", "jobdone")) {
		t.Errorf("ensure pass started a completed Job: %v", h.starts)
	}
	if hasName(h.starts, names.Host(names.KindCronJobTimer, "demo", "cronsus")) {
		t.Errorf("ensure pass started a suspended timer: %v", h.starts)
	}
}

func TestPlanLegacyMigrationChangesNothing(t *testing.T) {
	h := newNameMigrationHost(t)
	h.writeLegacyFiles(t)
	h.clearEvents()
	plan := h.srv.PlanLegacyMigration()
	if len(plan.Objects) != 7 {
		t.Fatalf("plan objects = %v, want 7", plan.Objects)
	}
	if len(plan.StopUnits) == 0 || len(plan.Files) == 0 || len(plan.Containers) == 0 || len(plan.Pods) == 0 {
		t.Fatalf("plan is incomplete: %+v", plan)
	}
	for _, path := range plan.Files {
		assertFile(t, path)
	}
	if len(h.stops) != 0 || len(h.starts) != 0 || h.reloads != 0 {
		t.Errorf("plan changed the host: stops %v starts %v reloads %d", h.stops, h.starts, h.reloads)
	}
	for _, call := range h.podman {
		if strings.HasPrefix(call, "rm ") || strings.HasPrefix(call, "pod rm ") {
			t.Errorf("plan ran a podman removal: %v", h.podman)
		}
	}
	if _, err := os.Stat(filepath.Join(h.dataDir, namesMarkerName)); !os.IsNotExist(err) {
		t.Errorf("plan wrote the marker")
	}
}

func TestNewFormatPVCClaimsListsNewVolumes(t *testing.T) {
	h := newNameMigrationHost(t)
	if claims := h.srv.NewFormatPVCClaims(); len(claims) != 0 {
		t.Fatalf("legacy PVC reported as new: %v", claims)
	}
	pvc, err := h.st.GetPVC("demo", "data")
	if err != nil {
		t.Fatalf("GetPVC: %v", err)
	}
	pvc.Spec.VolumeName = names.Host(names.KindPVCVolume, "demo", "data")
	if _, err := h.st.UpdatePVC(pvc); err != nil {
		t.Fatalf("UpdatePVC: %v", err)
	}
	if got, want := h.srv.NewFormatPVCClaims(), []string{"demo/data"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("claims = %v, want %v", got, want)
	}
}

func TestPlanNewNamesRollbackRemovesOnlyNewArtifacts(t *testing.T) {
	h := newNameMigrationHost(t)
	h.writeNewFiles(t)
	// A legacy claim keeps its old .volume file through a rollback. The
	// command refuses before this on a new-format claim, but the legacy file
	// must survive the plan itself as well.
	h.writeFile(h.file(h.legacyPVC+".volume"), "[Volume]\nVolumeName="+h.legacyPVC+"\n")
	h.writeFile(h.envFile(names.Host(names.KindEnvJob, "", "jobrun")), "A=1\n")
	h.writeFile(h.envFile(names.Host(names.KindEnvCronJob, "", "cronactive")), "A=1\n")
	h.clearEvents()

	plan := h.srv.PlanNewNamesRollback()
	if len(plan.Objects) == 0 {
		t.Fatal("rollback plan found no new artifacts")
	}
	for _, want := range []string{
		h.file(names.Host(names.KindPodContainerFile, "demo", "app", "")),
		h.file(names.Host(names.KindPodFile, "demo", "duo")),
		h.file(names.Host(names.KindJobFile, "demo", "jobrun")),
		h.file(names.Host(names.KindCronJobFile, "demo", "cronactive")),
		h.systemdFile(names.Host(names.KindCronJobTimer, "demo", "cronactive")),
		h.envFile(names.Host(names.KindEnvJob, "", "jobrun")),
		h.envFile(names.Host(names.KindEnvCronJob, "", "cronactive")),
	} {
		if !hasName(plan.Files, want) {
			t.Errorf("rollback plan does not remove %s; files: %v", want, plan.Files)
		}
	}
	if !hasName(plan.StopUnits, names.Host(names.KindPodService, "demo", "app")) {
		t.Errorf("rollback plan does not stop the new pod service: %v", plan.StopUnits)
	}
	if !hasName(plan.DisableUnits, names.Host(names.KindCronJobTimer, "demo", "cronactive")) {
		t.Errorf("rollback plan does not disable the new CronJob timer: %v", plan.DisableUnits)
	}
	if hasName(plan.Files, h.file(h.legacyPVC+".volume")) {
		t.Errorf("rollback plan removes the legacy PVC .volume file: %v", plan.Files)
	}

	if err := h.srv.ApplyNameMigrationPlan(plan); err != nil {
		t.Fatalf("ApplyNameMigrationPlan: %v", err)
	}
	assertNoFile(t, h.file(names.Host(names.KindPodContainerFile, "demo", "app", "")))
	assertNoFile(t, h.file(names.Host(names.KindPodFile, "demo", "duo")))
	assertNoFile(t, h.file(names.Host(names.KindJobFile, "demo", "jobrun")))
	assertNoFile(t, h.systemdFile(names.Host(names.KindCronJobTimer, "demo", "cronactive")))
	assertFile(t, h.file(h.legacyPVC+".volume"))
	if !hasName(h.podman, "rm -f "+names.Host(names.KindPodContainer, "demo", "app", "")) {
		t.Errorf("new container was not removed: %v", h.podman)
	}
	if !hasName(h.podman, "pod rm -f --ignore "+names.Host(names.KindPod, "demo", "duo")) {
		t.Errorf("new Podman pod was not removed: %v", h.podman)
	}
	if h.reloads == 0 {
		t.Error("rollback did not daemon-reload")
	}
}

// TestCronTimerStampsFollowTheRename: a CronJob timer uses Persistent=true,
// and a timer without a stamp fires as soon as it starts. The stamp moves to
// the new timer name (and back on rollback) with its mtime, so the first
// start after the rename does not run a slot the old timer already ran.
func TestCronTimerStampsFollowTheRename(t *testing.T) {
	h := newNameMigrationHost(t)
	dir := t.TempDir()
	legacy := filepath.Join(dir, "stamp-"+names.LegacyHost(names.KindCronJobTimer, "demo", "cronactive"))
	renamed := filepath.Join(dir, "stamp-"+names.Host(names.KindCronJobTimer, "demo", "cronactive"))
	if err := os.WriteFile(legacy, nil, 0644); err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC)
	if err := os.Chtimes(legacy, when, when); err != nil {
		t.Fatal(err)
	}

	h.srv.moveCronTimerStamps(dir, true)
	assertNoFile(t, legacy)
	info, err := os.Stat(renamed)
	if err != nil {
		t.Fatalf("stamp not moved to the new timer: %v", err)
	}
	if !info.ModTime().Equal(when) {
		t.Fatalf("stamp mtime = %v, want %v", info.ModTime(), when)
	}

	h.srv.moveCronTimerStamps(dir, false)
	assertNoFile(t, renamed)
	assertFile(t, legacy)

	// Both stamps present (an earlier upgrade and rollback): the newer wins.
	later := when.Add(time.Hour)
	if err := os.WriteFile(renamed, nil, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(renamed, later, later); err != nil {
		t.Fatal(err)
	}
	h.srv.moveCronTimerStamps(dir, false)
	assertNoFile(t, renamed)
	if info, err := os.Stat(legacy); err != nil || !info.ModTime().Equal(later) {
		t.Fatalf("newer stamp did not win: %v %v", info, err)
	}
	if err := os.WriteFile(renamed, nil, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(renamed, when, when); err != nil {
		t.Fatal(err)
	}
	h.srv.moveCronTimerStamps(dir, true)
	if info, err := os.Stat(renamed); err != nil || !info.ModTime().Equal(later) {
		t.Fatalf("older target stamp was kept: %v %v", info, err)
	}
	assertNoFile(t, legacy)
}
