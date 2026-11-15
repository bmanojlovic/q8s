package server

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"q8s/internal/store"
	"q8s/pkg/install"
)

// newRevocationTestServer is newEnforceTestServer with a revocation file.
func newRevocationTestServer(t *testing.T, ca *testCA, revokedFile string) *httptest.Server {
	t.Helper()
	serverCert := ca.issue(t, "server", nil)
	certBytes, keyBytes := certPEM(t, serverCert)
	srv, err := New(Config{
		Store:       store.New(),
		CACert:      ca.pem,
		CertPEM:     certBytes,
		KeyPEM:      keyBytes,
		RevokedFile: revokedFile,
	})
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
	return ts
}

func writeRevoked(t *testing.T, path string, users, serials []string) {
	t.Helper()
	if err := install.SaveRevokedCertificates(path, install.RevokedCertificates{Users: users, Serials: serials}); err != nil {
		t.Fatalf("save revoked: %v", err)
	}
}

func doAsHeaders(t *testing.T, client *http.Client, method, url string, body interface{}, headers map[string][]string) *http.Response {
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
	for name, values := range headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func writeFileString(path, content string) error {
	return os.WriteFile(path, []byte(content), 0600)
}

func removeFile(path string) error {
	return os.Remove(path)
}

func TestRevocationHTTP(t *testing.T) {
	ca := newTestCA(t)
	revokedFile := filepath.Join(t.TempDir(), "revoked.json")
	ts := newRevocationTestServer(t, ca, revokedFile)

	alice := ca.issue(t, "alice", nil)
	bob := ca.issue(t, "bob", nil)
	carol := ca.issue(t, "carol", nil)
	aliceClient := tlsClient(alice)
	bobClient := tlsClient(bob)
	carolClient := tlsClient(carol)

	// Missing revocation file means nothing is revoked.
	requireStatus(t, doAs(t, aliceClient, http.MethodGet, ts.URL+"/version", nil), http.StatusOK)

	writeRevoked(t, revokedFile, []string{"alice"}, []string{install.SerialHex(bob.Leaf.SerialNumber)})
	requireUnauthorized(t, doAs(t, aliceClient, http.MethodGet, ts.URL+"/version", nil))
	requireUnauthorized(t, doAs(t, bobClient, http.MethodGet, ts.URL+"/version", nil))
	requireStatus(t, doAs(t, carolClient, http.MethodGet, ts.URL+"/version", nil), http.StatusOK)

	// The next write is picked up without a restart, including when the new
	// list is the same size as the old one.
	writeRevoked(t, revokedFile, []string{"carol"}, nil)
	requireUnauthorized(t, doAs(t, carolClient, http.MethodGet, ts.URL+"/version", nil))
	requireStatus(t, doAs(t, aliceClient, http.MethodGet, ts.URL+"/version", nil), http.StatusOK)

	// A corrupt file keeps the last good list: alice stays unrevoked and
	// carol stays revoked.
	writeRevoked(t, revokedFile, []string{"carol"}, nil)
	if err := writeFileString(revokedFile, "{not json"); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, doAs(t, aliceClient, http.MethodGet, ts.URL+"/version", nil), http.StatusOK)
	requireUnauthorized(t, doAs(t, carolClient, http.MethodGet, ts.URL+"/version", nil))

	// Deleting the file clears revocation.
	if err := removeFile(revokedFile); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, doAs(t, carolClient, http.MethodGet, ts.URL+"/version", nil), http.StatusOK)
}

func TestRevocationDataDirFallback(t *testing.T) {
	ca := newTestCA(t)
	dir := t.TempDir()
	serverCert := ca.issue(t, "server", nil)
	certBytes, keyBytes := certPEM(t, serverCert)
	srv, err := New(Config{
		Store:   store.New(),
		CACert:  ca.pem,
		CertPEM: certBytes,
		KeyPEM:  keyBytes,
		DataDir: dir,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.pem)
	ts := httptest.NewUnstartedServer(srv)
	ts.TLS = &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool, MinVersion: tls.VersionTLS12}
	ts.StartTLS()
	t.Cleanup(ts.Close)

	writeRevoked(t, install.RevokedPath(dir), []string{"alice"}, nil)
	requireUnauthorized(t, doAs(t, tlsClient(ca.issue(t, "alice", nil)), http.MethodGet, ts.URL+"/version", nil))
}

