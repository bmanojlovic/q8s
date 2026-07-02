package server_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"q8s/internal/server"
	"q8s/internal/store"
)

// --- test helpers ---

func genTestCert(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return
}

func newTestServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	certPEM, keyPEM := genTestCert(t)
	st := store.New()
	srv, err := server.New(server.Config{
		Store:   st,
		CertPEM: certPEM,
		KeyPEM:  keyPEM,
		// CACert nil → auth disabled
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, st
}

func get(t *testing.T, url string) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}

func post(t *testing.T, url string, body interface{}) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

func do(t *testing.T, method, url string, body interface{}) *http.Response {
	t.Helper()
	var rb io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rb = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rb)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

func decodeBody(t *testing.T, resp *http.Response) map[string]interface{} {
	t.Helper()
	defer resp.Body.Close()
	var m map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return m
}

func assertStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("expected status %d, got %d: %s", want, resp.StatusCode, body)
	}
}

func assertKind(t *testing.T, m map[string]interface{}, want string) {
	t.Helper()
	if m["kind"] != want {
		t.Fatalf("expected kind=%q, got %v", want, m["kind"])
	}
}

// --- Discovery ---

func TestHealthz(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := get(t, ts.URL+"/healthz")
	assertStatus(t, resp, 200)
	resp.Body.Close()
}

func TestVersion(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := get(t, ts.URL+"/version")
	assertStatus(t, resp, 200)
	m := decodeBody(t, resp)
	if m["major"] != "1" {
		t.Fatalf("expected major=1, got %v", m["major"])
	}
}

func TestAPIRoot(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := get(t, ts.URL+"/api")
	assertStatus(t, resp, 200)
	assertKind(t, decodeBody(t, resp), "APIVersions")
}

func TestAPIV1(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := get(t, ts.URL+"/api/v1")
	assertStatus(t, resp, 200)
	assertKind(t, decodeBody(t, resp), "APIResourceList")
}

func TestAPIsRoot(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := get(t, ts.URL+"/apis")
	assertStatus(t, resp, 200)
	assertKind(t, decodeBody(t, resp), "APIGroupList")
}

func TestBatchRoot(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := get(t, ts.URL+"/apis/batch")
	assertStatus(t, resp, 200)
	assertKind(t, decodeBody(t, resp), "APIGroup")
}

func TestBatchV1(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := get(t, ts.URL+"/apis/batch/v1")
	assertStatus(t, resp, 200)
	assertKind(t, decodeBody(t, resp), "APIResourceList")
}

// --- Namespace ---

func nsBody(name string) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata":   map[string]interface{}{"name": name},
	}
}

func TestNamespaceList(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := get(t, ts.URL+"/api/v1/namespaces")
	assertStatus(t, resp, 200)
	assertKind(t, decodeBody(t, resp), "NamespaceList")
}

func TestNamespaceCRUD(t *testing.T) {
	ts, _ := newTestServer(t)

	resp := post(t, ts.URL+"/api/v1/namespaces", nsBody("testing"))
	assertStatus(t, resp, 201)
	assertKind(t, decodeBody(t, resp), "Namespace")

	resp = get(t, ts.URL+"/api/v1/namespaces/testing")
	assertStatus(t, resp, 200)
	resp.Body.Close()

	resp = do(t, http.MethodDelete, ts.URL+"/api/v1/namespaces/testing", nil)
	assertStatus(t, resp, 200)
	resp.Body.Close()

	resp = get(t, ts.URL+"/api/v1/namespaces/testing")
	assertStatus(t, resp, 404)
	resp.Body.Close()
}

func TestNamespaceDuplicate(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := post(t, ts.URL+"/api/v1/namespaces", nsBody("dup"))
	assertStatus(t, resp, 201)
	resp.Body.Close()

	resp = post(t, ts.URL+"/api/v1/namespaces", nsBody("dup"))
	assertStatus(t, resp, 409)
	resp.Body.Close()
}

func TestNamespaceDeleteNotFound(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := do(t, http.MethodDelete, ts.URL+"/api/v1/namespaces/nonexistent", nil)
	assertStatus(t, resp, 404)
	resp.Body.Close()
}

// --- Pod ---

