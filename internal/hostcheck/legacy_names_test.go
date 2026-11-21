package hostcheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"q8s/internal/names"
	"q8s/internal/store"
)

func legacyNamesStore(t *testing.T) []byte {
	t.Helper()
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
	if _, err := st.CreatePVC(&corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "default"},
	}); err != nil {
		t.Fatalf("CreatePVC: %v", err)
	}
	data, err := os.ReadFile(storeFile)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	return data
}

func TestLegacyNamesReportsLeftoversAndPrintsCommands(t *testing.T) {
	storeData := legacyNamesStore(t)
	legacyPodFile := filepath.Join("/etc/containers/systemd", names.LegacyHost(names.KindPodContainerFile, "default", "app", ""))
	legacyVolumeFile := filepath.Join("/etc/containers/systemd", names.LegacyHost(names.KindPVCFile, "default", "data"))
	files := map[string][]byte{
		"/etc/q8s/store.json": storeData,
		legacyPodFile:         []byte("[Container]\n"),
		legacyVolumeFile:      []byte("[Volume]\n"),
	}
	env := Env{
		Dirs: Dirs{Data: "/etc/q8s", Quadlet: "/etc/containers/systemd"},
		ReadFile: func(path string) ([]byte, error) {
			if data, ok := files[path]; ok {
				return data, nil
			}
			return nil, os.ErrNotExist
		},
		Command: func(name string, args ...string) ([]byte, error) {
			if name == "podman" && strings.Join(args, " ") ==
				"ps -a --filter label=app.kubernetes.io/managed-by=q8s --format {{.Names}}" {
				return []byte("default-app\ndefault_app\n"), nil
			}
			return nil, os.ErrNotExist
		},
	}
	c := checkLegacyNames(env)
	if c.Status != StatusWarn {
		t.Fatalf("status = %q, want warn (detail %q)", c.Status, c.Detail)
	}
	for _, want := range []string{legacyPodFile, legacyVolumeFile, "default-app (legacy container)"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail %q does not report %q", c.Detail, want)
		}
	}
	if strings.Contains(c.Detail, "default_app (legacy container)") {
		t.Errorf("detail %q reports the new container name", c.Detail)
	}
	for _, want := range []string{
		"systemctl --user stop default-app.service",
		"rm -f " + legacyPodFile,
		"rm -f " + legacyVolumeFile,
		"systemctl --user stop default-data-volume.service",
		"podman rm -f default-app",
	} {
		if !strings.Contains(c.Fix, want) {
			t.Errorf("fix %q does not contain %q", c.Fix, want)
		}
	}
}

func TestLegacyNamesOKWithOnlyNewNames(t *testing.T) {
	storeData := legacyNamesStore(t)
	newPodFile := filepath.Join("/etc/containers/systemd", names.Host(names.KindPodContainerFile, "default", "app", ""))
	env := Env{
		Dirs: Dirs{Data: "/etc/q8s", Quadlet: "/etc/containers/systemd"},
		ReadFile: func(path string) ([]byte, error) {
			if path == "/etc/q8s/store.json" {
				return storeData, nil
			}
			if path == newPodFile {
				return []byte("[Container]\n"), nil
			}
			return nil, os.ErrNotExist
		},
		Command: func(name string, args ...string) ([]byte, error) {
			if name == "podman" {
				return []byte("default_app\n"), nil
			}
			return nil, os.ErrNotExist
		},
	}
	c := checkLegacyNames(env)
	if c.Status != StatusOK {
		t.Fatalf("status = %q, want ok (detail %q)", c.Status, c.Detail)
	}
}

func TestOrphanedVolumesUsesPersistedVolumeName(t *testing.T) {
	storeData := legacyNamesStore(t)
	files := map[string][]byte{"/etc/q8s/store.json": storeData}
	env := Env{
		Dirs: Dirs{Data: "/etc/q8s", Quadlet: "/etc/containers/systemd"},
		ReadFile: func(path string) ([]byte, error) {
			if data, ok := files[path]; ok {
				return data, nil
			}
			return nil, os.ErrNotExist
		},
		Command: func(name string, args ...string) ([]byte, error) {
			if name == "podman" && strings.Join(args, " ") == "volume ls --format json" {
				return volumeListJSON(t, []podmanVolume{
					{Name: "default-data", Labels: map[string]string{"app.kubernetes.io/managed-by": "q8s"}},
					{Name: "default_deleted", Labels: map[string]string{"app.kubernetes.io/managed-by": "q8s"}},
				}), nil
			}
			return nil, os.ErrNotExist
		},
	}
	c := checkOrphanedVolumes(env)
	if c.Status != StatusWarn {
		t.Fatalf("status = %q, want warn (detail %q)", c.Status, c.Detail)
	}
	if strings.Contains(c.Detail, "default-data") {
		t.Errorf("detail %q reports an in-use legacy claim volume", c.Detail)
	}
	if !strings.Contains(c.Detail, "default_deleted (data of deleted PVC)") {
		t.Errorf("detail %q does not report a deleted new-format PVC volume", c.Detail)
	}
}

func TestOrphanedVolumesAcceptsCustomPersistedVolumeName(t *testing.T) {
	storeData := legacyNamesStore(t)
	// Replace the store data with a claim whose persisted volume name is not
	// derivable from the namespace and claim name.
	storeData = []byte(strings.Replace(string(storeData), `"volumeName":"default_data"`, `"volumeName":"custom-volume"`, 1))
	env := Env{
		Dirs: Dirs{Data: "/etc/q8s", Quadlet: "/etc/containers/systemd"},
		ReadFile: func(path string) ([]byte, error) {
			if path == "/etc/q8s/store.json" {
				return storeData, nil
			}
			return nil, os.ErrNotExist
		},
		Command: func(name string, args ...string) ([]byte, error) {
			if name == "podman" && strings.Join(args, " ") == "volume ls --format json" {
				return volumeListJSON(t, []podmanVolume{{
					Name:   "custom-volume",
					Labels: map[string]string{"app.kubernetes.io/managed-by": "q8s"},
				}}), nil
			}
			return nil, os.ErrNotExist
		},
	}
	c := checkOrphanedVolumes(env)
	if c.Status != StatusOK {
		t.Fatalf("status = %q, want ok (detail %q)", c.Status, c.Detail)
	}
}
