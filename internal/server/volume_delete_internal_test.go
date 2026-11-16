package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"q8s/internal/store"
)

// recordPodmanCommand replaces the podman seam and returns the recorded
// calls. volumeList is the stdout of `podman volume ls --format {{.Name}}`;
// every other invocation produces no output.
func recordPodmanCommand(t *testing.T, volumeList ...string) *[]string {
	t.Helper()
	calls := &[]string{}
	old := podmanCommand
	podmanCommand = func(ctx context.Context, args ...string) ([]byte, error) {
		*calls = append(*calls, strings.Join(args, " "))
		if len(args) >= 2 && args[0] == "volume" && args[1] == "ls" {
			if len(volumeList) == 0 {
				return nil, nil
			}
			return []byte(strings.Join(volumeList, "\n") + "\n"), nil
		}
		return nil, nil
	}
	t.Cleanup(func() { podmanCommand = old })
	return calls
}

func volumeRmCalls(calls []string) []string {
	var got []string
	for _, call := range calls {
		if strings.HasPrefix(call, "volume rm ") {
			got = append(got, call)
		}
	}
	return got
}

func emptyDirSpec(volume string) corev1.PodSpec {
	return corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:  "app",
			Image: "nginx",
			VolumeMounts: []corev1.VolumeMount{{
				Name:      volume,
				MountPath: "/data",
			}},
		}},
		Volumes: []corev1.Volume{{
			Name:         volume,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		}},
	}
}

func TestDeleteBarePodRemovesItsEmptyDirVolumes(t *testing.T) {
	st := store.New()
	srv := &Server{config: Config{Store: st}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "solo", Namespace: "default"},
		Spec:       emptyDirSpec("data"),
	}
	if _, err := st.CreatePod(pod); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	calls := recordPodmanCommand(t)

	if err := srv.deletePodByName("default", "solo"); err != nil {
		t.Fatalf("deletePodByName: %v", err)
	}

	want := []string{"volume rm --force q8s_default_solo_data"}
	if got := volumeRmCalls(*calls); !equalStrings(got, want) {
		t.Errorf("volume rm calls = %v, want %v", got, want)
	}
	if _, err := st.GetPod("default", "solo"); err == nil {
		t.Fatal("deleted pod is still in the store")
	}
}

func TestDeleteDeploymentRemovesTemplateEmptyDirVolumesOnce(t *testing.T) {
	st := store.New()
	srv := &Server{config: Config{Store: st}}
	replicas := int32(2)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{Spec: emptyDirSpec("cache")},
		},
	}
	if _, err := st.CreateDeployment(dep); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	calls := recordPodmanCommand(t)

	if err := srv.deleteDeploymentByName("default", "web"); err != nil {
		t.Fatalf("deleteDeploymentByName: %v", err)
	}

	want := []string{"volume rm --force q8s_default_web_cache"}
	if got := volumeRmCalls(*calls); !equalStrings(got, want) {
		t.Errorf("volume rm calls = %v, want %v (one shared template volume)", got, want)
	}
}

func TestDeleteJobRemovesTemplateEmptyDirVolumes(t *testing.T) {
	st := store.New()
	srv := &Server{config: Config{Store: st}}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "pipeline", Namespace: "default"},
		Spec:       batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: emptyDirSpec("scratch")}},
	}
	if _, err := st.CreateJob(job); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	calls := recordPodmanCommand(t)

	if err := srv.deleteJobByName("default", "pipeline"); err != nil {
		t.Fatalf("deleteJobByName: %v", err)
	}

	want := []string{"volume rm --force q8s_default_pipeline_scratch"}
	if got := volumeRmCalls(*calls); !equalStrings(got, want) {
		t.Errorf("volume rm calls = %v, want %v", got, want)
	}
}

func TestDeleteCronJobRemovesTemplateEmptyDirVolumes(t *testing.T) {
	st := store.New()
	srv := &Server{config: Config{Store: st}}
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "default"},
		Spec: batchv1.CronJobSpec{
			Schedule:    "0 2 * * *",
			JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: emptyDirSpec("work")}}},
		},
	}
	if _, err := st.CreateCronJob(cj); err != nil {
		t.Fatalf("CreateCronJob: %v", err)
	}
	calls := recordPodmanCommand(t)

	if err := srv.deleteCronJobByName("default", "nightly"); err != nil {
		t.Fatalf("deleteCronJobByName: %v", err)
	}

	want := []string{"volume rm --force q8s_default_nightly_work"}
	if got := volumeRmCalls(*calls); !equalStrings(got, want) {
		t.Errorf("volume rm calls = %v, want %v", got, want)
	}
}

