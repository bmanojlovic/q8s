package store_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"q8s/internal/store"
)

func generatedNamePod(ns, name string, containers ...corev1.Container) *corev1.Pod {
	return &corev1.Pod{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       corev1.PodSpec{Containers: containers},
	}
}

func generatedNameContainer(name, image string) corev1.Container {
	return corev1.Container{Name: name, Image: image}
}

func generatedNameDeployment(ns, name string, replicas int32, containers ...corev1.Container) *appsv1.Deployment {
	return &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: containers}},
		},
	}
}

func generatedNameJob(ns, name string, volumes []corev1.Volume, mounts []corev1.VolumeMount) *batchv1.Job {
	return &batchv1.Job{
		TypeMeta:   metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:         "c",
					Image:        "img",
					VolumeMounts: mounts,
				}},
				Volumes: volumes,
			}},
		},
	}
}

func generatedNameCronJob(ns, name string) *batchv1.CronJob {
	return &batchv1.CronJob{
		TypeMeta:   metav1.TypeMeta{APIVersion: "batch/v1", Kind: "CronJob"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: batchv1.CronJobSpec{
			Schedule: "* * * * *",
			JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{
				Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "c", Image: "img"}},
				}},
			}},
		},
	}
}

func wantGeneratedNameConflict(t *testing.T, err error, name string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a generated name conflict for %q, got nil", name)
	}
	if !errors.Is(err, store.ErrGeneratedNameConflict) {
		t.Fatalf("error = %v, want it to wrap store.ErrGeneratedNameConflict", err)
	}
	if !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "would share the generated name") {
		t.Fatalf("error = %v, want it to name %q and explain the conflict", err, name)
	}
}

// Gap 1: a Job's implicit -job suffix and a CronJob's -cron suffix create the
// same container, unit and EnvironmentFile names as a Pod that carries the
// suffix in its own name.
func TestGeneratedNamesJobAndCronJobSuffixes(t *testing.T) {
	t.Run("job then pod", func(t *testing.T) {
		st := newStore(t)
		if _, err := st.CreateJob(generatedNameJob("default", "x", nil, nil)); err != nil {
			t.Fatalf("CreateJob: %v", err)
		}
		_, err := st.CreatePod(generatedNamePod("default", "x-job", generatedNameContainer("c", "img")))
		wantGeneratedNameConflict(t, err, "default-x-job")
	})
	t.Run("pod then job", func(t *testing.T) {
		st := newStore(t)
		if _, err := st.CreatePod(generatedNamePod("default", "x-job", generatedNameContainer("c", "img"))); err != nil {
			t.Fatalf("CreatePod: %v", err)
		}
		_, err := st.CreateJob(generatedNameJob("default", "x", nil, nil))
		wantGeneratedNameConflict(t, err, "default-x-job")
	})
	t.Run("cronjob then pod", func(t *testing.T) {
		st := newStore(t)
		if _, err := st.CreateCronJob(generatedNameCronJob("default", "x")); err != nil {
			t.Fatalf("CreateCronJob: %v", err)
		}
		_, err := st.CreatePod(generatedNamePod("default", "x-cron", generatedNameContainer("c", "img")))
		wantGeneratedNameConflict(t, err, "default-x-cron")
	})
	t.Run("pod then cronjob", func(t *testing.T) {
		st := newStore(t)
		if _, err := st.CreatePod(generatedNamePod("default", "x-cron", generatedNameContainer("c", "img"))); err != nil {
			t.Fatalf("CreatePod: %v", err)
		}
		_, err := st.CreateCronJob(generatedNameCronJob("default", "x"))
		wantGeneratedNameConflict(t, err, "default-x-cron")
	})
}

// Gap 2: multi-container member names are <namespace>-<pod>-<container>, so
// hyphens make two different pod/container splits share one container name.
func TestGeneratedNamesMultiContainerMembers(t *testing.T) {
	t.Run("pod a container b-c vs pod a-b container c", func(t *testing.T) {
		st := newStore(t)
		if _, err := st.CreatePod(generatedNamePod("default", "a",
			generatedNameContainer("b-c", "img"),
			generatedNameContainer("side", "img"),
		)); err != nil {
			t.Fatalf("CreatePod a: %v", err)
		}
		_, err := st.CreatePod(generatedNamePod("default", "a-b",
			generatedNameContainer("c", "img"),
			generatedNameContainer("side", "img"),
		))
		wantGeneratedNameConflict(t, err, "default-a-b-c")
	})
	t.Run("single container pod a-b-c vs multi container pod a", func(t *testing.T) {
		st := newStore(t)
		if _, err := st.CreatePod(generatedNamePod("default", "a",
			generatedNameContainer("b-c", "img"),
			generatedNameContainer("side", "img"),
		)); err != nil {
			t.Fatalf("CreatePod a: %v", err)
		}
		_, err := st.CreatePod(generatedNamePod("default", "a-b-c", generatedNameContainer("c", "img")))
		wantGeneratedNameConflict(t, err, "default-a-b-c")
	})
}

