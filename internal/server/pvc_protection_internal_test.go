package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"q8s/internal/quadlet"
)

func deletePVCObject(t *testing.T, srv *Server, ns, name string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/namespaces/"+ns+"/persistentvolumeclaims/"+name, nil)
	srv.handlePVCs(rec, req, ns, name)
	return rec
}

func runningPodUsingClaim(ns, name, claim string) *corev1.Pod {
	pod := podUsingClaim(ns, name, claim)
	pod.Status.Phase = corev1.PodRunning
	return pod
}

func addPVCWithVolumeFile(t *testing.T, srv *Server, ns, name string) string {
	t.Helper()
	created, err := srv.config.Store.CreatePVC(&corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
	})
	if err != nil {
		t.Fatalf("CreatePVC %s/%s: %v", ns, name, err)
	}
	path := filepath.Join(srv.config.QuadletDir, quadlet.PVCVolumeFileBase(created)+".volume")
	if err := os.WriteFile(path, []byte("[Volume]\nVolumeName="+created.Spec.VolumeName+"\n"), 0644); err != nil {
		t.Fatalf("write volume quadlet: %v", err)
	}
	return path
}

func TestPVCProtectionDeleteOfUnusedClaimIsImmediate(t *testing.T) {
	srv, volumeFile := pvcDeleteServer(t, "demo", "data")
	var stops []string
	srv.stopUnitHook = func(name string) { stops = append(stops, name) }

	rec := deletePVCObject(t, srv, "demo", "data")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body %s", rec.Code, rec.Body.String())
	}
	if _, err := srv.config.Store.GetPVC("demo", "data"); err == nil {
		t.Fatal("unused PVC was not deleted")
	}
	if _, err := os.Stat(volumeFile); !os.IsNotExist(err) {
		t.Errorf("unused PVC left its volume file, stat err=%v", err)
	}
	if want := []string{"demo_data-volume.service"}; !equalStrings(stops, want) {
		t.Errorf("stopped units = %v, want %v", stops, want)
	}
}

func TestPVCProtectionDeleteOfUsedClaimWaitsForPod(t *testing.T) {
	srv, volumeFile := pvcDeleteServer(t, "demo", "data")
	if _, err := srv.config.Store.CreatePod(runningPodUsingClaim("demo", "app", "data")); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	var stops []string
	srv.stopUnitHook = func(name string) { stops = append(stops, name) }

	rec := deletePVCObject(t, srv, "demo", "data")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body %s", rec.Code, rec.Body.String())
	}
	var response corev1.PersistentVolumeClaim
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode delete response: %v", err)
	}
	if response.DeletionTimestamp == nil {
		t.Errorf("delete response has no deletionTimestamp: %s", rec.Body.String())
	}
	if !pvcHasFinalizer(&response, pvcProtectionFinalizer) {
		t.Errorf("delete response finalizers = %v", response.Finalizers)
	}

	stored, err := srv.config.Store.GetPVC("demo", "data")
	if err != nil {
		t.Fatalf("PVC was removed while a pod uses it: %v", err)
	}
	if stored.DeletionTimestamp == nil {
		t.Error("stored PVC has no deletionTimestamp")
	}
	if !pvcHasFinalizer(stored, pvcProtectionFinalizer) {
		t.Errorf("stored PVC finalizers = %v", stored.Finalizers)
	}
	rv := stored.ResourceVersion
	if _, err := os.Stat(volumeFile); err != nil {
		t.Errorf("volume file was removed while the claim is Terminating: %v", err)
	}
	if len(stops) != 0 {
		t.Errorf("volume unit was stopped while a pod uses the claim: %v", stops)
	}

	// A repeated DELETE is a no-op: same object, no new write, no stop.
	rec = deletePVCObject(t, srv, "demo", "data")
	if rec.Code != http.StatusOK {
		t.Fatalf("repeated delete status = %d, body %s", rec.Code, rec.Body.String())
	}
	repeated, err := srv.config.Store.GetPVC("demo", "data")
	if err != nil {
		t.Fatalf("repeated delete removed the PVC: %v", err)
	}
	if repeated.ResourceVersion != rv {
		t.Errorf("repeated delete changed resourceVersion from %q to %q", rv, repeated.ResourceVersion)
	}
	if !repeated.DeletionTimestamp.Equal(stored.DeletionTimestamp) {
		t.Errorf("repeated delete changed deletionTimestamp from %v to %v", stored.DeletionTimestamp, repeated.DeletionTimestamp)
	}
	if len(stops) != 0 {
		t.Errorf("repeated delete stopped units: %v", stops)
	}
}