func requireUnauthorized(t *testing.T, resp *http.Response) {
	t.Helper()
	if resp.StatusCode != http.StatusUnauthorized {
		body := readAll(t, resp)
		t.Fatalf("status = %d, want 401: %s", resp.StatusCode, body)
	}
	body := readAll(t, resp)
	if body != "client certificate revoked\n" {
		t.Fatalf("body = %q, want %q", body, "client certificate revoked\n")
	}
}

func TestImpersonationHTTP(t *testing.T) {
	ca := newTestCA(t)
	ts, srv, _ := newEnforceTestServer(t, ca)
	admin := tlsClient(ca.issue(t, "root", []string{"system:masters"}))
	alice := tlsClient(ca.issue(t, "alice", nil))

	createRoleForTest(t, srv, "demo", "read-secrets", rbacv1.PolicyRule{
		APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get"},
	})
	createRoleBindingForTest(t, srv, "demo", "alice-read-secrets", "Role", "read-secrets",
		rbacv1.Subject{Kind: "User", Name: "alice"})

	t.Run("admin impersonates a user and is authorized as that user", func(t *testing.T) {
		requireStatus(t, doAsHeaders(t, admin, http.MethodGet, ts.URL+"/api/v1/namespaces/demo/secrets/demo-tls", nil,
			map[string][]string{"Impersonate-User": {"alice"}}), http.StatusNotFound)

		resp := doAsHeaders(t, admin, http.MethodGet, ts.URL+"/api/v1/namespaces/demo/secrets", nil,
			map[string][]string{"Impersonate-User": {"alice"}})
		// The list verb is not granted, so the request must be judged as
		// alice, not as the admin.
		requireForbidden(t, resp, `secrets is forbidden: User "alice" cannot list resource "secrets" in API group "" in the namespace "demo"`)

		resp = doAsHeaders(t, admin, http.MethodGet, ts.URL+"/api/v1/namespaces/demo/secrets/demo-tls", nil,
			map[string][]string{"Impersonate-User": {"bob"}})
		requireForbidden(t, resp, `secrets "demo-tls" is forbidden: User "bob" cannot get resource "secrets" in API group "" in the namespace "demo"`)
	})

	t.Run("non-admin may not impersonate", func(t *testing.T) {
		resp := doAsHeaders(t, alice, http.MethodGet, ts.URL+"/version", nil,
			map[string][]string{"Impersonate-User": {"bob"}})
		requireForbidden(t, resp, `users "bob" is forbidden: User "alice" cannot impersonate resource "users" in API group "" at the cluster scope`)

		resp = doAsHeaders(t, alice, http.MethodGet, ts.URL+"/version", nil,
			map[string][]string{"Impersonate-Uid": {"1234"}})
		requireForbidden(t, resp, `uids "1234" is forbidden: User "alice" cannot impersonate resource "uids" in API group "" at the cluster scope`)
	})

	t.Run("group only impersonation is a bad request", func(t *testing.T) {
		requireStatus(t, doAsHeaders(t, admin, http.MethodGet, ts.URL+"/version", nil,
			map[string][]string{"Impersonate-Group": {"team"}}), http.StatusBadRequest)
	})
}

