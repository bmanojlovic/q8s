package server

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"q8s/internal/store"
)

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "q8s test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{
		cert: cert,
		key:  key,
		pem:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
	}
}

func (ca *testCA) issue(t *testing.T, commonName string, organizations []string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: commonName, Organization: organizations},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

func certPEM(t *testing.T, cert tls.Certificate) (certBytes, keyBytes []byte) {
	t.Helper()
	key, ok := cert.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		t.Fatalf("unexpected private key type %T", cert.PrivateKey)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// newEnforceTestServer starts the real Server with client-cert authentication
// enabled, so requests exercise authentication, authorization and handlers in
// that order.
func newEnforceTestServer(t *testing.T, ca *testCA) (*httptest.Server, *Server, *store.Store) {
	t.Helper()
	serverCert := ca.issue(t, "server", nil)
	certBytes, keyBytes := certPEM(t, serverCert)
	st := store.New()
	srv, err := New(Config{Store: st, CACert: ca.pem, CertPEM: certBytes, KeyPEM: keyBytes})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.pem) {
		t.Fatal("append CA")
	}
	ts := httptest.NewUnstartedServer(srv)
	ts.TLS = &tls.Config{
		ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs:  pool,
		MinVersion: tls.VersionTLS12,
	}
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts, srv, st
}

func tlsClient(cert tls.Certificate) *http.Client {
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		Certificates:       []tls.Certificate{cert},
		InsecureSkipVerify: true, // the httptest server certificate is self-signed
		MinVersion:         tls.VersionTLS12,
	}}}
}

func doAs(t *testing.T, client *http.Client, method, url string, body interface{}) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

func statusBody(t *testing.T, resp *http.Response) map[string]interface{} {
	t.Helper()
	defer resp.Body.Close()
	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return body
}

func requireForbidden(t *testing.T, resp *http.Response, wantMessage string) {
	t.Helper()
	if resp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("status = %d, want 403: %s", resp.StatusCode, body)
	}
	body := statusBody(t, resp)
	if body["reason"] != "Forbidden" {
		t.Fatalf("reason = %v, want Forbidden", body["reason"])
	}
	if got, _ := body["message"].(string); got != wantMessage {
		t.Fatalf("message = %q, want %q", got, wantMessage)
	}
}

func requireStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("status = %d, want %d: %s", resp.StatusCode, want, body)
	}
	resp.Body.Close()
}

