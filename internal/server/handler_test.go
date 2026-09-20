package server_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

func TestNameValidationBlocksPathTraversal(t *testing.T) {
	ts, _ := newTestServer(t)

	// A ConfigMap whose namespace is a traversal string, submitted directly
	// to the server (bypassing kubectl's own client-side name validation,
	// which is not a server-side control) — this used to write files outside
	// ConfigDir via filepath.Join(ConfigDir, cm.Namespace, cm.Name).
	body := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]interface{}{
			"name":      "x",
			"namespace": "../../../../../../tmp/q8s-test-should-not-exist",
		},
		"data": map[string]string{"proof": "should never be written"},
	}
	resp := post(t, ts.URL+"/api/v1/namespaces/default/configmaps", body)
	assertStatus(t, resp, 400)
	resp.Body.Close()

	// A data key containing a traversal sequence must also be rejected —
	// this can be introduced via PATCH on an otherwise-valid object, so it
	// needs its own check independent of namespace/name.
	resp = post(t, ts.URL+"/api/v1/namespaces/default/configmaps", map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]interface{}{"name": "safe-name"},
		"data":       map[string]string{"../../evil": "value"},
	})
	assertStatus(t, resp, 400)
	resp.Body.Close()

	// A namespace/name starting with '-' must be rejected too (argument
	// injection into podman/systemctl command-line arguments built from
	// these values, e.g. "podman rm -f <ns>-<name>").
	resp = post(t, ts.URL+"/api/v1/namespaces/default/pods", podBody("-flaglike", "x", "nginx:latest"))
	assertStatus(t, resp, 400)
	resp.Body.Close()

	// Sanity check: an ordinary, valid ConfigMap still works.
	resp = post(t, ts.URL+"/api/v1/namespaces/default/configmaps", map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]interface{}{"name": "valid-name"},
		"data":       map[string]string{"key.yaml": "value"},
	})
	assertStatus(t, resp, 201)
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

func TestPodWatch(t *testing.T) {
	ts, st := newTestServer(t)

	// Pre-existing pod: must NOT be replayed as an ADDED event once the watch starts.
	resp := post(t, ts.URL+"/api/v1/namespaces/default/pods", podBody("default", "seed", "nginx:latest"))
	assertStatus(t, resp, 201)
	resp.Body.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/pods?watch=true", nil)
	if err != nil {
		t.Fatal(err)
	}
	watchResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer watchResp.Body.Close()
	assertStatus(t, watchResp, 200)

	type event struct {
		Type   string
		Object map[string]interface{}
	}
	events := make(chan event, 10)
	go func() {
		dec := json.NewDecoder(watchResp.Body)
		for {
			var raw struct {
				Type   string                 `json:"type"`
				Object map[string]interface{} `json:"object"`
			}
			if err := dec.Decode(&raw); err != nil {
				close(events)
				return
			}
			events <- event{Type: raw.Type, Object: raw.Object}
		}
	}()

	// Let serveWatch take its initial snapshot before mutating, so "seed" is
	// guaranteed to be seen as pre-existing rather than racing an ADDED event.
	time.Sleep(150 * time.Millisecond)

	// Small gaps between mutations mirror realistic client cadence (separate
	// kubectl invocations, or a sync-loop tick between creation and a phase
	// change) and give the watch goroutine a chance to observe each
	// intermediate state — the diff-based watch has no event queue, so state
	// changes that all land between two wakeups would otherwise coalesce.
	resp = post(t, ts.URL+"/api/v1/namespaces/default/pods", podBody("default", "watched", "nginx:latest"))
	assertStatus(t, resp, 201)
	resp.Body.Close()
	time.Sleep(50 * time.Millisecond)

	st.UpdatePodPhase("default", "watched", corev1.PodRunning)
	time.Sleep(50 * time.Millisecond)

	resp = do(t, http.MethodDelete, ts.URL+"/api/v1/namespaces/default/pods/watched", nil)
	assertStatus(t, resp, 200)
	resp.Body.Close()

	seen := map[string]int{}
	deadline := time.After(4 * time.Second)
	for seen["ADDED"] == 0 || seen["MODIFIED"] == 0 || seen["DELETED"] == 0 {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatalf("watch stream closed early; events so far: %v", seen)
			}
			meta, _ := ev.Object["metadata"].(map[string]interface{})
			name, _ := meta["name"].(string)
			if name == "seed" && ev.Type == "ADDED" {
				t.Fatalf("pre-existing pod %q must not be replayed as ADDED", name)
			}
			if name == "watched" {
				seen[ev.Type]++
			}
		case <-deadline:
			t.Fatalf("timed out waiting for watch events; got %v", seen)
		}
	}
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

func newTestServerWithQuadletDir(t *testing.T, st *store.Store, quadletDir string) *server.Server {
	t.Helper()
	certPEM, keyPEM := genTestCert(t)
	srv, err := server.New(server.Config{
		Store:      st,
		CertPEM:    certPEM,
		KeyPEM:     keyPEM,
		QuadletDir: quadletDir,
		// secretBaseDir() derives from ConfigDir's parent -- set so tests
		// that resolve Secret-sourced env vars into an EnvironmentFile
		// (TestEnvValueFromResolvedInQuadlet) have somewhere to write it.
		ConfigDir: filepath.Join(t.TempDir(), "configmaps"),
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	return srv
}

func TestPVCCreateBindsAndWritesQuadlet(t *testing.T) {
	st := store.New()
	quadletDir := t.TempDir()
	srv := newTestServerWithQuadletDir(t, st, quadletDir)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	resp := post(t, ts.URL+"/api/v1/namespaces/default/persistentvolumeclaims", pvcBody("default", "data"))
	assertStatus(t, resp, 201)
	body := decodeBody(t, resp)

	status := body["status"].(map[string]interface{})
	if status["phase"] != "Bound" {
		t.Fatalf("expected status.phase Bound, got %v", status["phase"])
	}
	spec := body["spec"].(map[string]interface{})
	if spec["storageClassName"] != "standard" {
		t.Fatalf("expected defaulted storageClassName standard, got %v", spec["storageClassName"])
	}
	if spec["volumeName"] != "default-data" {
		t.Fatalf("expected volumeName default-data, got %v", spec["volumeName"])
	}

	content, err := os.ReadFile(filepath.Join(quadletDir, "default-data.volume"))
	if err != nil {
		t.Fatalf("expected quadlet volume file: %v", err)
	}
	if want := "[Volume]\nVolumeName=default-data\n"; string(content) != want {
		t.Fatalf("unexpected volume file content:\n%s", content)
	}

	events := st.AllEvents()
	if len(events) != 1 || events[0].Reason != "ProvisioningSucceeded" {
		t.Fatalf("expected 1 ProvisioningSucceeded event, got %+v", events)
	}
}

func TestReconcileQuadletsBindsExistingPVC(t *testing.T) {
	st := store.New()
	quadletDir := t.TempDir()

	// Simulate a PVC created before binding existed: create through the
	// store, then revert it to the legacy empty-status shape.
	created, err := st.CreatePVC(&corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "legacy", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("CreatePVC: %v", err)
	}
	legacy := created.DeepCopy()
	legacy.Status = corev1.PersistentVolumeClaimStatus{}
	legacy.Spec.StorageClassName = nil
	legacy.Spec.VolumeName = ""
	if _, err := st.UpdatePVC(legacy); err != nil {
		t.Fatalf("UpdatePVC: %v", err)
	}

	// Pre-write the pre-scoping volume file shape: same claim, but the
	// volume name is the bare claim name, not namespace-scoped.
	volFile := filepath.Join(quadletDir, "default-legacy.volume")
	if err := os.WriteFile(volFile, []byte("[Volume]\nVolumeName=legacy\n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	srv := newTestServerWithQuadletDir(t, st, quadletDir)
	srv.ReconcileQuadlets()

	got, err := st.GetPVC("default", "legacy")
	if err != nil {
		t.Fatalf("GetPVC: %v", err)
	}
	if got.Status.Phase != corev1.ClaimBound {
		t.Fatalf("expected reconcile to bind legacy PVC, phase=%q", got.Status.Phase)
	}
	if got.Spec.StorageClassName == nil || *got.Spec.StorageClassName != "standard" {
		t.Fatalf("expected defaulted storageClassName, got %v", got.Spec.StorageClassName)
	}
	if got.Spec.VolumeName != "default-legacy" {
		t.Fatalf("expected volumeName default-legacy, got %q", got.Spec.VolumeName)
	}
	content, err := os.ReadFile(volFile)
	if err != nil {
		t.Fatalf("expected reconcile to write volume file: %v", err)
	}
	if want := "[Volume]\nVolumeName=default-legacy\n"; string(content) != want {
		t.Fatalf("expected reconcile to rewrite unscoped volume file, got:\n%s", content)
	}
}

// TestReconcileSweepsOrphanedSockets pins tic-845d: a stale legacy per-port
// .socket file (which no current q8s writes) left in the quadlet dir is
// swept at startup, even with no Service to trigger the reactive cleanup —
// while unrelated quadlet files are left untouched.
func TestReconcileSweepsOrphanedSockets(t *testing.T) {
	st := store.New()
	quadletDir := t.TempDir()

	orphan := filepath.Join(quadletDir, "mozak-brain-13131.socket")
	if err := os.WriteFile(orphan, []byte("[Socket]\nListenStream=13131\n"), 0644); err != nil {
		t.Fatalf("WriteFile socket: %v", err)
	}
	keep := filepath.Join(quadletDir, "default-web-0.container")
	if err := os.WriteFile(keep, []byte("[Container]\n"), 0644); err != nil {
		t.Fatalf("WriteFile container: %v", err)
	}

	srv := newTestServerWithQuadletDir(t, st, quadletDir)
	srv.ReconcileQuadlets()

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("expected orphaned .socket to be swept, stat err=%v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("unrelated quadlet file should survive the sweep: %v", err)
	}
}

// TestRestoreEnvFilesRerendersAfterWipe pins tic-1507: the per-pod
// Secret-derived _env/*.env files live on tmpfs and are wiped on a q8s-api
// restart, but the quadlet units' ExecStart --env-file depends on them.
// RestoreEnvFiles must re-render them on startup for stored workloads, even
// though the .container files already exist (so ReconcileQuadlets skips them).
func TestRestoreEnvFilesRerendersAfterWipe(t *testing.T) {
	certPEM, keyPEM := genTestCert(t)
	base := t.TempDir()
	st := store.New()
	srv, err := server.New(server.Config{
		Store:      st,
		CertPEM:    certPEM,
		KeyPEM:     keyPEM,
		QuadletDir: t.TempDir(),
		ConfigDir:  filepath.Join(base, "configmaps"),
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}

	if _, err := st.CreateSecret(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "appsecret", Namespace: "default"},
		Data:       map[string][]byte{"API_KEY": []byte("s3cr3t")},
	}); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	replicas := int32(1)
	if _, err := st.CreateDeployment(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "envapp", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "envapp"}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:  "c",
					Image: "busybox",
					Env: []corev1.EnvVar{{
						Name: "API_KEY",
						ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: "appsecret"},
							Key:                  "API_KEY",
						}},
					}},
				}}},
			},
		},
	}); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}

	envPath := filepath.Join(base, "secrets", "default", "_env", "envapp-0.env")
	// Simulate the tmpfs wipe: ensure it's absent before restore.
	os.RemoveAll(filepath.Join(base, "secrets", "default", "_env"))

	srv.RestoreEnvFiles()

	got, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("env file not re-rendered on restore: %v", err)
	}
	if want := "API_KEY=s3cr3t\n"; string(got) != want {
		t.Errorf("env file = %q, want %q", got, want)
	}
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

