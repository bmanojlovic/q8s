package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"q8s/internal/quadlet"
	"q8s/internal/store"
)

func recordPodmanCalls(t *testing.T) *[]string {
	t.Helper()
	calls := &[]string{}
	old := podmanCommand
	podmanCommand = func(ctx context.Context, args ...string) error {
		*calls = append(*calls, strings.Join(args, " "))
		return nil
	}
	t.Cleanup(func() { podmanCommand = old })
	return calls
}

func hasCall(calls []string, want string) bool {
	for _, call := range calls {
		if call == want {
			return true
		}
	}
	return false
}

func callIndex(calls []string, want string) int {
	for i, call := range calls {
		if call == want {
			return i
		}
	}
	return -1
}

func writeOwnedUnit(t *testing.T, dir, name, ns, podName string) string {
	t.Helper()
	content := "[Container]\n" + quadlet.ManagedByLabel + "\n" +
		"Label=io.kubernetes.pod.namespace=" + ns + "\n" +
		"Label=io.kubernetes.pod.name=" + podName + "\n"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func multiContainerSpec() corev1.PodSpec {
	return corev1.PodSpec{Containers: []corev1.Container{
		{Name: "web", Image: "nginx"},
		{Name: "sidecar", Image: "busybox"},
	}}
}

func TestDeleteMultiContainerPodStopsPodBeforeQuadletRemoval(t *testing.T) {
	dir := t.TempDir()
	st := store.New()
	srv := &Server{config: Config{Store: st, QuadletDir: dir}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "duo", Namespace: "default"},
		Spec:       multiContainerSpec(),
	}
	if _, err := st.CreatePod(pod); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	podPath := writeOwnedUnit(t, dir, "default-duo.pod", "default", "duo")

	var events []string
	filePresentAtStop := false
	srv.stopUnitHook = func(name string) {
		events = append(events, "stop "+name)
		if name == "default-duo-pod.service" {
			if _, err := os.Stat(podPath); err == nil {
				filePresentAtStop = true
			}
		}
	}
	old := podmanCommand
	podmanCommand = func(ctx context.Context, args ...string) error {
		events = append(events, "podman "+strings.Join(args, " "))
		if len(args) > 1 && args[0] == "pod" && args[1] == "rm" {
			if _, err := os.Stat(podPath); err != nil {
				filePresentAtStop = false
			}
		}
		return nil
	}
	t.Cleanup(func() { podmanCommand = old })

	if err := srv.deletePodByName("default", "duo"); err != nil {
		t.Fatalf("deletePodByName: %v", err)
	}

	for _, want := range []string{
		"stop default-duo-web.service",
		"stop default-duo-sidecar.service",
		"stop default-duo-pod.service",
		"podman pod rm -f --ignore default-duo",
	} {
		if !hasCall(events, want) {
			t.Errorf("missing %q in events %v", want, events)
		}
	}
	if idxStop, idxRm := callIndex(events, "stop default-duo-pod.service"), callIndex(events, "podman pod rm -f --ignore default-duo"); idxStop < 0 || idxRm < 0 || idxStop > idxRm {
		t.Errorf("pod service must stop before podman pod rm, events: %v", events)
	}
	if !filePresentAtStop {
		t.Error("pod quadlet file was already gone when the pod was stopped or removed")
	}
	if _, err := os.Stat(podPath); !os.IsNotExist(err) {
		t.Errorf("pod quadlet file survived delete, stat err=%v", err)
	}
}

