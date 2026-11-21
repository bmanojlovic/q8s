package server_test

import (
	"net/http"
	"strings"
	"testing"
)

// TestJobAndPodSuffixNamesCoexist: the new names put the fixed job suffix
// behind "__", so a Job x and a Pod x-job are distinct objects.
func TestJobAndPodSuffixNamesCoexist(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := post(t, ts.URL+"/apis/batch/v1/namespaces/default/jobs", jobBody("default", "x"))
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	resp = post(t, ts.URL+"/api/v1/namespaces/default/pods", podBody("default", "x-job", "busybox"))
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()
}

// TestGeneratedNameConflictOnDeploymentScale covers the update path: scaling
// a Deployment up adds instance names, so a Pod that already owns one is a
// 409 instead of a silently overwritten unit.
func TestGeneratedNameConflictOnDeploymentScale(t *testing.T) {
	ts, _ := newTestServer(t)
	body := deployBody("default", "web", "nginx:latest")
	body["spec"].(map[string]interface{})["replicas"] = 1
	resp := post(t, ts.URL+"/apis/apps/v1/namespaces/default/deployments", body)
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	resp = post(t, ts.URL+"/api/v1/namespaces/default/pods", podBody("default", "web-1", "busybox"))
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	scale := map[string]interface{}{
		"apiVersion": "autoscaling/v1",
		"kind":       "Scale",
		"metadata":   map[string]interface{}{"name": "web", "namespace": "default"},
		"spec":       map[string]interface{}{"replicas": 2},
	}
	resp = do(t, http.MethodPatch, ts.URL+"/apis/apps/v1/namespaces/default/deployments/web/scale", scale)
	assertStatus(t, resp, http.StatusConflict)
	if msg := responseMessage(t, resp); !strings.Contains(msg, `would share the generated name "web-1.env"`) {
		t.Fatalf("message = %q", msg)
	}
}