func createRoleForTest(t *testing.T, s *Server, ns, name string, rules ...rbacv1.PolicyRule) {
	t.Helper()
	_, err := s.config.Store.CreateRole(&rbacv1.Role{
		TypeMeta:   metav1.TypeMeta{APIVersion: rbacAPIVersion, Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Rules:      rules,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func createRoleBindingForTest(t *testing.T, s *Server, ns, name, kind, roleName string, subjects ...rbacv1.Subject) {
	t.Helper()
	_, err := s.config.Store.CreateRoleBinding(&rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: rbacAPIVersion, Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacAPIGroup, Kind: kind, Name: roleName},
		Subjects:   subjects,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func createClusterRoleBindingForTest(t *testing.T, s *Server, name, roleName string, subjects ...rbacv1.Subject) {
	t.Helper()
	_, err := s.config.Store.CreateClusterRoleBinding(&rbacv1.ClusterRoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: rbacAPIVersion, Kind: "ClusterRoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacAPIGroup, Kind: "ClusterRole", Name: roleName},
		Subjects:   subjects,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAuthorizeHTTPEnforcement(t *testing.T) {
	ca := newTestCA(t)
	ts, srv, st := newEnforceTestServer(t, ca)
	alice := tlsClient(ca.issue(t, "alice", nil))
	carol := tlsClient(ca.issue(t, "carol", []string{"developers"}))
	root := tlsClient(ca.issue(t, "root", []string{"system:masters"}))
	legacy := tlsClient(ca.issue(t, "q8s-user", []string{"q8s-user"}))

	if _, err := st.CreateSecret(&corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: "demo-tls", Namespace: "demo"},
		Data:       map[string][]byte{"tls.crt": []byte("demo")},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateSecret(&corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: "other-tls", Namespace: "demo"},
	}); err != nil {
		t.Fatal(err)
	}

	t.Run("denied get includes the namespace and object name", func(t *testing.T) {
		resp := doAs(t, alice, http.MethodGet, ts.URL+"/api/v1/namespaces/demo/secrets/demo-tls", nil)
		requireForbidden(t, resp, `secrets "demo-tls" is forbidden: User "alice" cannot get resource "secrets" in API group "" in the namespace "demo"`)
	})

	t.Run("denied list omits the object name", func(t *testing.T) {
		resp := doAs(t, alice, http.MethodGet, ts.URL+"/api/v1/namespaces/demo/secrets", nil)
		requireForbidden(t, resp, `secrets is forbidden: User "alice" cannot list resource "secrets" in API group "" in the namespace "demo"`)
	})

	t.Run("denied cluster scoped list says cluster scope", func(t *testing.T) {
		resp := doAs(t, alice, http.MethodGet, ts.URL+"/api/v1/nodes", nil)
		requireForbidden(t, resp, `nodes is forbidden: User "alice" cannot list resource "nodes" in API group "" at the cluster scope`)
	})

	t.Run("denied non-resource path", func(t *testing.T) {
		resp := doAs(t, alice, http.MethodGet, ts.URL+"/metrics", nil)
		requireForbidden(t, resp, `forbidden: User "alice" cannot get path "/metrics"`)
	})

	t.Run("discovery is allowed", func(t *testing.T) {
		requireStatus(t, doAs(t, alice, http.MethodGet, ts.URL+"/api", nil), http.StatusOK)
		requireStatus(t, doAs(t, alice, http.MethodGet, ts.URL+"/apis/rbac.authorization.k8s.io", nil), http.StatusOK)
		requireStatus(t, doAs(t, alice, http.MethodGet, ts.URL+"/version", nil), http.StatusOK)
	})

	t.Run("role binding grants the named secret only", func(t *testing.T) {
		createRoleForTest(t, srv, "demo", "read-one", rbacv1.PolicyRule{
			APIGroups: []string{""}, Resources: []string{"secrets"},
			ResourceNames: []string{"demo-tls"}, Verbs: []string{"get"},
		})
		createRoleBindingForTest(t, srv, "demo", "alice-read-one", "Role", "read-one", rbacv1.Subject{Kind: "User", Name: "alice"})

		requireStatus(t, doAs(t, alice, http.MethodGet, ts.URL+"/api/v1/namespaces/demo/secrets/demo-tls", nil), http.StatusOK)

		resp := doAs(t, alice, http.MethodGet, ts.URL+"/api/v1/namespaces/demo/secrets/other-tls", nil)
		requireForbidden(t, resp, `secrets "other-tls" is forbidden: User "alice" cannot get resource "secrets" in API group "" in the namespace "demo"`)
	})

	t.Run("watch is authorized as watch", func(t *testing.T) {
		resp := doAs(t, alice, http.MethodGet, ts.URL+"/api/v1/namespaces/demo/secrets?watch=true", nil)
		requireForbidden(t, resp, `secrets is forbidden: User "alice" cannot watch resource "secrets" in API group "" in the namespace "demo"`)
	})

	t.Run("group subject matches a certificate organization", func(t *testing.T) {
		createRoleForTest(t, srv, "demo", "read-pods", rbacv1.PolicyRule{
			APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"list"},
		})
		createRoleBindingForTest(t, srv, "demo", "developers-read-pods", "Role", "read-pods", rbacv1.Subject{Kind: "Group", Name: "developers"})
		requireStatus(t, doAs(t, carol, http.MethodGet, ts.URL+"/api/v1/namespaces/demo/pods", nil), http.StatusOK)
	})

	t.Run("exec needs create on pods/exec", func(t *testing.T) {
		createRoleForTest(t, srv, "demo", "write-pods", rbacv1.PolicyRule{
			APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"create"},
		})
		createRoleBindingForTest(t, srv, "demo", "carol-write-pods", "Role", "write-pods", rbacv1.Subject{Kind: "User", Name: "carol"})
		resp := doAs(t, carol, http.MethodPost, ts.URL+"/api/v1/namespaces/demo/pods/web/exec", nil)
		requireForbidden(t, resp, `pods "web" is forbidden: User "carol" cannot create resource "pods/exec" in API group "" in the namespace "demo"`)

		createRoleForTest(t, srv, "demo", "pod-exec", rbacv1.PolicyRule{
			APIGroups: []string{""}, Resources: []string{"pods/exec"}, Verbs: []string{"create"},
		})
		createRoleBindingForTest(t, srv, "demo", "carol-pod-exec", "Role", "pod-exec", rbacv1.Subject{Kind: "User", Name: "carol"})
		resp = doAs(t, carol, http.MethodPost, ts.URL+"/api/v1/namespaces/demo/pods/web/exec", nil)
		if resp.StatusCode == http.StatusForbidden {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("exec still forbidden: %s", body)
		}
		resp.Body.Close()
	})

	t.Run("rbac writes are admin only", func(t *testing.T) {
		_, err := st.CreateClusterRole(&rbacv1.ClusterRole{
			TypeMeta:   metav1.TypeMeta{APIVersion: rbacAPIVersion, Kind: "ClusterRole"},
			ObjectMeta: metav1.ObjectMeta{Name: "all"},
			Rules:      []rbacv1.PolicyRule{{APIGroups: []string{"*"}, Resources: []string{"*"}, Verbs: []string{"*"}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		createClusterRoleBindingForTest(t, srv, "carol-all", "all", rbacv1.Subject{Kind: "User", Name: "carol"})

		requireStatus(t, doAs(t, carol, http.MethodGet, ts.URL+rbacBaseURL+"/clusterroles/view", nil), http.StatusOK)

		role := map[string]interface{}{
			"apiVersion": rbacAPIVersion,
			"kind":       "ClusterRole",
			"metadata":   map[string]interface{}{"name": "escalate"},
			"rules": []interface{}{map[string]interface{}{
				"apiGroups": []interface{}{"*"}, "resources": []interface{}{"*"}, "verbs": []interface{}{"*"},
			}},
		}
		resp := doAs(t, carol, http.MethodPost, ts.URL+rbacBaseURL+"/clusterroles", role)
		requireForbidden(t, resp, `clusterroles is forbidden: User "carol" cannot create resource "clusterroles" in API group "rbac.authorization.k8s.io" at the cluster scope`)
	})

	t.Run("admins bypass authorization", func(t *testing.T) {
		requireStatus(t, doAs(t, root, http.MethodGet, ts.URL+"/api/v1/namespaces/demo/secrets/demo-tls", nil), http.StatusOK)
		// /metrics is not a route in q8s, so a bypassed admin reaches the
		// router and gets 404 instead of a 403 from the authorizer.
		requireStatus(t, doAs(t, root, http.MethodGet, ts.URL+"/metrics", nil), http.StatusNotFound)

		// The certificate q8s install has always issued, CN=q8s-user and
		// O=q8s-user, keeps full access.
		requireStatus(t, doAs(t, legacy, http.MethodGet, ts.URL+"/api/v1/namespaces/demo/secrets/demo-tls", nil), http.StatusOK)
	})
}

const rbacBaseURL = "/apis/rbac.authorization.k8s.io/v1"

func TestBuiltinClusterRolesHTTP(t *testing.T) {
	ca := newTestCA(t)
	ts, _, _ := newEnforceTestServer(t, ca)
	admin := tlsClient(ca.issue(t, "root", []string{"system:masters"}))

	t.Run("get and list include the built-ins", func(t *testing.T) {
		resp := doAs(t, admin, http.MethodGet, ts.URL+rbacBaseURL+"/clusterroles", nil)
		requireStatus(t, resp, http.StatusOK)
		// The body was consumed by requireStatus; fetch again for the list.
		resp = doAs(t, admin, http.MethodGet, ts.URL+rbacBaseURL+"/clusterroles", nil)
		body := statusBody(t, resp)
		items, _ := body["items"].([]interface{})
		names := map[string]map[string]interface{}{}
		for _, item := range items {
			obj, _ := item.(map[string]interface{})
			meta, _ := obj["metadata"].(map[string]interface{})
			name, _ := meta["name"].(string)
			names[name] = obj
		}
		for _, want := range []string{"cluster-admin", "admin", "edit", "view"} {
			obj, ok := names[want]
			if !ok {
				t.Fatalf("clusterrole %q missing from list: %v", want, names)
			}
			meta, _ := obj["metadata"].(map[string]interface{})
			labels, _ := meta["labels"].(map[string]interface{})
			if labels[builtinClusterRoleBootstrappingLabel] != builtinClusterRoleBootstrappingValue {
				t.Fatalf("clusterrole %q labels = %v", want, labels)
			}
		}

		resp = doAs(t, admin, http.MethodGet, ts.URL+rbacBaseURL+"/clusterroles/view", nil)
		body = statusBody(t, resp)
		if body["kind"] != "ClusterRole" || body["metadata"].(map[string]interface{})["name"] != "view" {
			t.Fatalf("GET view = %v", body)
		}
		if body["aggregationRule"] == nil {
			t.Fatal("built-in view has no aggregationRule")
		}
	})

	t.Run("built-ins are read-only", func(t *testing.T) {
		role := map[string]interface{}{
			"apiVersion": rbacAPIVersion,
			"kind":       "ClusterRole",
			"metadata":   map[string]interface{}{"name": "view"},
			"rules": []interface{}{map[string]interface{}{
				"apiGroups": []interface{}{""}, "resources": []interface{}{"pods"}, "verbs": []interface{}{"get"},
			}},
		}
		requireStatus(t, doAs(t, admin, http.MethodPost, ts.URL+rbacBaseURL+"/clusterroles", role), http.StatusConflict)
		requireStatus(t, doAs(t, admin, http.MethodPut, ts.URL+rbacBaseURL+"/clusterroles/view", role), http.StatusMethodNotAllowed)
		requireStatus(t, doAs(t, admin, http.MethodPatch, ts.URL+rbacBaseURL+"/clusterroles/view", map[string]interface{}{
			"metadata": map[string]interface{}{"labels": map[string]interface{}{"changed": "yes"}},
		}), http.StatusMethodNotAllowed)
		requireStatus(t, doAs(t, admin, http.MethodDelete, ts.URL+rbacBaseURL+"/clusterroles/view", nil), http.StatusMethodNotAllowed)

		resp := doAs(t, admin, http.MethodGet, ts.URL+rbacBaseURL+"/clusterroles/view", nil)
		body := statusBody(t, resp)
		meta, _ := body["metadata"].(map[string]interface{})
		labels, _ := meta["labels"].(map[string]interface{})
		if labels["changed"] != nil {
			t.Fatalf("built-in view was modified: %v", labels)
		}
	})
}