func TestSecretJSONPatch(t *testing.T) {
	ts, st := newTestServer(t)

	body := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]interface{}{"name": "creds", "namespace": "default"},
		"data":       map[string]interface{}{"user": "YWRtaW4=", "pass": "c2VjcmV0"},
	}
	resp := post(t, ts.URL+"/api/v1/namespaces/default/secrets", body)
	assertStatus(t, resp, 201)
	resp.Body.Close()

	jsonPatch := func(t *testing.T, ops []map[string]interface{}) *http.Response {
		t.Helper()
		b, err := json.Marshal(ops)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest(http.MethodPatch, ts.URL+"/api/v1/namespaces/default/secrets/creds", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json-patch+json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// Terraform-style JSON Patch: replace one key, remove another, add a third.
	resp = jsonPatch(t, []map[string]interface{}{
		{"op": "replace", "path": "/data/pass", "value": "bjN3cGFzcw=="},
		{"op": "remove", "path": "/data/user"},
		{"op": "add", "path": "/data/token", "value": "dG9rZW4="},
	})
	assertStatus(t, resp, 200)
	patched := decodeBody(t, resp)
	data := patched["data"].(map[string]interface{})
	if len(data) != 2 || data["pass"] != "bjN3cGFzcw==" || data["token"] != "dG9rZW4=" {
		t.Fatalf("unexpected patched data: %v", data)
	}
	if _, ok := data["user"]; ok {
		t.Fatalf("user key should have been removed: %v", data)
	}

	// A failing patch must not mutate the stored secret (atomicity).
	resp = jsonPatch(t, []map[string]interface{}{
		{"op": "remove", "path": "/data/nonexistent"},
	})
	assertStatus(t, resp, 400)
	resp.Body.Close()

	got, err := st.GetSecret("default", "creds")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Data) != 2 || got.Data["pass"] == nil || got.Data["token"] == nil {
		t.Fatalf("secret mutated by failed patch: %v", got.Data)
	}
}

func TestSecretJSONPatchRemovesStaleFiles(t *testing.T) {
	certPEM, keyPEM := genTestCert(t)
	st := store.New()
	cfgDir := t.TempDir()
	srv, err := server.New(server.Config{
		Store:     st,
		CertPEM:   certPEM,
		KeyPEM:    keyPEM,
		ConfigDir: filepath.Join(cfgDir, "configmaps"),
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	body := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]interface{}{"name": "creds", "namespace": "default"},
		"data":       map[string]interface{}{"user": "YWRtaW4=", "pass": "c2VjcmV0"},
	}
	resp := post(t, ts.URL+"/api/v1/namespaces/default/secrets", body)
	assertStatus(t, resp, 201)
	resp.Body.Close()

	secretDir := filepath.Join(cfgDir, "secrets", "default", "creds")
	if _, err := os.Stat(filepath.Join(secretDir, "user")); err != nil {
		t.Fatalf("expected user file after create: %v", err)
	}

	b, _ := json.Marshal([]map[string]interface{}{
		{"op": "remove", "path": "/data/user"},
		{"op": "replace", "path": "/data/pass", "value": "bjN3cGFzcw=="},
	})
	req, _ := http.NewRequest(http.MethodPatch, ts.URL+"/api/v1/namespaces/default/secrets/creds", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json-patch+json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, resp, 200)
	resp.Body.Close()

	if _, err := os.Stat(filepath.Join(secretDir, "user")); !os.IsNotExist(err) {
		t.Fatalf("stale user file should have been removed, stat err: %v", err)
	}
	pass, err := os.ReadFile(filepath.Join(secretDir, "pass"))
	if err != nil {
		t.Fatalf("read pass file: %v", err)
	}
	if string(pass) != "n3wpass" {
		t.Fatalf("expected updated pass file, got %q", pass)
	}
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

func TestEnvValueFromResolvedInQuadlet(t *testing.T) {
	st := store.New()
	quadletDir := t.TempDir()
	srv := newTestServerWithQuadletDir(t, st, quadletDir)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	cmBody := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]interface{}{"name": "envsrc", "namespace": "default"},
		"data":       map[string]interface{}{"TOKEN": "cm-val"},
	}
	resp := post(t, ts.URL+"/api/v1/namespaces/default/configmaps", cmBody)
	assertStatus(t, resp, 201)
	resp.Body.Close()

	secBody := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]interface{}{"name": "envsec", "namespace": "default"},
		"stringData": map[string]interface{}{"PW": "sec-val"},
	}
	resp = post(t, ts.URL+"/api/v1/namespaces/default/secrets", secBody)
	assertStatus(t, resp, 201)
	resp.Body.Close()

	env := []interface{}{
		map[string]interface{}{"name": "FROM_CM", "valueFrom": map[string]interface{}{"configMapKeyRef": map[string]interface{}{"name": "envsrc", "key": "TOKEN"}}},
		map[string]interface{}{"name": "FROM_SEC", "valueFrom": map[string]interface{}{"secretKeyRef": map[string]interface{}{"name": "envsec", "key": "PW"}}},
	}

	// Pod path
	podB := podBody("default", "envpod", "myimage")
	podB["spec"].(map[string]interface{})["containers"].([]interface{})[0].(map[string]interface{})["env"] = env
	resp = post(t, ts.URL+"/api/v1/namespaces/default/pods", podB)
	assertStatus(t, resp, 201)
	resp.Body.Close()

	podOut := readTestFile(t, filepath.Join(quadletDir, "default-envpod.container"))
	if !strings.Contains(podOut, "Environment=FROM_CM=cm-val") {
		t.Fatalf("expected resolved ConfigMap env value in pod quadlet:\n%s", podOut)
	}
	// Secret-derived values must NOT land inline in the 0644 .container file
	// -- they go to a 0600 EnvironmentFile instead (see writeEnvFile).
	if strings.Contains(podOut, "sec-val") {
		t.Fatalf("secret value leaked into the world-readable quadlet file:\n%s", podOut)
	}
	envFileMatch := regexp.MustCompile(`EnvironmentFile=(\S+)`).FindStringSubmatch(podOut)
	if envFileMatch == nil {
		t.Fatalf("expected EnvironmentFile= line in pod quadlet:\n%s", podOut)
	}
	envFilePath := envFileMatch[1]
	info, err := os.Stat(envFilePath)
	if err != nil {
		t.Fatalf("stat env file %s: %v", envFilePath, err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("expected env file mode 0600, got %o", perm)
	}
	envFileContent := readTestFile(t, envFilePath)
	if !strings.Contains(envFileContent, "FROM_SEC=sec-val") {
		t.Fatalf("expected FROM_SEC in env file:\n%s", envFileContent)
	}

	// The stored pod keeps the ValueFrom references, not the plaintext values.
	got, err := st.GetPod("default", "envpod")
	if err != nil {
		t.Fatal(err)
	}
	storedEnv := got.Spec.Containers[0].Env
	if len(storedEnv) != 2 || storedEnv[0].ValueFrom == nil || storedEnv[1].ValueFrom == nil {
		t.Fatalf("expected unresolved ValueFrom refs in stored pod, got %+v", storedEnv)
	}

	// Job path
	jobB := jobBody("default", "envjob")
	jobB["spec"].(map[string]interface{})["template"].(map[string]interface{})["spec"].(map[string]interface{})["containers"].([]interface{})[0].(map[string]interface{})["env"] = env
	resp = post(t, ts.URL+"/apis/batch/v1/namespaces/default/jobs", jobB)
	assertStatus(t, resp, 201)
	resp.Body.Close()

	jobOut := readTestFile(t, filepath.Join(quadletDir, "default-envjob-job.container"))
	if !strings.Contains(jobOut, "Environment=FROM_CM=cm-val") {
		t.Fatalf("expected resolved ConfigMap env value in job quadlet:\n%s", jobOut)
	}
	if strings.Contains(jobOut, "sec-val") {
		t.Fatalf("secret value leaked into the world-readable quadlet file:\n%s", jobOut)
	}
	if !strings.Contains(jobOut, "EnvironmentFile=") {
		t.Fatalf("expected EnvironmentFile= line in job quadlet:\n%s", jobOut)
	}
}

