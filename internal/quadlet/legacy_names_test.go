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

// TestLegacyHostMatchesCurrentHelpers is the bridge for PR 2. Every call site
// can be switched to names.Host only after names.LegacyHost is proven to
// produce exactly what the current helper produces.
func TestLegacyHostMatchesCurrentHelpers(t *testing.T) {
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
		{"podBaseName", names.LegacyHost(names.KindPod, namespace, object), podBaseName(namespace, object)},
		{"pod unit file", names.LegacyHost(names.KindPodFile, namespace, object), podBaseName(namespace, object) + ".pod"},
		{"pod service", names.LegacyHost(names.KindPodService, namespace, object), podBaseName(namespace, object) + ".service"},
		{"pod unit service", names.LegacyHost(names.KindPodUnit, namespace, object), podBaseName(namespace, object) + "-pod.service"},
		{"podContainerName standalone", names.LegacyHost(names.KindPodContainer, namespace, object, ""), podContainerName(namespace, object, "")},
		{"podContainerName member", names.LegacyHost(names.KindPodContainer, namespace, object, container), podContainerName(namespace, object, container)},
		{"standalone container file", names.LegacyHost(names.KindPodContainerFile, namespace, object, ""), podContainerName(namespace, object, "") + ".container"},
		{"member container file", names.LegacyHost(names.KindPodContainerFile, namespace, object, container), podContainerName(namespace, object, container) + ".container"},
		{"standalone container service", names.LegacyHost(names.KindPodContainerService, namespace, object, ""), podContainerName(namespace, object, "") + ".service"},
		{"member container service", names.LegacyHost(names.KindPodContainerService, namespace, object, container), podContainerName(namespace, object, container) + ".service"},
		{"DeploymentInstanceName", names.LegacyHost(names.KindDeploymentPod, "", "web", indexText), DeploymentInstanceName("web", index)},
		{"EnvFileName Pod", names.LegacyHost(names.KindEnvPod, namespace, object), EnvFileName(object)},
		{"EnvFileName Job", names.LegacyHost(names.KindEnvJob, namespace, job), EnvFileName(JobEnvBaseName(job))},
		{"EnvFileName CronJob", names.LegacyHost(names.KindEnvCronJob, namespace, cronJob), EnvFileName(CronJobEnvBaseName(cronJob))},
		{"JobContainerName", names.LegacyHost(names.KindJob, namespace, job), cronruns.JobContainerName(namespace, job)},
		{"JobUnitName", names.LegacyHost(names.KindJobService, namespace, job), cronruns.JobUnitName(namespace, job)},
		{"CronJob ContainerName", names.LegacyHost(names.KindCronJob, namespace, cronJob), cronruns.ContainerName(namespace, cronJob)},
		{"CronJob UnitName", names.LegacyHost(names.KindCronJobService, namespace, cronJob), cronruns.UnitName(namespace, cronJob)},
		{"PVCVolumeName", names.LegacyHost(names.KindPVCVolume, namespace, claim), PVCVolumeName(namespace, claim)},
		{"PVC volume file", names.LegacyHost(names.KindPVCFile, namespace, claim), PVCVolumeName(namespace, claim) + ".volume"},
		{"socket file", names.LegacyHost(names.KindSocketFile, namespace, object, entry), namespace + "-" + object + "-" + entry + ".socket"},
		{"socket service", names.LegacyHost(names.KindSocketService, namespace, object), namespace + "-" + object + ".service"},
		{"network", names.LegacyHost(names.KindNetwork, namespace, ""), "q8s-" + namespace},
		{"emptyDir volume", names.LegacyHost(names.KindEmptyDirVolume, namespace, "web", claim), EmptyDirVolumeName(namespace, "web", claim)},
		{"DeploymentReplicaVolumeOwner", names.LegacyHost(names.KindDeploymentVolumeOwner, "", "web", indexText), DeploymentReplicaVolumeOwner("web", index)},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s: names.LegacyHost = %q, current helper = %q", tc.what, tc.got, tc.want)
		}
	}
}

// TestLegacyHostMatchesRenderedQuadlets checks the table above against actual
// generator output, so a drift in how a helper is used is caught as well as a
// drift in the helper itself.
func TestLegacyHostMatchesRenderedQuadlets(t *testing.T) {
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
	assertLine("Pod", podContent, "ContainerName="+names.LegacyHost(names.KindPod, "team", "prod-db"))

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
	assertLine("Job", jobContent, "ContainerName="+names.LegacyHost(names.KindJob, "team", "batch"))

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
	assertLine("CronJob", cronContent, "ContainerName="+names.LegacyHost(names.KindCronJob, "team", "nightly"))
}