func TestPVCProtectionFinishAfterLastPodGone(t *testing.T) {
	srv, volumeFile := pvcDeleteServer(t, "demo", "data")
	if _, err := srv.config.Store.CreatePod(runningPodUsingClaim("demo", "app", "data")); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	if rec := deletePVCObject(t, srv, "demo", "data"); rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body %s", rec.Code, rec.Body.String())
	}

	var stops []string
	srv.stopUnitHook = func(name string) { stops = append(stops, name) }

	srv.FinishTerminatingPVCs()
	if _, err := srv.config.Store.GetPVC("demo", "data"); err != nil {
		t.Fatalf("finish removed a claim that still has a user: %v", err)
	}
	if len(stops) != 0 {
		t.Errorf("finish stopped a used claim's unit: %v", stops)
	}

	if err := srv.config.Store.DeletePod("demo", "app"); err != nil {
		t.Fatalf("DeletePod: %v", err)
	}
	srv.FinishTerminatingPVCs()

	if _, err := srv.config.Store.GetPVC("demo", "data"); err == nil {
		t.Fatal("Terminating PVC was not removed after its last pod was gone")
	}
	if want := []string{"demo_data-volume.service"}; !equalStrings(stops, want) {
		t.Errorf("stopped units = %v, want %v", stops, want)
	}
	if _, err := os.Stat(volumeFile); !os.IsNotExist(err) {
		t.Errorf("volume file survived the finish, stat err=%v", err)
	}

	srv.FinishTerminatingPVCs()
	if want := []string{"demo_data-volume.service"}; !equalStrings(stops, want) {
		t.Errorf("second finish pass did work again, stopped units = %v", stops)
	}
}

func TestPVCProtectionWaitingPodDoesNotBlockFinish(t *testing.T) {
	srv, _ := pvcDeleteServer(t, "demo", "data")
	if _, err := srv.config.Store.CreatePod(runningPodUsingClaim("demo", "app", "data")); err != nil {
		t.Fatalf("CreatePod app: %v", err)
	}
	if rec := deletePVCObject(t, srv, "demo", "data"); rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body %s", rec.Code, rec.Body.String())
	}

	// A pod created after the claim became Terminating is a waiter, not a
	// user, so it must not hold the claim forever.
	postPodDirect(t, srv, pvcPod("demo", "waiter", "data"))
	waiter, err := srv.config.Store.GetPod("demo", "waiter")
	if err != nil {
		t.Fatalf("GetPod waiter: %v", err)
	}
	if !podWaitingForClaim(waiter) {
		t.Fatalf("waiter has no wait marker: %+v", waiter.Status.Conditions)
	}
	wantMessage := pvcWaitDeletingMessage("data")
	for _, c := range waiter.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Message != wantMessage {
			t.Fatalf("PodScheduled message = %q, want %q", c.Message, wantMessage)
		}
	}

	if err := srv.config.Store.DeletePod("demo", "app"); err != nil {
		t.Fatalf("DeletePod app: %v", err)
	}
	srv.FinishTerminatingPVCs()
	if _, err := srv.config.Store.GetPVC("demo", "data"); err == nil {
		t.Fatal("waiting pod blocked the Terminating PVC finish")
	}
	if _, err := srv.config.Store.GetPod("demo", "waiter"); err != nil {
		t.Fatalf("finish removed the waiting pod: %v", err)
	}
}

