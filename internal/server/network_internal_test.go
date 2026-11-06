package server

import (
	"bytes"
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
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"q8s/internal/store"
)

type netTestCall struct {
	Network   string
	Container string
	IP        string
}

// fakeNetTestOps records every network operation and serves canned subnets.
// Tests replace Server.network.ops with it, so nothing shells out to podman.
type fakeNetTestOps struct {
	mu           sync.Mutex
	subnets      map[string]string
	ensures      []string
	connects     []netTestCall
	disconnects  []netTestCall
	onDisconnect func(network, container string)
}

func (f *fakeNetTestOps) EnsureNetwork(ns string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensures = append(f.ensures, ns)
	return nil
}

func (f *fakeNetTestOps) NetworkSubnet(name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	subnet, ok := f.subnets[name]
	if !ok {
		return "", fmt.Errorf("no such network %s", name)
	}
	return subnet, nil
}

func (f *fakeNetTestOps) NetworkConnect(network, container, ip string) error {
	f.mu.Lock()
	f.connects = append(f.connects, netTestCall{Network: network, Container: container, IP: ip})
	f.mu.Unlock()
	return nil
}

func (f *fakeNetTestOps) NetworkDisconnect(network, container string) error {
	if f.onDisconnect != nil {
		f.onDisconnect(network, container)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disconnects = append(f.disconnects, netTestCall{Network: network, Container: container})
	return nil
}

func (f *fakeNetTestOps) ensureCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.ensures)
}

func (f *fakeNetTestOps) connectCalls() []netTestCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]netTestCall(nil), f.connects...)
}

func (f *fakeNetTestOps) disconnectCalls() []netTestCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]netTestCall(nil), f.disconnects...)
}

func netTestCertPEM(t *testing.T) (certPEM, keyPEM []byte) {
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
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// newNetTestServer builds a Manager-nil server (as the existing tests do)
// and installs the fake network operations explicitly.
func newNetTestServer(t *testing.T, st *store.Store, quadletDir, systemdDir string) (*Server, *httptest.Server, *fakeNetTestOps) {
	t.Helper()
	certPEM, keyPEM := netTestCertPEM(t)
	srv, err := New(Config{
		Store:      st,
		CertPEM:    certPEM,
		KeyPEM:     keyPEM,
		QuadletDir: quadletDir,
		SystemdDir: systemdDir,
		ConfigDir:  filepath.Join(t.TempDir(), "configmaps"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	fake := &fakeNetTestOps{subnets: map[string]string{}}
	srv.network.ops = fake
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return srv, ts, fake
}

func netTestRequest(t *testing.T, method, url string, body interface{}) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, reader)
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

func netTestCreateNamespace(t *testing.T, ts *httptest.Server, name string) {
	t.Helper()
	resp := netTestRequest(t, http.MethodPost, ts.URL+"/api/v1/namespaces", map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata":   map[string]interface{}{"name": name},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create namespace %s: status %d", name, resp.StatusCode)
	}
}

func netTestCreatePod(t *testing.T, ts *httptest.Server, ns, name string, annotations map[string]string) {
	t.Helper()
	metadata := map[string]interface{}{"name": name, "namespace": ns}
	if annotations != nil {
		metadata["annotations"] = annotations
	}
	resp := netTestRequest(t, http.MethodPost, ts.URL+"/api/v1/namespaces/"+ns+"/pods", map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   metadata,
		"spec": map[string]interface{}{
			"containers": []interface{}{
				map[string]interface{}{"name": name, "image": "traefik:v3"},
			},
		},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("create pod %s/%s: status %d: %s", ns, name, resp.StatusCode, raw)
	}
}

func netTestReadFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

func TestNetworkAttachments(t *testing.T) {
	st := store.New()
	for _, ns := range []string{"a", "b"} {
		if _, err := st.CreateNamespace(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			t.Fatalf("CreateNamespace %s: %v", ns, err)
		}
	}
	srv, _, fake := newNetTestServer(t, st, t.TempDir(), t.TempDir())
	fake.subnets["q8s-default"] = "10.89.1.0/24"
	fake.subnets["q8s-a"] = "10.89.2.0/24"
	fake.subnets["q8s-b"] = "10.89.3.0/24"

	if got := srv.networkAttachments(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "plain", Namespace: "a"},
	}); got != nil {
		t.Fatalf("pod without annotation: %v, want nil", got)
	}
	if got := srv.networkAttachments(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "host",
			Namespace:   "a",
			Annotations: map[string]string{annAttachAllNetworks: "true"},
		},
		Spec: corev1.PodSpec{HostNetwork: true},
	}); got != nil {
		t.Fatalf("hostNetwork pod: %v, want nil", got)
	}

	got := srv.networkAttachments(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "traefik",
			Namespace:   "a",
			Annotations: map[string]string{annAttachAllNetworks: "true"},
		},
	})
	want := map[string]string{
		"default": "10.89.1.254",
		"a":       "10.89.2.254",
		"b":       "10.89.3.254",
	}
	if len(got) != len(want) {
		t.Fatalf("attachments = %v, want %d entries", got, len(want))
	}
	for _, attachment := range got {
		if attachment.IP != want[attachment.Namespace] {
			t.Errorf("attachment %s IP = %q, want %q", attachment.Namespace, attachment.IP, want[attachment.Namespace])
		}
	}
}

