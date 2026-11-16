package quadlet_test

import (
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"q8s/internal/quadlet"
)

func TestJobAndCronJobContainersCarryOwnerLabels(t *testing.T) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "demo"},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "app", Image: "example.com/app:latest"}},
				},
			},
		},
	}
	out, err := quadlet.JobContainer(job.Name, job, "", nil, "")
	if err != nil {
		t.Fatalf("JobContainer: %v", err)
	}
	for _, want := range []string{
		"Label=io.kubernetes.job.name=nightly\n",
		"Label=io.kubernetes.pod.namespace=demo\n",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("JobContainer output missing %q:\n%s", want, out)
		}
	}

	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "demo"},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 * * * *",
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "app", Image: "example.com/app:latest"}},
						},
					},
				},
			},
		},
	}
	out, err = quadlet.CronContainer(cj.Name, cj, "", nil, "")
	if err != nil {
		t.Fatalf("CronContainer: %v", err)
	}
	for _, want := range []string{
		"Label=io.kubernetes.cronjob.name=nightly\n",
		"Label=io.kubernetes.pod.namespace=demo\n",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("CronContainer output missing %q:\n%s", want, out)
		}
	}
}
