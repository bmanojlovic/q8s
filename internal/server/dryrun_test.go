package server_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"q8s/internal/store"
)

func TestDryRunPodCreateDoesNotStoreOrWriteUnits(t *testing.T) {
	st := store.New()
	quadletDir := t.TempDir()
	srv := newTestServerWithQuadletDir(t, st, quadletDir)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	resp := post(t, ts.URL+"/api/v1/namespaces/default/pods?dryRun=All", podBody("default", "example", "nginx"))
	assertStatus(t, resp, 201)
	body := decodeBody(t, resp)
	assertKind(t, body, "Pod")
	metadata, _ := body["metadata"].(map[string]interface{})
	if metadata["name"] != "example" {
		t.Fatalf("metadata.name = %v", metadata["name"])
	}

	resp = get(t, ts.URL+"/api/v1/namespaces/default/pods/example")
	assertStatus(t, resp, 404)
	resp.Body.Close()

	if entries, err := os.ReadDir(quadletDir); err != nil {
		t.Fatal(err)
	} else if len(entries) != 0 {
		t.Fatalf("dry-run wrote quadlet files: %v", entries)
	}
	if events := st.AllEvents(); len(events) != 0 {
		t.Fatalf("dry-run recorded events: %+v", events)
	}

	// The name is still free, so the real create succeeds.
	resp = post(t, ts.URL+"/api/v1/namespaces/default/pods", podBody("default", "example", "nginx"))
	assertStatus(t, resp, 201)
	resp.Body.Close()
	if _, err := os.Stat(filepath.Join(quadletDir, "default-example.container")); err != nil {
		t.Fatalf("real create did not write the unit: %v", err)
	}
}

func TestDryRunPodCreateConflict(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := post(t, ts.URL+"/api/v1/namespaces/default/pods", podBody("default", "example", "nginx"))
	assertStatus(t, resp, 201)
	resp.Body.Close()

	resp = post(t, ts.URL+"/api/v1/namespaces/default/pods?dryRun=All", podBody("default", "example", "nginx"))
	assertStatus(t, resp, http.StatusConflict)
	resp.Body.Close()
}

func TestDryRunInvalidValue(t *testing.T) {
	ts, st := newTestServer(t)
	resp := post(t, ts.URL+"/api/v1/namespaces/default/pods?dryRun=No", podBody("default", "example", "nginx"))
	assertStatus(t, resp, http.StatusBadRequest)
	resp.Body.Close()

	if pods := st.Pods("default"); len(pods) != 0 {
		t.Fatalf("invalid dryRun stored %d pod(s)", len(pods))
	}
}

func TestDryRunPodPatchDoesNotChangeStore(t *testing.T) {
	ts, st := newTestServer(t)
	resp := post(t, ts.URL+"/api/v1/namespaces/default/pods", podBody("default", "example", "nginx"))
	assertStatus(t, resp, 201)
	resp.Body.Close()

	resp = strategicPatch(t, ts.URL+"/api/v1/namespaces/default/pods/example?dryRun=All", map[string]interface{}{
		"metadata": map[string]interface{}{"labels": map[string]interface{}{"app": "patched"}},
	})
	assertStatus(t, resp, http.StatusOK)
	body := decodeBody(t, resp)
	metadata, _ := body["metadata"].(map[string]interface{})
	labels, _ := metadata["labels"].(map[string]interface{})
	if labels["app"] != "patched" {
		t.Fatalf("dry-run response labels = %v", labels)
	}

	pod, err := st.GetPod("default", "example")
	if err != nil {
		t.Fatal(err)
	}
	if pod.Labels["app"] != "" {
		t.Fatalf("dry-run patch changed the stored pod: labels = %v", pod.Labels)
	}
}

func TestDryRunPodDeleteKeepsStore(t *testing.T) {
	ts, st := newTestServer(t)
	resp := post(t, ts.URL+"/api/v1/namespaces/default/pods", podBody("default", "example", "nginx"))
	assertStatus(t, resp, 201)
	resp.Body.Close()

	resp = do(t, http.MethodDelete, ts.URL+"/api/v1/namespaces/default/pods/example?dryRun=All", nil)
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	if _, err := st.GetPod("default", "example"); err != nil {
		t.Fatalf("dry-run delete removed the pod: %v", err)
	}
}

func TestDryRunCollectionDeleteKeepsStore(t *testing.T) {
	ts, st := newTestServer(t)
	for _, name := range []string{"one", "two"} {
		resp := post(t, ts.URL+"/api/v1/namespaces/default/pods", podBody("default", name, "nginx"))
		assertStatus(t, resp, 201)
		resp.Body.Close()
	}

	resp := do(t, http.MethodDelete, ts.URL+"/api/v1/namespaces/default/pods?dryRun=All", nil)
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	if pods := st.Pods("default"); len(pods) != 2 {
		t.Fatalf("dry-run collection delete left %d pod(s), want 2", len(pods))
	}
}

func TestDryRunRejectedOutsidePods(t *testing.T) {
	st := store.New()
	configDir := filepath.Join(t.TempDir(), "configmaps")
	srv := newTestServerWithConfigDir(t, st, configDir)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	body := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]interface{}{"name": "example", "namespace": "default"},
		"data":       map[string]interface{}{"key": "value"},
	}
	resp := post(t, ts.URL+"/api/v1/namespaces/default/configmaps?dryRun=All", body)
	assertStatus(t, resp, http.StatusBadRequest)
	resp.Body.Close()

	if _, err := st.GetConfigMap("default", "example"); err == nil {
		t.Fatal("rejected dry-run stored the configmap")
	}
	if _, err := os.Stat(filepath.Join(configDir, "default", "example")); !os.IsNotExist(err) {
		t.Fatalf("rejected dry-run wrote ConfigMap files: %v", err)
	}
}
