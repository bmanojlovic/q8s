package server

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"q8s/internal/store"
)

// newReviewServer starts a development-mode server (no client certificates)
// with temporary write directories. Handler-level tests use it to exercise
// request validation without going through authentication.
func newReviewServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	ca := newTestCA(t)
	cert := ca.issue(t, "review-server", nil)
	certBytes, keyBytes := certPEM(t, cert)
	st := store.New()
	srv, err := New(Config{
		Store:      st,
		CertPEM:    certBytes,
		KeyPEM:     keyBytes,
		ConfigDir:  t.TempDir(),
		QuadletDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv, st
}

func reviewRequest(t *testing.T, srv *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func reviewStatus(t *testing.T, rec *httptest.ResponseRecorder) (int, string) {
	t.Helper()
	var status metav1.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v (body %s)", err, rec.Body.String())
	}
	return rec.Code, status.Message
}

// TestEncodedAPISegmentsAreRejected pins the discovery bypass fix: an encoded
// slash or dot must never be treated as a parseable resource path (or as
// discovery) and reach a handler with no RBAC check.
func TestEncodedAPISegmentsAreRejected(t *testing.T) {
	ca := newTestCA(t)
	ts, _, _ := newEnforceTestServer(t, ca)
	alice := tlsClient(ca.issue(t, "alice", nil))

	for _, path := range []string{
		"/api/v1/namespaces/team/secrets%2F%2F",
		"/api/v1/namespaces/team/events/%2E%2E",
	} {
		t.Run(path, func(t *testing.T) {
			resp := doAs(t, alice, http.MethodGet, ts.URL+path, nil)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want 400: %s", resp.StatusCode, body)
			}
		})
	}

	t.Run("plain secrets list stays forbidden", func(t *testing.T) {
		resp := doAs(t, alice, http.MethodGet, ts.URL+"/api/v1/namespaces/team/secrets", nil)
		requireStatus(t, resp, http.StatusForbidden)
	})

	t.Run("discovery still works", func(t *testing.T) {
		resp := doAs(t, alice, http.MethodGet, ts.URL+"/apis/apps/v1", nil)
		requireStatus(t, resp, http.StatusOK)
	})

	t.Run("trailing slash still works", func(t *testing.T) {
		resp := doAs(t, alice, http.MethodGet, ts.URL+"/api/v1/namespaces/team/secrets/", nil)
		requireStatus(t, resp, http.StatusForbidden)
	})
}

