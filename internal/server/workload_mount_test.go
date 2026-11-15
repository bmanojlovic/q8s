package server_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"q8s/internal/server"
	"q8s/internal/store"
)

const serverTestWorkloadDir = "/run/q8s/workload"

func newWorkloadTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	certPEM, keyPEM := genTestCert(t)
	quadletDir := t.TempDir()
	srv, err := server.New(server.Config{
		Store:          store.New(),
		CertPEM:        certPEM,
		KeyPEM:         keyPEM,
		QuadletDir:     quadletDir,
		WorkloadDir:    serverTestWorkloadDir,
		WorkloadBinDir: "/etc/q8s/workload-bin",
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, quadletDir
}

func workloadTestContainer(name string) map[string]interface{} {
	return map[string]interface{}{"name": name, "image": "busybox"}
}

func workloadPodSpec(serviceAccount string, containers ...interface{}) map[string]interface{} {
	return map[string]interface{}{
		"serviceAccountName": serviceAccount,
		"containers":         containers,
	}
}

func TestWorkloadMountRenderedByServer(t *testing.T) {
	ts, quadletDir := newWorkloadTestServer(t)

	tests := []struct {
		name  string
		url   string
		body  map[string]interface{}
		files []string
	}{
		{
			name: "standalone pod",
			url:  "/api/v1/namespaces/default/pods",
			body: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Pod",
				"metadata":   map[string]interface{}{"name": "sa-pod", "namespace": "default"},
				"spec":       workloadPodSpec("worker", workloadTestContainer("app")),
			},
			files: []string{"default-sa-pod.container"},
		},
		{
			name: "multi-container pod",
			url:  "/api/v1/namespaces/default/pods",
			body: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Pod",
				"metadata":   map[string]interface{}{"name": "sa-multi", "namespace": "default"},
				"spec":       workloadPodSpec("worker", workloadTestContainer("web"), workloadTestContainer("sidecar")),
			},
			files: []string{"default-sa-multi-web.container", "default-sa-multi-sidecar.container"},
		},
		{
			name: "deployment replica",
			url:  "/apis/apps/v1/namespaces/default/deployments",
			body: map[string]interface{}{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata":   map[string]interface{}{"name": "sa-dep", "namespace": "default"},
				"spec": map[string]interface{}{
					"replicas": 1,
					"selector": map[string]interface{}{"matchLabels": map[string]interface{}{"app": "sa-dep"}},
					"template": map[string]interface{}{
						"metadata": map[string]interface{}{"labels": map[string]interface{}{"app": "sa-dep"}},
						"spec":     workloadPodSpec("worker", workloadTestContainer("app")),
					},
				},
			},
			files: []string{"default-sa-dep-0.container"},
		},
		{
			name: "job",
			url:  "/apis/batch/v1/namespaces/default/jobs",
			body: map[string]interface{}{
				"apiVersion": "batch/v1",
				"kind":       "Job",
				"metadata":   map[string]interface{}{"name": "sa-job", "namespace": "default"},
				"spec": map[string]interface{}{
					"template": map[string]interface{}{
						"spec": workloadPodSpec("worker", workloadTestContainer("app")),
					},
				},
			},
			files: []string{"default-sa-job-job.container"},
		},
		{
			name: "cronjob",
			url:  "/apis/batch/v1/namespaces/default/cronjobs",
			body: map[string]interface{}{
				"apiVersion": "batch/v1",
				"kind":       "CronJob",
				"metadata":   map[string]interface{}{"name": "sa-cron", "namespace": "default"},
				"spec": map[string]interface{}{
					"schedule": "0 3 * * *",
					"jobTemplate": map[string]interface{}{
						"spec": map[string]interface{}{
							"template": map[string]interface{}{
								"spec": workloadPodSpec("worker", workloadTestContainer("app")),
							},
						},
					},
				},
			},
			files: []string{"default-sa-cron-cron.container"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := post(t, ts.URL+tt.url, tt.body)
			assertStatus(t, resp, http.StatusCreated)
			resp.Body.Close()

			for _, filename := range tt.files {
				content, err := os.ReadFile(filepath.Join(quadletDir, filename))
				if err != nil {
					t.Fatalf("read %s: %v", filename, err)
				}
				for _, want := range []string{
					"Volume=" + serverTestWorkloadDir + ":/var/run/q8s/workload:ro,z\n",
					"Environment=KUBECONFIG=/var/run/q8s/workload/ns/default/kubeconfig\n",
				} {
					if !strings.Contains(string(content), want) {
						t.Errorf("%s is missing %q:\n%s", filename, want, content)
					}
				}
			}
		})
	}
}

func TestPodRejectsReservedWorkloadMount(t *testing.T) {
	ts, _ := newTestServer(t)

	body := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   map[string]interface{}{"name": "conflict", "namespace": "default"},
		"spec": map[string]interface{}{
			"containers": []interface{}{map[string]interface{}{
				"name":  "app",
				"image": "busybox",
				"volumeMounts": []interface{}{map[string]interface{}{
					"name":      "credentials",
					"mountPath": "/var/run/q8s/workload",
				}},
			}},
			"volumes": []interface{}{map[string]interface{}{
				"name":     "credentials",
				"emptyDir": map[string]interface{}{},
			}},
		},
	}

	resp := post(t, ts.URL+"/api/v1/namespaces/default/pods", body)
	assertStatus(t, resp, http.StatusUnprocessableEntity)
	status := decodeBody(t, resp)
	if status["reason"] != "Invalid" {
		t.Fatalf("expected reason Invalid, got %v", status["reason"])
	}
	if msg, _ := status["message"].(string); !strings.Contains(msg, "/var/run/q8s/workload") {
		t.Fatalf("message does not name the reserved path: %q", msg)
	}
}

func TestPodWithoutServiceAccountKeepsRenderedUnit(t *testing.T) {
	// Capture the old rendering on a server whose WorkloadDir is empty, then
	// compare with a server where the feature is enabled. The pod itself
	// does not opt in, so both units must be byte-identical.
	plainTS, plainDir := newWorkloadTestServer(t)

	certPEM, keyPEM := genTestCert(t)
	oldDir := t.TempDir()
	oldSrv, err := server.New(server.Config{
		Store:      store.New(),
		CertPEM:    certPEM,
		KeyPEM:     keyPEM,
		QuadletDir: oldDir,
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	oldTS := httptest.NewServer(oldSrv)
	t.Cleanup(oldTS.Close)

	body := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   map[string]interface{}{"name": "plain", "namespace": "default"},
		"spec":       workloadPodSpec("", workloadTestContainer("app")),
	}

	for _, candidate := range []struct {
		ts  *httptest.Server
		dir string
	}{{oldTS, oldDir}, {plainTS, plainDir}} {
		resp := post(t, candidate.ts.URL+"/api/v1/namespaces/default/pods", body)
		assertStatus(t, resp, http.StatusCreated)
		resp.Body.Close()
	}

	oldContent, err := os.ReadFile(filepath.Join(oldDir, "default-plain.container"))
	if err != nil {
		t.Fatal(err)
	}
	newContent, err := os.ReadFile(filepath.Join(plainDir, "default-plain.container"))
	if err != nil {
		t.Fatal(err)
	}
	if string(oldContent) != string(newContent) {
		t.Fatalf("pod without serviceAccountName changed:\nold:\n%s\nnew:\n%s", oldContent, newContent)
	}
	if strings.Contains(string(newContent), "/var/run/q8s/workload") {
		t.Fatalf("pod without serviceAccountName got the workload mount:\n%s", newContent)
	}
}
