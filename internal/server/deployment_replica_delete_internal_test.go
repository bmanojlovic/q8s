package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"q8s/internal/store"
)

func newReplicaTestServer(t *testing.T) (*store.Store, *Server) {
	t.Helper()
	st := store.New()
	srv := &Server{config: Config{Store: st, QuadletDir: t.TempDir()}}
	return st, srv
}

func replicaDeployment(name string, replicas int32, volume string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{Spec: emptyDirSpec(volume)},
		},
	}
}

func deletePodViaAPI(t *testing.T, srv *Server, name string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/namespaces/default/pods/"+name, nil)
	srv.handlePods(rec, req, "default", name)
	return rec
}

func TestAPIDeleteDeploymentReplicaRecreatesItWithFreshEmptyDir(t *testing.T) {
	st, srv := newReplicaTestServer(t)
	created, err := st.CreateDeployment(replicaDeployment("web", 2, "cache"))
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	srv.generateDeploymentQuadlets(created)

	oldReplica, err := st.GetPod("default", "web-0")
	if err != nil {
		t.Fatalf("GetPod web-0: %v", err)
	}
	other, err := st.GetPod("default", "web-1")
	if err != nil {
		t.Fatalf("GetPod web-1: %v", err)
	}

	calls := recordPodmanCommand(t)
	var starts []string
	srv.startUnitHook = func(name string) { starts = append(starts, name) }

	rec := deletePodViaAPI(t, srv, "web-0")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body %s", rec.Code, rec.Body.String())
	}

	if got, want := volumeRmCalls(*calls), []string{"volume rm --force q8s_default_web_0_cache"}; !equalStrings(got, want) {
		t.Errorf("volume rm calls = %v, want %v", got, want)
	}
	recreated, err := st.GetPod("default", "web-0")
	if err != nil {
		t.Fatalf("replica web-0 was not recreated: %v", err)
	}
	if recreated.UID == oldReplica.UID {
		t.Fatalf("recreated replica kept the old UID %q", oldReplica.UID)
	}
	// tic-e589: the DELETE body is the deleted Pod with the OLD uid, so
	// kubectl waits for that uid and not for the recreated replica.
	var body corev1.Pod
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode delete body: %v (%s)", err, rec.Body.String())
	}
	if body.Kind != "Pod" || body.UID != oldReplica.UID || body.DeletionTimestamp == nil {
		t.Fatalf("delete body = kind %q uid %q deletionTimestamp %v, want the deleted Pod with uid %q",
			body.Kind, body.UID, body.DeletionTimestamp, oldReplica.UID)
	}
	if !hasCall(starts, "default-web-0.service") {
		t.Errorf("recreated replica was not started, starts %v", starts)
	}
	stillThere, err := st.GetPod("default", "web-1")
	if err != nil {
		t.Fatalf("other replica was removed: %v", err)
	}
	if stillThere.UID != other.UID {
		t.Errorf("other replica UID changed from %q to %q", other.UID, stillThere.UID)
	}
}

func TestCollectionDeleteRecreatesEveryDeploymentReplica(t *testing.T) {
	st, srv := newReplicaTestServer(t)
	created, err := st.CreateDeployment(replicaDeployment("web", 2, "cache"))
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	srv.generateDeploymentQuadlets(created)
	old := make(map[string]string)
	for _, name := range []string{"web-0", "web-1"} {
		pod, err := st.GetPod("default", name)
		if err != nil {
			t.Fatalf("GetPod %s: %v", name, err)
		}
		old[name] = string(pod.UID)
	}

	calls := recordPodmanCommand(t)
	var starts []string
	srv.startUnitHook = func(name string) { starts = append(starts, name) }

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/namespaces/default/pods", nil)
	srv.handlePods(rec, req, "default", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("collection delete status = %d, body %s", rec.Code, rec.Body.String())
	}

	want := []string{
		"volume rm --force q8s_default_web_0_cache",
		"volume rm --force q8s_default_web_1_cache",
	}
	if got := volumeRmCalls(*calls); !equalStrings(got, want) {
		t.Errorf("volume rm calls = %v, want %v", got, want)
	}
	for _, name := range []string{"web-0", "web-1"} {
		pod, err := st.GetPod("default", name)
		if err != nil {
			t.Fatalf("replica %s was not recreated: %v", name, err)
		}
		if string(pod.UID) == old[name] {
			t.Errorf("replica %s kept the old UID %q", name, old[name])
		}
		if !hasCall(starts, "default-"+name+".service") {
			t.Errorf("replica %s was not started, starts %v", name, starts)
		}
	}
}

func TestScaleDownRemovesDroppedReplicaVolumes(t *testing.T) {
	st, srv := newReplicaTestServer(t)
	created, err := st.CreateDeployment(replicaDeployment("web", 3, "cache"))
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	srv.generateDeploymentQuadlets(created)
	calls := recordPodmanCommand(t)

	srv.scaleDeployment(created, 3, 1)

	want := []string{
		"volume rm --force q8s_default_web_1_cache",
		"volume rm --force q8s_default_web_2_cache",
	}
	if got := volumeRmCalls(*calls); !equalStrings(got, want) {
		t.Errorf("volume rm calls = %v, want %v", got, want)
	}
	if _, err := st.GetPod("default", "web-0"); err != nil {
		t.Fatalf("kept replica web-0 is gone: %v", err)
	}
	for _, name := range []string{"web-1", "web-2"} {
		if _, err := st.GetPod("default", name); err == nil {
			t.Errorf("scaled-down replica %s is still in the store", name)
		}
	}
}

