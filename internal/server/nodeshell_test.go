package server_test

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"q8s/internal/store"
)

// nodeShellPod is the pod spec the kubectl node-shell plugin sends: a
// privileged host-namespace nsenter container with stdin and a tty.
func nodeShellPod() map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   map[string]interface{}{"name": "node-shell", "namespace": "default"},
		"spec": map[string]interface{}{
			"nodeName":    "host1",
			"hostPID":     true,
			"hostIPC":     true,
			"hostNetwork": true,
			"containers": []interface{}{map[string]interface{}{
				"name":      "nsenter",
				"image":     "docker.io/library/alpine",
				"command":   []interface{}{"nsenter"},
				"args":      []interface{}{"--target", "1", "--mount", "--uts", "--ipc", "--net", "--pid", "--", "bash", "-l"},
				"stdin":     true,
				"stdinOnce": true,
				"tty":       true,
				"securityContext": map[string]interface{}{
					"privileged": true,
				},
			}},
		},
	}
}

func TestNodeShellPodRendersHostNamespaceUnit(t *testing.T) {
	st := store.New()
	quadletDir := t.TempDir()
	srv := newTestServerWithQuadletDir(t, st, quadletDir)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	resp := post(t, ts.URL+"/api/v1/namespaces/default/pods", nodeShellPod())
	if resp.StatusCode != 201 {
		t.Fatalf("create status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	content, err := os.ReadFile(filepath.Join(quadletDir, "default-node-shell.container"))
	if err != nil {
		t.Fatal(err)
	}
	out := string(content)
	for _, want := range []string{
		"PodmanArgs=--privileged\n",
		"PodmanArgs=--pid=host\n",
		"PodmanArgs=--ipc=host\n",
		"PodmanArgs=--interactive\n",
		"PodmanArgs=--tty\n",
		"Network=host\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("unit is missing %q:\n%s", want, out)
		}
	}
}