func TestAccessReviewHTTP(t *testing.T) {
	ca := newTestCA(t)
	ts, srv, st := newEnforceTestServer(t, ca)
	admin := tlsClient(ca.issue(t, "root", []string{"system:masters"}))
	alice := tlsClient(ca.issue(t, "alice", nil))

	createRoleForTest(t, srv, "demo", "pod-read", rbacv1.PolicyRule{
		APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list"},
	})
	createRoleBindingForTest(t, srv, "demo", "alice-pod-read", "Role", "pod-read",
		rbacv1.Subject{Kind: "User", Name: "alice"})

	// A group rule for the SubjectAccessReview tests.
	if _, err := st.CreateClusterRole(&rbacv1.ClusterRole{
		TypeMeta:   metav1.TypeMeta{APIVersion: rbacAPIVersion, Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{Name: "secret-reader"},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get"},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	createClusterRoleBindingForTest(t, srv, "developers-secret-reader", "secret-reader",
		rbacv1.Subject{Kind: "Group", Name: "developers"})

	ssarURL := ts.URL + "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews"
	sarURL := ts.URL + "/apis/authorization.k8s.io/v1/subjectaccessreviews"

	t.Run("can-i allowed for a resource", func(t *testing.T) {
		resp := doAs(t, alice, http.MethodPost, ssarURL, map[string]interface{}{
			"apiVersion": "authorization.k8s.io/v1",
			"kind":       "SelfSubjectAccessReview",
			"spec": map[string]interface{}{
				"resourceAttributes": map[string]interface{}{
					"namespace": "demo", "verb": "list", "group": "", "resource": "pods",
				},
			},
		})
		body := statusBody(t, resp)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("status = %d, want 201", resp.StatusCode)
		}
		status := body["status"].(map[string]interface{})
		if status["allowed"] != true {
			t.Fatalf("allowed = %v, reason = %v", status["allowed"], status["reason"])
		}
		if reason, _ := status["reason"].(string); reason == "" {
			t.Error("allowed review has no reason")
		}
		if body["kind"] != "SelfSubjectAccessReview" || body["apiVersion"] != "authorization.k8s.io/v1" {
			t.Errorf("response type = %v %v", body["apiVersion"], body["kind"])
		}
	})

	t.Run("can-i denied for a resource", func(t *testing.T) {
		resp := doAs(t, alice, http.MethodPost, ssarURL, map[string]interface{}{
			"apiVersion": "authorization.k8s.io/v1",
			"kind":       "SelfSubjectAccessReview",
			"spec": map[string]interface{}{
				"resourceAttributes": map[string]interface{}{
					"namespace": "demo", "verb": "delete", "group": "", "resource": "pods",
				},
			},
		})
		body := statusBody(t, resp)
		status := body["status"].(map[string]interface{})
		if status["allowed"] != false {
			t.Fatalf("allowed = %v, want false", status["allowed"])
		}
		if reason, _ := status["reason"].(string); reason == "" {
			t.Error("denied review has no reason")
		}
	})

	t.Run("can-i non-resource", func(t *testing.T) {
		denied := doAs(t, alice, http.MethodPost, ssarURL, map[string]interface{}{
			"spec": map[string]interface{}{
				"nonResourceAttributes": map[string]interface{}{"path": "/metrics", "verb": "get"},
			},
		})
		status := statusBody(t, denied)["status"].(map[string]interface{})
		if status["allowed"] != false {
			t.Fatalf("non-resource /metrics allowed = %v, want false", status["allowed"])
		}

		if _, err := st.CreateClusterRole(&rbacv1.ClusterRole{
			TypeMeta:   metav1.TypeMeta{APIVersion: rbacAPIVersion, Kind: "ClusterRole"},
			ObjectMeta: metav1.ObjectMeta{Name: "metrics-reader"},
			Rules:      []rbacv1.PolicyRule{{NonResourceURLs: []string{"/metrics"}, Verbs: []string{"get"}}},
		}); err != nil {
			t.Fatal(err)
		}
		createClusterRoleBindingForTest(t, srv, "alice-metrics-reader", "metrics-reader",
			rbacv1.Subject{Kind: "User", Name: "alice"})

		allowed := doAs(t, alice, http.MethodPost, ssarURL, map[string]interface{}{
			"spec": map[string]interface{}{
				"nonResourceAttributes": map[string]interface{}{"path": "/metrics", "verb": "get"},
			},
		})
		status = statusBody(t, allowed)["status"].(map[string]interface{})
		if status["allowed"] != true {
			t.Fatalf("non-resource /metrics allowed = %v, reason = %v", status["allowed"], status["reason"])
		}
	})

	t.Run("review attribute validation", func(t *testing.T) {
		requireStatus(t, doAs(t, alice, http.MethodPost, ssarURL, map[string]interface{}{"spec": map[string]interface{}{}}), http.StatusBadRequest)
		requireStatus(t, doAs(t, alice, http.MethodPost, ssarURL, map[string]interface{}{
			"spec": map[string]interface{}{
				"resourceAttributes":    map[string]interface{}{"verb": "get", "resource": "pods"},
				"nonResourceAttributes": map[string]interface{}{"path": "/metrics", "verb": "get"},
			},
		}), http.StatusBadRequest)
		requireStatus(t, doAs(t, admin, http.MethodGet, ssarURL, nil), http.StatusMethodNotAllowed)
		requireStatus(t, doAs(t, admin, http.MethodGet, sarURL, nil), http.StatusMethodNotAllowed)
	})

	t.Run("subject access review is admin only", func(t *testing.T) {
		resp := doAs(t, alice, http.MethodPost, sarURL, map[string]interface{}{
			"spec": map[string]interface{}{
				"user":   "alice",
				"groups": []string{"system:authenticated"},
				"resourceAttributes": map[string]interface{}{
					"namespace": "demo", "verb": "list", "group": "", "resource": "pods",
				},
			},
		})
		requireStatus(t, resp, http.StatusForbidden)

		resp = doAs(t, admin, http.MethodPost, sarURL, map[string]interface{}{
			"apiVersion": "authorization.k8s.io/v1",
			"kind":       "SubjectAccessReview",
			"spec": map[string]interface{}{
				"user":   "carol",
				"groups": []string{"developers"},
				"resourceAttributes": map[string]interface{}{
					"namespace": "demo", "verb": "get", "group": "", "resource": "secrets",
				},
			},
		})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("status = %d, want 201", resp.StatusCode)
		}
		body := statusBody(t, resp)
		status := body["status"].(map[string]interface{})
		if status["allowed"] != true {
			t.Fatalf("SAR allowed = %v, reason = %v", status["allowed"], status["reason"])
		}
		if body["kind"] != "SubjectAccessReview" {
			t.Errorf("kind = %v", body["kind"])
		}

		resp = doAs(t, admin, http.MethodPost, sarURL, map[string]interface{}{
			"spec": map[string]interface{}{
				"user": "bob",
				"resourceAttributes": map[string]interface{}{
					"namespace": "demo", "verb": "get", "group": "", "resource": "secrets",
				},
			},
		})
		body = statusBody(t, resp)
		status = body["status"].(map[string]interface{})
		if status["allowed"] != false {
			t.Fatalf("SAR for bob allowed = %v, want false", status["allowed"])
		}
	})
}

