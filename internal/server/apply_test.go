package server_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"q8s/internal/store"
)

func applyPatch(t *testing.T, url, body string) *http.Response {
	t.Helper()
	return applyPatchWithType(t, url, body, "application/apply-patch+yaml")
}

func applyPatchWithType(t *testing.T, url, body, contentType string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPatch, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new apply request: %v", err)
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH %s: %v", url, err)
	}
	return resp
}

func TestApplyConfigMapCreateAndUpdate(t *testing.T) {
	ts, st := newTestServer(t)
	url := ts.URL + "/api/v1/namespaces/default/configmaps/applied"

	// Content-Type parameters after ';' are allowed, like client-go sends.
	body := `apiVersion: v1
kind: ConfigMap
metadata:
  name: applied
  namespace: default
  labels:
    app: demo
data:
  key: first
`
	resp := applyPatchWithType(t, url, body, "application/apply-patch+yaml; charset=utf-8")
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	cm, err := st.GetConfigMap("default", "applied")
	if err != nil {
		t.Fatalf("created ConfigMap not persisted: %v", err)
	}
	if cm.Data["key"] != "first" || cm.Labels["app"] != "demo" {
		t.Fatalf("created ConfigMap = %+v", cm)
	}

	body = strings.Replace(body, "key: first", "key: second", 1)
	resp = applyPatch(t, url+"?fieldManager=Terraform&force=false", body)
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	cm, err = st.GetConfigMap("default", "applied")
	if err != nil {
		t.Fatal(err)
	}
	if cm.Data["key"] != "second" {
		t.Fatalf("updated data = %v", cm.Data)
	}
}

func TestApplyRemovesDataKeyAndLabel(t *testing.T) {
	ts, st := newTestServer(t)
	url := ts.URL + "/api/v1/namespaces/default/configmaps/removed"

	body := `apiVersion: v1
kind: ConfigMap
metadata:
  name: removed
  namespace: default
  labels:
    keep: "yes"
    drop: "me"
data:
  keep: one
  drop: two
`
	resp := applyPatch(t, url, body)
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	body = `apiVersion: v1
kind: ConfigMap
metadata:
  name: removed
  namespace: default
  labels:
    keep: "yes"
data:
  keep: one
`
	resp = applyPatch(t, url, body)
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	cm, err := st.GetConfigMap("default", "removed")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cm.Data["drop"]; ok {
		t.Errorf("data key drop was not removed: %v", cm.Data)
	}
	if cm.Data["keep"] != "one" {
		t.Errorf("data keep = %q", cm.Data["keep"])
	}
	if _, ok := cm.Labels["drop"]; ok {
		t.Errorf("label drop was not removed: %v", cm.Labels)
	}
	if cm.Labels["keep"] != "yes" {
		t.Errorf("label keep = %q", cm.Labels["keep"])
	}
}