func runningJobUsingClaim(ns, name, claim string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{Spec: pvcPod(ns, "unused", claim).Spec},
		},
		Status: batchv1.JobStatus{Active: 1},
	}
}

func TestPVCProtectionRunningJobBlocksFinish(t *testing.T) {
	srv, _ := pvcDeleteServer(t, "demo", "data")
	job, err := srv.config.Store.CreateJob(runningJobUsingClaim("demo", "once", "data"))
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if rec := deletePVCObject(t, srv, "demo", "data"); rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body %s", rec.Code, rec.Body.String())
	}

	srv.FinishTerminatingPVCs()
	if _, err := srv.config.Store.GetPVC("demo", "data"); err != nil {
		t.Fatalf("finish removed a claim used by a running Job: %v", err)
	}

	if err := srv.config.Store.DeleteJob(job.Namespace, job.Name); err != nil {
		t.Fatalf("DeleteJob: %v", err)
	}
	srv.FinishTerminatingPVCs()
	if _, err := srv.config.Store.GetPVC("demo", "data"); err == nil {
		t.Fatal("Terminating PVC was not removed after its running Job was gone")
	}
}

func TestPVCProtectionFinishedJobDoesNotBlock(t *testing.T) {
	tests := []struct {
		name   string
		status batchv1.JobStatus
	}{
		{
			name: "complete condition",
			status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
				Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
			}}},
		},
		{
			name:   "handled by the sync loop count",
			status: batchv1.JobStatus{Succeeded: 1},
		},
		{
			name:   "failed job",
			status: batchv1.JobStatus{Failed: 1},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, volumeFile := pvcDeleteServer(t, "demo", "data")
			job := runningJobUsingClaim("demo", "once", "data")
			job.Status = tc.status
			if _, err := srv.config.Store.CreateJob(job); err != nil {
				t.Fatalf("CreateJob: %v", err)
			}
			rec := deletePVCObject(t, srv, "demo", "data")
			if rec.Code != http.StatusOK {
				t.Fatalf("delete status = %d, body %s", rec.Code, rec.Body.String())
			}
			if _, err := srv.config.Store.GetPVC("demo", "data"); err == nil {
				t.Fatal("finished Job blocked the PVC delete")
			}
			if _, err := os.Stat(volumeFile); !os.IsNotExist(err) {
				t.Errorf("volume file survived, stat err=%v", err)
			}
		})
	}
}

func TestPVCProtectionNewPodWaitsForTerminatingClaim(t *testing.T) {
	srv, _ := pvcDeleteServer(t, "demo", "data")
	if _, err := srv.config.Store.CreatePod(runningPodUsingClaim("demo", "app", "data")); err != nil {
		t.Fatalf("CreatePod app: %v", err)
	}
	if rec := deletePVCObject(t, srv, "demo", "data"); rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body %s", rec.Code, rec.Body.String())
	}

	postPodDirect(t, srv, pvcPod("demo", "late", "data"))
	pod, err := srv.config.Store.GetPod("demo", "late")
	if err != nil {
		t.Fatalf("GetPod late: %v", err)
	}
	want := pvcWaitDeletingMessage("data")
	found := false
	for _, c := range pod.Status.Conditions {
		if c.Type != corev1.PodScheduled {
			continue
		}
		found = true
		if c.Status != corev1.ConditionFalse || c.Reason != pvcWaitReason || c.Message != want {
			t.Fatalf("PodScheduled condition = %+v, want message %q", c, want)
		}
	}
	if !found {
		t.Fatalf("pod has no PodScheduled condition: %+v", pod.Status.Conditions)
	}
	if fileExists(t, filepath.Join(srv.config.QuadletDir, "demo-late.container")) {
		t.Error("pod unit was written for a Terminating claim")
	}
}