func TestDeleteDeploymentRemovesEveryReplicaVolumeAndDoesNotRecreate(t *testing.T) {
	st, srv := newReplicaTestServer(t)
	created, err := st.CreateDeployment(replicaDeployment("web", 2, "cache"))
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	srv.generateDeploymentQuadlets(created)
	extra := srv.materializeDeploymentInstance(created, 5)
	if _, err := st.CreatePod(extra); err != nil {
		t.Fatalf("CreatePod web-5: %v", err)
	}

	calls := recordPodmanCommand(t)
	var starts []string
	srv.startUnitHook = func(name string) { starts = append(starts, name) }

	if err := srv.deleteDeploymentByName("default", "web"); err != nil {
		t.Fatalf("deleteDeploymentByName: %v", err)
	}

	want := []string{
		"volume rm --force q8s_default_web_0_cache",
		"volume rm --force q8s_default_web_1_cache",
		"volume rm --force q8s_default_web_5_cache",
	}
	if got := volumeRmCalls(*calls); !equalStrings(got, want) {
		t.Errorf("volume rm calls = %v, want %v", got, want)
	}
	if len(starts) != 0 {
		t.Errorf("Deployment delete started units: %v", starts)
	}
	if _, err := st.GetDeployment("default", "web"); err == nil {
		t.Fatal("deleted Deployment is still in the store")
	}
	for _, name := range []string{"web-0", "web-1"} {
		if _, err := st.GetPod("default", name); err == nil {
			t.Errorf("deleted Deployment replica %s is still in the store", name)
		}
	}
}

func TestNamespaceDeleteDoesNotRecreateDeploymentReplica(t *testing.T) {
	st, srv := newReplicaTestServer(t)
	created, err := st.CreateDeployment(replicaDeployment("web", 1, "cache"))
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	srv.generateDeploymentQuadlets(created)
	calls := recordPodmanCommand(t, "q8s_default_web_0_cache")
	var starts []string
	srv.startUnitHook = func(name string) { starts = append(starts, name) }

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/namespaces/default", nil)
	srv.handleNamespaceDelete(rec, req, "default")
	if rec.Code != http.StatusOK {
		t.Fatalf("namespace delete status = %d, body %s", rec.Code, rec.Body.String())
	}

	if got, want := volumeRmCalls(*calls), []string{"volume rm --force q8s_default_web_0_cache"}; !equalStrings(got, want) {
		t.Errorf("volume rm calls = %v, want %v", got, want)
	}
	if len(starts) != 0 {
		t.Errorf("namespace delete started units: %v", starts)
	}
	if _, err := st.GetPod("default", "web-0"); err == nil {
		t.Fatal("namespace delete recreated the Deployment replica")
	}
}

func TestDeletedReplicaIsNotRecreatedWithoutLiveDeploymentOrWantedIndex(t *testing.T) {
	t.Run("deployment gone", func(t *testing.T) {
		st, srv := newReplicaTestServer(t)
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "gone-0",
				Namespace: "default",
				OwnerReferences: []metav1.OwnerReference{{
					Kind: "Deployment",
					Name: "gone",
				}},
			},
			Spec: emptyDirSpec("cache"),
		}
		if _, err := st.CreatePod(pod); err != nil {
			t.Fatalf("CreatePod: %v", err)
		}
		calls := recordPodmanCommand(t)
		var starts []string
		srv.startUnitHook = func(name string) { starts = append(starts, name) }

		if rec := deletePodViaAPI(t, srv, "gone-0"); rec.Code != http.StatusOK {
			t.Fatalf("delete status = %d, body %s", rec.Code, rec.Body.String())
		}
		if _, err := st.GetPod("default", "gone-0"); err == nil {
			t.Fatal("pod whose Deployment is gone was recreated")
		}
		if len(starts) != 0 {
			t.Errorf("delete started units: %v", starts)
		}
		if got, want := volumeRmCalls(*calls), []string{"volume rm --force q8s_default_gone_0_cache"}; !equalStrings(got, want) {
			t.Errorf("volume rm calls = %v, want %v", got, want)
		}
	})

	t.Run("index outside spec.replicas", func(t *testing.T) {
		st, srv := newReplicaTestServer(t)
		if _, err := st.CreateDeployment(replicaDeployment("web", 1, "cache")); err != nil {
			t.Fatalf("CreateDeployment: %v", err)
		}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "web-5",
				Namespace: "default",
				OwnerReferences: []metav1.OwnerReference{{
					Kind: "Deployment",
					Name: "web",
				}},
			},
			Spec: emptyDirSpec("cache"),
		}
		if _, err := st.CreatePod(pod); err != nil {
			t.Fatalf("CreatePod: %v", err)
		}
		calls := recordPodmanCommand(t)
		var starts []string
		srv.startUnitHook = func(name string) { starts = append(starts, name) }

		if rec := deletePodViaAPI(t, srv, "web-5"); rec.Code != http.StatusOK {
			t.Fatalf("delete status = %d, body %s", rec.Code, rec.Body.String())
		}
		if _, err := st.GetPod("default", "web-5"); err == nil {
			t.Fatal("pod at an index outside spec.replicas was recreated")
		}
		for _, call := range starts {
			if strings.Contains(call, "web-") {
				t.Errorf("out-of-range replica started a unit: %s", call)
			}
		}
		if got, want := volumeRmCalls(*calls), []string{"volume rm --force q8s_default_web_5_cache"}; !equalStrings(got, want) {
			t.Errorf("volume rm calls = %v, want %v", got, want)
		}
	})
}