// TestIsDiscoveryRequestShapes pins the exact discovery allowlist, so a path
// that only looks like an API prefix is never treated as discovery.
func TestIsDiscoveryRequestShapes(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{path: "/api", want: true},
		{path: "/api/v1", want: true},
		{path: "/apis", want: true},
		{path: "/apis/apps", want: true},
		{path: "/apis/apps/v1", want: true},
		{path: "/openapi/v3/apis/apps/v1", want: true},
		{path: "/api/v1/namespaces/team/secrets", want: false},
		{path: "/api/v1/namespaces/team/secrets//", want: false},
		{path: "/apis/apps/v1/namespaces/team/deployments", want: false},
		{path: "/apis/apps/v1/..", want: false},
	}
	for _, tc := range cases {
		if got := isDiscoveryRequest(nonResourceAttrs(tc.path, "get")); got != tc.want {
			t.Errorf("isDiscoveryRequest(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// TestCreateRejectsBodyNamespaceMismatch covers the create paths of one core,
// one secret and one RBAC resource: the body namespace is never allowed to
// differ from the namespace in the URL, and nothing is stored.
func TestCreateRejectsBodyNamespaceMismatch(t *testing.T) {
	srv, st := newReviewServer(t)
	const message = "the namespace of the provided object does not match the namespace sent on the request"

	cases := []struct {
		name string
		path string
		body string
		gone func() bool
	}{
		{
			name: "pod",
			path: "/api/v1/namespaces/team/pods",
			body: `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"evil","namespace":"prod"},"spec":{"containers":[{"name":"web","image":"nginx"}]}}`,
			gone: func() bool { return len(st.Pods("prod")) == 0 && len(st.Pods("team")) == 0 },
		},
		{
			name: "secret",
			path: "/api/v1/namespaces/team/secrets",
			body: `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"evil","namespace":"prod"},"stringData":{"k":"v"}}`,
			gone: func() bool { return len(st.Secrets("prod")) == 0 && len(st.Secrets("team")) == 0 },
		},
		{
			name: "rolebinding",
			path: "/apis/rbac.authorization.k8s.io/v1/namespaces/team/rolebindings",
			body: `{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"RoleBinding","metadata":{"name":"evil","namespace":"prod"},"roleRef":{"apiGroup":"rbac.authorization.k8s.io","kind":"ClusterRole","name":"view"},"subjects":[{"kind":"User","name":"alice"}]}`,
			gone: func() bool {
				return len(st.RoleBindings("prod")) == 0 && len(st.RoleBindings("team")) == 0
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := reviewRequest(t, srv, http.MethodPost, tc.path, tc.body)
			code, msg := reviewStatus(t, rec)
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", code, rec.Body.String())
			}
			if msg != message {
				t.Fatalf("message = %q, want %q", msg, message)
			}
			if !tc.gone() {
				t.Fatal("the mismatched object was stored")
			}
		})
	}

	t.Run("empty body namespace is filled from the URL", func(t *testing.T) {
		rec := reviewRequest(t, srv, http.MethodPost, "/api/v1/namespaces/team/secrets",
			`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"fine"},"stringData":{"k":"v"}}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
		}
		secret, err := st.GetSecret("team", "fine")
		if err != nil {
			t.Fatalf("secret was not stored in the URL namespace: %v", err)
		}
		if secret.Namespace != "team" {
			t.Fatalf("stored namespace = %q, want team", secret.Namespace)
		}
	})
}

// TestPatchAndPutRejectIdentityMismatch covers update and patch, including
// the name message, on top of the create cases.
func TestPatchAndPutRejectIdentityMismatch(t *testing.T) {
	srv, st := newReviewServer(t)

	rec := reviewRequest(t, srv, http.MethodPost, "/api/v1/namespaces/team/configmaps",
		`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"keep"},"data":{"k":"v"}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", rec.Code, rec.Body.String())
	}

	t.Run("patch namespace", func(t *testing.T) {
		rec := reviewRequest(t, srv, http.MethodPatch, "/api/v1/namespaces/team/configmaps/keep",
			`{"metadata":{"namespace":"prod"}}`)
		code, msg := reviewStatus(t, rec)
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", code, rec.Body.String())
		}
		if msg != "the namespace of the provided object does not match the namespace sent on the request" {
			t.Fatalf("message = %q", msg)
		}
		cm, err := st.GetConfigMap("team", "keep")
		if err != nil || cm.Namespace != "team" {
			t.Fatalf("configmap moved after rejected patch: %+v, %v", cm, err)
		}
	})

	t.Run("put name", func(t *testing.T) {
		rec := reviewRequest(t, srv, http.MethodPut, "/api/v1/namespaces/team/configmaps/keep",
			`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"other","namespace":"team"},"data":{"k":"v"}}`)
		code, msg := reviewStatus(t, rec)
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", code, rec.Body.String())
		}
		if msg != "the name of the object (other) does not match the name on the URL (keep)" {
			t.Fatalf("message = %q", msg)
		}
	})
}

// TestApplyRejectsBodyNamespaceMismatch covers the server-side apply path,
// which runs before the normal create and patch handlers.
func TestApplyRejectsBodyNamespaceMismatch(t *testing.T) {
	srv, _ := newReviewServer(t)

	req := httptest.NewRequest(http.MethodPatch, "/api/v1/namespaces/team/configmaps/applied", strings.NewReader(`apiVersion: v1
kind: ConfigMap
metadata:
  name: applied
  namespace: prod
data:
  k: v
`))
	req.Header.Set("Content-Type", "application/apply-patch+yaml")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	code, msg := reviewStatus(t, rec)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", code, rec.Body.String())
	}
	if msg != "the namespace of the provided object does not match the namespace sent on the request" {
		t.Fatalf("message = %q", msg)
	}
}