func TestDeleteDeploymentReplicaPodKeepsSharedEmptyDirVolume(t *testing.T) {
	st := store.New()
	srv := &Server{config: Config{Store: st}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-0",
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

	if err := srv.deletePodByName("default", "web-0"); err != nil {
		t.Fatalf("deletePodByName: %v", err)
	}

	if got := volumeRmCalls(*calls); len(got) != 0 {
		t.Errorf("deleting one replica removed shared volumes: %v", got)
	}
}

func TestDryRunPodDeleteRemovesNoVolumes(t *testing.T) {
	st := store.New()
	srv := &Server{config: Config{Store: st}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "solo", Namespace: "default"},
		Spec:       emptyDirSpec("data"),
	}
	if _, err := st.CreatePod(pod); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	calls := recordPodmanCommand(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/namespaces/default/pods/solo?dryRun=All", nil)
	srv.handlePods(rec, req, "default", "solo")
	if rec.Code != http.StatusOK {
		t.Fatalf("dry-run delete status = %d, body %s", rec.Code, rec.Body.String())
	}

	if got := volumeRmCalls(*calls); len(got) != 0 {
		t.Errorf("dry-run delete removed volumes: %v", got)
	}
	if _, err := st.GetPod("default", "solo"); err != nil {
		t.Fatalf("dry-run delete removed the pod: %v", err)
	}
}

func TestNamespaceDeleteRemovesOnlyItsEmptyDirVolumes(t *testing.T) {
	st := store.New()
	srv := &Server{config: Config{Store: st}}
	calls := recordPodmanCommand(t,
		"q8s_default_solo_data",
		"q8s_default_web_cache",
		"q8s_defaultx_solo_data",
		"q8s_other_solo_data",
		"default-keep",
	)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/namespaces/default", nil)
	srv.handleNamespaceDelete(rec, req, "default")
	if rec.Code != http.StatusOK {
		t.Fatalf("namespace delete status = %d, body %s", rec.Code, rec.Body.String())
	}

	want := []string{
		"volume rm --force q8s_default_solo_data",
		"volume rm --force q8s_default_web_cache",
	}
	if got := volumeRmCalls(*calls); !equalStrings(got, want) {
		t.Errorf("volume rm calls = %v, want %v", got, want)
	}
	for _, call := range *calls {
		if strings.Contains(call, "q8s_defaultx_") || strings.Contains(call, "q8s_other_") {
			t.Errorf("namespace delete touched another namespace's volume: %s", call)
		}
		if call == "volume rm --force default-keep" {
			t.Errorf("namespace delete removed a PVC volume: %s", call)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestDryRunDeploymentDeleteRemovesNoVolumes(t *testing.T) {
	st := store.New()
	srv := &Server{config: Config{Store: st}}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec:       appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: emptyDirSpec("cache")}},
	}
	if _, err := st.CreateDeployment(dep); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	calls := recordPodmanCommand(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/apis/apps/v1/namespaces/default/deployments/web?dryRun=All", nil)
	srv.handleDeployments(rec, req, "default", "web")
	if rec.Code != http.StatusOK {
		t.Fatalf("dry-run delete status = %d, body %s", rec.Code, rec.Body.String())
	}

	if got := volumeRmCalls(*calls); len(got) != 0 {
		t.Errorf("dry-run delete removed volumes: %v", got)
	}
	if _, err := st.GetDeployment("default", "web"); err != nil {
		t.Fatalf("dry-run delete removed the deployment: %v", err)
	}
}

func TestDryRunNamespaceDeleteRemovesNoVolumes(t *testing.T) {
	st := store.New()
	srv := &Server{config: Config{Store: st}}
	calls := recordPodmanCommand(t, "q8s_default_solo_data")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/namespaces/default?dryRun=All", nil)
	srv.handleNamespaceDelete(rec, req, "default")
	if rec.Code != http.StatusOK {
		t.Fatalf("dry-run delete status = %d, body %s", rec.Code, rec.Body.String())
	}

	if got := volumeRmCalls(*calls); len(got) != 0 {
		t.Errorf("dry-run delete removed volumes: %v", got)
	}
	if _, err := st.GetNamespace("default"); err != nil {
		t.Fatalf("dry-run delete removed the namespace: %v", err)
	}
}