func patchPVC(t *testing.T, srv *Server, ns, name, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/namespaces/"+ns+"/persistentvolumeclaims/"+name, bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/merge-patch+json")
	srv.handlePVCs(rec, req, ns, name)
	return rec
}

func TestPVCProtectionPatchKeepsTimestampAndRemovingFinalizerDeletes(t *testing.T) {
	srv, volumeFile := pvcDeleteServer(t, "demo", "data")
	if _, err := srv.config.Store.CreatePod(runningPodUsingClaim("demo", "app", "data")); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	if rec := deletePVCObject(t, srv, "demo", "data"); rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body %s", rec.Code, rec.Body.String())
	}
	terminating, err := srv.config.Store.GetPVC("demo", "data")
	if err != nil {
		t.Fatalf("GetPVC: %v", err)
	}

	rec := patchPVC(t, srv, "demo", "data", `{"metadata":{"labels":{"app":"demo-app"}}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("label patch status = %d, body %s", rec.Code, rec.Body.String())
	}
	stored, err := srv.config.Store.GetPVC("demo", "data")
	if err != nil {
		t.Fatalf("label patch removed the PVC: %v", err)
	}
	if stored.DeletionTimestamp == nil || !stored.DeletionTimestamp.Equal(terminating.DeletionTimestamp) {
		t.Errorf("label patch changed deletionTimestamp to %v", stored.DeletionTimestamp)
	}
	if !pvcHasFinalizer(stored, pvcProtectionFinalizer) {
		t.Errorf("label patch removed the finalizer: %v", stored.Finalizers)
	}
	if stored.Labels["app"] != "demo-app" {
		t.Errorf("label patch was not stored: %v", stored.Labels)
	}

	rec = patchPVC(t, srv, "demo", "data", `{"metadata":{"finalizers":[]}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("finalizer patch status = %d, body %s", rec.Code, rec.Body.String())
	}
	if _, err := srv.config.Store.GetPVC("demo", "data"); err == nil {
		t.Fatal("removing the protection finalizer did not delete the PVC")
	}
	if _, err := os.Stat(volumeFile); !os.IsNotExist(err) {
		t.Errorf("volume file survived the forced delete, stat err=%v", err)
	}
}

func TestPVCProtectionDeleteCollectionKeepsUsedClaim(t *testing.T) {
	srv, usedFile := pvcDeleteServer(t, "demo", "data")
	spareFile := addPVCWithVolumeFile(t, srv, "demo", "spare")
	if _, err := srv.config.Store.CreatePod(runningPodUsingClaim("demo", "app", "data")); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/namespaces/demo/persistentvolumeclaims", nil)
	srv.handlePVCs(rec, req, "demo", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("deletecollection status = %d, body %s", rec.Code, rec.Body.String())
	}

	used, err := srv.config.Store.GetPVC("demo", "data")
	if err != nil {
		t.Fatalf("used PVC was deleted by deletecollection: %v", err)
	}
	if used.DeletionTimestamp == nil || !pvcHasFinalizer(used, pvcProtectionFinalizer) {
		t.Errorf("used PVC is not Terminating with the finalizer: %+v", used.ObjectMeta)
	}
	if _, err := srv.config.Store.GetPVC("demo", "spare"); err == nil {
		t.Fatal("unused PVC survived deletecollection")
	}
	if _, err := os.Stat(spareFile); !os.IsNotExist(err) {
		t.Errorf("unused PVC's volume file survived, stat err=%v", err)
	}
	if _, err := os.Stat(usedFile); err != nil {
		t.Errorf("used PVC's volume file was removed: %v", err)
	}
	if !strings.Contains(rec.Body.String(), "deleted 2 object(s)") {
		t.Errorf("deletecollection body = %s", rec.Body.String())
	}
}

