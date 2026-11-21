package quadlet_test

import (
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"q8s/internal/quadlet"
)

const (
	testWorkloadDir    = "/run/q8s/workload"
	testWorkloadBinDir = "/etc/q8s/workload-bin"
)

func workloadJob() *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "job", Namespace: "default"},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "worker", Image: "busybox"}},
			},
		}},
	}
}

func workloadCronJob() *batchv1.CronJob {
	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "cron", Namespace: "default"},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 3 * * *",
			JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{
				Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "worker", Image: "busybox"}},
				}},
			}},
		},
	}
}

func containerWithWorkload(t *testing.T, pod *corev1.Pod, workload quadlet.WorkloadMount) string {
	t.Helper()
	out, err := quadlet.Container(pod.Name, pod, "", nil, nil, "", nil, workload)
	if err != nil {
		t.Fatalf("Container: %v", err)
	}
	return string(out)
}

func jobWithWorkload(t *testing.T, job *batchv1.Job, workload quadlet.WorkloadMount) string {
	t.Helper()
	out, err := quadlet.JobContainer(job.Name, job, "", nil, "", workload)
	if err != nil {
		t.Fatalf("JobContainer: %v", err)
	}
	return string(out)
}

func cronWithWorkload(t *testing.T, cj *batchv1.CronJob, workload quadlet.WorkloadMount) string {
	t.Helper()
	out, err := quadlet.CronContainer(cj.Name, cj, "", nil, "", workload)
	if err != nil {
		t.Fatalf("CronContainer: %v", err)
	}
	return string(out)
}

func assertWorkloadMount(t *testing.T, out string) {
	t.Helper()
	assertContains(t, out, "Volume="+testWorkloadDir+":/var/run/q8s/workload:ro,z\n")
	assertContains(t, out, "Volume="+testWorkloadBinDir+":/var/run/q8s/bin:ro,z\n")
	assertContains(t, out, "Environment=KUBECONFIG=/var/run/q8s/workload/kubeconfig\n")
}

func TestWorkloadMountStandalonePod(t *testing.T) {
	pod := simplePod("default", "app", "nginx:latest")
	pod.Spec.ServiceAccountName = "worker"

	out := containerWithWorkload(t, pod, quadlet.WorkloadMountFor(&pod.Spec, testWorkloadDir, testWorkloadBinDir))
	assertWorkloadMount(t, out)
}

func TestWorkloadMountDeploymentReplica(t *testing.T) {
	pod := deploymentOwnedPod(nil)
	pod.Spec.ServiceAccountName = "worker"

	out := containerWithWorkload(t, pod, quadlet.WorkloadMountFor(&pod.Spec, testWorkloadDir, testWorkloadBinDir))
	assertWorkloadMount(t, out)
}

func TestWorkloadMountMultiContainerPod(t *testing.T) {
	pod := multiPod()
	pod.Spec.ServiceAccountName = "worker"

	files, err := quadlet.PodUnits(pod.Name, pod, "", nil, nil, "", nil, quadlet.WorkloadMountFor(&pod.Spec, testWorkloadDir, testWorkloadBinDir))
	if err != nil {
		t.Fatalf("PodUnits: %v", err)
	}
	for _, c := range pod.Spec.Containers {
		filename := "default_multi_" + c.Name + ".container"
		content, ok := files[filename]
		if !ok {
			t.Fatalf("missing %s in %v", filename, files)
		}
		assertWorkloadMount(t, string(content))
	}
}

func TestWorkloadMountJobAndCronJob(t *testing.T) {
	job := workloadJob()
	job.Spec.Template.Spec.ServiceAccountName = "worker"
	assertWorkloadMount(t, jobWithWorkload(t, job, quadlet.WorkloadMountFor(&job.Spec.Template.Spec, testWorkloadDir, testWorkloadBinDir)))

	cj := workloadCronJob()
	cj.Spec.JobTemplate.Spec.Template.Spec.ServiceAccountName = "worker"
	assertWorkloadMount(t, cronWithWorkload(t, cj, quadlet.WorkloadMountFor(&cj.Spec.JobTemplate.Spec.Template.Spec, testWorkloadDir, testWorkloadBinDir)))
}