// TestSelfSubjectRulesReviewIgnoresRoleBindingNonResourceRules pins that only
// ClusterRoleBinding rules contribute nonResourceRules, as in Kubernetes.
func TestSelfSubjectRulesReviewIgnoresRoleBindingNonResourceRules(t *testing.T) {
	ca := newTestCA(t)
	ts, srv, _ := newEnforceTestServer(t, ca)
	alice := tlsClient(ca.issue(t, "alice", nil))

	role := &rbacv1.ClusterRole{
		TypeMeta:   metav1.TypeMeta{APIVersion: rbacAPIVersion, Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{Name: "metrics-and-pods"},
		Rules: []rbacv1.PolicyRule{
			{NonResourceURLs: []string{"/metrics"}, Verbs: []string{"get"}},
			{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get"}},
		},
	}
	if _, err := srv.config.Store.CreateClusterRole(role); err != nil {
		t.Fatal(err)
	}
	createRoleBindingForTest(t, srv, "demo", "alice-metrics", "ClusterRole", role.Name, userSubject("alice"))

	review := postRulesReview(t, alice, ts.URL+rulesReviewPath, "demo", nil)
	if len(review.Status.NonResourceRules) != 0 {
		t.Fatalf("nonResourceRules = %+v, want none from a RoleBinding", review.Status.NonResourceRules)
	}
	if !hasResourceRule(review.Status.ResourceRules, "", "pods", "get") {
		t.Fatalf("the bound resource rule is missing: %+v", review.Status.ResourceRules)
	}

	createClusterRoleBindingForTest(t, srv, "alice-metrics-cluster", role.Name, userSubject("alice"))
	review = postRulesReview(t, alice, ts.URL+rulesReviewPath, "demo", nil)
	if len(review.Status.NonResourceRules) != 1 {
		t.Fatalf("nonResourceRules = %+v, want the ClusterRoleBinding rule", review.Status.NonResourceRules)
	}
}

// TestBuiltinRoleAggregationChain pins the Kubernetes label chain: a role
// labelled aggregate-to-view must reach view, edit and admin users.
func TestBuiltinRoleAggregationChain(t *testing.T) {
	for _, role := range []string{"view", "edit", "admin"} {
		t.Run(role, func(t *testing.T) {
			s := newAuthorizerTestServer()
			addClusterRole(t, s, "widget-reader", map[string]string{aggregateToViewLabel: "true"},
				rbacv1.PolicyRule{APIGroups: []string{"example.com"}, Resources: []string{"widgets"}, Verbs: []string{"get"}})
			addClusterRoleBinding(t, s, "alice-"+role, role, userSubject("alice"))

			got, reason := s.authorize(userInfo{Name: "alice", Groups: []string{"system:authenticated"}},
				resourceAttrs("example.com", "demo", "widgets", "", "", "get"))
			if !got {
				t.Fatalf("%s did not aggregate the labelled ClusterRole: %s", role, reason)
			}
		})
	}

	t.Run("built-in labels", func(t *testing.T) {
		view, _ := builtinClusterRole("view")
		if view.Labels[aggregateToEditLabel] != "true" {
			t.Errorf("view labels = %v, want %s=true", view.Labels, aggregateToEditLabel)
		}
		edit, _ := builtinClusterRole("edit")
		if edit.Labels[aggregateToAdminLabel] != "true" {
			t.Errorf("edit labels = %v, want %s=true", edit.Labels, aggregateToAdminLabel)
		}
		if edit.AggregationRule == nil || len(edit.AggregationRule.ClusterRoleSelectors) != 1 ||
			edit.AggregationRule.ClusterRoleSelectors[0].MatchLabels[aggregateToEditLabel] != "true" {
			t.Errorf("edit aggregationRule = %+v", edit.AggregationRule)
		}
		admin, _ := builtinClusterRole("admin")
		if admin.AggregationRule == nil || len(admin.AggregationRule.ClusterRoleSelectors) != 1 ||
			admin.AggregationRule.ClusterRoleSelectors[0].MatchLabels[aggregateToAdminLabel] != "true" {
			t.Errorf("admin aggregationRule = %+v", admin.AggregationRule)
		}
	})
}

// TestDiskGuardRunsAfterAuthorization pins that 401 wins over 403, 403 wins
// over the 507 disk guard, and that only administrators see filesystem paths
// in the message.
func TestDiskGuardRunsAfterAuthorization(t *testing.T) {
	setStatfs(t, statfsResult(1<<20, 10<<30))
	ca := newTestCA(t)
	serverCert := ca.issue(t, "server", nil)
	certBytes, keyBytes := certPEM(t, serverCert)
	st := store.New()
	dataDir := t.TempDir()
	srv, err := New(Config{
		Store:      st,
		CACert:     ca.pem,
		CertPEM:    certBytes,
		KeyPEM:     keyBytes,
		ConfigDir:  dataDir,
		QuadletDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const writePath = "/api/v1/namespaces/default/configmaps"
	const body = `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"example"}}`

	t.Run("unauthenticated request is 401", func(t *testing.T) {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, writePath, strings.NewReader(body)))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("unauthorized request is 403 before the disk guard", func(t *testing.T) {
		aliceCert := ca.issue(t, "alice", nil)
		req := httptest.NewRequest(http.MethodPost, writePath, strings.NewReader(body))
		req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{aliceCert.Leaf}}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
	})

	if _, err := st.CreateRole(&rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "configmap-writer", Namespace: "default"},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{"configmaps"},
			Verbs:     []string{"create"},
		}},
	}); err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	if _, err := st.CreateRoleBinding(&rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "alice-configmap-writer", Namespace: "default"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "configmap-writer"},
		Subjects:   []rbacv1.Subject{{Kind: "User", Name: "alice"}},
	}); err != nil {
		t.Fatalf("CreateRoleBinding: %v", err)
	}

	t.Run("authorized non-admin gets 507 without paths", func(t *testing.T) {
		aliceCert := ca.issue(t, "alice", nil)
		req := httptest.NewRequest(http.MethodPost, writePath, strings.NewReader(body))
		req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{aliceCert.Leaf}}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusInsufficientStorage {
			t.Fatalf("status = %d, want 507: %s", rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), dataDir) {
			t.Fatalf("non-admin message leaks a filesystem path: %s", rec.Body.String())
		}
	})

	t.Run("admin message keeps the detail", func(t *testing.T) {
		adminCert := ca.issue(t, "root", []string{"system:masters"})
		req := httptest.NewRequest(http.MethodPost, writePath, strings.NewReader(body))
		req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{adminCert.Leaf}}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusInsufficientStorage {
			t.Fatalf("status = %d, want 507: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "free, need") {
			t.Fatalf("admin message lost the low disk detail: %s", rec.Body.String())
		}
	})
}