func applyDeploymentYAML(name, image string) string {
	return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: default
spec:
  replicas: 1
  selector:
    matchLabels:
      app: %s
  template:
    metadata:
      labels:
        app: %s
    spec:
      containers:
      - name: %s
        image: %s
`, name, name, name, name, image)
}

func TestApplyDeployment(t *testing.T) {
	st := store.New()
	srv := newTestServerWithQuadletDir(t, st, t.TempDir())
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	url := ts.URL + "/apis/apps/v1/namespaces/default/deployments/web"
	resp := applyPatch(t, url, applyDeploymentYAML("web", "nginx:1.0"))
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	resp = applyPatch(t, url, applyDeploymentYAML("web", "nginx:2.0"))
	assertStatus(t, resp, http.StatusOK)
	m := decodeBody(t, resp)
	if got := deploymentImage(t, m); got != "nginx:2.0" {
		t.Fatalf("apply response image = %q, want nginx:2.0", got)
	}

	dep, err := st.GetDeployment("default", "web")
	if err != nil {
		t.Fatal(err)
	}
	if got := dep.Spec.Template.Spec.Containers[0].Image; got != "nginx:2.0" {
		t.Fatalf("stored Deployment image = %q, want nginx:2.0", got)
	}
	// The instance pod is synced by the normal Deployment update path, so an
	// apply reaches the same code a PUT/PATCH does.
	pod, err := st.GetPod("default", "web-0")
	if err != nil {
		t.Fatalf("instance pod: %v", err)
	}
	if got := pod.Spec.Containers[0].Image; got != "nginx:2.0" {
		t.Fatalf("instance pod image = %q, want nginx:2.0", got)
	}
}

// TestApplyDeploymentRemovesEnvVar: an env var dropped from the applied
// manifest disappears from the stored Deployment. The PATCH handlers merge
// container lists by name, so apply must replace spec as a whole value.
func TestApplyDeploymentRemovesEnvVar(t *testing.T) {
	st := store.New()
	srv := newTestServerWithQuadletDir(t, st, t.TempDir())
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	manifest := func(env string) string {
		return `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: default
spec:
  replicas: 1
  selector:
    matchLabels: {app: web}
  template:
    metadata:
      labels: {app: web}
    spec:
      containers:
      - name: web
        image: nginx:1.0
        env:
` + env
	}
	url := ts.URL + "/apis/apps/v1/namespaces/default/deployments/web"
	resp := applyPatch(t, url, manifest("        - {name: A, value: \"1\"}\n        - {name: B, value: \"2\"}\n"))
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()
	resp = applyPatch(t, url, manifest("        - {name: A, value: \"1\"}\n"))
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	dep, err := st.GetDeployment("default", "web")
	if err != nil {
		t.Fatal(err)
	}
	env := dep.Spec.Template.Spec.Containers[0].Env
	if len(env) != 1 || env[0].Name != "A" {
		t.Fatalf("env after apply = %+v, want only A", env)
	}
}

func TestApplyNamespace(t *testing.T) {
	ts, st := newTestServer(t)
	url := ts.URL + "/api/v1/namespaces/applied-ns"

	resp := applyPatch(t, url, `apiVersion: v1
kind: Namespace
metadata:
  name: applied-ns
  labels:
    app: demo
`)
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	ns, err := st.GetNamespace("applied-ns")
	if err != nil {
		t.Fatalf("applied Namespace not persisted: %v", err)
	}
	if ns.Labels["app"] != "demo" {
		t.Fatalf("Namespace labels = %v", ns.Labels)
	}
	uid := ns.UID

	// An existing Namespace is cluster-scoped too: apply must update it
	// rather than try to create it again.
	resp = applyPatch(t, url, `apiVersion: v1
kind: Namespace
metadata:
  name: applied-ns
  labels:
    app: updated
`)
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()
	ns, err = st.GetNamespace("applied-ns")
	if err != nil {
		t.Fatal(err)
	}
	if ns.Labels["app"] != "updated" {
		t.Fatalf("updated Namespace labels = %v", ns.Labels)
	}
	if ns.UID != uid {
		t.Fatalf("Namespace UID changed from %q to %q", uid, ns.UID)
	}

	resp = get(t, url)
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()
}

func TestApplyValidation(t *testing.T) {
	ts, _ := newTestServer(t)

	t.Run("name mismatch", func(t *testing.T) {
		resp := applyPatch(t, ts.URL+"/api/v1/namespaces/default/configmaps/applied", `apiVersion: v1
kind: ConfigMap
metadata:
  name: other
  namespace: default
`)
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusBadRequest)
		if msg := statusMessage(t, resp); !strings.Contains(msg, "metadata.name") {
			t.Fatalf("message = %q", msg)
		}
		resp = get(t, ts.URL+"/api/v1/namespaces/default/configmaps/applied")
		assertStatus(t, resp, http.StatusNotFound)
		resp.Body.Close()
	})

	t.Run("namespace mismatch", func(t *testing.T) {
		resp := applyPatch(t, ts.URL+"/api/v1/namespaces/default/configmaps/applied", `apiVersion: v1
kind: ConfigMap
metadata:
  name: applied
  namespace: other
`)
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusBadRequest)
	})

	t.Run("dry run", func(t *testing.T) {
		resp := applyPatch(t, ts.URL+"/api/v1/namespaces/default/configmaps/applied?dryRun=All", `apiVersion: v1
kind: ConfigMap
metadata:
  name: applied
  namespace: default
data:
  a: b
`)
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusBadRequest)
		if msg := statusMessage(t, resp); !strings.Contains(msg, "dryRun is not supported") {
			t.Fatalf("message = %q", msg)
		}
		resp = get(t, ts.URL+"/api/v1/namespaces/default/configmaps/applied")
		assertStatus(t, resp, http.StatusNotFound)
		resp.Body.Close()
	})

	t.Run("invalid yaml", func(t *testing.T) {
		resp := applyPatch(t, ts.URL+"/api/v1/namespaces/default/configmaps/applied", "apiVersion: v1\nmetadata: [unclosed\n")
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusBadRequest)
	})

	t.Run("missing kind", func(t *testing.T) {
		resp := applyPatch(t, ts.URL+"/api/v1/namespaces/default/configmaps/applied", `apiVersion: v1
metadata:
  name: applied
  namespace: default
`)
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusBadRequest)
	})
}

func statusMessage(t *testing.T, resp *http.Response) string {
	t.Helper()
	var status struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatalf("decode Status: %v", err)
	}
	return status.Message
}