// Gap 3: Deployment replicas are stored as Pods named <deployment>-<index>,
// and scaling up adds those names.
func TestGeneratedNamesDeploymentReplicas(t *testing.T) {
	t.Run("create pod at replica name", func(t *testing.T) {
		st := newStore(t)
		if _, err := st.CreateDeployment(generatedNameDeployment("default", "web", 2, generatedNameContainer("app", "img"))); err != nil {
			t.Fatalf("CreateDeployment: %v", err)
		}
		_, err := st.CreatePod(generatedNamePod("default", "web-1", generatedNameContainer("app", "img")))
		wantGeneratedNameConflict(t, err, "default-web-1")
	})
	t.Run("scale up into an existing pod name", func(t *testing.T) {
		st := newStore(t)
		if _, err := st.CreateDeployment(generatedNameDeployment("default", "web", 1, generatedNameContainer("app", "img"))); err != nil {
			t.Fatalf("CreateDeployment: %v", err)
		}
		if _, err := st.CreatePod(generatedNamePod("default", "web-1", generatedNameContainer("app", "img"))); err != nil {
			t.Fatalf("CreatePod web-1: %v", err)
		}
		dep, err := st.GetDeployment("default", "web")
		if err != nil {
			t.Fatalf("GetDeployment: %v", err)
		}
		two := int32(2)
		dep.Spec.Replicas = &two
		_, err = st.UpdateDeployment(dep)
		wantGeneratedNameConflict(t, err, "default-web-1")
	})
	t.Run("scale down is allowed", func(t *testing.T) {
		st := newStore(t)
		if _, err := st.CreateDeployment(generatedNameDeployment("default", "web", 2, generatedNameContainer("app", "img"))); err != nil {
			t.Fatalf("CreateDeployment: %v", err)
		}
		dep, err := st.GetDeployment("default", "web")
		if err != nil {
			t.Fatalf("GetDeployment: %v", err)
		}
		one := int32(1)
		dep.Spec.Replicas = &one
		if _, err := st.UpdateDeployment(dep); err != nil {
			t.Fatalf("scale down: %v", err)
		}
	})
}