func podBody(ns, name, image string) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   map[string]interface{}{"name": name, "namespace": ns},
		"spec": map[string]interface{}{
			"containers": []interface{}{
				map[string]interface{}{"name": name, "image": image},
			},
		},
	}
}

func TestPodCRUD(t *testing.T) {
	ts, _ := newTestServer(t)

	resp := post(t, ts.URL+"/api/v1/namespaces/default/pods", podBody("default", "nginx", "nginx:latest"))
	assertStatus(t, resp, 201)
	assertKind(t, decodeBody(t, resp), "Pod")

	resp = get(t, ts.URL+"/api/v1/namespaces/default/pods")
	assertStatus(t, resp, 200)
	m := decodeBody(t, resp)
	assertKind(t, m, "PodList")
	items, _ := m["items"].([]interface{})
	if len(items) != 1 {
		t.Fatalf("expected 1 pod in list, got %d", len(items))
	}

	resp = get(t, ts.URL+"/api/v1/namespaces/default/pods/nginx")
	assertStatus(t, resp, 200)
	resp.Body.Close()

	resp = do(t, http.MethodDelete, ts.URL+"/api/v1/namespaces/default/pods/nginx", nil)
	assertStatus(t, resp, 200)
	resp.Body.Close()

	resp = get(t, ts.URL+"/api/v1/namespaces/default/pods/nginx")
	assertStatus(t, resp, 404)
	resp.Body.Close()
}

func TestPodDuplicate(t *testing.T) {
	ts, _ := newTestServer(t)
	body := podBody("default", "dupe", "nginx")
	resp := post(t, ts.URL+"/api/v1/namespaces/default/pods", body)
	assertStatus(t, resp, 201)
	resp.Body.Close()

	resp = post(t, ts.URL+"/api/v1/namespaces/default/pods", body)
	assertStatus(t, resp, 409)
	resp.Body.Close()
}

func TestPodBadJSON(t *testing.T) {
	ts, _ := newTestServer(t)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/namespaces/default/pods", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, resp, 400)
	resp.Body.Close()
}

func TestPodNotFound(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := get(t, ts.URL+"/api/v1/namespaces/default/pods/missing")
	assertStatus(t, resp, 404)
	resp.Body.Close()
}

func TestPodDeleteNotFound(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := do(t, http.MethodDelete, ts.URL+"/api/v1/namespaces/default/pods/ghost", nil)
	assertStatus(t, resp, 404)
	resp.Body.Close()
}

// --- Service ---

func svcBody(ns, name string) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Service",
		"metadata":   map[string]interface{}{"name": name, "namespace": ns},
		"spec": map[string]interface{}{
			"ports": []interface{}{
				map[string]interface{}{"port": 80, "protocol": "TCP"},
			},
		},
	}
}

func TestServiceCRUD(t *testing.T) {
	ts, _ := newTestServer(t)

	resp := post(t, ts.URL+"/api/v1/namespaces/default/services", svcBody("default", "web"))
	assertStatus(t, resp, 201)
	resp.Body.Close()

	resp = get(t, ts.URL+"/api/v1/namespaces/default/services")
	assertStatus(t, resp, 200)
	assertKind(t, decodeBody(t, resp), "ServiceList")

	resp = get(t, ts.URL+"/api/v1/namespaces/default/services/web")
	assertStatus(t, resp, 200)
	resp.Body.Close()

	resp = do(t, http.MethodDelete, ts.URL+"/api/v1/namespaces/default/services/web", nil)
	assertStatus(t, resp, 200)
	resp.Body.Close()

	resp = get(t, ts.URL+"/api/v1/namespaces/default/services/web")
	assertStatus(t, resp, 404)
	resp.Body.Close()
}

func TestServiceDuplicate(t *testing.T) {
	ts, _ := newTestServer(t)
	body := svcBody("default", "dup")
	resp := post(t, ts.URL+"/api/v1/namespaces/default/services", body)
	assertStatus(t, resp, 201)
	resp.Body.Close()

	resp = post(t, ts.URL+"/api/v1/namespaces/default/services", body)
	assertStatus(t, resp, 409)
	resp.Body.Close()
}

// --- PVC ---

func pvcBody(ns, name string) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata":   map[string]interface{}{"name": name, "namespace": ns},
		"spec": map[string]interface{}{
			"accessModes": []interface{}{"ReadWriteOnce"},
		},
	}
}

