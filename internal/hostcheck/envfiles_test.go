package hostcheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"q8s/internal/quadlet"
	"q8s/internal/store"
)

func writeEnvTestFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte("TOKEN=old\n"), 0600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestOrphanedEnvFilesFindsOnlyFilesNoStoredWorkloadOwns(t *testing.T) {
	storeFile := filepath.Join(t.TempDir(), "store.json")
	st, err := store.Load(storeFile)
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	if _, err := st.CreatePod(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "default"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:  "app",
			Image: "nginx",
		}}},
	}); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	replicas := int32(2)
	if _, err := st.CreateDeployment(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "web", Image: "nginx"}},
			}},
		},
	}); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	if _, err := st.CreateJob(&batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "once", Namespace: "default"},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "worker", Image: "busybox"}},
		}}},
	}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if _, err := st.CreateCronJob(&batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "cron", Namespace: "default"},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 * * * *",
			JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{
				Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "task", Image: "alpine"}},
				}},
			}},
		},
	}); err != nil {
		t.Fatalf("CreateCronJob: %v", err)
	}
	secretDir := filepath.Join(t.TempDir(), "secrets")
	defaultEnv := filepath.Join(secretDir, "default", "_env")
	for _, name := range []string{
		quadlet.EnvFileName("pod"),
		quadlet.EnvFileName("web-0"),
		quadlet.EnvFileName("web-1"),
		quadlet.EnvFileName(quadlet.JobEnvBaseName("once")),
		quadlet.EnvFileName(quadlet.CronJobEnvBaseName("cron")),
	} {
		writeEnvTestFile(t, filepath.Join(defaultEnv, name))
	}
	stale := []string{
		quadlet.EnvFileName("gone"),
		quadlet.EnvFileName("web-2"),
		quadlet.EnvFileName(quadlet.JobEnvBaseName("gone")),
	}
	for _, name := range stale {
		writeEnvTestFile(t, filepath.Join(defaultEnv, name))
	}
	writeEnvTestFile(t, filepath.Join(secretDir, "demo", "_env", quadlet.EnvFileName("stale")))

	env := Env{
		Dirs:     Dirs{Data: filepath.Dir(storeFile), Secret: secretDir},
		ReadFile: os.ReadFile,
		ReadDir:  os.ReadDir,
	}
	c := checkOrphanedEnvFiles(env)
	if c.Status != StatusWarn {
		t.Fatalf("status = %q, want warn (detail %q)", c.Status, c.Detail)
	}
	for _, name := range stale {
		if !strings.Contains(c.Detail, filepath.Join("default", name)+" (no stored workload uses it)") {
			t.Errorf("detail %q does not report stale default/%s", c.Detail, name)
		}
		if !strings.Contains(c.Fix, "rm "+filepath.Join(defaultEnv, name)) {
			t.Errorf("fix %q does not remove default/%s", c.Fix, name)
		}
	}
	if !strings.Contains(c.Detail, filepath.Join("demo", quadlet.EnvFileName("stale"))+" (no stored workload uses it)") {
		t.Errorf("detail %q does not report the stale file in another namespace", c.Detail)
	}
	for _, name := range []string{
		quadlet.EnvFileName("pod"),
		quadlet.EnvFileName("web-0"),
		quadlet.EnvFileName("web-1"),
		quadlet.EnvFileName(quadlet.JobEnvBaseName("once")),
		quadlet.EnvFileName(quadlet.CronJobEnvBaseName("cron")),
	} {
		if strings.Contains(c.Detail, filepath.Join("default", name)) || strings.Contains(c.Fix, filepath.Join(defaultEnv, name)) {
			t.Errorf("used env file default/%s was reported as orphaned", name)
		}
	}
}

func TestOrphanedEnvFilesOKWhenEveryFileIsUsed(t *testing.T) {
	storeFile := filepath.Join(t.TempDir(), "store.json")
	st, err := store.Load(storeFile)
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	if _, err := st.CreatePod(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:  "app",
			Image: "nginx",
		}}},
	}); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	secretDir := t.TempDir()
	writeEnvTestFile(t, filepath.Join(secretDir, "default", "_env", quadlet.EnvFileName("app")))

	env := Env{
		Dirs:     Dirs{Data: filepath.Dir(storeFile), Secret: secretDir},
		ReadFile: os.ReadFile,
		ReadDir:  os.ReadDir,
	}
	c := checkOrphanedEnvFiles(env)
	if c.Status != StatusOK {
		t.Fatalf("status = %q, want ok (detail %q)", c.Status, c.Detail)
	}
}
