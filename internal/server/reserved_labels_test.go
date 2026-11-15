package server_test

import "testing"

// TestPodRejectsReservedContainerLabels: pod labels are copied onto the
// container after q8s's own and podman keeps the last value of a key, so a
// pod could otherwise claim another pod's identity (and its ServiceAccount
// credential) through io.kubernetes.pod.name.
func TestPodRejectsReservedContainerLabels(t *testing.T) {
	ts, _ := newTestServer(t)
	for _, key := range []string{"io.kubernetes.pod.name", "io.kubernetes.pod.namespace", "PODMAN_SYSTEMD_UNIT", "app.kubernetes.io/managed-by", "q8s.io/template-hash"} {
		body := map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]interface{}{
				"name": "spoof", "namespace": "default",
				"labels": map[string]interface{}{key: "victim"},
			},
			"spec": map[string]interface{}{
				"containers": []interface{}{map[string]interface{}{"name": "app", "image": "busybox"}},
			},
		}
		resp := post(t, ts.URL+"/api/v1/namespaces/default/pods", body)
		if resp.StatusCode < 400 || resp.StatusCode >= 500 {
			t.Errorf("label %q: status %d, want a 4xx rejection", key, resp.StatusCode)
		}
		resp.Body.Close()
	}
}