// Gap 4: a bare Pod and a Job with the same name share the emptyDir volume
// name q8s_<ns>_<owner>_<volume>.
func TestGeneratedNamesEmptyDirVolume(t *testing.T) {
	volumes := []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
	mounts := []corev1.VolumeMount{{Name: "data", MountPath: "/data"}}

	t.Run("pod and job share volume", func(t *testing.T) {
		st := newStore(t)
		pod := generatedNamePod("default", "web", generatedNameContainer("app", "img"))
		pod.Spec.Volumes = volumes
		pod.Spec.Containers[0].VolumeMounts = mounts
		if _, err := st.CreatePod(pod); err != nil {
			t.Fatalf("CreatePod: %v", err)
		}
		_, err := st.CreateJob(generatedNameJob("default", "web", volumes, mounts))
		wantGeneratedNameConflict(t, err, "q8s_default_web_data")
	})
	t.Run("distinct volume names are fine", func(t *testing.T) {
		st := newStore(t)
		pod := generatedNamePod("default", "web", generatedNameContainer("app", "img"))
		pod.Spec.Volumes = volumes
		pod.Spec.Containers[0].VolumeMounts = mounts
		if _, err := st.CreatePod(pod); err != nil {
			t.Fatalf("CreatePod: %v", err)
		}
		otherVolumes := []corev1.Volume{{Name: "cache", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
		otherMounts := []corev1.VolumeMount{{Name: "cache", MountPath: "/cache"}}
		if _, err := st.CreateJob(generatedNameJob("default", "web", otherVolumes, otherMounts)); err != nil {
			t.Fatalf("CreateJob with distinct volume: %v", err)
		}
	})
}

// Gap 5: suffixes also collide across namespaces when the hyphen moves from
// the namespace to the name.
func TestGeneratedNamesAcrossNamespacesWithSuffix(t *testing.T) {
	st := newStore(t)
	if _, err := st.CreateJob(generatedNameJob("a", "b", nil, nil)); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	_, err := st.CreatePod(generatedNamePod("a-b", "job", generatedNameContainer("c", "img")))
	wantGeneratedNameConflict(t, err, "a-b-job")
}

// An update is checked for the names it adds, not for every name it keeps.
func TestGeneratedNamesUpdateAddsNames(t *testing.T) {
	t.Run("pod gains a colliding member", func(t *testing.T) {
		st := newStore(t)
		if _, err := st.CreatePod(generatedNamePod("default", "a", generatedNameContainer("old", "img"))); err != nil {
			t.Fatalf("CreatePod a: %v", err)
		}
		if _, err := st.CreatePod(generatedNamePod("default", "a-b-c", generatedNameContainer("c", "img"))); err != nil {
			t.Fatalf("CreatePod a-b-c: %v", err)
		}
		pod, err := st.GetPod("default", "a")
		if err != nil {
			t.Fatalf("GetPod: %v", err)
		}
		pod.Spec.Containers = []corev1.Container{
			generatedNameContainer("b-c", "img"),
			generatedNameContainer("side", "img"),
		}
		_, err = st.UpdatePod(pod)
		wantGeneratedNameConflict(t, err, "default-a-b-c")
	})
	t.Run("job gains a colliding volume", func(t *testing.T) {
		st := newStore(t)
		volumes := []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
		mounts := []corev1.VolumeMount{{Name: "data", MountPath: "/data"}}
		pod := generatedNamePod("default", "x", generatedNameContainer("app", "img"))
		pod.Spec.Volumes = volumes
		pod.Spec.Containers[0].VolumeMounts = mounts
		if _, err := st.CreatePod(pod); err != nil {
			t.Fatalf("CreatePod x: %v", err)
		}
		if _, err := st.CreateJob(generatedNameJob("default", "x", nil, nil)); err != nil {
			t.Fatalf("CreateJob x: %v", err)
		}
		job, err := st.GetJob("default", "x")
		if err != nil {
			t.Fatalf("GetJob: %v", err)
		}
		job.Spec.Template.Spec.Volumes = volumes
		job.Spec.Template.Spec.Containers[0].VolumeMounts = mounts
		_, err = st.UpdateJob(job)
		wantGeneratedNameConflict(t, err, "q8s_default_x_data")
	})
}

// A legacy store may already contain a collision. An update that adds no new
// generated name must still be accepted, so operators can edit labels or
// scale a colliding Deployment down.
func TestGeneratedNamesUpdateOfLegacyCollisionIsAllowed(t *testing.T) {
	pod := generatedNamePod("default", "web-1", generatedNameContainer("app", "img"))
	dep := generatedNameDeployment("default", "web", 2, generatedNameContainer("app", "img"))
	pods, err := json.Marshal([]interface{}{pod})
	if err != nil {
		t.Fatal(err)
	}
	deps, err := json.Marshal([]interface{}{dep})
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"resourceVersion":7,"pods":` + string(pods) + `,"deployments":` + string(deps) + `}`)
	st, err := store.LoadReadOnly(raw)
	if err != nil {
		t.Fatalf("LoadReadOnly: %v", err)
	}

	loadedPod, err := st.GetPod("default", "web-1")
	if err != nil {
		t.Fatalf("GetPod: %v", err)
	}
	if loadedPod.Labels == nil {
		loadedPod.Labels = map[string]string{}
	}
	loadedPod.Labels["example.com/edited"] = "true"
	if _, err := st.UpdatePod(loadedPod); err != nil {
		t.Fatalf("unrelated pod update of a legacy collision: %v", err)
	}

	loadedDep, err := st.GetDeployment("default", "web")
	if err != nil {
		t.Fatalf("GetDeployment: %v", err)
	}
	one := int32(1)
	loadedDep.Spec.Replicas = &one
	if _, err := st.UpdateDeployment(loadedDep); err != nil {
		t.Fatalf("scale down of a legacy collision: %v", err)
	}
}

// A Deployment replica is materialized as a Pod that carries its owning
// Deployment in OwnerReferences. Creating that instance must not conflict
// with the Deployment that already owns the name.
func TestGeneratedNamesDeploymentReplicaCreate(t *testing.T) {
	st := newStore(t)
	dep, err := st.CreateDeployment(generatedNameDeployment("default", "web", 2, generatedNameContainer("app", "img")))
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	for _, name := range []string{"web-0", "web-1"} {
		pod := generatedNamePod("default", name, generatedNameContainer("app", "img"))
		pod.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
			Name:       dep.Name,
			UID:        dep.UID,
		}}
		if _, err := st.CreatePod(pod); err != nil {
			t.Fatalf("CreatePod %s for its Deployment: %v", name, err)
		}
	}
}