// TestEnvValueFromMissingRefNotSilentlyBlank verifies that a non-optional
// secretKeyRef/configMapKeyRef pointing at something that doesn't exist
// stops quadlet generation instead of rendering as an empty Environment=
// line -- matching real Kubernetes, which never starts such a pod
// (CreateContainerConfigError) rather than running it with a blank value
// (confirmed live 2026-08-29: this exact gap left MOZAK_VIEW_TOKEN empty
// and the app exited 0 looking "successful").
func TestEnvValueFromMissingRefNotSilentlyBlank(t *testing.T) {
	st := store.New()
	quadletDir := t.TempDir()
	srv := newTestServerWithQuadletDir(t, st, quadletDir)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	env := []interface{}{
		map[string]interface{}{"name": "MISSING", "valueFrom": map[string]interface{}{"secretKeyRef": map[string]interface{}{"name": "no-such-secret", "key": "PW"}}},
	}
	podB := podBody("default", "missingrefpod", "myimage")
	podB["spec"].(map[string]interface{})["containers"].([]interface{})[0].(map[string]interface{})["env"] = env
	resp := post(t, ts.URL+"/api/v1/namespaces/default/pods", podB)
	// The pod object itself is still allowed to be stored (matches real k8s:
	// the Pod resource creates fine, it just never gets a running container).
	assertStatus(t, resp, 201)
	resp.Body.Close()

	if _, err := os.Stat(filepath.Join(quadletDir, "default-missingrefpod.container")); !os.IsNotExist(err) {
		t.Fatalf("expected no quadlet file to be written for a pod with an unresolvable required secretKeyRef, err=%v", err)
	}

	// Optional refs, by contrast, must not block generation.
	optEnv := []interface{}{
		map[string]interface{}{"name": "MISSING_OPT", "valueFrom": map[string]interface{}{"secretKeyRef": map[string]interface{}{"name": "no-such-secret", "key": "PW", "optional": true}}},
	}
	podB2 := podBody("default", "optionalrefpod", "myimage")
	podB2["spec"].(map[string]interface{})["containers"].([]interface{})[0].(map[string]interface{})["env"] = optEnv
	resp = post(t, ts.URL+"/api/v1/namespaces/default/pods", podB2)
	assertStatus(t, resp, 201)
	resp.Body.Close()

	if _, err := os.Stat(filepath.Join(quadletDir, "default-optionalrefpod.container")); err != nil {
		t.Fatalf("expected quadlet file for a pod with only an optional unresolvable ref, err=%v", err)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
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

// --- Ingress ---

func ingressBody(ns, name, host, svcName string, svcPort int) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "Ingress",
		"metadata":   map[string]interface{}{"name": name, "namespace": ns},
		"spec": map[string]interface{}{
			"rules": []interface{}{
				map[string]interface{}{
					"host": host,
					"http": map[string]interface{}{
						"paths": []interface{}{
							map[string]interface{}{
								"path":     "/",
								"pathType": "Prefix",
								"backend": map[string]interface{}{
									"service": map[string]interface{}{
										"name": svcName,
										"port": map[string]interface{}{"number": svcPort},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

func TestIngressCRUD(t *testing.T) {
	ts, _ := newTestServer(t)

	resp := post(t, ts.URL+"/apis/networking.k8s.io/v1/namespaces/default/ingresses", ingressBody("default", "myingress", "example.com", "myservice", 80))
	assertStatus(t, resp, 201)
	resp.Body.Close()

	resp = get(t, ts.URL+"/apis/networking.k8s.io/v1/namespaces/default/ingresses")
	assertStatus(t, resp, 200)
	assertKind(t, decodeBody(t, resp), "IngressList")

	resp = get(t, ts.URL+"/apis/networking.k8s.io/v1/namespaces/default/ingresses/myingress")
	assertStatus(t, resp, 200)
	resp.Body.Close()

	resp = do(t, http.MethodDelete, ts.URL+"/apis/networking.k8s.io/v1/namespaces/default/ingresses/myingress", nil)
	assertStatus(t, resp, 200)
	resp.Body.Close()

	resp = get(t, ts.URL+"/apis/networking.k8s.io/v1/namespaces/default/ingresses/myingress")
	assertStatus(t, resp, 404)
	resp.Body.Close()
}

func TestIngressUpdate(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := post(t, ts.URL+"/apis/networking.k8s.io/v1/namespaces/default/ingresses", ingressBody("default", "upd", "old.example.com", "myservice", 80))
	assertStatus(t, resp, 201)
	resp.Body.Close()

	resp = do(t, http.MethodPut, ts.URL+"/apis/networking.k8s.io/v1/namespaces/default/ingresses/upd", ingressBody("default", "upd", "new.example.com", "myservice", 80))
	assertStatus(t, resp, 200)
	m := decodeBody(t, resp)
	spec, _ := m["spec"].(map[string]interface{})
	rules, _ := spec["rules"].([]interface{})
	rule, _ := rules[0].(map[string]interface{})
	if rule["host"] != "new.example.com" {
		t.Fatalf("expected host=new.example.com, got %v", rule["host"])
	}

	resp = get(t, ts.URL+"/apis/networking.k8s.io/v1/namespaces/default/ingresses/upd")
	assertStatus(t, resp, 200)
	m = decodeBody(t, resp)
	spec, _ = m["spec"].(map[string]interface{})
	rules, _ = spec["rules"].([]interface{})
	rule, _ = rules[0].(map[string]interface{})
	if rule["host"] != "new.example.com" {
		t.Fatalf("expected persisted host=new.example.com, got %v", rule["host"])
	}
}

func TestIngressUpdateNotFound(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := do(t, http.MethodPut, ts.URL+"/apis/networking.k8s.io/v1/namespaces/default/ingresses/missing", ingressBody("default", "missing", "example.com", "myservice", 80))
	assertStatus(t, resp, 404)
	resp.Body.Close()
}

func TestIngressDuplicate(t *testing.T) {
	ts, _ := newTestServer(t)
	body := ingressBody("default", "dup", "example.com", "myservice", 80)
	resp := post(t, ts.URL+"/apis/networking.k8s.io/v1/namespaces/default/ingresses", body)
	assertStatus(t, resp, 201)
	resp.Body.Close()

	resp = post(t, ts.URL+"/apis/networking.k8s.io/v1/namespaces/default/ingresses", body)
	assertStatus(t, resp, 409)
	resp.Body.Close()
}

func TestIngressRejectsInvalidHost(t *testing.T) {
	ts, _ := newTestServer(t)
	body := ingressBody("default", "badhost", "not a host!", "myservice", 80)
	resp := post(t, ts.URL+"/apis/networking.k8s.io/v1/namespaces/default/ingresses", body)
	assertStatus(t, resp, 400)
	resp.Body.Close()
}

func TestIngressAcceptsWildcardHost(t *testing.T) {
	ts, _ := newTestServer(t)
	body := ingressBody("default", "wildcard", "*.example.com", "myservice", 80)
	resp := post(t, ts.URL+"/apis/networking.k8s.io/v1/namespaces/default/ingresses", body)
	assertStatus(t, resp, 201)
	resp.Body.Close()
}

func TestIngressRejectsPathTraversal(t *testing.T) {
	ts, _ := newTestServer(t)
	body := ingressBody("default", "badpath", "example.com", "myservice", 80)
	spec := body["spec"].(map[string]interface{})
	rules := spec["rules"].([]interface{})
	rule := rules[0].(map[string]interface{})
	httpVal := rule["http"].(map[string]interface{})
	paths := httpVal["paths"].([]interface{})
	p := paths[0].(map[string]interface{})
	p["path"] = "/../../etc"

	resp := post(t, ts.URL+"/apis/networking.k8s.io/v1/namespaces/default/ingresses", body)
	assertStatus(t, resp, 400)
	resp.Body.Close()
}

func TestIngressRejectsNewlineInHost(t *testing.T) {
	ts, _ := newTestServer(t)
	body := ingressBody("default", "badnewline", "example.com\n\n[extra]\nrouter=evil", "myservice", 80)
	resp := post(t, ts.URL+"/apis/networking.k8s.io/v1/namespaces/default/ingresses", body)
	assertStatus(t, resp, 400)
	resp.Body.Close()
}

func TestIngressRejectsInvalidPort(t *testing.T) {
	ts, _ := newTestServer(t)
	body := ingressBody("default", "badport", "example.com", "myservice", 70000)
	resp := post(t, ts.URL+"/apis/networking.k8s.io/v1/namespaces/default/ingresses", body)
	assertStatus(t, resp, 400)
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

	// Explicit 0 is a real desired state (scale to zero) and must survive,
	// not be floored to the 1-replica default.
	patchBody = map[string]interface{}{
		"spec": map[string]interface{}{"replicas": 0},
	}
	resp = do(t, http.MethodPatch, ts.URL+"/apis/apps/v1/namespaces/default/deployments/scaled", patchBody)
	assertStatus(t, resp, 200)
	m = decodeBody(t, resp)
	spec = m["spec"].(map[string]interface{})
	if spec["replicas"] != float64(0) {
		t.Fatalf("expected replicas=0 after patch, got %v", spec["replicas"])
	}

	// Scale back up from zero via the /scale subresource.
	scaleBody = map[string]interface{}{
		"apiVersion": "autoscaling/v1",
		"kind":       "Scale",
		"spec":       map[string]interface{}{"replicas": 2},
	}
	resp = do(t, http.MethodPatch, ts.URL+"/apis/apps/v1/namespaces/default/deployments/scaled/scale", scaleBody)
	assertStatus(t, resp, 200)
	m = decodeBody(t, resp)
	scaleSpec = m["spec"].(map[string]interface{})
	if scaleSpec["replicas"] != float64(2) {
		t.Fatalf("expected replicas=2 after scale-up from zero, got %v", scaleSpec["replicas"])
	}
}

// TestDeploymentStatusForRollout pins tic-773b: a Deployment GET must carry
// metadata.generation and status.observedGeneration (>= generation) plus ready
// counts derived from live pods, or `kubectl rollout status` hangs forever on
// "Waiting for deployment spec update to be observed". Also: a spec-changing
// patch must bump generation; a non-spec change must not.
func TestDeploymentStatusForRollout(t *testing.T) {
	ts, st := newTestServer(t)

	body := deployBody("default", "roll", "nginx:latest")
	body["spec"].(map[string]interface{})["replicas"] = 2
	resp := post(t, ts.URL+"/apis/apps/v1/namespaces/default/deployments", body)
	assertStatus(t, resp, 201)
	m := decodeBody(t, resp)
	resp.Body.Close()
	meta := m["metadata"].(map[string]interface{})
	if meta["generation"] != float64(1) {
		t.Fatalf("expected generation=1 on create, got %v", meta["generation"])
	}

	// Two matching pods, both Running, as reconcilePodmanPods would import.
	for i := 0; i < 2; i++ {
		name := fmt.Sprintf("roll-%d", i)
		if _, err := st.CreatePod(&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		}); err != nil {
			t.Fatalf("CreatePod %s: %v", name, err)
		}
		st.UpdatePodPhase("default", name, corev1.PodRunning)
	}

	// GET must show observedGeneration == generation and ready == desired,
	// which is exactly what lets rollout status terminate.
	resp = get(t, ts.URL+"/apis/apps/v1/namespaces/default/deployments/roll")
	assertStatus(t, resp, 200)
	m = decodeBody(t, resp)
	resp.Body.Close()
	status := m["status"].(map[string]interface{})
	gen := m["metadata"].(map[string]interface{})["generation"]
	if status["observedGeneration"] != gen {
		t.Fatalf("observedGeneration %v != generation %v — rollout status would hang", status["observedGeneration"], gen)
	}
	if status["readyReplicas"] != float64(2) {
		t.Fatalf("expected readyReplicas=2 from Running pods, got %v", status["readyReplicas"])
	}

	// A spec-changing patch (rollout restart-style annotation on the template)
	// must bump generation.
	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"metadata": map[string]interface{}{
					"annotations": map[string]interface{}{"kubectl.kubernetes.io/restartedAt": "2026-09-20T00:00:00Z"},
				},
			},
		},
	}
	resp = do(t, http.MethodPatch, ts.URL+"/apis/apps/v1/namespaces/default/deployments/roll", patch)
	assertStatus(t, resp, 200)
	m = decodeBody(t, resp)
	resp.Body.Close()
	if g := m["metadata"].(map[string]interface{})["generation"]; g != float64(2) {
		t.Fatalf("expected generation bumped to 2 after spec-changing patch, got %v", g)
	}

	// And the subsequent GET re-observes the new generation (no permanent lag).
	resp = get(t, ts.URL+"/apis/apps/v1/namespaces/default/deployments/roll")
	assertStatus(t, resp, 200)
	m = decodeBody(t, resp)
	resp.Body.Close()
	status = m["status"].(map[string]interface{})
	if status["observedGeneration"] != float64(2) {
		t.Fatalf("observedGeneration did not catch up to gen 2: %v", status["observedGeneration"])
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

func TestPodListLabelSelector(t *testing.T) {
	ts, _ := newTestServer(t)

	mkPod := func(name string, labels map[string]string) {
		resp := post(t, ts.URL+"/api/v1/namespaces/default/pods", map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata":   map[string]interface{}{"name": name, "labels": labels},
			"spec": map[string]interface{}{
				"containers": []interface{}{map[string]interface{}{"name": name, "image": "img"}},
			},
		})
		assertStatus(t, resp, 201)
		resp.Body.Close()
	}
	mkPod("web-1", map[string]string{"app": "web", "tier": "frontend"})
	mkPod("web-2", map[string]string{"app": "web", "tier": "frontend"})
	mkPod("db-1", map[string]string{"app": "db", "tier": "backend"})

	names := func(resp *http.Response) []string {
		m := decodeBody(t, resp)
		items, _ := m["items"].([]interface{})
		var got []string
		for _, it := range items {
			obj, _ := it.(map[string]interface{})
			meta, _ := obj["metadata"].(map[string]interface{})
			got = append(got, meta["name"].(string))
		}
		return got
	}

	resp := get(t, ts.URL+"/api/v1/namespaces/default/pods?labelSelector=app%3Dweb")
	assertStatus(t, resp, 200)
	got := names(resp)
	if len(got) != 2 {
		t.Fatalf("expected 2 pods matching app=web, got %v", got)
	}

	resp = get(t, ts.URL+"/api/v1/namespaces/default/pods?labelSelector=app%3Ddb")
	assertStatus(t, resp, 200)
	got = names(resp)
	if len(got) != 1 || got[0] != "db-1" {
		t.Fatalf("expected only db-1 matching app=db, got %v", got)
	}

	resp = get(t, ts.URL+"/api/v1/namespaces/default/pods?labelSelector=app!%3Dweb")
	assertStatus(t, resp, 200)
	got = names(resp)
	if len(got) != 1 || got[0] != "db-1" {
		t.Fatalf("expected only db-1 matching app!=web, got %v", got)
	}

	resp = get(t, ts.URL+"/api/v1/namespaces/default/pods?labelSelector=nonexistent%3Dvalue")
	assertStatus(t, resp, 200)
	if got := names(resp); len(got) != 0 {
		t.Fatalf("expected no matches for nonexistent label, got %v", got)
	}
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

// --- Stub resources (daemonsets/statefulsets/replicasets) ---

func TestAppsStubResources(t *testing.T) {
	ts, _ := newTestServer(t)
	cases := []struct {
		resource string
		kind     string
	}{
		{"daemonsets", "DaemonSetList"},
		{"statefulsets", "StatefulSetList"},
		{"replicasets", "ReplicaSetList"},
	}
	for _, c := range cases {
		resp := get(t, ts.URL+"/apis/apps/v1/"+c.resource)
		assertStatus(t, resp, 200)
		m := decodeBody(t, resp)
		assertKind(t, m, c.kind)
		if items, _ := m["items"].([]interface{}); len(items) != 0 {
			t.Fatalf("%s: expected 0 items, got %d", c.resource, len(items))
		}

		resp = get(t, ts.URL+"/apis/apps/v1/namespaces/default/"+c.resource)
		assertStatus(t, resp, 200)
		assertKind(t, decodeBody(t, resp), c.kind)

		resp = do(t, http.MethodPost, ts.URL+"/apis/apps/v1/namespaces/default/"+c.resource, map[string]string{})
		assertStatus(t, resp, 405)
		resp.Body.Close()
	}
}

// --- Events ---

func TestClusterEventList(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := get(t, ts.URL+"/api/v1/events")
	assertStatus(t, resp, 200)
	assertKind(t, decodeBody(t, resp), "EventList")
}

func TestEventGeneration(t *testing.T) {
	ts, st := newTestServer(t)

	resp := post(t, ts.URL+"/api/v1/namespaces/default/pods", podBody("default", "evented", "nginx:latest"))
	assertStatus(t, resp, 201)
	resp.Body.Close()

	st.UpdatePodPhase("default", "evented", corev1.PodRunning)

	resp = do(t, http.MethodDelete, ts.URL+"/api/v1/namespaces/default/pods/evented", nil)
	assertStatus(t, resp, 200)
	resp.Body.Close()

	resp = get(t, ts.URL+"/api/v1/namespaces/default/events")
	assertStatus(t, resp, 200)
	m := decodeBody(t, resp)
	assertKind(t, m, "EventList")
	items, _ := m["items"].([]interface{})
	if len(items) != 3 {
		t.Fatalf("expected 3 events (Created/Started/Deleted), got %d: %v", len(items), items)
	}
	wantReasons := []string{"Created", "Started", "Deleted"}
	for i, want := range wantReasons {
		ev, _ := items[i].(map[string]interface{})
		if ev["reason"] != want {
			t.Fatalf("event %d: expected reason %q, got %v", i, want, ev["reason"])
		}
		obj, _ := ev["involvedObject"].(map[string]interface{})
		if obj["name"] != "evented" || obj["kind"] != "Pod" {
			t.Fatalf("event %d: unexpected involvedObject: %v", i, obj)
		}
	}

	// A different pod's events must not leak into a namespace that never had one.
	resp = get(t, ts.URL+"/api/v1/namespaces/other/events")
	assertStatus(t, resp, 200)
	m = decodeBody(t, resp)
	if items, _ := m["items"].([]interface{}); len(items) != 0 {
		t.Fatalf("expected 0 events in unrelated namespace, got %d", len(items))
	}

	// PATCH/DELETE aren't valid for a synthetic resource.
	resp = do(t, http.MethodDelete, ts.URL+"/api/v1/namespaces/default/events", nil)
	assertStatus(t, resp, 405)
	resp.Body.Close()
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

// --- StorageClasses ---

func TestStorageClassList(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := get(t, ts.URL+"/apis/storage.k8s.io/v1/storageclasses")
	assertStatus(t, resp, 200)
	m := decodeBody(t, resp)
	assertKind(t, m, "StorageClassList")

	items := m["items"].([]interface{})
	if len(items) != 3 {
		t.Fatalf("expected 3 storage classes, got %d", len(items))
	}

	names := make(map[string]bool)
	for _, item := range items {
		sc := item.(map[string]interface{})
		meta := sc["metadata"].(map[string]interface{})
		names[meta["name"].(string)] = true
	}
	for _, want := range []string{"standard", "standard-shared", "hostpath"} {
		if !names[want] {
			t.Errorf("missing storage class %q", want)
		}
	}
}

func TestStorageClassGet(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := get(t, ts.URL+"/apis/storage.k8s.io/v1/storageclasses/standard")
	assertStatus(t, resp, 200)
	m := decodeBody(t, resp)
	if m["kind"] != "StorageClass" {
		t.Fatalf("expected kind=StorageClass, got %v", m["kind"])
	}
	meta := m["metadata"].(map[string]interface{})
	if meta["name"] != "standard" {
		t.Fatalf("expected name=standard, got %v", meta["name"])
	}
}

func TestStorageClassGetNotFound(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := get(t, ts.URL+"/apis/storage.k8s.io/v1/storageclasses/nonexistent")
	assertStatus(t, resp, 404)
	resp.Body.Close()
}

// TestIngressRejectsBacktickAndQuoteInPath pins the charset fix: the path is
// spliced into a Traefik PathPrefix(`...`) rule and a YAML double-quoted
// scalar, so a backtick or quote breaks out of its enclosing context and
// injects rule/config syntax.
func TestIngressRejectsBacktickAndQuoteInPath(t *testing.T) {
	for _, bad := range []string{"/x` || Host(`evil.example`)", `/x" && anything`} {
		ts, _ := newTestServer(t)
		body := ingressBody("default", "badpath", "example.com", "myservice", 80)
		spec := body["spec"].(map[string]interface{})
		rule := spec["rules"].([]interface{})[0].(map[string]interface{})
		p := rule["http"].(map[string]interface{})["paths"].([]interface{})[0].(map[string]interface{})
		p["path"] = bad

		resp := post(t, ts.URL+"/apis/networking.k8s.io/v1/namespaces/default/ingresses", body)
		assertStatus(t, resp, 400)
		resp.Body.Close()
		ts.Close()
	}
}

// TestIngressNilBackendSkippedInTraefikConfig pins the nil-backend guard: an
// Ingress path without a Service backend used to panic inside
// generateTraefikConfig after the object was stored, leaving the client with
// a dropped connection and a half-created Ingress.
func TestIngressNilBackendSkippedInTraefikConfig(t *testing.T) {
	traefikDir := t.TempDir()
	certPEM, keyPEM := genTestCert(t)
	srv, err := server.New(server.Config{
		Store:      store.New(),
		CertPEM:    certPEM,
		KeyPEM:     keyPEM,
		TraefikDir: traefikDir,
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	// One rule with a valid path and one with no backend at all.
	body := map[string]interface{}{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "Ingress",
		"metadata":   map[string]interface{}{"name": "mixed", "namespace": "default"},
		"spec": map[string]interface{}{
			"rules": []interface{}{
				map[string]interface{}{
					"host": "example.com",
					"http": map[string]interface{}{
						"paths": []interface{}{
							map[string]interface{}{
								"path":     "/valid",
								"pathType": "Prefix",
								"backend":  map[string]interface{}{},
							},
							map[string]interface{}{
								"path":     "/real",
								"pathType": "Prefix",
								"backend": map[string]interface{}{
									"service": map[string]interface{}{
										"name": "svc",
										"port": map[string]interface{}{"number": 80},
									},
								},
							},
						},
					},
				},
			},
		},
	}
	resp := post(t, ts.URL+"/apis/networking.k8s.io/v1/namespaces/default/ingresses", body)
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	cfg, err := os.ReadFile(filepath.Join(traefikDir, "default-mixed.yaml"))
	if err != nil {
		t.Fatalf("traefik config not written: %v", err)
	}
	if strings.Contains(string(cfg), "/valid") {
		t.Errorf("nil-backend path leaked into traefik config:\n%s", cfg)
	}
	if !strings.Contains(string(cfg), "/real") {
		t.Errorf("valid path missing from traefik config:\n%s", cfg)
	}
}

// --- auth ---

// TestServerRejectsUnparseableCA pins fail-closed auth: a corrupt CA PEM must
// abort server startup, not silently disable client-cert verification.
func TestServerRejectsUnparseableCA(t *testing.T) {
	certPEM, keyPEM := genTestCert(t)
	_, err := server.New(server.Config{
		Store:   store.New(),
		CACert:  []byte("this is not a certificate"),
		CertPEM: certPEM,
		KeyPEM:  keyPEM,
	})
	if err == nil {
		t.Fatal("expected error for unparseable CA PEM, got none")
	}
	if !strings.Contains(err.Error(), "CA certificate") {
		t.Errorf("error should mention the CA certificate, got: %v", err)
	}
}

// TestPatchCannotRenameAcrossNamespace pins the PATCH identity check: a
// merge patch that changes metadata.namespace (or name) must be rejected
// instead of silently re-keying the stored object — or, when a same-named
// object exists at the target identity, overwriting that other object.
func TestPatchCannotRenameAcrossNamespace(t *testing.T) {
	ts, _ := newTestServer(t)

	// Create configmaps: default/steal-me and other/innocent.
	for _, name := range []string{"steal-me", "innocent"} {
		ns := "default"
		if name == "innocent" {
			ns = "other"
		}
		body := map[string]interface{}{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]interface{}{"name": name, "namespace": ns},
			"data":     map[string]interface{}{"k": "v"},
		}
		resp := post(t, ts.URL+"/api/v1/namespaces/"+ns+"/configmaps", body)
		assertStatus(t, resp, http.StatusCreated)
		resp.Body.Close()
	}

	// PATCH default/steal-me to claim other/innocent's identity.
	patch := []byte(`{"metadata":{"namespace":"other","name":"innocent"}}`)
	req, _ := http.NewRequest(http.MethodPatch,
		ts.URL+"/api/v1/namespaces/default/configmaps/steal-me", bytes.NewReader(patch))
	req.Header.Set("Content-Type", "application/merge-patch+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, resp, http.StatusBadRequest)
	resp.Body.Close()

	// other/innocent must be untouched and default/steal-me must still exist.
	for _, check := range []struct{ url, name string }{
		{ts.URL + "/api/v1/namespaces/other/configmaps/innocent", "innocent"},
		{ts.URL + "/api/v1/namespaces/default/configmaps/steal-me", "steal-me"},
	} {
		resp, err := http.Get(check.url)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusOK)
		var cm corev1.ConfigMap
		if err := json.NewDecoder(resp.Body).Decode(&cm); err != nil {
			t.Fatal(err)
		}
		if cm.Name != check.name {
			t.Errorf("object at %s was renamed to %q", check.url, cm.Name)
		}
	}
}

// --- Ingress selector-based endpoints ---

// ingressTestServer builds a server with a TraefikDir and returns its URL.
func ingressTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	traefikDir := t.TempDir()
	// QuadletDir must be set: deployment instance pods (which carry the
	// allocated replica ports) are only materialized when quadlet
	// generation runs.
	quadletDir := t.TempDir()
	certPEM, keyPEM := genTestCert(t)
	srv, err := server.New(server.Config{
		Store:      store.New(),
		CertPEM:    certPEM,
		KeyPEM:     keyPEM,
		TraefikDir: traefikDir,
		QuadletDir: quadletDir,
		ConfigDir:  filepath.Join(t.TempDir(), "configmaps"),
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, traefikDir
}

func postPodWithPort(t *testing.T, url, ns, name string, labels map[string]string, containerPort, hostPort int32) {
	t.Helper()
	body := map[string]interface{}{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]interface{}{"name": name, "namespace": ns, "labels": labels},
		"spec": map[string]interface{}{
			"containers": []interface{}{map[string]interface{}{
				"name":  "c",
				"image": "nginx",
				"ports": []interface{}{map[string]interface{}{
					"containerPort": containerPort,
					"hostPort":      hostPort,
				}},
			}},
		},
	}
	resp := post(t, url+"/api/v1/namespaces/"+ns+"/pods", body)
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()
}

func readTraefikConfig(t *testing.T, dir, ns, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, ns+"-"+name+".yaml"))
	if err != nil {
		t.Fatalf("read traefik config: %v", err)
	}
	return string(b)
}

// TestIngressEndpointsFromSelectorPods: a Service selector resolves to every
// matching pod, each contributing its own hostPort — different ports per pod
// are fine, exactly like k8s Endpoints.
func TestIngressEndpointsFromSelectorPods(t *testing.T) {
	ts, traefikDir := ingressTestServer(t)

	// Three standalone pods, same labels, distinct hostPorts.
	for i, hp := range []int32{20001, 20002, 20003} {
		postPodWithPort(t, ts.URL, "default", fmt.Sprintf("web-%d", i),
			map[string]string{"app": "web"}, 8080, hp)
	}

	// Service selecting them, with a targetPort.
	svc := map[string]interface{}{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]interface{}{"name": "web", "namespace": "default"},
		"spec": map[string]interface{}{
			"selector": map[string]string{"app": "web"},
			"ports":    []interface{}{map[string]interface{}{"port": 80, "targetPort": 8080}},
		},
	}
	resp := post(t, ts.URL+"/api/v1/namespaces/default/services", svc)
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	resp = post(t, ts.URL+"/apis/networking.k8s.io/v1/namespaces/default/ingresses",
		ingressBody("default", "web", "example.com", "web", 80))
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	cfg := readTraefikConfig(t, traefikDir, "default", "web")
	for _, want := range []string{
		`- url: "http://127.0.0.1:20001"`,
		`- url: "http://127.0.0.1:20002"`,
		`- url: "http://127.0.0.1:20003"`,
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("missing server %s in:\n%s", want, cfg)
		}
	}
	if strings.Contains(cfg, "localhost") {
		t.Errorf("fallback localhost URL leaked into selector-resolved config:\n%s", cfg)
	}
}

// TestIngressSkipsPodsWithoutHostPort: pods that publish nothing are not
// addressable by an on-host Traefik and must be left out of the server list.
func TestIngressSkipsPodsWithoutHostPort(t *testing.T) {
	ts, traefikDir := ingressTestServer(t)

	postPodWithPort(t, ts.URL, "default", "pub", map[string]string{"app": "web"}, 8080, 20010)
	// containerPort only — no hostPort.
	body := map[string]interface{}{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]interface{}{"name": "dark", "namespace": "default", "labels": map[string]string{"app": "web"}},
		"spec": map[string]interface{}{
			"containers": []interface{}{map[string]interface{}{
				"name": "c", "image": "nginx",
				"ports": []interface{}{map[string]interface{}{"containerPort": 8080}},
			}},
		},
	}
	resp := post(t, ts.URL+"/api/v1/namespaces/default/pods", body)
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	svc := map[string]interface{}{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]interface{}{"name": "web", "namespace": "default"},
		"spec": map[string]interface{}{
			"selector": map[string]string{"app": "web"},
			"ports":    []interface{}{map[string]interface{}{"port": 80, "targetPort": 8080}},
		},
	}
	resp = post(t, ts.URL+"/api/v1/namespaces/default/services", svc)
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	resp = post(t, ts.URL+"/apis/networking.k8s.io/v1/namespaces/default/ingresses",
		ingressBody("default", "web", "example.com", "web", 80))
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	cfg := readTraefikConfig(t, traefikDir, "default", "web")
	if !strings.Contains(cfg, `http://127.0.0.1:20010`) {
		t.Errorf("published pod missing from config:\n%s", cfg)
	}
	if got := strings.Count(cfg, "- url:"); got != 1 {
		t.Errorf("expected exactly 1 server, got %d:\n%s", got, cfg)
	}
}

// TestIngressFallbackWithoutService: no Service (or none with reachable
// pods) falls back to the ingress-declared port on localhost.
func TestIngressFallbackWithoutService(t *testing.T) {
	ts, traefikDir := ingressTestServer(t)

	resp := post(t, ts.URL+"/apis/networking.k8s.io/v1/namespaces/default/ingresses",
		ingressBody("default", "lonely", "example.com", "nosuch", 8080))
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	cfg := readTraefikConfig(t, traefikDir, "default", "lonely")
	if !strings.Contains(cfg, `- url: "http://localhost:8080"`) {
		t.Errorf("fallback URL missing:\n%s", cfg)
	}
}

// TestIngressRegeneratesOnPodChurn: creating a selector-matching pod (or
// deleting one) rewrites the Traefik config, so the servers list tracks
// reality the way Endpoints do.
func TestIngressRegeneratesOnPodChurn(t *testing.T) {
	ts, traefikDir := ingressTestServer(t)

	svc := map[string]interface{}{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]interface{}{"name": "web", "namespace": "default"},
		"spec": map[string]interface{}{
			"selector": map[string]string{"app": "web"},
			"ports":    []interface{}{map[string]interface{}{"port": 80, "targetPort": 8080}},
		},
	}
	resp := post(t, ts.URL+"/api/v1/namespaces/default/services", svc)
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	resp = post(t, ts.URL+"/apis/networking.k8s.io/v1/namespaces/default/ingresses",
		ingressBody("default", "web", "example.com", "web", 80))
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	// Before any pod: fallback.
	cfg := readTraefikConfig(t, traefikDir, "default", "web")
	if !strings.Contains(cfg, "localhost:80") {
		t.Errorf("expected fallback before pods exist:\n%s", cfg)
	}

	// Create a matching pod -> config picks up its hostPort.
	postPodWithPort(t, ts.URL, "default", "web-0", map[string]string{"app": "web"}, 8080, 20077)
	cfg = readTraefikConfig(t, traefikDir, "default", "web")
	if !strings.Contains(cfg, "127.0.0.1:20077") {
		t.Errorf("pod create did not regenerate config:\n%s", cfg)
	}

	// Delete it -> back to fallback.
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/namespaces/default/pods/web-0", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	cfg = readTraefikConfig(t, traefikDir, "default", "web")
	if strings.Contains(cfg, "20077") {
		t.Errorf("pod delete did not regenerate config:\n%s", cfg)
	}
}

// TestIngressNamedTargetPort: a string targetPort resolves through the
// container port name, not just numbers.
func TestIngressNamedTargetPort(t *testing.T) {
	ts, traefikDir := ingressTestServer(t)

	body := map[string]interface{}{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]interface{}{"name": "named", "namespace": "default", "labels": map[string]string{"app": "web"}},
		"spec": map[string]interface{}{
			"containers": []interface{}{map[string]interface{}{
				"name":  "c",
				"image": "nginx",
				"ports": []interface{}{map[string]interface{}{
					"name": "http", "containerPort": 8080, "hostPort": 20090,
				}},
			}},
		},
	}
	resp := post(t, ts.URL+"/api/v1/namespaces/default/pods", body)
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	svc := map[string]interface{}{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]interface{}{"name": "web", "namespace": "default"},
		"spec": map[string]interface{}{
			"selector": map[string]string{"app": "web"},
			"ports":    []interface{}{map[string]interface{}{"port": 80, "targetPort": "http"}},
		},
	}
	resp = post(t, ts.URL+"/api/v1/namespaces/default/services", svc)
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	resp = post(t, ts.URL+"/apis/networking.k8s.io/v1/namespaces/default/ingresses",
		ingressBody("default", "web", "example.com", "web", 80))
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	cfg := readTraefikConfig(t, traefikDir, "default", "web")
	if !strings.Contains(cfg, "127.0.0.1:20090") {
		t.Errorf("named targetPort not resolved:\n%s", cfg)
	}
}

// --- Deployment replica ports through Traefik (kube-proxy emulation) ---

func postDeployment(t *testing.T, url, ns, name string, replicas int, labels map[string]string) {
	t.Helper()
	n := int32(replicas)
	body := map[string]interface{}{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]interface{}{"name": name, "namespace": ns},
		"spec": map[string]interface{}{
			"replicas": n,
			"selector": map[string]interface{}{"matchLabels": labels},
			"template": map[string]interface{}{
				"metadata": map[string]interface{}{"labels": labels},
				"spec": map[string]interface{}{
					"containers": []interface{}{map[string]interface{}{
						"name": "c", "image": "nginx",
						"ports": []interface{}{map[string]interface{}{"containerPort": 8080}},
					}},
				},
			},
		},
	}
	resp := post(t, url+"/apis/apps/v1/namespaces/"+ns+"/deployments", body)
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()
}

