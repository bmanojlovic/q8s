package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"q8s/internal/quadlet"
	"q8s/internal/store"
)

// pvcDeleteServer returns a server with one stored namespace and PVC and its
// rendered .volume file, the state a PVC delete starts from.
func pvcDeleteServer(t *testing.T, ns, name string) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	st := store.New()
	if _, err := st.CreateNamespace(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	created, err := st.CreatePVC(pvc)
	if err != nil {
		t.Fatalf("CreatePVC: %v", err)
	}
	path := filepath.Join(dir, quadlet.PVCVolumeFileBase(created)+".volume")
	if err := os.WriteFile(path, []byte("[Volume]\nVolumeName="+created.Spec.VolumeName+"\n"), 0644); err != nil {
		t.Fatalf("write volume quadlet: %v", err)
	}
	return &Server{config: Config{Store: st, QuadletDir: dir}}, path
}

func podUsingClaim(ns, name, claim string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
			Volumes: []corev1.Volume{{
				Name: "data",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim},
				},
			}},
		},
	}
}

func deletePVCThroughAPI(t *testing.T, srv *Server, ns, name string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/namespaces/"+ns+"/persistentvolumeclaims/"+name, nil)
	srv.handlePVCs(rec, req, ns, name)
	if rec.Code != http.StatusOK {
		t.Fatalf("pvc delete status = %d, body %s", rec.Code, rec.Body.String())
	}
}

func TestDeleteUnusedPVCStopsVolumeUnitBeforeFileRemoval(t *testing.T) {
	srv, volumeFile := pvcDeleteServer(t, "demo", "data")

	var stops []string
	filePresentAtStop := false
	srv.stopUnitHook = func(name string) {
		stops = append(stops, name)
		if name == "demo_data-volume.service" {
			if _, err := os.Stat(volumeFile); err == nil {
				filePresentAtStop = true
			}
		}
	}

	deletePVCThroughAPI(t, srv, "demo", "data")

	if want := []string{"demo_data-volume.service"}; !equalStrings(stops, want) {
		t.Errorf("stopped units = %v, want %v", stops, want)
	}
	if !filePresentAtStop {
		t.Error("volume quadlet was already gone when the volume unit was stopped")
	}
	if _, err := os.Stat(volumeFile); !os.IsNotExist(err) {
		t.Errorf("volume quadlet survived delete, stat err=%v", err)
	}
}

// TestDeleteLegacyPVCStopsLegacyVolumeUnit: a claim created before the naming
// upgrade has spec.volumeName <ns>-<claim> and a <ns>-<claim>.volume file, and
// delete must stop exactly that unit and remove that file.
func TestDeleteLegacyPVCStopsLegacyVolumeUnit(t *testing.T) {
	dir := t.TempDir()
	st := store.New()
	if _, err := st.CreateNamespace(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	created, err := st.CreatePVC(&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "demo"}})
	if err != nil {
		t.Fatalf("CreatePVC: %v", err)
	}
	legacy := created.DeepCopy()
	legacy.Spec.VolumeName = "demo-data"
	if _, err := st.UpdatePVC(legacy); err != nil {
		t.Fatalf("UpdatePVC: %v", err)
	}
	legacyFile := filepath.Join(dir, "demo-data.volume")
	if err := os.WriteFile(legacyFile, []byte("[Volume]\nVolumeName=demo-data\n"), 0644); err != nil {
		t.Fatalf("write legacy volume quadlet: %v", err)
	}
	srv := &Server{config: Config{Store: st, QuadletDir: dir}}
	var stops []string
	srv.stopUnitHook = func(name string) { stops = append(stops, name) }

	deletePVCThroughAPI(t, srv, "demo", "data")

	if want := []string{"demo-data-volume.service"}; !equalStrings(stops, want) {
		t.Errorf("stopped units = %v, want %v", stops, want)
	}
	if _, err := os.Stat(legacyFile); !os.IsNotExist(err) {
		t.Errorf("legacy volume file survived delete, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "demo_data.volume")); !os.IsNotExist(err) {
		t.Errorf("delete touched a new-format file, stat err=%v", err)
	}
}