func TestWorkloadMountUsesUserNSOptions(t *testing.T) {
	restore := quadlet.SetUserNS(true)
	defer restore()

	pod := simplePod("default", "app", "nginx:latest")
	pod.Spec.ServiceAccountName = "worker"

	out := containerWithWorkload(t, pod, quadlet.WorkloadMountFor(&pod.Spec, testWorkloadDir, testWorkloadBinDir))
	assertContains(t, out, "Volume="+testWorkloadDir+":/var/run/q8s/workload:ro,z,idmap\n")
	assertContains(t, out, "Environment=KUBECONFIG=/var/run/q8s/workload/kubeconfig\n")
}

func TestWorkloadMountDisabledKeepsOldRendering(t *testing.T) {
	t.Run("empty workload dir", func(t *testing.T) {
		job := workloadJob()
		job.Spec.Template.Spec.ServiceAccountName = "worker"
		old := jobWithWorkload(t, job, quadlet.WorkloadMount{})
		got := jobWithWorkload(t, job, quadlet.WorkloadMountFor(&job.Spec.Template.Spec, "", testWorkloadBinDir))
		if got != old {
			t.Fatalf("empty WorkloadDir changed the rendering:\nold:\n%s\ngot:\n%s", old, got)
		}
		assertNotContains(t, got, "/var/run/q8s/workload")
	})

	t.Run("automount false", func(t *testing.T) {
		pod := simplePod("default", "app", "nginx:latest")
		pod.Spec.ServiceAccountName = "worker"
		pod.Spec.AutomountServiceAccountToken = boolPtr(false)
		old := containerWithWorkload(t, pod, quadlet.WorkloadMount{})
		got := containerWithWorkload(t, pod, quadlet.WorkloadMountFor(&pod.Spec, testWorkloadDir, testWorkloadBinDir))
		if got != old {
			t.Fatalf("automount false changed the rendering:\nold:\n%s\ngot:\n%s", old, got)
		}
		assertNotContains(t, got, "/var/run/q8s/workload")
	})

	t.Run("no service account", func(t *testing.T) {
		cj := workloadCronJob()
		old := cronWithWorkload(t, cj, quadlet.WorkloadMount{})
		got := cronWithWorkload(t, cj, quadlet.WorkloadMountFor(&cj.Spec.JobTemplate.Spec.Template.Spec, testWorkloadDir, testWorkloadBinDir))
		if got != old {
			t.Fatalf("pod without serviceAccountName changed the rendering:\nold:\n%s\ngot:\n%s", old, got)
		}
		assertNotContains(t, got, "/var/run/q8s/workload")
	})
}

func TestWorkloadMountKeepsContainerKubeconfig(t *testing.T) {
	pod := simplePod("default", "app", "nginx:latest")
	pod.Spec.ServiceAccountName = "worker"
	pod.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "KUBECONFIG", Value: "/custom/kubeconfig"}}

	out := containerWithWorkload(t, pod, quadlet.WorkloadMountFor(&pod.Spec, testWorkloadDir, testWorkloadBinDir))
	assertContains(t, out, "Volume="+testWorkloadDir+":/var/run/q8s/workload:ro,z\n")
	assertContains(t, out, "Environment=KUBECONFIG=/custom/kubeconfig\n")
	assertNotContains(t, out, "Environment=KUBECONFIG=/var/run/q8s/workload/kubeconfig\n")
	if got := strings.Count(out, "Environment=KUBECONFIG="); got != 1 {
		t.Fatalf("KUBECONFIG should be declared once, got %d:\n%s", got, out)
	}
}