func patchDeploymentScale(t *testing.T, url, ns, name string, replicas int) {
	t.Helper()
	body := []byte(fmt.Sprintf(`{"spec":{"replicas":%d}}`, replicas))
	req, _ := http.NewRequest(http.MethodPatch,
		url+"/apis/apps/v1/namespaces/"+ns+"/deployments/"+name+"/scale", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/merge-patch+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scale patch: %s", resp.Status)
	}
}

func countTraefikServers(t *testing.T, dir, ns, name string) (int, []string) {
	t.Helper()
	cfg := readTraefikConfig(t, dir, ns, name)
	var urls []string
	for _, line := range strings.Split(cfg, "\n") {
		if strings.Contains(line, "- url:") {
			urls = append(urls, strings.TrimSpace(line))
		}
	}
	return len(urls), urls
}

// TestDeploymentReplicasLoadBalancedThroughTraefik is the full
// kube-proxy-emulation scenario: one Deployment, N replicas, one Service,
// one Ingress — and a Traefik servers list with one loopback URL per
// replica, each on its own allocated port.
func TestDeploymentReplicasLoadBalancedThroughTraefik(t *testing.T) {
	ts, traefikDir := ingressTestServer(t)

	postDeployment(t, ts.URL, "default", "web", 3, map[string]string{"app": "web"})

	svc := map[string]interface{}{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]interface{}{"name": "web", "namespace": "default"},
		"spec": map[string]interface{}{
			"selector": map[string]string{"app": "web"},
			"ports":    []interface{}{map[string]interface{}{"port": 80, "targetPort": 8080}},
		},
	}
	resp := post(t, ts.URL+"/api/v1/namespaces/default/services", svc)
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	resp = post(t, ts.URL+"/apis/networking.k8s.io/v1/namespaces/default/ingresses",
		ingressBody("default", "web", "example.com", "web", 80))
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	n, urls := countTraefikServers(t, traefikDir, "default", "web")
	if n != 3 {
		t.Fatalf("expected 3 servers (one per replica), got %d:\n%s", n, urls)
	}
	seen := map[string]bool{}
	for _, u := range urls {
		if seen[u] {
			t.Errorf("duplicate server %s", u)
		}
		seen[u] = true
		if !strings.Contains(u, "127.0.0.1:") {
			t.Errorf("server not loopback-bound: %s", u)
		}
	}

	// Scale down to 1: the servers list shrinks.
	patchDeploymentScale(t, ts.URL, "default", "web", 1)
	n, _ = countTraefikServers(t, traefikDir, "default", "web")
	if n != 1 {
		t.Fatalf("expected 1 server after scale-down, got %d", n)
	}

	// Scale back to 3: three distinct servers again (freed ports reused).
	patchDeploymentScale(t, ts.URL, "default", "web", 3)
	n, urls = countTraefikServers(t, traefikDir, "default", "web")
	if n != 3 {
		t.Fatalf("expected 3 servers after scale-up, got %d: %v", n, urls)
	}

	// Delete the deployment: back to the localhost fallback.
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/apis/apps/v1/namespaces/default/deployments/web", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	cfg := readTraefikConfig(t, traefikDir, "default", "web")
	if strings.Contains(cfg, "127.0.0.1:2") {
		t.Errorf("allocated ports still present after deployment delete:\n%s", cfg)
	}
	if !strings.Contains(cfg, "localhost:80") {
		t.Errorf("expected fallback after deployment delete:\n%s", cfg)
	}
}