func TestDeleteMultiContainerDeploymentRemovesBothPods(t *testing.T) {
	dir := t.TempDir()
	st := store.New()
	srv := &Server{config: Config{Store: st, QuadletDir: dir}}
	replicas := int32(2)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "duo", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{Spec: multiContainerSpec()},
		},
	}
	if _, err := st.CreateDeployment(dep); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	writeOwnedUnit(t, dir, "default-duo-0.pod", "default", "duo-0")
	writeOwnedUnit(t, dir, "default-duo-1.pod", "default", "duo-1")
	calls := recordPodmanCalls(t)

	if err := srv.deleteDeploymentByName("default", "duo"); err != nil {
		t.Fatalf("deleteDeploymentByName: %v", err)
	}

	for _, want := range []string{
		"pod rm -f --ignore default-duo-0",
		"pod rm -f --ignore default-duo-1",
	} {
		if !hasCall(*calls, want) {
			t.Errorf("missing %q in podman calls %v", want, *calls)
		}
	}
	if _, err := st.GetDeployment("default", "duo"); err == nil {
		t.Fatal("deleted deployment is still in the store")
	}
	for _, name := range []string{"duo-0", "duo-1"} {
		if _, err := st.GetPod("default", name); err == nil {
			t.Errorf("deleted deployment instance %q is still in the store", name)
		}
		if _, err := os.Stat(filepath.Join(dir, "default-"+name+".pod")); !os.IsNotExist(err) {
			t.Errorf("deleted instance %q quadlet survived, stat err=%v", name, err)
		}
	}
}

func TestScaleDownMultiContainerDeploymentRemovesOnlyDroppedInstance(t *testing.T) {
	dir := t.TempDir()
	st := store.New()
	srv := &Server{config: Config{Store: st, QuadletDir: dir}}
	replicas := int32(2)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "duo", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{Spec: multiContainerSpec()},
		},
	}
	if _, err := st.CreateDeployment(dep); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	writeOwnedUnit(t, dir, "default-duo-0.pod", "default", "duo-0")
	dropped := writeOwnedUnit(t, dir, "default-duo-1.pod", "default", "duo-1")
	calls := recordPodmanCalls(t)

	srv.scaleDeployment(dep, 2, 1)

	if !hasCall(*calls, "pod rm -f --ignore default-duo-1") {
		t.Errorf("missing pod rm for dropped instance in %v", *calls)
	}
	if hasCall(*calls, "pod rm -f --ignore default-duo-0") {
		t.Errorf("instance -0 must survive scale-down, calls %v", *calls)
	}
	if _, err := os.Stat(dropped); !os.IsNotExist(err) {
		t.Errorf("dropped instance quadlet survived, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "default-duo-0.pod")); err != nil {
		t.Errorf("instance -0 quadlet was removed: %v", err)
	}
}

func TestDeleteSingleContainerPodMakesNoPodmanPodCall(t *testing.T) {
	dir := t.TempDir()
	st := store.New()
	srv := &Server{config: Config{Store: st, QuadletDir: dir}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "solo", Namespace: "default"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "app", Image: "nginx"},
		}},
	}
	if _, err := st.CreatePod(pod); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	writeOwnedUnit(t, dir, "default-solo.container", "default", "solo")
	calls := recordPodmanCalls(t)

	if err := srv.deletePodByName("default", "solo"); err != nil {
		t.Fatalf("deletePodByName: %v", err)
	}

	if !hasCall(*calls, "rm -f default-solo") {
		t.Errorf("missing container removal in %v", *calls)
	}
	for _, call := range *calls {
		if strings.HasPrefix(call, "pod ") {
			t.Errorf("single-container delete must not touch podman pods, calls %v", *calls)
			break
		}
	}
	if _, err := st.GetPod("default", "solo"); err == nil {
		t.Fatal("deleted pod is still in the store")
	}
	if _, err := os.Stat(filepath.Join(dir, "default-solo.container")); !os.IsNotExist(err) {
		t.Fatalf("deleted pod quadlet survived, stat err=%v", err)
	}
}

func TestStalePodUnitRemovesPodmanPod(t *testing.T) {
	dir := t.TempDir()
	srv := &Server{config: Config{QuadletDir: dir}}
	stale := writeOwnedUnit(t, dir, "default-duo.pod", "default", "duo")
	keep := writeOwnedUnit(t, dir, "default-duo.container", "default", "duo")
	calls := recordPodmanCalls(t)

	removed := srv.removeStaleOwnedUnits(dir, "default", "duo", map[string]bool{"default-duo.container": true})
	if !removed {
		t.Fatal("expected stale .pod to be removed")
	}
	if !hasCall(*calls, "pod rm -f --ignore default-duo") {
		t.Errorf("missing pod rm for stale .pod in %v", *calls)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale .pod survived, stat err=%v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("kept container unit was removed: %v", err)
	}
}