func TestDeletePVCUsedByPodMarksItTerminating(t *testing.T) {
	srv, volumeFile := pvcDeleteServer(t, "demo", "data")
	if _, err := srv.config.Store.CreatePod(podUsingClaim("demo", "app", "data")); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}

	var stops []string
	srv.stopUnitHook = func(name string) { stops = append(stops, name) }

	logged := captureStdout(t, func() {
		deletePVCThroughAPI(t, srv, "demo", "data")
	})

	if len(stops) != 0 {
		t.Errorf("volume unit was stopped while a pod uses the claim: %v", stops)
	}
	if _, err := os.Stat(volumeFile); err != nil {
		t.Errorf("volume quadlet was removed while the claim is Terminating: %v", err)
	}
	pvc, err := srv.config.Store.GetPVC("demo", "data")
	if err != nil {
		t.Fatalf("PVC was removed while a pod uses it: %v", err)
	}
	if pvc.DeletionTimestamp == nil {
		t.Error("PVC has no deletionTimestamp")
	}
	if !pvcHasFinalizer(pvc, pvcProtectionFinalizer) {
		t.Errorf("PVC finalizers = %v, want %q", pvc.Finalizers, pvcProtectionFinalizer)
	}
	if !strings.Contains(logged, "mark persistentvolumeclaim demo/data terminating") ||
		!strings.Contains(logged, "Pod/app uses it") {
		t.Errorf("no line about the Terminating claim, output: %q", logged)
	}
}

func TestNamespaceDeleteStopsWorkloadsBeforeVolumeUnit(t *testing.T) {
	srv, volumeFile := pvcDeleteServer(t, "demo", "data")
	if _, err := srv.config.Store.CreatePod(podUsingClaim("demo", "app", "data")); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}

	var events []string
	srv.stopUnitHook = func(name string) { events = append(events, "stop "+name) }
	old := podmanCommand
	podmanCommand = func(ctx context.Context, args ...string) ([]byte, error) {
		return nil, nil
	}
	t.Cleanup(func() { podmanCommand = old })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/namespaces/demo", nil)
	srv.handleNamespaceDelete(rec, req, "demo")
	if rec.Code != http.StatusOK {
		t.Fatalf("namespace delete status = %d, body %s", rec.Code, rec.Body.String())
	}

	idxPod := callIndex(events, "stop demo_app.service")
	idxVolume := callIndex(events, "stop demo_data-volume.service")
	if idxPod < 0 || idxVolume < 0 || idxPod > idxVolume {
		t.Errorf("volume unit must stop after the workload units, events: %v", events)
	}
	if _, err := os.Stat(volumeFile); !os.IsNotExist(err) {
		t.Errorf("volume quadlet survived namespace delete, stat err=%v", err)
	}
}

func TestDeleteHostPathPVCDoesNotStopAUnit(t *testing.T) {
	srv, volumeFile := pvcDeleteServer(t, "demo", "data")
	pvc, err := srv.config.Store.GetPVC("demo", "data")
	if err != nil {
		t.Fatalf("GetPVC: %v", err)
	}
	hostPath := quadlet.StorageClassHostPath
	pvc.Spec.StorageClassName = &hostPath
	if _, err := srv.config.Store.UpdatePVC(pvc); err != nil {
		t.Fatalf("UpdatePVC: %v", err)
	}
	// A hostpath claim never gets a .volume file; remove the one the helper
	// wrote so the test starts from the real state.
	if err := os.Remove(volumeFile); err != nil {
		t.Fatalf("remove volume quadlet: %v", err)
	}

	var stops []string
	srv.stopUnitHook = func(name string) { stops = append(stops, name) }

	deletePVCThroughAPI(t, srv, "demo", "data")

	if len(stops) != 0 {
		t.Errorf("hostpath PVC delete stopped units: %v", stops)
	}
}

// captureStdout runs fn and returns everything it wrote to os.Stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = writer
	defer func() {
		os.Stdout = old
		writer.Close()
		reader.Close()
	}()
	fn()
	writer.Close()
	os.Stdout = old
	out, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read captured log: %v", err)
	}
	return string(out)
}