// TestServiceCRUDWritesNoUnitFiles pins the removal of the dead Service
// ".socket" feature: Service ports are a port map (DNS aliases + ingress
// backends), never a host binding. Creating, patching, or deleting a
// Service must not leave any unit file behind.
func TestServiceCRUDWritesNoUnitFiles(t *testing.T) {
	quadletDir := t.TempDir()
	st := store.New()
	certPEM, keyPEM := genTestCert(t)
	srv, err := server.New(server.Config{
		Store:      st,
		CertPEM:    certPEM,
		KeyPEM:     keyPEM,
		QuadletDir: quadletDir,
		ConfigDir:  filepath.Join(t.TempDir(), "configmaps"),
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	svc := map[string]interface{}{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]interface{}{"name": "web", "namespace": "default"},
		"spec": map[string]interface{}{
			"selector": map[string]string{"app": "web"},
			"ports":    []interface{}{map[string]interface{}{"port": 80, "targetPort": 8080}},
		},
	}
	resp := post(t, ts.URL+"/api/v1/namespaces/default/services", svc)
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	patch := []byte(`{"spec":{"ports":[{"port":8081,"targetPort":8080}]}}`)
	req, _ := http.NewRequest(http.MethodPatch, ts.URL+"/api/v1/namespaces/default/services/web", bytes.NewReader(patch))
	req.Header.Set("Content-Type", "application/merge-patch+json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	entries, err := os.ReadDir(quadletDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		for _, e := range entries {
			t.Errorf("Service CRUD wrote unexpected file: %s", e.Name())
		}
	}

	req, _ = http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/namespaces/default/services/web", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()
}

