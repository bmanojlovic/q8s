package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// forceDeletePVCThroughAPI drives the real force-delete path: a DELETE marks
// the claim Terminating while a pod uses it, then a PATCH removes the
// protection finalizer and deletes the claim even though that pod still exists.
func forceDeletePVCThroughAPI(t *testing.T, srv *Server, ns, name string) {
	t.Helper()
	if rec := deletePVCObject(t, srv, ns, name); rec.Code != http.StatusOK {
		t.Fatalf("pvc delete status = %d, body %s", rec.Code, rec.Body.String())
	}
	if rec := patchPVC(t, srv, ns, name, `{"metadata":{"finalizers":[]}}`); rec.Code != http.StatusOK {
		t.Fatalf("pvc finalizer patch status = %d, body %s", rec.Code, rec.Body.String())
	}
	if _, err := srv.config.Store.GetPVC(ns, name); err == nil {
		t.Fatal("removing the protection finalizer did not delete the PVC")
	}
}

func deletePodInNamespaceThroughAPI(t *testing.T, srv *Server, ns, name string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/namespaces/"+ns+"/pods/"+name, nil)
	srv.handlePods(rec, req, ns, name)
	if rec.Code != http.StatusOK {
		t.Fatalf("pod delete status = %d, body %s", rec.Code, rec.Body.String())
	}
}

// recordVolumeUnitStops records only volume-unit stops, so a test can ignore
// the workload unit the same delete also stops.
func recordVolumeUnitStops(srv *Server) *[]string {
	stops := &[]string{}
	srv.stopUnitHook = func(name string) {
		if strings.HasSuffix(name, "-volume.service") {
			*stops = append(*stops, name)
		}
	}
	return stops
}

func TestPodDeleteStopsVolumeUnitOfForceDeletedClaim(t *testing.T) {
	srv, volumeFile := pvcDeleteServer(t, "demo", "data")
	recordPodmanCommand(t)
	if _, err := srv.config.Store.CreatePod(runningPodUsingClaim("demo", "app", "data")); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	stops := recordVolumeUnitStops(srv)

	forceDeletePVCThroughAPI(t, srv, "demo", "data")
	if len(*stops) != 0 {
		t.Fatalf("volume unit was stopped while the pod still uses the claim: %v", *stops)
	}
	// The forced delete removes the quadlet file but must leave the unit
	// running: the pod's container unit Requires= it.
	if _, err := os.Stat(volumeFile); !os.IsNotExist(err) {
		t.Errorf("volume quadlet survived the forced delete, stat err=%v", err)
	}

	deletePodInNamespaceThroughAPI(t, srv, "demo", "app")

	if want := []string{"demo-data-volume.service"}; !equalStrings(*stops, want) {
		t.Errorf("stopped volume units = %v, want %v", *stops, want)
	}
}

func TestDeploymentDeleteStopsVolumeUnitOfForceDeletedClaim(t *testing.T) {
	srv, _ := pvcDeleteServer(t, "demo", "data")
	recordPodmanCommand(t)
	replicas := int32(1)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "demo"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{Spec: podUsingClaim("demo", "unused", "data").Spec},
		},
	}
	created, err := srv.config.Store.CreateDeployment(dep)
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	instance := runningPodUsingClaim("demo", "web-0", "data")
	instance.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "apps/v1", Kind: "Deployment", Name: created.Name, UID: created.UID,
	}}
	if _, err := srv.config.Store.CreatePod(instance); err != nil {
		t.Fatalf("CreatePod web-0: %v", err)
	}
	stops := recordVolumeUnitStops(srv)

	forceDeletePVCThroughAPI(t, srv, "demo", "data")
	if len(*stops) != 0 {
		t.Fatalf("volume unit was stopped while the replica uses the claim: %v", *stops)
	}

	if err := srv.deleteDeploymentByName("demo", "web"); err != nil {
		t.Fatalf("deleteDeploymentByName: %v", err)
	}

	if want := []string{"demo-data-volume.service"}; !equalStrings(*stops, want) {
		t.Errorf("stopped volume units = %v, want %v", *stops, want)
	}
}

