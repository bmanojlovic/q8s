package quadlet_test

import (
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"q8s/internal/quadlet"
)

// generatedNameSet indexes GeneratedNames by both the host name and the
// collision key. A drift assertion only needs the host name.
func generatedNameSet(t *testing.T, obj interface{}) map[string]quadlet.GeneratedName {
	t.Helper()
	set := make(map[string]quadlet.GeneratedName)
	for _, name := range quadlet.GeneratedNames(obj) {
		set[name.Name] = name
	}
	return set
}

func wantGeneratedName(t *testing.T, set map[string]quadlet.GeneratedName, name string) {
	t.Helper()
	if _, ok := set[name]; !ok {
		t.Fatalf("generated name %q is missing from the set %v", name, set)
	}
}

// containerNames returns every ContainerName= value in a rendered quadlet.
func containerNames(t *testing.T, content []byte) []string {
	t.Helper()
	var names []string
	for _, line := range strings.Split(string(content), "\n") {
		if value, ok := strings.CutPrefix(line, "ContainerName="); ok {
			names = append(names, value)
		}
	}
	return names
}

// emptyDirVolumeNames returns the q8s-managed volume names in Volume= lines.
// A bind mount (host path, ConfigMap, Secret) and a PVC volume are not part
// of the workload's generated emptyDir names.
func emptyDirVolumeNames(t *testing.T, content []byte) []string {
	t.Helper()
	var names []string
	for _, line := range strings.Split(string(content), "\n") {
		value, ok := strings.CutPrefix(line, "Volume=")
		if !ok {
			continue
		}
		name, _, _ := strings.Cut(value, ":")
		if strings.HasPrefix(name, "q8s_") {
			names = append(names, name)
		}
	}
	return names
}

func namingPod(ns, name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:         "app",
				Image:        "img",
				VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/data"}},
			}},
			Volumes: []corev1.Volume{{
				Name:         "data",
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			}},
		},
	}
}

// TestGeneratedNamesMatchGenerator is the drift guard: it renders real
// quadlets and asserts every container, systemd unit and emptyDir volume name
// they produce is in GeneratedNames. If the generator starts using a new
// suffix or separator, this fails before the store guard can miss it.
func TestGeneratedNamesMatchGenerator(t *testing.T) {
	t.Run("single container pod", func(t *testing.T) {
		pod := namingPod("team", "web")
		set := generatedNameSet(t, pod)
		content, err := quadlet.Container("web", pod, "", nil, nil, "", nil)
		if err != nil {
			t.Fatalf("Container: %v", err)
		}
		for _, name := range containerNames(t, content) {
			wantGeneratedName(t, set, name)
		}
		wantGeneratedName(t, set, "team_web.service")
		for _, name := range emptyDirVolumeNames(t, content) {
			wantGeneratedName(t, set, name)
		}
		wantGeneratedName(t, set, "web.env")
	})

	t.Run("multi container pod", func(t *testing.T) {
		pod := namingPod("team", "web")
		pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{
			Name:  "side",
			Image: "img",
			VolumeMounts: []corev1.VolumeMount{{
				Name: "data", MountPath: "/data",
			}},
		})
		set := generatedNameSet(t, pod)
		files, err := quadlet.PodUnits("web", pod, "", nil, nil, "", nil)
		if err != nil {
			t.Fatalf("PodUnits: %v", err)
		}
		for filename, content := range files {
			switch {
			case strings.HasSuffix(filename, ".pod"):
				base := strings.TrimSuffix(filename, ".pod")
				wantGeneratedName(t, set, filename)
				wantGeneratedName(t, set, base+"-pod.service")
			case strings.HasSuffix(filename, ".container"):
				wantGeneratedName(t, set, strings.TrimSuffix(filename, ".container")+".service")
			default:
				t.Fatalf("unexpected generated file %q", filename)
			}
			for _, name := range containerNames(t, content) {
				wantGeneratedName(t, set, name)
			}
			for _, name := range emptyDirVolumeNames(t, content) {
				wantGeneratedName(t, set, name)
			}
		}
		wantGeneratedName(t, set, "web.env")
	})

	t.Run("job", func(t *testing.T) {
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "batch", Namespace: "team"},
			Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{
				Spec: namingPod("team", "batch").Spec,
			}},
		}
		set := generatedNameSet(t, job)
		content, err := quadlet.JobContainer("batch", job, "", nil, "")
		if err != nil {
			t.Fatalf("JobContainer: %v", err)
		}
		for _, name := range containerNames(t, content) {
			wantGeneratedName(t, set, name)
		}
		wantGeneratedName(t, set, "team_batch__job.service")
		for _, name := range emptyDirVolumeNames(t, content) {
			wantGeneratedName(t, set, name)
		}
		wantGeneratedName(t, set, "batch__job.env")
	})

	t.Run("cronjob", func(t *testing.T) {
		cj := &batchv1.CronJob{
			ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "team"},
			Spec: batchv1.CronJobSpec{
				Schedule: "* * * * *",
				JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{Spec: namingPod("team", "nightly").Spec},
				}},
			},
		}
		set := generatedNameSet(t, cj)
		content, err := quadlet.CronContainer("nightly", cj, "", nil, "")
		if err != nil {
			t.Fatalf("CronContainer: %v", err)
		}
		for _, name := range containerNames(t, content) {
			wantGeneratedName(t, set, name)
		}
		wantGeneratedName(t, set, "team_nightly__cron.service")
		wantGeneratedName(t, set, "team_nightly__cron.timer")
		for _, name := range emptyDirVolumeNames(t, content) {
			wantGeneratedName(t, set, name)
		}
		wantGeneratedName(t, set, "nightly__cron.env")
	})
}

// TestGeneratedNamesKindSuffixesDiffer: a Job x and a Pod x-job (likewise a
// CronJob x and a Pod x-cron) must not share a container, unit or env file.
func TestGeneratedNamesKindSuffixesDiffer(t *testing.T) {
	simplePod := func(name string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team"},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "app", Image: "img",
			}}},
		}
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "team"},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "img"}}},
		}},
	}
	cronJob := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "team"},
		Spec: batchv1.CronJobSpec{
			Schedule: "* * * * *",
			JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "img"}}},
				},
			}},
		},
	}
	for _, tc := range []struct {
		name string
		a    interface{}
		b    interface{}
	}{
		{"job and pod x-job", job, simplePod("x-job")},
		{"cronjob and pod x-cron", cronJob, simplePod("x-cron")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := generatedNameSet(t, tc.a)
			b := generatedNameSet(t, tc.b)
			for name := range a {
				if _, ok := b[name]; ok {
					t.Errorf("%s and %s both generate %q", tc.name, "the other object", name)
				}
			}
		})
	}
}