// TestDeleteNamespacePersistFailureRestoresWorkload: the namespace purge
// removes its resources before the namespace entry; a failed final store write
// restores the files and units of the objects captured before the purge.
func TestDeleteNamespacePersistFailureRestoresWorkload(t *testing.T) {
	dataDir := t.TempDir()
	file := filepath.Join(dataDir, "store.json")
	st, err := store.Load(file)
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	dir := t.TempDir()
	srv := &Server{config: Config{Store: st, QuadletDir: dir}}
	if _, err := st.CreateNamespace(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team"}}); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	if _, err := st.CreatePod(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "team"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
	}); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	podPath := writeOwnedUnit(t, dir, "team-app.container", "team", "app")

	var stops, starts []string
	srv.stopUnitHook = func(name string) { stops = append(stops, name) }
	srv.startUnitHook = func(name string) { starts = append(starts, name) }
	recordPodmanCalls(t)

	if err := os.RemoveAll(dataDir); err != nil {
		t.Fatalf("remove data dir: %v", err)
	}
	if err := os.WriteFile(dataDir, []byte("not a directory"), 0600); err != nil {
		t.Fatalf("replace data dir with file: %v", err)
	}

	rec := httptest.NewRecorder()
	srv.handleNamespaceDelete(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/namespaces/team", nil), "team")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
	}
	if !hasCall(stops, "team-app.service") {
		t.Fatalf("pod unit was not stopped before the store delete: %v", stops)
	}
	if !hasCall(starts, "team-app.service") {
		t.Fatalf("restored namespace workload was not started: %v", starts)
	}
	if _, err := os.Stat(podPath); err != nil {
		t.Fatalf("namespace pod quadlet was not restored: %v", err)
	}
	if _, err := st.GetNamespace("team"); err != nil {
		t.Fatalf("namespace was not rolled back after failed delete: %v", err)
	}
	if _, err := st.GetPod("team", "app"); err != nil {
		t.Fatalf("namespace pod was not rolled back after failed delete: %v", err)
	}
}

// TestDeletePodPersistFailureRestoresWorkload is the delete-ordering
// regression: after stopping the unit and removing its quadlet, a failed
// final store write restores the pod through the create path, keeps it in the
// store and answers with the persist error.
func TestDeletePodPersistFailureRestoresWorkload(t *testing.T) {
	dataDir := t.TempDir()
	file := filepath.Join(dataDir, "store.json")
	st, err := store.Load(file)
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	dir := t.TempDir()
	srv := &Server{config: Config{Store: st, QuadletDir: dir}}
	if _, err := st.CreatePod(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "solo", Namespace: "default"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
	}); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	podPath := writeOwnedUnit(t, dir, "default-solo.container", "default", "solo")

	var stops, starts []string
	srv.stopUnitHook = func(name string) { stops = append(stops, name) }
	srv.startUnitHook = func(name string) { starts = append(starts, name) }
	calls := recordPodmanCalls(t)

	// Every following store write fails (root ignores permissions but not a
	// regular file where a directory is expected).
	if err := os.RemoveAll(dataDir); err != nil {
		t.Fatalf("remove data dir: %v", err)
	}
	if err := os.WriteFile(dataDir, []byte("not a directory"), 0600); err != nil {
		t.Fatalf("replace data dir with file: %v", err)
	}

	err = srv.deletePodByName("default", "solo")
	if !errors.Is(err, store.ErrPersist) {
		t.Fatalf("deletePodByName error = %v, want one wrapping ErrPersist", err)
	}
	if !hasCall(stops, "default-solo.service") {
		t.Fatalf("unit was not stopped before the store delete: %v", stops)
	}
	if !hasCall(*calls, "rm -f default-solo") {
		t.Fatalf("container was not removed before the store delete: %v", *calls)
	}
	if !hasCall(starts, "default-solo.service") {
		t.Fatalf("restored workload was not started: %v", starts)
	}
	if _, err := os.Stat(podPath); err != nil {
		t.Fatalf("quadlet file was not restored after failed persist: %v", err)
	}
	if _, err := st.GetPod("default", "solo"); err != nil {
		t.Fatalf("pod was not rolled back after failed persist: %v", err)
	}
}