func TestNetworkAttachmentsSubnetLookupFailure(t *testing.T) {
	st := store.New()
	srv, _, fake := newNetTestServer(t, st, t.TempDir(), t.TempDir())
	// No subnets configured: every lookup fails, but the attachment list is
	// still rendered with an empty IP so the Network= line survives.
	got := srv.networkAttachments(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "traefik",
			Namespace:   "default",
			Annotations: map[string]string{annAttachAllNetworks: "true"},
		},
	})
	if len(got) != 1 || got[0].Namespace != "default" || got[0].IP != "" {
		t.Fatalf("attachments = %v, want default with empty IP", got)
	}
	if fake.ensureCount() != 0 {
		t.Fatalf("networkAttachments must not create networks, got %d ensures", fake.ensureCount())
	}
}

// TestOrdinaryPodNeverJoinsOtherNamespaceNetworks is the isolation guard:
// without the opt-in annotation a pod renders only its own Network= line and
// namespace creation never connects it to anything.
func TestOrdinaryPodNeverJoinsOtherNamespaceNetworks(t *testing.T) {
	st := store.New()
	quadletDir := t.TempDir()
	_, ts, fake := newNetTestServer(t, st, quadletDir, t.TempDir())
	fake.subnets["q8s-default"] = "10.89.1.0/24"
	fake.subnets["q8s-a"] = "10.89.2.0/24"
	fake.subnets["q8s-b"] = "10.89.3.0/24"
	fake.subnets["q8s-c"] = "10.89.4.0/24"

	netTestCreateNamespace(t, ts, "a")
	netTestCreateNamespace(t, ts, "b")
	netTestCreatePod(t, ts, "a", "web", nil)
	netTestCreateNamespace(t, ts, "c")

	content := netTestReadFile(t, filepath.Join(quadletDir, "a-web.container"))
	if !netTestContainsLine(content, "Network=q8s-a.network") {
		t.Fatalf("ordinary pod missing own Network= line:\n%s", content)
	}
	for _, other := range []string{"q8s-b.network", "q8s-c.network", "q8s-default.network"} {
		if netTestContains(content, other) {
			t.Fatalf("ordinary pod joined %s:\n%s", other, content)
		}
	}
	if calls := fake.connectCalls(); len(calls) != 0 {
		t.Fatalf("ordinary pod got NetworkConnect calls: %v", calls)
	}
}

func TestNamespaceCreateAttachesOptedInPod(t *testing.T) {
	st := store.New()
	quadletDir := t.TempDir()
	_, ts, fake := newNetTestServer(t, st, quadletDir, t.TempDir())
	fake.subnets["q8s-default"] = "10.89.1.0/24"
	fake.subnets["q8s-a"] = "10.89.2.0/24"
	fake.subnets["q8s-c"] = "10.89.3.0/24"

	netTestCreateNamespace(t, ts, "a")
	netTestCreatePod(t, ts, "a", "traefik", map[string]string{annAttachAllNetworks: "true"})
	netTestCreateNamespace(t, ts, "c")

	calls := fake.connectCalls()
	want := netTestCall{Network: "q8s-c", Container: "a-traefik", IP: "10.89.3.254"}
	found := false
	for _, call := range calls {
		if call == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("NetworkConnect calls = %v, want %v", calls, want)
	}

	content := netTestReadFile(t, filepath.Join(quadletDir, "a-traefik.container"))
	if !netTestContainsLine(content, "Network=q8s-c.network:ip=10.89.3.254") {
		t.Fatalf("quadlet missing new namespace network line:\n%s", content)
	}
}

func TestNamespaceDeleteDisconnectsBeforeRemovingNetwork(t *testing.T) {
	st := store.New()
	quadletDir := t.TempDir()
	_, ts, fake := newNetTestServer(t, st, quadletDir, t.TempDir())
	fake.subnets["q8s-default"] = "10.89.1.0/24"
	fake.subnets["q8s-a"] = "10.89.2.0/24"
	fake.subnets["q8s-b"] = "10.89.3.0/24"

	netTestCreateNamespace(t, ts, "a")
	netTestCreateNamespace(t, ts, "b")
	netTestCreatePod(t, ts, "a", "traefik", map[string]string{annAttachAllNetworks: "true"})

	networkFile := filepath.Join(quadletDir, "q8s-b.network")
	if _, err := os.Stat(networkFile); err != nil {
		t.Fatalf("expected network quadlet before delete: %v", err)
	}
	fileExistedAtDisconnect := false
	fake.onDisconnect = func(network, container string) {
		if network == "q8s-b" {
			if _, err := os.Stat(networkFile); err == nil {
				fileExistedAtDisconnect = true
			}
		}
	}

	resp := netTestRequest(t, http.MethodDelete, ts.URL+"/api/v1/namespaces/b", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete namespace b: status %d", resp.StatusCode)
	}

	disconnects := fake.disconnectCalls()
	want := netTestCall{Network: "q8s-b", Container: "a-traefik"}
	found := false
	for _, call := range disconnects {
		if call.Network == want.Network && call.Container == want.Container {
			found = true
		}
	}
	if !found {
		t.Fatalf("NetworkDisconnect calls = %v, want %v", disconnects, want)
	}
	if !fileExistedAtDisconnect {
		t.Fatal("network quadlet was removed before the disconnect")
	}
	if _, err := os.Stat(networkFile); !os.IsNotExist(err) {
		t.Fatalf("network quadlet still exists after delete, err=%v", err)
	}
	content := netTestReadFile(t, filepath.Join(quadletDir, "a-traefik.container"))
	if netTestContains(content, "q8s-b.network") {
		t.Fatalf("pod quadlet still references deleted namespace:\n%s", content)
	}
}

func netTestContains(haystack, needle string) bool {
	return bytes.Contains([]byte(haystack), []byte(needle))
}

func netTestContainsLine(haystack, line string) bool {
	for _, got := range bytes.Split([]byte(haystack), []byte("\n")) {
		if string(got) == line {
			return true
		}
	}
	return false
}