func TestPVCProtectionPutWithoutFinalizerDeletes(t *testing.T) {
	srv, volumeFile := pvcDeleteServer(t, "demo", "data")
	if _, err := srv.config.Store.CreatePod(runningPodUsingClaim("demo", "app", "data")); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	if rec := deletePVCObject(t, srv, "demo", "data"); rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body %s", rec.Code, rec.Body.String())
	}
	terminating, err := srv.config.Store.GetPVC("demo", "data")
	if err != nil {
		t.Fatalf("GetPVC: %v", err)
	}
	terminating.Finalizers = nil
	body, err := json.Marshal(terminating)
	if err != nil {
		t.Fatalf("marshal PVC: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/namespaces/demo/persistentvolumeclaims/data", bytes.NewReader(body))
	srv.handlePVCs(rec, req, "demo", "data")
	if rec.Code != http.StatusOK {
		t.Fatalf("put status = %d, body %s", rec.Code, rec.Body.String())
	}
	if _, err := srv.config.Store.GetPVC("demo", "data"); err == nil {
		t.Fatal("PUT without the protection finalizer did not delete the PVC")
	}
	if _, err := os.Stat(volumeFile); !os.IsNotExist(err) {
		t.Errorf("volume file survived the forced delete, stat err=%v", err)
	}
}

func TestPVCProtectionStatusSyncKeepsTerminatingWaitMarker(t *testing.T) {
	srv, _ := pvcDeleteServer(t, "demo", "data")
	pvc, err := srv.config.Store.GetPVC("demo", "data")
	if err != nil {
		t.Fatalf("GetPVC: %v", err)
	}
	now := metav1.Now()
	pvc.DeletionTimestamp = &now
	addPVCFinalizer(pvc, pvcProtectionFinalizer)
	if _, err := srv.config.Store.UpdatePVC(pvc); err != nil {
		t.Fatalf("UpdatePVC: %v", err)
	}

	postPodDirect(t, srv, pvcPod("demo", "app", "data"))
	srv.config.Store.UpdatePodPhase("demo", "app", corev1.PodPending, "")
	stored, err := srv.config.Store.GetPod("demo", "app")
	if err != nil {
		t.Fatalf("GetPod: %v", err)
	}
	want := pvcWaitDeletingMessage("data")
	for _, c := range stored.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Message != want {
			t.Fatalf("PodScheduled message = %q, want %q", c.Message, want)
		}
	}
	if !podWaitingForClaim(stored) {
		t.Fatalf("status sync overwrote the Terminating wait marker: %+v", stored.Status.Conditions)
	}
}

// TestPVCUpdateKeepsBoundVolumeName: PATCH or PUT cannot repoint a bound
// claim at another namespace's volume.
func TestPVCUpdateKeepsBoundVolumeName(t *testing.T) {
	existing := &corev1.PersistentVolumeClaim{Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "team_db"}}
	updated := existing.DeepCopy()
	updated.Spec.VolumeName = "other_db"
	preparePVCUpdate(existing, updated)
	if updated.Spec.VolumeName != "team_db" {
		t.Fatalf("volumeName = %q, want team_db", updated.Spec.VolumeName)
	}
}

// TestPVCUpdateCannotSetVolumeNameOrClass: a hostpath claim (no volumeName)
// cannot be switched to another class or given a volume name by an update.
func TestPVCUpdateCannotSetVolumeNameOrClass(t *testing.T) {
	hostpath := "hostpath"
	existing := &corev1.PersistentVolumeClaim{Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: &hostpath}}
	updated := existing.DeepCopy()
	standard := "standard"
	updated.Spec.StorageClassName = &standard
	updated.Spec.VolumeName = "team-prod-db"
	preparePVCUpdate(existing, updated)
	if updated.Spec.VolumeName != "" {
		t.Fatalf("volumeName = %q, want empty", updated.Spec.VolumeName)
	}
	if updated.Spec.StorageClassName == nil || *updated.Spec.StorageClassName != "hostpath" {
		t.Fatalf("storageClassName changed to %v", updated.Spec.StorageClassName)
	}
}
