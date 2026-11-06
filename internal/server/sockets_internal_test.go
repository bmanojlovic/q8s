package server

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"q8s/internal/quadlet"
	"q8s/internal/store"
)

func socketFilesIn(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*.socket"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	return matches
}

func TestPodSocketsAreWrittenToSystemdDirAndRemovedOnDelete(t *testing.T) {
	st := store.New()
	quadletDir := t.TempDir()
	systemdDir := t.TempDir()
	_, ts, _ := newNetTestServer(t, st, quadletDir, systemdDir)

	netTestCreatePod(t, ts, "default", "traefik", map[string]string{
		quadlet.AnnotationSocketActivation: "web=8080",
	})

	webSocket := filepath.Join(systemdDir, "default-traefik-web.socket")
	if _, err := os.Stat(webSocket); err != nil {
		t.Fatalf("expected %s: %v", webSocket, err)
	}
	if files := socketFilesIn(t, quadletDir); len(files) != 0 {
		t.Fatalf("socket files must never land in QuadletDir: %v", files)
	}
	content := netTestReadFile(t, webSocket)
	if !netTestContainsLine(content, "ListenStream=8080") {
		t.Fatalf("unexpected socket content:\n%s", content)
	}
	container := netTestReadFile(t, filepath.Join(quadletDir, "default-traefik.container"))
	if !netTestContainsLine(container, "Requires=default-traefik-web.socket") {
		t.Fatalf("container does not require its socket unit:\n%s", container)
	}

	// Changing the annotation replaces the socket entry.
	resp := netTestRequest(t, http.MethodPatch, ts.URL+"/api/v1/namespaces/default/pods/traefik", map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]interface{}{
				quadlet.AnnotationSocketActivation: "api=9090",
			},
		},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch pod: status %d", resp.StatusCode)
	}
	if _, err := os.Stat(webSocket); !os.IsNotExist(err) {
		t.Fatalf("old web socket was not removed, err=%v", err)
	}
	apiSocket := filepath.Join(systemdDir, "default-traefik-api.socket")
	if _, err := os.Stat(apiSocket); err != nil {
		t.Fatalf("expected %s after patch: %v", apiSocket, err)
	}
	if content := netTestReadFile(t, apiSocket); !netTestContainsLine(content, "ListenStream=9090") {
		t.Fatalf("unexpected api socket content:\n%s", content)
	}

	// Deleting the pod stops/disables and removes its sockets.
	resp = netTestRequest(t, http.MethodDelete, ts.URL+"/api/v1/namespaces/default/pods/traefik", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete pod: status %d", resp.StatusCode)
	}
	if _, err := os.Stat(apiSocket); !os.IsNotExist(err) {
		t.Fatalf("socket still exists after pod delete, err=%v", err)
	}
}

func TestDeploymentInstanceSocketsUseInstanceName(t *testing.T) {
	st := store.New()
	quadletDir := t.TempDir()
	systemdDir := t.TempDir()
	_, ts, _ := newNetTestServer(t, st, quadletDir, systemdDir)

	resp := netTestRequest(t, http.MethodPost, ts.URL+"/apis/apps/v1/namespaces/default/deployments", map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]interface{}{"name": "app", "namespace": "default"},
		"spec": map[string]interface{}{
			"replicas": 1,
			"template": map[string]interface{}{
				"metadata": map[string]interface{}{
					"annotations": map[string]interface{}{
						quadlet.AnnotationSocketActivation: "web=8080",
					},
				},
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{"name": "app", "image": "nginx"},
					},
				},
			},
		},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create deployment: status %d", resp.StatusCode)
	}

	socket := filepath.Join(systemdDir, "default-app-0-web.socket")
	if _, err := os.Stat(socket); err != nil {
		t.Fatalf("expected deployment instance socket %s: %v", socket, err)
	}
	if files := socketFilesIn(t, quadletDir); len(files) != 0 {
		t.Fatalf("deployment socket files must never land in QuadletDir: %v", files)
	}
}
