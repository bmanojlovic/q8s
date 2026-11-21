package server_test

import (
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"q8s/internal/store"
)

func serviceSpec(t *testing.T, url string) map[string]interface{} {
	t.Helper()
	resp := get(t, url)
	assertStatus(t, resp, http.StatusOK)
	return nested(decodeBody(t, resp), "spec")
}

// A refused nodePort must not stay in the store. Before the fix the Service
// was written before the nodePort checks, so a 409 left the bad nodePort
// behind and a matching Pod then published it on 0.0.0.0.
func TestRefusedNodePortUpdateLeavesServiceUnchanged(t *testing.T) {
	ts, _ := newTestServer(t)
	used := store.NodePortMin + 610
	previous := store.NodePortMin + 611

	resp := post(t, ts.URL+"/api/v1/namespaces/default/services", nodePortSvcBody("default", "a", 80, used))
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()
	resp = post(t, ts.URL+"/api/v1/namespaces/default/services", nodePortSvcBody("default", "b", 80, previous))
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	url := ts.URL + "/api/v1/namespaces/default/services/b"
	before := serviceSpec(t, url)
	if got := svcNodePort(t, map[string]interface{}{"spec": before}); got != previous {
		t.Fatalf("stored nodePort = %d, want %d", got, previous)
	}

	patch := func(nodePort int) map[string]interface{} {
		return map[string]interface{}{
			"spec": map[string]interface{}{"ports": []interface{}{map[string]interface{}{
				"port": 80, "targetPort": 80, "protocol": "TCP", "nodePort": nodePort,
			}}},
		}
	}
	for _, tc := range []struct {
		name     string
		method   string
		body     map[string]interface{}
		response int
	}{
		{"PUT duplicate", http.MethodPut, nodePortSvcBody("default", "b", 80, used), http.StatusConflict},
		{"PATCH duplicate", http.MethodPatch, patch(used), http.StatusConflict},
		{"PUT out of range", http.MethodPut, nodePortSvcBody("default", "b", 80, 8080), http.StatusConflict},
		{"PATCH out of range", http.MethodPatch, patch(8080), http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := do(t, tc.method, url, tc.body)
			assertStatus(t, resp, tc.response)
			resp.Body.Close()

			after := serviceSpec(t, url)
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("refused %s changed the stored Service:\nbefore: %v\nafter:  %v", tc.method, before, after)
			}
		})
	}

	// A valid nodePort update still works and is stored.
	valid := store.NodePortMin + 612
	resp = do(t, http.MethodPut, url, nodePortSvcBody("default", "b", 80, valid))
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()
	if got := svcNodePort(t, map[string]interface{}{"spec": serviceSpec(t, url)}); got != valid {
		t.Fatalf("valid update stored nodePort %d, want %d", got, valid)
	}
}

// Server-side apply reaches the same PATCH handler; a refused nodePort must
// leave the stored Service alone there too.
func TestApplyRefusedNodePortLeavesServiceUnchanged(t *testing.T) {
	ts, _ := newTestServer(t)
	used := store.NodePortMin + 620
	previous := store.NodePortMin + 621

	resp := post(t, ts.URL+"/api/v1/namespaces/default/services", nodePortSvcBody("default", "a", 80, used))
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()
	resp = post(t, ts.URL+"/api/v1/namespaces/default/services", nodePortSvcBody("default", "b", 80, previous))
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	url := ts.URL + "/api/v1/namespaces/default/services/b"
	before := serviceSpec(t, url)
	body := fmt.Sprintf(`apiVersion: v1
kind: Service
metadata:
  name: b
  namespace: default
spec:
  type: NodePort
  selector:
    app: b
  ports:
  - port: 80
    targetPort: 80
    protocol: TCP
    nodePort: %d
`, used)
	resp = applyPatch(t, url, body)
	assertStatus(t, resp, http.StatusConflict)
	resp.Body.Close()

	if after := serviceSpec(t, url); !reflect.DeepEqual(before, after) {
		t.Fatalf("refused apply changed the stored Service:\nbefore: %v\nafter:  %v", before, after)
	}
}
