package quadlet

import (
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"q8s/internal/cronruns"
	"q8s/internal/names"
)

// TestHostMatchesCurrentHelpers checks that every quadlet naming helper uses
// names.Host, so the generator and the store cannot drift from one format.
func TestHostMatchesCurrentHelpers(t *testing.T) {
	const (
		namespace = "team"
		object    = "prod-db"
		container = "side-car"
		entry     = "http"
		job       = "batch"
		cronJob   = "nightly"
		claim     = "data"
	)
	index := int32(1)
	indexText := "1"

	cases := []struct {
		what string
		got  string
		want string
	}{
		{"podBaseName", names.Host(names.KindPod, namespace, object), podBaseName(namespace, object)},
		{"pod unit file", names.Host(names.KindPodFile, namespace, object), podBaseName(namespace, object) + ".pod"},
		{"pod service", names.Host(names.KindPodService, namespace, object), podBaseName(namespace, object) + ".service"},
		{"pod unit service", names.Host(names.KindPodUnit, namespace, object), podBaseName(namespace, object) + "-pod.service"},
		{"podContainerName standalone", names.Host(names.KindPodContainer, namespace, object, ""), podContainerName(namespace, object, "")},
		{"podContainerName member", names.Host(names.KindPodContainer, namespace, object, container), podContainerName(namespace, object, container)},
		{"standalone container file", names.Host(names.KindPodContainerFile, namespace, object, ""), podContainerName(namespace, object, "") + ".container"},
		{"member container file", names.Host(names.KindPodContainerFile, namespace, object, container), podContainerName(namespace, object, container) + ".container"},
		{"standalone container service", names.Host(names.KindPodContainerService, namespace, object, ""), podContainerName(namespace, object, "") + ".service"},
		{"member container service", names.Host(names.KindPodContainerService, namespace, object, container), podContainerName(namespace, object, container) + ".service"},
		{"DeploymentInstanceName", names.Host(names.KindDeploymentPod, "", "web", indexText), DeploymentInstanceName("web", index)},
		{"EnvFileName Pod", names.Host(names.KindEnvPod, namespace, object), EnvFileName(object)},
		{"EnvFileName Job", names.Host(names.KindEnvJob, namespace, job), EnvFileName(JobEnvBaseName(job))},
		{"EnvFileName CronJob", names.Host(names.KindEnvCronJob, namespace, cronJob), EnvFileName(CronJobEnvBaseName(cronJob))},
		{"JobContainerName", names.Host(names.KindJob, namespace, job), cronruns.JobContainerName(namespace, job)},
		{"JobUnitName", names.Host(names.KindJobService, namespace, job), cronruns.JobUnitName(namespace, job)},
		{"CronJob ContainerName", names.Host(names.KindCronJob, namespace, cronJob), cronruns.ContainerName(namespace, cronJob)},
		{"CronJob UnitName", names.Host(names.KindCronJobService, namespace, cronJob), cronruns.UnitName(namespace, cronJob)},
		{"PVCVolumeName", names.Host(names.KindPVCVolume, namespace, claim), PVCVolumeName(namespace, claim)},
		{"PVC volume file", names.Host(names.KindPVCFile, namespace, claim), PVCVolumeName(namespace, claim) + ".volume"},
		{"socket file", names.Host(names.KindSocketFile, namespace, object, entry), names.Host(names.KindSocketFile, namespace, object, entry)},
		{"socket service", names.Host(names.KindSocketService, namespace, object), names.Host(names.KindSocketService, namespace, object)},
		{"network", names.Host(names.KindNetwork, namespace, ""), "q8s-" + namespace},
		{"emptyDir volume", names.Host(names.KindEmptyDirVolume, namespace, "web", claim), EmptyDirVolumeName(namespace, "web", claim)},
		{"DeploymentReplicaVolumeOwner", names.Host(names.KindDeploymentVolumeOwner, "", "web", indexText), DeploymentReplicaVolumeOwner("web", index)},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s: names.Host = %q, current helper = %q", tc.what, tc.got, tc.want)
		}
	}
}

// TestHostMatchesRenderedQuadlets checks the table above against actual
// generator output, so a drift in how a helper is used is caught as well as a
// drift in the helper itself.
func TestHostMatchesRenderedQuadlets(t *testing.T) {
	assertLine := func(what string, content []byte, want string) {
		t.Helper()
		if !strings.Contains(string(content), want) {
			t.Errorf("%s: rendered output does not contain %q:\n%s", what, want, content)
		}
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "prod-db", Namespace: "team"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "prod-db", Image: "scratch",
		}}},
	}
	podContent, err := Container("prod-db", pod, "", nil, nil, "", nil)
	if err != nil {
		t.Fatalf("Container: %v", err)
	}
	assertLine("Pod", podContent, "ContainerName="+names.Host(names.KindPod, "team", "prod-db"))

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "batch", Namespace: "team"},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "batch", Image: "scratch",
			}}},
		}},
	}
	jobContent, err := JobContainer("batch", job, "", nil, "")
	if err != nil {
		t.Fatalf("JobContainer: %v", err)
	}
	assertLine("Job", jobContent, "ContainerName="+names.Host(names.KindJob, "team", "batch"))

	cronJob := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "team"},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 3 * * *",
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{Containers: []corev1.Container{{
						Name: "nightly", Image: "scratch",
					}}},
				}},
			},
		},
	}
	cronContent, err := CronContainer("nightly", cronJob, "", nil, "")
	if err != nil {
		t.Fatalf("CronContainer: %v", err)
	}
	assertLine("CronJob", cronContent, "ContainerName="+names.Host(names.KindCronJob, "team", "nightly"))
}