func TestPVCCRUD(t *testing.T) {
	ts, _ := newTestServer(t)

	resp := post(t, ts.URL+"/api/v1/namespaces/default/persistentvolumeclaims", pvcBody("default", "data"))
	assertStatus(t, resp, 201)
	resp.Body.Close()

	resp = get(t, ts.URL+"/api/v1/namespaces/default/persistentvolumeclaims")
	assertStatus(t, resp, 200)
	assertKind(t, decodeBody(t, resp), "PersistentVolumeClaimList")

	resp = get(t, ts.URL+"/api/v1/namespaces/default/persistentvolumeclaims/data")
	assertStatus(t, resp, 200)
	resp.Body.Close()

	resp = do(t, http.MethodDelete, ts.URL+"/api/v1/namespaces/default/persistentvolumeclaims/data", nil)
	assertStatus(t, resp, 200)
	resp.Body.Close()

	resp = get(t, ts.URL+"/api/v1/namespaces/default/persistentvolumeclaims/data")
	assertStatus(t, resp, 404)
	resp.Body.Close()
}

// --- ConfigMap ---

func TestConfigMapCRUD(t *testing.T) {
	ts, _ := newTestServer(t)

	body := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]interface{}{"name": "cfg", "namespace": "default"},
		"data":       map[string]interface{}{"key": "value"},
	}
	resp := post(t, ts.URL+"/api/v1/namespaces/default/configmaps", body)
	assertStatus(t, resp, 201)
	resp.Body.Close()

	resp = get(t, ts.URL+"/api/v1/namespaces/default/configmaps/cfg")
	assertStatus(t, resp, 200)
	m := decodeBody(t, resp)
	data, _ := m["data"].(map[string]interface{})
	if data["key"] != "value" {
		t.Fatalf("expected key=value, got %v", data["key"])
	}

	// Update
	body["data"] = map[string]interface{}{"key": "updated"}
	resp = do(t, http.MethodPut, ts.URL+"/api/v1/namespaces/default/configmaps/cfg", body)
	assertStatus(t, resp, 200)
	m = decodeBody(t, resp)
	data, _ = m["data"].(map[string]interface{})
	if data["key"] != "updated" {
		t.Fatalf("expected key=updated, got %v", data["key"])
	}

	resp = get(t, ts.URL+"/api/v1/namespaces/default/configmaps")
	assertStatus(t, resp, 200)
	assertKind(t, decodeBody(t, resp), "ConfigMapList")

	resp = do(t, http.MethodDelete, ts.URL+"/api/v1/namespaces/default/configmaps/cfg", nil)
	assertStatus(t, resp, 200)
	resp.Body.Close()

	resp = get(t, ts.URL+"/api/v1/namespaces/default/configmaps/cfg")
	assertStatus(t, resp, 404)
	resp.Body.Close()
}

func TestConfigMapUpdateNotFound(t *testing.T) {
	ts, _ := newTestServer(t)
	body := map[string]interface{}{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]interface{}{"name": "missing", "namespace": "default"},
	}
	resp := do(t, http.MethodPut, ts.URL+"/api/v1/namespaces/default/configmaps/missing", body)
	assertStatus(t, resp, 404)
	resp.Body.Close()
}

// --- Secret ---

func TestSecretCRUD(t *testing.T) {
	ts, _ := newTestServer(t)

	body := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]interface{}{"name": "mysecret", "namespace": "default"},
		"stringData": map[string]interface{}{"password": "s3cr3t"},
	}
	resp := post(t, ts.URL+"/api/v1/namespaces/default/secrets", body)
	assertStatus(t, resp, 201)
	resp.Body.Close()

	resp = get(t, ts.URL+"/api/v1/namespaces/default/secrets")
	assertStatus(t, resp, 200)
	assertKind(t, decodeBody(t, resp), "SecretList")

	resp = get(t, ts.URL+"/api/v1/namespaces/default/secrets/mysecret")
	assertStatus(t, resp, 200)
	resp.Body.Close()

	resp = do(t, http.MethodDelete, ts.URL+"/api/v1/namespaces/default/secrets/mysecret", nil)
	assertStatus(t, resp, 200)
	resp.Body.Close()

	resp = get(t, ts.URL+"/api/v1/namespaces/default/secrets/mysecret")
	assertStatus(t, resp, 404)
	resp.Body.Close()
}

