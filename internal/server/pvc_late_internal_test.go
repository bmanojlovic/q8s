package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"q8s/internal/store"
)

func pvcPod(ns, name, claim string) *corev1.Pod {
	return &corev1.Pod{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:         "app",
				Image:        "nginx:1.0",
				VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/data"}},
			}},
			Volumes: []corev1.Volume{{
				Name: "data",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim},
				},
			}},
		},
	}
}

func claim(name, ns string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
	}
}

func postPodDirect(t *testing.T, srv *Server, pod *corev1.Pod) {
	t.Helper()
	body, err := json.Marshal(pod)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/namespaces/"+pod.Namespace+"/pods", bytes.NewReader(body))
	srv.handlePods(rec, req, pod.Namespace, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create pod status = %d, body %s", rec.Code, rec.Body.String())
	}
}

func createClaimDirect(t *testing.T, st *store.Store, ns, name string) {
	t.Helper()
	if _, err := st.CreatePVC(claim(name, ns)); err != nil {
		t.Fatalf("CreatePVC %s/%s: %v", ns, name, err)
	}
}

func fileExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	return err == nil
}

func TestLateClaimStartsPodOnWaitPass(t *testing.T) {
	st, srv := newReplicaTestServer(t)
	var starts []string
	srv.startUnitHook = func(name string) { starts = append(starts, name) }

	postPodDirect(t, srv, pvcPod("default", "app", "data"))

	path := filepath.Join(srv.config.QuadletDir, "default_app.container")
	if fileExists(t, path) {
		t.Fatalf("pod unit was written before the claim existed")
	}
	stored, err := st.GetPod("default", "app")
	if err != nil {
		t.Fatalf("GetPod: %v", err)
	}
	if !podWaitingForClaim(stored) {
		t.Fatalf("pod does not carry the waiting marker: %+v", stored.Status.Conditions)
	}

	srv.StartPodsWaitingForClaims()
	if fileExists(t, path) || len(starts) != 0 {
		t.Fatalf("wait pass acted while the claim was still missing (starts %v)", starts)
	}

	createClaimDirect(t, st, "default", "data")
	srv.StartPodsWaitingForClaims()

	if !fileExists(t, path) {
		t.Fatalf("pod unit was not rendered after the claim appeared")
	}
	if want := []string{"default_app.service"}; !reflect.DeepEqual(starts, want) {
		t.Fatalf("starts = %v, want %v", starts, want)
	}
	stored, err = st.GetPod("default", "app")
	if err != nil {
		t.Fatalf("GetPod: %v", err)
	}
	if podWaitingForClaim(stored) {
		t.Fatalf("waiting marker was not cleared after rendering: %+v", stored.Status.Conditions)
	}

	// A second pass must not restart a pod whose marker is gone.
	srv.StartPodsWaitingForClaims()
	if want := []string{"default_app.service"}; !reflect.DeepEqual(starts, want) {
		t.Fatalf("second wait pass started units again: %v", starts)
	}
}

func TestLateClaimStartsEveryDeploymentReplica(t *testing.T) {
	st, srv := newReplicaTestServer(t)
	replicas := int32(2)
	dep, err := st.CreateDeployment(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "web"}},
				Spec:       pvcPod("default", "unused", "data").Spec,
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	var starts []string
	srv.startUnitHook = func(name string) { starts = append(starts, name) }

	srv.generateDeploymentQuadlets(dep)

	for _, name := range []string{"web-0", "web-1"} {
		if fileExists(t, filepath.Join(srv.config.QuadletDir, "default_"+name+".container")) {
			t.Fatalf("replica %s was rendered before its claim existed", name)
		}
		pod, err := st.GetPod("default", name)
		if err != nil {
			t.Fatalf("GetPod %s: %v", name, err)
		}
		if !podWaitingForClaim(pod) {
			t.Fatalf("replica %s has no waiting marker: %+v", name, pod.Status.Conditions)
		}
	}
	if len(starts) != 0 {
		t.Fatalf("replicas started without the claim: %v", starts)
	}

	createClaimDirect(t, st, "default", "data")
	srv.StartPodsWaitingForClaims()

	for _, name := range []string{"web-0", "web-1"} {
		if !fileExists(t, filepath.Join(srv.config.QuadletDir, "default_"+name+".container")) {
			t.Fatalf("replica %s was not rendered after the claim appeared", name)
		}
		pod, err := st.GetPod("default", name)
		if err != nil {
			t.Fatalf("GetPod %s: %v", name, err)
		}
		if podWaitingForClaim(pod) {
			t.Fatalf("replica %s kept its waiting marker", name)
		}
	}
	want := []string{"default_web-0.service", "default_web-1.service"}
	sort.Strings(starts)
	if !reflect.DeepEqual(starts, want) {
		t.Fatalf("starts = %v, want %v", starts, want)
	}

	srv.StartPodsWaitingForClaims()
	if !reflect.DeepEqual(starts, want) {
		t.Fatalf("second wait pass started replicas again: %v", starts)
	}
}