// TestScaleRejectsIdentityMismatch covers the deployment scale subresource,
// whose body may carry metadata even though only spec.replicas is used.
func TestScaleRejectsIdentityMismatch(t *testing.T) {
	srv, st := newReviewServer(t)
	body := `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web"},"spec":{"replicas":1,"selector":{"matchLabels":{"app":"web"}},"template":{"metadata":{"labels":{"app":"web"}},"spec":{"containers":[{"name":"web","image":"nginx"}]}}}}`
	rec := reviewRequest(t, srv, http.MethodPost, "/apis/apps/v1/namespaces/team/deployments", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", rec.Code, rec.Body.String())
	}

	for _, method := range []string{http.MethodPatch, http.MethodPut} {
		t.Run(method, func(t *testing.T) {
			rec := reviewRequest(t, srv, method, "/apis/apps/v1/namespaces/team/deployments/web/scale",
				`{"apiVersion":"autoscaling/v1","kind":"Scale","metadata":{"name":"web","namespace":"prod"},"spec":{"replicas":3}}`)
			code, msg := reviewStatus(t, rec)
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", code, rec.Body.String())
			}
			if msg != "the namespace of the provided object does not match the namespace sent on the request" {
				t.Fatalf("message = %q", msg)
			}
			dep, err := st.GetDeployment("team", "web")
			if err != nil {
				t.Fatal(err)
			}
			if deploymentReplicas(dep) != 1 {
				t.Fatalf("replicas = %d after rejected scale, want 1", deploymentReplicas(dep))
			}
		})
	}
}