func TestSecretDuplicate(t *testing.T) {
	ts, _ := newTestServer(t)
	body := map[string]interface{}{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]interface{}{"name": "s", "namespace": "default"},
	}
	resp := post(t, ts.URL+"/api/v1/namespaces/default/secrets", body)
	assertStatus(t, resp, 201)
	resp.Body.Close()

	resp = post(t, ts.URL+"/api/v1/namespaces/default/secrets", body)
	assertStatus(t, resp, 409)
	resp.Body.Close()
}

// --- Job ---

func jobBody(ns, name string) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata":   map[string]interface{}{"name": name, "namespace": ns},
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{"name": "worker", "image": "busybox"},
					},
					"restartPolicy": "Never",
				},
			},
		},
	}
}

func TestJobCRUD(t *testing.T) {
	ts, _ := newTestServer(t)

	resp := post(t, ts.URL+"/apis/batch/v1/namespaces/default/jobs", jobBody("default", "myjob"))
	assertStatus(t, resp, 201)
	resp.Body.Close()

	resp = get(t, ts.URL+"/apis/batch/v1/namespaces/default/jobs")
	assertStatus(t, resp, 200)
	assertKind(t, decodeBody(t, resp), "JobList")

	resp = get(t, ts.URL+"/apis/batch/v1/namespaces/default/jobs/myjob")
	assertStatus(t, resp, 200)
	resp.Body.Close()

	resp = do(t, http.MethodDelete, ts.URL+"/apis/batch/v1/namespaces/default/jobs/myjob", nil)
	assertStatus(t, resp, 200)
	resp.Body.Close()

	resp = get(t, ts.URL+"/apis/batch/v1/namespaces/default/jobs/myjob")
	assertStatus(t, resp, 404)
	resp.Body.Close()
}

func TestJobDuplicate(t *testing.T) {
	ts, _ := newTestServer(t)
	body := jobBody("default", "dup")
	resp := post(t, ts.URL+"/apis/batch/v1/namespaces/default/jobs", body)
	assertStatus(t, resp, 201)
	resp.Body.Close()

	resp = post(t, ts.URL+"/apis/batch/v1/namespaces/default/jobs", body)
	assertStatus(t, resp, 409)
	resp.Body.Close()
}

func TestJobNotFound(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := get(t, ts.URL+"/apis/batch/v1/namespaces/default/jobs/ghost")
	assertStatus(t, resp, 404)
	resp.Body.Close()
}

// --- CronJob ---

func cronJobBody(ns, name, schedule string) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "batch/v1",
		"kind":       "CronJob",
		"metadata":   map[string]interface{}{"name": name, "namespace": ns},
		"spec": map[string]interface{}{
			"schedule": schedule,
			"jobTemplate": map[string]interface{}{
				"spec": map[string]interface{}{
					"template": map[string]interface{}{
						"spec": map[string]interface{}{
							"containers": []interface{}{
								map[string]interface{}{"name": "task", "image": "alpine"},
							},
						},
					},
				},
			},
		},
	}
}

func TestCronJobCRUD(t *testing.T) {
	ts, _ := newTestServer(t)

	resp := post(t, ts.URL+"/apis/batch/v1/namespaces/default/cronjobs", cronJobBody("default", "mycron", "0 * * * *"))
	assertStatus(t, resp, 201)
	resp.Body.Close()

	resp = get(t, ts.URL+"/apis/batch/v1/namespaces/default/cronjobs")
	assertStatus(t, resp, 200)
	assertKind(t, decodeBody(t, resp), "CronJobList")

	resp = get(t, ts.URL+"/apis/batch/v1/namespaces/default/cronjobs/mycron")
	assertStatus(t, resp, 200)
	resp.Body.Close()

	resp = do(t, http.MethodDelete, ts.URL+"/apis/batch/v1/namespaces/default/cronjobs/mycron", nil)
	assertStatus(t, resp, 200)
	resp.Body.Close()

	resp = get(t, ts.URL+"/apis/batch/v1/namespaces/default/cronjobs/mycron")
	assertStatus(t, resp, 404)
	resp.Body.Close()
}

