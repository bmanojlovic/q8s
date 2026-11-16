package hostcheck

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"q8s/internal/store"
)

func volumeListJSON(t *testing.T, volumes []podmanVolume) []byte {
	t.Helper()
	out, err := json.Marshal(volumes)
	if err != nil {
		t.Fatalf("marshal volumes: %v", err)
	}
	return out
}

func volumeEnv(t *testing.T, storeData, listOut []byte) Env {
	t.Helper()
	return Env{
		Dirs: Dirs{Data: "/etc/q8s"},
		ReadFile: func(path string) ([]byte, error) {
			if path == "/etc/q8s/store.json" {
				return storeData, nil
			}
			return nil, os.ErrNotExist
		},
		Command: func(name string, args ...string) ([]byte, error) {
			if name == "podman" && strings.Join(args, " ") == "volume ls --format json" {
				return listOut, nil
			}
			return nil, os.ErrNotExist
		},
	}
}

func TestOrphanedVolumesFindsEachKindAndSkipsInUseOnes(t *testing.T) {
	storeFile := filepath.Join(t.TempDir(), "store.json")
	st, err := store.Load(storeFile)
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "app",
				Image: "nginx",
				VolumeMounts: []corev1.VolumeMount{{
					Name:      "data",
					MountPath: "/data",
				}},
			}},
			Volumes: []corev1.Volume{{
				Name:         "data",
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			}},
		},
	}
	if _, err := st.CreatePod(pod); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "keep", Namespace: "default"},
	}
	if _, err := st.CreatePVC(claim); err != nil {
		t.Fatalf("CreatePVC: %v", err)
	}
	storeData, err := os.ReadFile(storeFile)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}

	managedBy := map[string]string{"app.kubernetes.io/managed-by": "q8s"}
	volumes := []podmanVolume{
		{Name: "q8s_default_app_data"},
		{Name: "q8s_default_gone_data"},
		{Name: "data"},
		{Name: "default-keep", Labels: managedBy},
		{Name: "default-deleted", Labels: managedBy},
		{Name: "postgres-data"},
	}
	env := volumeEnv(t, storeData, volumeListJSON(t, volumes))
	c := checkOrphanedVolumes(env)

	if c.Status != StatusWarn {
		t.Fatalf("status = %q, want warn (detail %q)", c.Status, c.Detail)
	}
	for _, want := range []string{
		"q8s_default_gone_data (no stored workload uses it)",
		"data (legacy emptyDir name (before v0.11.1))",
		"default-deleted (data of deleted PVC)",
	} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail %q does not mention %q", c.Detail, want)
		}
	}
	for _, notWant := range []string{"q8s_default_app_data", "default-keep", "postgres-data"} {
		if strings.Contains(c.Detail, notWant) {
			t.Errorf("detail %q reports in-use or unrelated volume %q", c.Detail, notWant)
		}
	}
	for _, want := range []string{
		"podman volume rm q8s_default_gone_data",
		"podman volume rm data",
		"podman volume rm default-deleted",
	} {
		if !strings.Contains(c.Fix, want) {
			t.Errorf("fix %q does not contain %q", c.Fix, want)
		}
	}
}

func TestOrphanedVolumesOKWhenNothingIsLeft(t *testing.T) {
	storeFile := filepath.Join(t.TempDir(), "store.json")
	st, err := store.Load(storeFile)
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	if _, err := st.CreatePod(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "idle", Namespace: "default"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:  "app",
			Image: "nginx",
		}}},
	}); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	storeData, err := os.ReadFile(storeFile)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	env := volumeEnv(t, storeData, volumeListJSON(t, []podmanVolume{{Name: "postgres-data"}}))
	c := checkOrphanedVolumes(env)
	if c.Status != StatusOK {
		t.Fatalf("status = %q, want ok (detail %q)", c.Status, c.Detail)
	}
}

func TestOrphanedVolumesSkippedWithoutStore(t *testing.T) {
	env := Env{
		Dirs:     Dirs{Data: "/etc/q8s"},
		ReadFile: func(string) ([]byte, error) { return nil, os.ErrNotExist },
		Command: func(name string, args ...string) ([]byte, error) {
			t.Fatalf("podman must not run when the store is unreadable: %s %v", name, args)
			return nil, nil
		},
	}
	c := checkOrphanedVolumes(env)
	if c.Status != StatusInfo {
		t.Fatalf("status = %q, want info", c.Status)
	}
	if !strings.Contains(c.Detail, "skipped:") {
		t.Errorf("detail %q should start with a skipped reason", c.Detail)
	}
}