// --- hardening: body limit + WS frame cap ---

// TestRequestBodyLimitRejects413: a body over the cap must get a clean 413
// Status, not an OOM or a hung connection.
func TestRequestBodyLimitRejects413(t *testing.T) {
	ts, _ := newTestServer(t)

	// ~4MB of ConfigMap data.
	big := strings.Repeat("x", (4<<20)+16)
	body, _ := json.Marshal(map[string]interface{}{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]interface{}{"name": "big", "namespace": "default"},
		"data":     map[string]string{"k": big},
	})
	resp, err := http.Post(ts.URL+"/api/v1/namespaces/default/configmaps", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %s", resp.Status)
	}

	// Same via PATCH (readBody path). First create a small configmap.
	small, _ := json.Marshal(map[string]interface{}{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]interface{}{"name": "small", "namespace": "default"},
		"data":     map[string]string{"a": "b"},
	})
	resp2, err := http.Post(ts.URL+"/api/v1/namespaces/default/configmaps", "application/json", bytes.NewReader(small))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()

	patchBody := bytes.Repeat([]byte("x"), (4<<20)+16)
	req, _ := http.NewRequest(http.MethodPatch, ts.URL+"/api/v1/namespaces/default/configmaps/small", bytes.NewReader(patchBody))
	req.Header.Set("Content-Type", "application/merge-patch+json")
	resp3, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("PATCH: expected 413, got %s", resp3.Status)
	}
}