func TestCronJobDuplicate(t *testing.T) {
	ts, _ := newTestServer(t)
	body := cronJobBody("default", "dup", "*/5 * * * *")
	resp := post(t, ts.URL+"/apis/batch/v1/namespaces/default/cronjobs", body)
	assertStatus(t, resp, 201)
	resp.Body.Close()

	resp = post(t, ts.URL+"/apis/batch/v1/namespaces/default/cronjobs", body)
	assertStatus(t, resp, 409)
	resp.Body.Close()
}

// --- Deployment ---

func deployBody(ns, name, image string) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]interface{}{"name": name, "namespace": ns},
		"spec": map[string]interface{}{
			"selector": map[string]interface{}{
				"matchLabels": map[string]interface{}{"app": name},
			},
			"template": map[string]interface{}{
				"metadata": map[string]interface{}{"labels": map[string]interface{}{"app": name}},
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{"name": name, "image": image},
					},
				},
			},
		},
	}
}

func TestDeploymentCRUD(t *testing.T) {
	ts, _ := newTestServer(t)

	resp := post(t, ts.URL+"/apis/apps/v1/namespaces/default/deployments", deployBody("default", "myapp", "nginx:latest"))
	assertStatus(t, resp, 201)
	assertKind(t, decodeBody(t, resp), "Deployment")

	resp = get(t, ts.URL+"/apis/apps/v1/namespaces/default/deployments")
	assertStatus(t, resp, 200)
	assertKind(t, decodeBody(t, resp), "DeploymentList")

	resp = get(t, ts.URL+"/apis/apps/v1/namespaces/default/deployments/myapp")
	assertStatus(t, resp, 200)
	resp.Body.Close()

	resp = do(t, http.MethodDelete, ts.URL+"/apis/apps/v1/namespaces/default/deployments/myapp", nil)
	assertStatus(t, resp, 200)
	resp.Body.Close()

	resp = get(t, ts.URL+"/apis/apps/v1/namespaces/default/deployments/myapp")
	assertStatus(t, resp, 404)
	resp.Body.Close()
}

func TestDeploymentDuplicate(t *testing.T) {
	ts, _ := newTestServer(t)
	body := deployBody("default", "dup", "nginx")
	resp := post(t, ts.URL+"/apis/apps/v1/namespaces/default/deployments", body)
	assertStatus(t, resp, 201)
	resp.Body.Close()

	resp = post(t, ts.URL+"/apis/apps/v1/namespaces/default/deployments", body)
	assertStatus(t, resp, 409)
	resp.Body.Close()
}

func TestDeploymentNotFound(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := get(t, ts.URL+"/apis/apps/v1/namespaces/default/deployments/ghost")
	assertStatus(t, resp, 404)
	resp.Body.Close()
}

func TestDeploymentReplicas(t *testing.T) {
	ts, _ := newTestServer(t)

	// Create with 3 replicas
	body := deployBody("default", "scaled", "nginx:latest")
	body["spec"].(map[string]interface{})["replicas"] = 3
	resp := post(t, ts.URL+"/apis/apps/v1/namespaces/default/deployments", body)
	assertStatus(t, resp, 201)
	m := decodeBody(t, resp)
	spec := m["spec"].(map[string]interface{})
	if spec["replicas"] != float64(3) {
		t.Fatalf("expected replicas=3, got %v", spec["replicas"])
	}
	resp.Body.Close()

	// Scale down via /scale subresource
	scaleBody := map[string]interface{}{
		"apiVersion": "autoscaling/v1",
		"kind":       "Scale",
		"spec":       map[string]interface{}{"replicas": 1},
	}
	resp = do(t, http.MethodPatch, ts.URL+"/apis/apps/v1/namespaces/default/deployments/scaled/scale", scaleBody)
	assertStatus(t, resp, 200)
	m = decodeBody(t, resp)
	scaleSpec := m["spec"].(map[string]interface{})
	if scaleSpec["replicas"] != float64(1) {
		t.Fatalf("expected replicas=1 after scale down, got %v", scaleSpec["replicas"])
	}

	// GET /scale
	resp = get(t, ts.URL+"/apis/apps/v1/namespaces/default/deployments/scaled/scale")
	assertStatus(t, resp, 200)
	m = decodeBody(t, resp)
	if m["kind"] != "Scale" {
		t.Fatalf("expected kind=Scale, got %v", m["kind"])
	}

	// PATCH replicas on deployment directly
	patchBody := map[string]interface{}{
		"spec": map[string]interface{}{"replicas": 2},
	}
	resp = do(t, http.MethodPatch, ts.URL+"/apis/apps/v1/namespaces/default/deployments/scaled", patchBody)
	assertStatus(t, resp, 200)
	m = decodeBody(t, resp)
	spec = m["spec"].(map[string]interface{})
	if spec["replicas"] != float64(2) {
		t.Fatalf("expected replicas=2 after patch, got %v", spec["replicas"])
	}
}