func TestSelfSubjectReviewHTTP(t *testing.T) {
	ca := newTestCA(t)
	ts, _, _ := newEnforceTestServer(t, ca)
	admin := tlsClient(ca.issue(t, "root", []string{"system:masters"}))
	alice := tlsClient(ca.issue(t, "alice", []string{"developers"}))
	url := ts.URL + "/apis/authentication.k8s.io/v1/selfsubjectreviews"

	resp := doAs(t, alice, http.MethodPost, url, map[string]interface{}{
		"apiVersion": "authentication.k8s.io/v1",
		"kind":       "SelfSubjectReview",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	body := statusBody(t, resp)
	status := body["status"].(map[string]interface{})
	userInfoRaw := status["userInfo"].(map[string]interface{})
	if userInfoRaw["username"] != "alice" {
		t.Errorf("username = %v", userInfoRaw["username"])
	}
	groups := stringSlice(userInfoRaw["groups"])
	if !contains(groups, "developers") || !contains(groups, "system:authenticated") {
		t.Errorf("groups = %v", groups)
	}
	if body["kind"] != "SelfSubjectReview" || body["apiVersion"] != "authentication.k8s.io/v1" {
		t.Errorf("response type = %v %v", body["apiVersion"], body["kind"])
	}

	// Impersonation is reflected in whoami.
	resp = doAsHeaders(t, admin, http.MethodPost, url, map[string]interface{}{
		"apiVersion": "authentication.k8s.io/v1",
		"kind":       "SelfSubjectReview",
	}, map[string][]string{
		"Impersonate-User":  {"alice"},
		"Impersonate-Group": {"team", "developers"},
	})
	body = statusBody(t, resp)
	status = body["status"].(map[string]interface{})
	userInfoRaw = status["userInfo"].(map[string]interface{})
	if userInfoRaw["username"] != "alice" {
		t.Errorf("impersonated username = %v", userInfoRaw["username"])
	}
	groups = stringSlice(userInfoRaw["groups"])
	for _, want := range []string{"team", "developers", "system:authenticated"} {
		if !contains(groups, want) {
			t.Errorf("impersonated groups = %v, missing %q", groups, want)
		}
	}
	if contains(groups, "system:masters") {
		t.Errorf("impersonated groups = %v, must not keep system:masters", groups)
	}

	requireStatus(t, doAs(t, admin, http.MethodGet, url, nil), http.StatusMethodNotAllowed)
}

func TestAccessReviewDiscovery(t *testing.T) {
	ca := newTestCA(t)
	ts, _, _ := newEnforceTestServer(t, ca)
	alice := tlsClient(ca.issue(t, "alice", nil))

	for _, path := range []string{
		"/apis/authorization.k8s.io",
		"/apis/authorization.k8s.io/v1",
		"/apis/authentication.k8s.io",
		"/apis/authentication.k8s.io/v1",
	} {
		requireStatus(t, doAs(t, alice, http.MethodGet, ts.URL+path, nil), http.StatusOK)
	}

	resp := doAs(t, alice, http.MethodGet, ts.URL+"/apis/authorization.k8s.io/v1", nil)
	body := statusBody(t, resp)
	resources, _ := body["resources"].([]interface{})
	kinds := map[string]bool{}
	for _, resource := range resources {
		obj, _ := resource.(map[string]interface{})
		kind, _ := obj["kind"].(string)
		kinds[kind] = true
	}
	if !kinds["SelfSubjectAccessReview"] || !kinds["SubjectAccessReview"] {
		t.Fatalf("authorization discovery kinds = %v", kinds)
	}

	resp = doAs(t, alice, http.MethodGet, ts.URL+"/apis/authentication.k8s.io/v1", nil)
	body = statusBody(t, resp)
	resources, _ = body["resources"].([]interface{})
	found := false
	for _, resource := range resources {
		obj, _ := resource.(map[string]interface{})
		if obj["kind"] == "SelfSubjectReview" {
			found = true
		}
	}
	if !found {
		t.Fatalf("authentication discovery is missing SelfSubjectReview: %v", resources)
	}

	// The group list advertises both groups.
	resp = doAs(t, alice, http.MethodGet, ts.URL+"/apis", nil)
	body = statusBody(t, resp)
	groups, _ := body["groups"].([]interface{})
	names := map[string]bool{}
	for _, group := range groups {
		obj, _ := group.(map[string]interface{})
		name, _ := obj["name"].(string)
		names[name] = true
	}
	if !names["authorization.k8s.io"] || !names["authentication.k8s.io"] {
		t.Fatalf("/apis groups = %v", names)
	}
}

func stringSlice(value interface{}) []string {
	items, _ := value.([]interface{})
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