// --- pod PATCH dry-run + materialization events ---

// TestPodPatchRejectsUnrenderableSpec: a patch that would corrupt the
// generated unit file must 400, not 200-while-runtime-keeps-old-spec.
func TestPodPatchRejectsUnrenderableSpec(t *testing.T) {
	ts, _ := newTestServer(t)

	// Valid pod.
	body := map[string]interface{}{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]interface{}{"name": "p1", "namespace": "default"},
		"spec": map[string]interface{}{
			"containers": []interface{}{map[string]interface{}{"name": "c", "image": "nginx:latest"}},
		},
	}
	resp := post(t, ts.URL+"/api/v1/namespaces/default/pods", body)
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	// Patch the image to one that breaks unit-file rendering.
	patch := []byte(`{"spec":{"containers":[{"name":"c","image":"nginx\n[Service]\nExecStartPre=evil"}]}}`)
	req, _ := http.NewRequest(http.MethodPatch, ts.URL+"/api/v1/namespaces/default/pods/p1", bytes.NewReader(patch))
	req.Header.Set("Content-Type", "application/merge-patch+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for unrenderable patch, got %s", resp.Status)
	}
}

// TestPodMaterializationFailureRecordsEvent: a pod whose env references a
// missing secret is accepted (k8s allows creating pod before its secret),
// but the failure to materialize it must surface as a Warning event —
// visible via kubectl describe — instead of only the server log.
func TestPodMaterializationFailureRecordsEvent(t *testing.T) {
	quadletDir := t.TempDir()
	st := store.New()
	certPEM, keyPEM := genTestCert(t)
	srv, err := server.New(server.Config{
		Store:      st,
		CertPEM:    certPEM,
		KeyPEM:     keyPEM,
		QuadletDir: quadletDir,
		ConfigDir:  filepath.Join(t.TempDir(), "configmaps"),
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	body := map[string]interface{}{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]interface{}{"name": "needsec", "namespace": "default"},
		"spec": map[string]interface{}{
			"containers": []interface{}{map[string]interface{}{
				"name": "c", "image": "nginx:latest",
				"env": []interface{}{map[string]interface{}{
					"name": "PW",
					"valueFrom": map[string]interface{}{
						"secretKeyRef": map[string]interface{}{"name": "no-such", "key": "pw"},
					},
				}},
			}},
		},
	}
	resp := post(t, ts.URL+"/api/v1/namespaces/default/pods", body)
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	// The Warning event must be in the events feed.
	resp, err = http.Get(ts.URL + "/api/v1/namespaces/default/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "MaterializationFailed") {
		t.Errorf("expected MaterializationFailed event, got:\n%s", raw)
	}
	if !strings.Contains(string(raw), "no-such") {
		t.Errorf("event should mention the missing secret, got:\n%s", raw)
	}
}

// --- deletecollection ---

// TestDeleteCollectionPodsAndDeployments pins the deletecollection verb
// discovery advertises: DELETE on the collection path removes every object
// in the namespace (kubectl delete pods --all / deployments --all).
func TestDeleteCollectionPodsAndDeployments(t *testing.T) {
	quadletDir := t.TempDir()
	certPEM, keyPEM := genTestCert(t)
	srv, err := server.New(server.Config{
		Store:      store.New(),
		CertPEM:    certPEM,
		KeyPEM:     keyPEM,
		QuadletDir: quadletDir,
		ConfigDir:  filepath.Join(t.TempDir(), "configmaps"),
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	// Two pods + two deployments.
	for i := 0; i < 2; i++ {
		body := map[string]interface{}{
			"apiVersion": "v1", "kind": "Pod",
			"metadata": map[string]interface{}{"name": fmt.Sprintf("p%d", i), "namespace": "default"},
			"spec": map[string]interface{}{
				"containers": []interface{}{map[string]interface{}{"name": "c", "image": "nginx:latest"}},
			},
		}
		resp := post(t, ts.URL+"/api/v1/namespaces/default/pods", body)
		assertStatus(t, resp, http.StatusCreated)
		resp.Body.Close()
		postDeployment(t, ts.URL, "default", fmt.Sprintf("d%d", i), 1, map[string]string{"app": "d"})
	}

	// Collection-delete pods.
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/namespaces/default/pods", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp, _ = http.Get(ts.URL + "/api/v1/namespaces/default/pods")
	var list corev1.PodList
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list.Items) != 0 {
		t.Errorf("expected 0 pods after collection delete, got %d", len(list.Items))
	}

	// Collection-delete deployments.
	req, _ = http.NewRequest(http.MethodDelete, ts.URL+"/apis/apps/v1/namespaces/default/deployments", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp, _ = http.Get(ts.URL + "/apis/apps/v1/namespaces/default/deployments")
	var deps appsv1.DeploymentList
	json.NewDecoder(resp.Body).Decode(&deps)
	resp.Body.Close()
	if len(deps.Items) != 0 {
		t.Errorf("expected 0 deployments after collection delete, got %d", len(deps.Items))
	}
}

// --- NodePort services ---

// nodePortSvcBody builds a type: NodePort service body; nodePort 0 means
// "let q8s allocate".
func nodePortSvcBody(ns, name string, port, nodePort int) map[string]interface{} {
	p := map[string]interface{}{"port": port, "protocol": "TCP", "targetPort": port}
	if nodePort != 0 {
		p["nodePort"] = nodePort
	}
	return map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Service",
		"metadata":   map[string]interface{}{"name": name, "namespace": ns},
		"spec": map[string]interface{}{
			"type":     "NodePort",
			"selector": map[string]interface{}{"app": name},
			"ports":    []interface{}{p},
		},
	}
}

// svcNodePort pulls spec.ports[0].nodePort out of a decoded service body.
func svcNodePort(t *testing.T, m map[string]interface{}) int {
	t.Helper()
	spec, ok := m["spec"].(map[string]interface{})
	if !ok {
		t.Fatalf("no spec in service: %v", m)
	}
	ports, ok := spec["ports"].([]interface{})
	if !ok || len(ports) == 0 {
		t.Fatalf("no ports in service spec: %v", spec)
	}
	p0 := ports[0].(map[string]interface{})
	np, ok := p0["nodePort"].(float64)
	if !ok {
		t.Fatalf("no nodePort in port[0]: %v", p0)
	}
	return int(np)
}

// TestNodePortAutoAllocated: a NodePort service with no explicit nodePort
// gets one assigned in range and returned in the created object.
func TestNodePortAutoAllocated(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := post(t, ts.URL+"/api/v1/namespaces/default/services", nodePortSvcBody("default", "web", 80, 0))
	assertStatus(t, resp, 201)
	np := svcNodePort(t, decodeBody(t, resp))
	if np < store.NodePortMin || np > store.NodePortMax {
		t.Fatalf("auto-allocated nodePort %d out of range %d-%d", np, store.NodePortMin, store.NodePortMax)
	}
}

// TestNodePortExplicitHonored: an in-range explicit nodePort is kept.
func TestNodePortExplicitHonored(t *testing.T) {
	ts, _ := newTestServer(t)
	want := store.NodePortMin + 100
	resp := post(t, ts.URL+"/api/v1/namespaces/default/services", nodePortSvcBody("default", "web", 80, want))
	assertStatus(t, resp, 201)
	if np := svcNodePort(t, decodeBody(t, resp)); np != want {
		t.Fatalf("explicit nodePort not honored: got %d want %d", np, want)
	}
}

// TestNodePortOutOfRangeRejected: an explicit nodePort below the range is a 409.
func TestNodePortOutOfRangeRejected(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := post(t, ts.URL+"/api/v1/namespaces/default/services", nodePortSvcBody("default", "web", 80, 8080))
	assertStatus(t, resp, 409)
	resp.Body.Close()
}

// TestNodePortDuplicateRejected: two services can't claim the same nodePort.
func TestNodePortDuplicateRejected(t *testing.T) {
	ts, _ := newTestServer(t)
	want := store.NodePortMin + 200
	resp := post(t, ts.URL+"/api/v1/namespaces/default/services", nodePortSvcBody("default", "a", 80, want))
	assertStatus(t, resp, 201)
	resp.Body.Close()
	resp = post(t, ts.URL+"/api/v1/namespaces/default/services", nodePortSvcBody("default", "b", 80, want))
	assertStatus(t, resp, 409)
	resp.Body.Close()
}

// TestSecretStringDataFoldedToData is the regression test for tic-b31a: a
// Secret created with only stringData must be stored with its content in
// Data (StringData cleared), so it survives a restart where restoreSecretFiles
// rewrites the on-disk files from Data alone. Before the fix, content lived
// only in StringData, the store persisted it, but restore wrote empty files.
func TestSecretStringDataFoldedToData(t *testing.T) {
	ts, st := newTestServer(t)
	body := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]interface{}{"name": "tls", "namespace": "default"},
		"stringData": map[string]interface{}{"cert.pem": "PEMDATA", "key.pem": "KEYDATA"},
	}
	resp := post(t, ts.URL+"/api/v1/namespaces/default/secrets", body)
	assertStatus(t, resp, 201)
	resp.Body.Close()

	sec, err := st.GetSecret("default", "tls")
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	if len(sec.StringData) != 0 {
		t.Errorf("StringData should be cleared after folding, got %v", sec.StringData)
	}
	if got := string(sec.Data["cert.pem"]); got != "PEMDATA" {
		t.Errorf("cert.pem not folded into Data: got %q", got)
	}
	if got := string(sec.Data["key.pem"]); got != "KEYDATA" {
		t.Errorf("key.pem not folded into Data: got %q", got)
	}
}

// TestSecretStringDataWinsOnConflict: when both data and stringData carry the
// same key, stringData wins — matching the Kubernetes API server.
func TestSecretStringDataWinsOnConflict(t *testing.T) {
	ts, st := newTestServer(t)
	body := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]interface{}{"name": "both", "namespace": "default"},
		// data is base64("old"), stringData is raw "new" — stringData should win
		"data":       map[string]interface{}{"k": "b2xk"},
		"stringData": map[string]interface{}{"k": "new"},
	}
	resp := post(t, ts.URL+"/api/v1/namespaces/default/secrets", body)
	assertStatus(t, resp, 201)
	resp.Body.Close()

	sec, err := st.GetSecret("default", "both")
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	if got := string(sec.Data["k"]); got != "new" {
		t.Errorf("stringData should win on conflict: got %q want %q", got, "new")
	}
	if len(sec.StringData) != 0 {
		t.Errorf("StringData should be cleared, got %v", sec.StringData)
	}
}