func TestAppsDiscovery(t *testing.T) {
	ts, _ := newTestServer(t)

	resp := get(t, ts.URL+"/apis/apps")
	assertStatus(t, resp, 200)
	assertKind(t, decodeBody(t, resp), "APIGroup")

	resp = get(t, ts.URL+"/apis/apps/v1")
	assertStatus(t, resp, 200)
	assertKind(t, decodeBody(t, resp), "APIResourceList")
}

func TestClusterDeploymentList(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := post(t, ts.URL+"/apis/apps/v1/namespaces/default/deployments", deployBody("default", "d1", "nginx"))
	assertStatus(t, resp, 201)
	resp.Body.Close()

	resp = get(t, ts.URL+"/apis/apps/v1/deployments")
	assertStatus(t, resp, 200)
	assertKind(t, decodeBody(t, resp), "DeploymentList")
}

// --- Cluster-scoped lists ---

func TestClusterPodList(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := post(t, ts.URL+"/api/v1/namespaces/default/pods", podBody("default", "a", "img"))
	assertStatus(t, resp, 201)
	resp.Body.Close()

	resp = get(t, ts.URL+"/api/v1/pods")
	assertStatus(t, resp, 200)
	assertKind(t, decodeBody(t, resp), "PodList")
}

func TestClusterServiceList(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := get(t, ts.URL+"/api/v1/services")
	assertStatus(t, resp, 200)
	assertKind(t, decodeBody(t, resp), "ServiceList")
}

func TestClusterPVCList(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := get(t, ts.URL+"/api/v1/persistentvolumeclaims")
	assertStatus(t, resp, 200)
	assertKind(t, decodeBody(t, resp), "PersistentVolumeClaimList")
}

func TestClusterConfigMapList(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := get(t, ts.URL+"/api/v1/configmaps")
	assertStatus(t, resp, 200)
	assertKind(t, decodeBody(t, resp), "ConfigMapList")
}

func TestClusterSecretList(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := get(t, ts.URL+"/api/v1/secrets")
	assertStatus(t, resp, 200)
	assertKind(t, decodeBody(t, resp), "SecretList")
}

func TestClusterJobList(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := post(t, ts.URL+"/apis/batch/v1/namespaces/default/jobs", jobBody("default", "j1"))
	assertStatus(t, resp, 201)
	resp.Body.Close()

	resp = get(t, ts.URL+"/apis/batch/v1/jobs")
	assertStatus(t, resp, 200)
	assertKind(t, decodeBody(t, resp), "JobList")
}

func TestClusterCronJobList(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := get(t, ts.URL+"/apis/batch/v1/cronjobs")
	assertStatus(t, resp, 200)
	assertKind(t, decodeBody(t, resp), "CronJobList")
}

// --- Method not allowed ---

func TestPodMethodNotAllowed(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := do(t, http.MethodPut, ts.URL+"/api/v1/namespaces/default/pods", nil)
	assertStatus(t, resp, 405)
	resp.Body.Close()
}

func TestClusterListMethodNotAllowed(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := do(t, http.MethodPost, ts.URL+"/api/v1/pods", nil)
	assertStatus(t, resp, 405)
	resp.Body.Close()
}

func TestBatchClusterListMethodNotAllowed(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := do(t, http.MethodDelete, ts.URL+"/apis/batch/v1/jobs", nil)
	assertStatus(t, resp, 405)
	resp.Body.Close()
}

// --- Unknown resource ---

func TestUnknownResource(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := get(t, ts.URL+"/api/v1/namespaces/default/widgets")
	assertStatus(t, resp, 404)
	resp.Body.Close()
}