func TestClaimBeforePodKeepsSingleStart(t *testing.T) {
	st, srv := newReplicaTestServer(t)
	var starts []string
	srv.startUnitHook = func(name string) { starts = append(starts, name) }

	createClaimDirect(t, st, "default", "data")
	postPodDirect(t, srv, pvcPod("default", "app", "data"))

	if !fileExists(t, filepath.Join(srv.config.QuadletDir, "default_app.container")) {
		t.Fatalf("pod was not rendered when its claim already existed")
	}
	if want := []string{"default_app.service"}; !reflect.DeepEqual(starts, want) {
		t.Fatalf("starts = %v, want %v", starts, want)
	}
	srv.StartPodsWaitingForClaims()
	if !reflect.DeepEqual(starts, []string{"default_app.service"}) {
		t.Fatalf("wait pass restarted an already rendered pod: %v", starts)
	}
}

func TestMissingClaimInAnotherNamespaceDoesNotReleasePod(t *testing.T) {
	st, srv := newReplicaTestServer(t)
	var starts []string
	srv.startUnitHook = func(name string) { starts = append(starts, name) }

	createClaimDirect(t, st, "other", "data")
	postPodDirect(t, srv, pvcPod("default", "app", "data"))

	srv.StartPodsWaitingForClaims()
	if fileExists(t, filepath.Join(srv.config.QuadletDir, "default_app.container")) || len(starts) != 0 {
		t.Fatalf("a claim in another namespace released the pod (starts %v)", starts)
	}

	createClaimDirect(t, st, "default", "data")
	srv.StartPodsWaitingForClaims()
	if !fileExists(t, filepath.Join(srv.config.QuadletDir, "default_app.container")) {
		t.Fatalf("pod did not render after its own claim appeared")
	}
	if want := []string{"default_app.service"}; !reflect.DeepEqual(starts, want) {
		t.Fatalf("starts = %v, want %v", starts, want)
	}
}

func TestPodWithUnitAndClaimIsNotRestarted(t *testing.T) {
	st, srv := newReplicaTestServer(t)
	var starts []string
	srv.startUnitHook = func(name string) { starts = append(starts, name) }

	createClaimDirect(t, st, "default", "data")
	postPodDirect(t, srv, pvcPod("default", "app", "data"))

	for i := 0; i < 3; i++ {
		srv.StartPodsWaitingForClaims()
	}
	if want := []string{"default_app.service"}; !reflect.DeepEqual(starts, want) {
		t.Fatalf("pod with a unit was restarted: starts %v, want %v", starts, want)
	}
}

func TestStatusSyncKeepsClaimWaitMarker(t *testing.T) {
	st, srv := newReplicaTestServer(t)
	postPodDirect(t, srv, pvcPod("default", "app", "data"))

	st.UpdatePodPhase("default", "app", corev1.PodPending, "")
	stored, err := st.GetPod("default", "app")
	if err != nil {
		t.Fatalf("GetPod: %v", err)
	}
	if !podWaitingForClaim(stored) {
		t.Fatalf("status sync overwrote the waiting marker: %+v", stored.Status.Conditions)
	}
	for _, c := range stored.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Message != pvcWaitMessage("data") {
			t.Fatalf("PodScheduled message = %q, want %q", c.Message, pvcWaitMessage("data"))
		}
	}
}

func TestLateClaimRendersCronJobWithoutStartingTimer(t *testing.T) {
	st, srv := newReplicaTestServer(t)
	cj, err := st.CreateCronJob(&batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "export", Namespace: "default"},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 4 * * *",
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: pvcPod("default", "unused", "data").Spec}},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateCronJob: %v", err)
	}
	var starts []string
	srv.startUnitHook = func(name string) { starts = append(starts, name) }

	srv.generateCronJobQuadlets(cj)
	if fileExists(t, filepath.Join(srv.config.QuadletDir, "default_export__cron.container")) {
		t.Fatalf("cronjob container was rendered before its claim existed")
	}

	createClaimDirect(t, st, "default", "data")
	srv.StartPodsWaitingForClaims()

	if !fileExists(t, filepath.Join(srv.config.QuadletDir, "default_export__cron.container")) {
		t.Fatalf("cronjob container was not rendered after the claim appeared")
	}
	if !fileExists(t, filepath.Join(srv.config.QuadletDir, "default_export__cron.timer")) {
		t.Fatalf("cronjob timer was not rendered after the claim appeared")
	}
	if len(starts) != 0 {
		t.Fatalf("cronjob timer was started immediately: %v", starts)
	}
}

func TestLateClaimStartsJob(t *testing.T) {
	st, srv := newReplicaTestServer(t)
	job, err := st.CreateJob(&batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "once", Namespace: "default"},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{Spec: pvcPod("default", "unused", "data").Spec},
		},
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	var starts []string
	srv.startUnitHook = func(name string) { starts = append(starts, name) }

	srv.generateJobQuadlet(job)
	if fileExists(t, filepath.Join(srv.config.QuadletDir, "default_once__job.container")) {
		t.Fatalf("job container was rendered before its claim existed")
	}

	createClaimDirect(t, st, "default", "data")
	srv.StartPodsWaitingForClaims()

	if !fileExists(t, filepath.Join(srv.config.QuadletDir, "default_once__job.container")) {
		t.Fatalf("job container was not rendered after the claim appeared")
	}
	if want := []string{"default_once__job.service"}; !reflect.DeepEqual(starts, want) {
		t.Fatalf("starts = %v, want %v", starts, want)
	}
}
