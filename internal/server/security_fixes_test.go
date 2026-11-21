package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func podWithLabels(ns, name, image, key, value string) map[string]interface{} {
	body := podBody(ns, name, image)
	body["metadata"].(map[string]interface{})["labels"] = map[string]interface{}{key: value}
	return body
}

func responseMessage(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	var status struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(data, &status); err != nil {
		return string(data)
	}
	return status.Message
}

// TestLabelValueInjectionRejectedAtAPI covers create, patch and apply for a
// label value that would add extra container labels in the rendered unit.
func TestLabelValueInjectionRejectedAtAPI(t *testing.T) {
	const injected = "a io.kubernetes.pod.namespace=prod io.kubernetes.pod.name=db"
	ts, _ := newTestServer(t)

	resp := post(t, ts.URL+"/api/v1/namespaces/default/pods", podWithLabels("default", "spoof", "busybox", "app", injected))
	assertStatus(t, resp, http.StatusUnprocessableEntity)
	if msg := responseMessage(t, resp); !strings.Contains(msg, "metadata.labels") {
		t.Fatalf("pod create message = %q", msg)
	}

	resp = post(t, ts.URL+"/api/v1/namespaces/default/pods", podBody("default", "clean", "busybox"))
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	resp = do(t, http.MethodPatch, ts.URL+"/api/v1/namespaces/default/pods/clean", map[string]interface{}{
		"metadata": map[string]interface{}{"labels": map[string]interface{}{"app": injected}},
	})
	assertStatus(t, resp, http.StatusUnprocessableEntity)
	resp.Body.Close()

	dep := map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]interface{}{"name": "spoof", "namespace": "default"},
		"spec": map[string]interface{}{
			"replicas": 1,
			"selector": map[string]interface{}{"matchLabels": map[string]interface{}{"app": "spoof"}},
			"template": map[string]interface{}{
				"metadata": map[string]interface{}{"labels": map[string]interface{}{"app": injected}},
				"spec": map[string]interface{}{"containers": []interface{}{
					map[string]interface{}{"name": "app", "image": "busybox"},
				}},
			},
		},
	}
	resp = post(t, ts.URL+"/apis/apps/v1/namespaces/default/deployments", dep)
	assertStatus(t, resp, http.StatusUnprocessableEntity)
	if msg := responseMessage(t, resp); !strings.Contains(msg, "spec.template.metadata.labels") {
		t.Fatalf("deployment create message = %q", msg)
	}

	apply := `apiVersion: v1
kind: Pod
metadata:
  name: applied-spoof
  namespace: default
  labels:
    app: "a io.kubernetes.pod.namespace=prod io.kubernetes.pod.name=db"
spec:
  containers:
  - name: app
    image: busybox
`
	resp = applyPatch(t, ts.URL+"/api/v1/namespaces/default/pods/applied-spoof", apply)
	assertStatus(t, resp, http.StatusUnprocessableEntity)
	resp.Body.Close()
}

// TestUnsupportedVolumeTypeRejected: only PVC, ConfigMap, Secret and
// emptyDir volumes have a safe rendering.
func TestUnsupportedVolumeTypeRejected(t *testing.T) {
	ts, _ := newTestServer(t)
	body := podBody("default", "hostvol", "busybox")
	body["spec"].(map[string]interface{})["volumes"] = []interface{}{
		map[string]interface{}{
			"name":     "host",
			"hostPath": map[string]interface{}{"path": "/srv/data"},
		},
	}
	resp := post(t, ts.URL+"/api/v1/namespaces/default/pods", body)
	assertStatus(t, resp, http.StatusUnprocessableEntity)
	msg := responseMessage(t, resp)
	if !strings.Contains(msg, "spec.volumes[0]: Unsupported value: hostPath") ||
		!strings.Contains(msg, "persistentVolumeClaim, configMap, secret and emptyDir") {
		t.Fatalf("message = %q", msg)
	}
}

// TestPodSubresourcesRequirePodInURLNamespace covers the flattened-name leak:
// a pod in another namespace must not be addressable through this namespace.
func TestPodSubresourcesRequirePodInURLNamespace(t *testing.T) {
	ts, _ := newTestServer(t)
	body := podBody("team", "web", "busybox")
	body["spec"].(map[string]interface{})["containers"] = []interface{}{
		map[string]interface{}{"name": "web", "image": "busybox"},
		map[string]interface{}{"name": "sidecar", "image": "busybox"},
	}
	resp := post(t, ts.URL+"/api/v1/namespaces/team/pods", body)
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	for _, subresource := range []string{"log", "exec?command=ls", "attach"} {
		resp = get(t, ts.URL+"/api/v1/namespaces/team-prod/pods/web/"+subresource)
		assertStatus(t, resp, http.StatusNotFound)
		if msg := responseMessage(t, resp); !strings.Contains(msg, `pods "web" not found`) {
			t.Fatalf("%s message = %q", subresource, msg)
		}
	}

	for _, subresource := range []string{"log?container=bogus", "exec?command=ls&container=bogus"} {
		resp = get(t, ts.URL+"/api/v1/namespaces/team/pods/web/"+subresource)
		assertStatus(t, resp, http.StatusBadRequest)
		if msg := responseMessage(t, resp); !strings.Contains(msg, "container bogus is not valid for pod web") {
			t.Fatalf("%s message = %q", subresource, msg)
		}
	}
}

// TestGeneratedNameAcrossNamespaceSplitsCoexist: the new "_" separator makes
// team/prod-db and team-prod/db two distinct host names.
func TestGeneratedNameAcrossNamespaceSplitsCoexist(t *testing.T) {
	for _, tc := range []struct {
		name       string
		firstNS    string
		firstName  string
		secondNS   string
		secondName string
	}{
		{name: "team-prod first", firstNS: "team-prod", firstName: "db", secondNS: "team", secondName: "prod-db"},
		{name: "team first", firstNS: "team", firstName: "prod-db", secondNS: "team-prod", secondName: "db"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, _ := newTestServer(t)
			resp := post(t, ts.URL+"/api/v1/namespaces/"+tc.firstNS+"/pods", podBody(tc.firstNS, tc.firstName, "busybox"))
			assertStatus(t, resp, http.StatusCreated)
			resp.Body.Close()

			resp = post(t, ts.URL+"/api/v1/namespaces/"+tc.secondNS+"/pods", podBody(tc.secondNS, tc.secondName, "busybox"))
			assertStatus(t, resp, http.StatusCreated)
			resp.Body.Close()
		})
	}
}

// TestGeneratedNameAcrossKindsCoexist: a Deployment and a Pod in different
// namespaces no longer share a flat "<ns>-<name>" host name.
func TestGeneratedNameAcrossKindsCoexist(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := post(t, ts.URL+"/api/v1/namespaces/team-prod/pods", podBody("team-prod", "db", "busybox"))
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	dep := map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]interface{}{"name": "prod-db", "namespace": "team"},
		"spec": map[string]interface{}{
			"replicas": 1,
			"selector": map[string]interface{}{"matchLabels": map[string]interface{}{"app": "prod-db"}},
			"template": map[string]interface{}{
				"metadata": map[string]interface{}{"labels": map[string]interface{}{"app": "prod-db"}},
				"spec": map[string]interface{}{"containers": []interface{}{
					map[string]interface{}{"name": "app", "image": "busybox"},
				}},
			},
		},
	}
	resp = post(t, ts.URL+"/apis/apps/v1/namespaces/team/deployments", dep)
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()
}