func TestDeploymentScaleDownStopsVolumeUnitOfForceDeletedClaim(t *testing.T) {
	srv, _ := pvcDeleteServer(t, "demo", "data")
	recordPodmanCommand(t)
	replicas := int32(1)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "demo"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{Spec: podUsingClaim("demo", "unused", "data").Spec},
		},
	}
	created, err := srv.config.Store.CreateDeployment(dep)
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	instance := runningPodUsingClaim("demo", "web-0", "data")
	instance.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "apps/v1", Kind: "Deployment", Name: created.Name, UID: created.UID,
	}}
	if _, err := srv.config.Store.CreatePod(instance); err != nil {
		t.Fatalf("CreatePod web-0: %v", err)
	}
	stops := recordVolumeUnitStops(srv)

	forceDeletePVCThroughAPI(t, srv, "demo", "data")
	if len(*stops) != 0 {
		t.Fatalf("volume unit was stopped while the replica uses the claim: %v", *stops)
	}

	srv.scaleDeployment(created, 1, 0)

	if want := []string{"demo-data-volume.service"}; !equalStrings(*stops, want) {
		t.Errorf("stopped volume units = %v, want %v", *stops, want)
	}
}

func TestDeletePodKeepsVolumeUnitOfStoredClaim(t *testing.T) {
	srv, _ := pvcDeleteServer(t, "demo", "data")
	recordPodmanCommand(t)
	if _, err := srv.config.Store.CreatePod(runningPodUsingClaim("demo", "app", "data")); err != nil {
		t.Fatalf("CreatePod app: %v", err)
	}
	stops := recordVolumeUnitStops(srv)

	deletePodInNamespaceThroughAPI(t, srv, "demo", "app")

	if len(*stops) != 0 {
		t.Errorf("volume unit was stopped while the claim is still stored: %v", *stops)
	}
	if _, err := srv.config.Store.GetPVC("demo", "data"); err != nil {
		t.Errorf("claim disappeared from the store: %v", err)
	}
}

func TestSecondPodDeleteStopsVolumeUnitOfForceDeletedClaim(t *testing.T) {
	srv, _ := pvcDeleteServer(t, "demo", "data")
	recordPodmanCommand(t)
	if _, err := srv.config.Store.CreatePod(runningPodUsingClaim("demo", "app", "data")); err != nil {
		t.Fatalf("CreatePod app: %v", err)
	}
	if _, err := srv.config.Store.CreatePod(runningPodUsingClaim("demo", "other", "data")); err != nil {
		t.Fatalf("CreatePod other: %v", err)
	}
	stops := recordVolumeUnitStops(srv)

	forceDeletePVCThroughAPI(t, srv, "demo", "data")
	if len(*stops) != 0 {
		t.Fatalf("volume unit was stopped while a pod still uses the claim: %v", *stops)
	}

	deletePodInNamespaceThroughAPI(t, srv, "demo", "app")
	if len(*stops) != 0 {
		t.Fatalf("volume unit was stopped while another pod still uses the claim: %v", *stops)
	}

	deletePodInNamespaceThroughAPI(t, srv, "demo", "other")
	if want := []string{"demo-data-volume.service"}; !equalStrings(*stops, want) {
		t.Errorf("stopped volume units = %v, want %v", *stops, want)
	}
}

func TestNamespaceDeleteStopsVolumeUnitOfForceDeletedClaim(t *testing.T) {
	srv, _ := pvcDeleteServer(t, "demo", "data")
	recordPodmanCommand(t)
	if _, err := srv.config.Store.CreatePod(runningPodUsingClaim("demo", "app", "data")); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}

	forceDeletePVCThroughAPI(t, srv, "demo", "data")

	var events []string
	srv.stopUnitHook = func(name string) { events = append(events, "stop "+name) }

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/namespaces/demo", nil)
	srv.handleNamespaceDelete(rec, req, "demo")
	if rec.Code != http.StatusOK {
		t.Fatalf("namespace delete status = %d, body %s", rec.Code, rec.Body.String())
	}

	idxPod := callIndex(events, "stop demo-app.service")
	idxVolume := callIndex(events, "stop demo-data-volume.service")
	if idxVolume < 0 {
		t.Fatalf("volume unit of the force-deleted claim was not stopped, events: %v", events)
	}
	if idxPod < 0 || idxPod > idxVolume {
		t.Errorf("volume unit must stop after the workload unit, events: %v", events)
	}
}

func TestJobDeleteStopsVolumeUnitOfForceDeletedClaim(t *testing.T) {
	srv, _ := pvcDeleteServer(t, "demo", "data")
	recordPodmanCommand(t)
	if _, err := srv.config.Store.CreateJob(runningJobUsingClaim("demo", "once", "data")); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	stops := recordVolumeUnitStops(srv)

	forceDeletePVCThroughAPI(t, srv, "demo", "data")
	if len(*stops) != 0 {
		t.Fatalf("volume unit was stopped while the Job uses the claim: %v", *stops)
	}

	if err := srv.deleteJobByName("demo", "once"); err != nil {
		t.Fatalf("deleteJobByName: %v", err)
	}

	if want := []string{"demo-data-volume.service"}; !equalStrings(*stops, want) {
		t.Errorf("stopped volume units = %v, want %v", *stops, want)
	}
}
