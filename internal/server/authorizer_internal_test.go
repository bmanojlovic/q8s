package server

import (
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"q8s/internal/store"
)

func newAuthorizerTestServer() *Server {
	return &Server{config: Config{Store: store.New()}}
}

func addRole(t *testing.T, s *Server, ns, name string, rules ...rbacv1.PolicyRule) {
	t.Helper()
	_, err := s.config.Store.CreateRole(&rbacv1.Role{
		TypeMeta:   metav1.TypeMeta{APIVersion: rbacAPIVersion, Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Rules:      rules,
	})
	if err != nil {
		t.Fatalf("create role %s/%s: %v", ns, name, err)
	}
}

func addRoleBinding(t *testing.T, s *Server, ns, name, kind, roleName string, subjects ...rbacv1.Subject) {
	t.Helper()
	_, err := s.config.Store.CreateRoleBinding(&rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: rbacAPIVersion, Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacAPIGroup, Kind: kind, Name: roleName},
		Subjects:   subjects,
	})
	if err != nil {
		t.Fatalf("create rolebinding %s/%s: %v", ns, name, err)
	}
}

func addClusterRole(t *testing.T, s *Server, name string, labels map[string]string, rules ...rbacv1.PolicyRule) {
	t.Helper()
	_, err := s.config.Store.CreateClusterRole(&rbacv1.ClusterRole{
		TypeMeta:   metav1.TypeMeta{APIVersion: rbacAPIVersion, Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Rules:      rules,
	})
	if err != nil {
		t.Fatalf("create clusterrole %s: %v", name, err)
	}
}

func addClusterRoleBinding(t *testing.T, s *Server, name, roleName string, subjects ...rbacv1.Subject) {
	t.Helper()
	_, err := s.config.Store.CreateClusterRoleBinding(&rbacv1.ClusterRoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: rbacAPIVersion, Kind: "ClusterRoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacAPIGroup, Kind: "ClusterRole", Name: roleName},
		Subjects:   subjects,
	})
	if err != nil {
		t.Fatalf("create clusterrolebinding %s: %v", name, err)
	}
}

func userSubject(name string) rbacv1.Subject {
	return rbacv1.Subject{Kind: "User", Name: name}
}

func groupSubject(name string) rbacv1.Subject {
	return rbacv1.Subject{Kind: "Group", Name: name}
}

func resourceAttrs(apiGroup, namespace, resource, subresource, name, verb string) requestAttributes {
	return requestAttributes{
		IsResourceRequest: true,
		Verb:              verb,
		APIGroup:          apiGroup,
		Namespace:         namespace,
		Resource:          resource,
		Subresource:       subresource,
		Name:              name,
	}
}

func nonResourceAttrs(path, verb string) requestAttributes {
	return requestAttributes{Path: path, Verb: verb}
}

func TestAuthorize(t *testing.T) {
	alice := userInfo{Name: "alice", Groups: []string{"system:authenticated"}}
	bob := userInfo{Name: "bob", Groups: []string{"system:authenticated"}}

	allowRule := func(group, resource string, verbs ...string) rbacv1.PolicyRule {
		return rbacv1.PolicyRule{APIGroups: []string{group}, Resources: []string{resource}, Verbs: verbs}
	}

	cases := []struct {
		name  string
		user  userInfo
		attrs requestAttributes
		setup func(t *testing.T, s *Server)
		want  bool
	}{
		{
			name:  "system:masters bypasses authorization",
			user:  userInfo{Name: "root", Groups: []string{"system:masters", "system:authenticated"}},
			attrs: resourceAttrs(rbacAPIGroup, "demo", "clusterroles", "", "admin", "create"),
			want:  true,
		},
		{
			name:  "q8s-user bypasses authorization",
			user:  userInfo{Name: "q8s-user", Groups: []string{"q8s-user", "system:authenticated"}},
			attrs: resourceAttrs("", "demo", "secrets", "", "demo-tls", "delete"),
			want:  true,
		},
		{
			name:  "discovery api root",
			user:  alice,
			attrs: nonResourceAttrs("/api", "get"),
			want:  true,
		},
		{
			name:  "discovery nested apis path",
			user:  alice,
			attrs: nonResourceAttrs("/apis/apps/v1", "get"),
			want:  true,
		},
		{
			name:  "discovery version with trailing slash",
			user:  alice,
			attrs: nonResourceAttrs("/version/", "get"),
			want:  true,
		},
		{
			name:  "discovery healthz",
			user:  alice,
			attrs: nonResourceAttrs("/healthz", "get"),
			want:  true,
		},
		{
			name:  "discovery openapi v3 nested path",
			user:  alice,
			attrs: nonResourceAttrs("/openapi/v3/apis/apps/v1", "get"),
			want:  true,
		},
		{
			name:  "discovery does not allow other non-resource paths",
			user:  alice,
			attrs: nonResourceAttrs("/metrics", "get"),
			want:  false,
		},
		{
			name:  "discovery does not allow writes",
			user:  alice,
			attrs: nonResourceAttrs("/api", "post"),
			want:  false,
		},
		{
			name:  "non-resource rule grants a path",
			user:  alice,
			attrs: nonResourceAttrs("/metrics", "get"),
			setup: func(t *testing.T, s *Server) {
				addClusterRole(t, s, "metrics-reader", nil, rbacv1.PolicyRule{NonResourceURLs: []string{"/metrics"}, Verbs: []string{"get"}})
				addClusterRoleBinding(t, s, "alice-metrics", "metrics-reader", userSubject("alice"))
			},
			want: true,
		},
		{
			name:  "non-resource exact rule does not grant a subpath",
			user:  alice,
			attrs: nonResourceAttrs("/metrics/pods", "get"),
			setup: func(t *testing.T, s *Server) {
				addClusterRole(t, s, "metrics-reader", nil, rbacv1.PolicyRule{NonResourceURLs: []string{"/metrics"}, Verbs: []string{"get"}})
				addClusterRoleBinding(t, s, "alice-metrics", "metrics-reader", userSubject("alice"))
			},
			want: false,
		},
		{
			name:  "non-resource prefix rule",
			user:  alice,
			attrs: nonResourceAttrs("/metrics/pods", "get"),
			setup: func(t *testing.T, s *Server) {
				addClusterRole(t, s, "metrics-reader", nil, rbacv1.PolicyRule{NonResourceURLs: []string{"/metrics/*"}, Verbs: []string{"get"}})
				addClusterRoleBinding(t, s, "alice-metrics", "metrics-reader", userSubject("alice"))
			},
			want: true,
		},
		{
			name:  "non-resource rule does not grant resource requests",
			user:  alice,
			attrs: resourceAttrs("", "demo", "pods", "", "", "list"),
			setup: func(t *testing.T, s *Server) {
				addClusterRole(t, s, "urls", nil, rbacv1.PolicyRule{NonResourceURLs: []string{"/*"}, Verbs: []string{"*"}})
				addClusterRoleBinding(t, s, "alice-urls", "urls", userSubject("alice"))
			},
			want: false,
		},
		{
			name:  "role grants get by resourceName but not list",
			user:  alice,
			attrs: resourceAttrs("", "demo", "secrets", "", "demo-tls", "get"),
			setup: func(t *testing.T, s *Server) {
				addRole(t, s, "demo", "read-one", rbacv1.PolicyRule{
					APIGroups: []string{""}, Resources: []string{"secrets"},
					ResourceNames: []string{"demo-tls"}, Verbs: []string{"get", "list"},
				})
				addRoleBinding(t, s, "demo", "alice-read-one", "Role", "read-one", userSubject("alice"))
			},
			want: true,
		},
		{
			name:  "resourceName rule does not grant another name",
			user:  alice,
			attrs: resourceAttrs("", "demo", "secrets", "", "other-tls", "get"),
			setup: func(t *testing.T, s *Server) {
				addRole(t, s, "demo", "read-one", rbacv1.PolicyRule{
					APIGroups: []string{""}, Resources: []string{"secrets"},
					ResourceNames: []string{"demo-tls"}, Verbs: []string{"get", "list"},
				})
				addRoleBinding(t, s, "demo", "alice-read-one", "Role", "read-one", userSubject("alice"))
			},
			want: false,
		},
		{
			name:  "resourceName rule does not grant list",
			user:  alice,
			attrs: resourceAttrs("", "demo", "secrets", "", "", "list"),
			setup: func(t *testing.T, s *Server) {
				addRole(t, s, "demo", "read-one", rbacv1.PolicyRule{
					APIGroups: []string{""}, Resources: []string{"secrets"},
					ResourceNames: []string{"demo-tls"}, Verbs: []string{"get", "list"},
				})
				addRoleBinding(t, s, "demo", "alice-read-one", "Role", "read-one", userSubject("alice"))
			},
			want: false,
		},
		{
			name:  "resourceName wildcard is a literal name",
			user:  alice,
			attrs: resourceAttrs("", "demo", "secrets", "", "demo-tls", "get"),
			setup: func(t *testing.T, s *Server) {
				addRole(t, s, "demo", "read-any", rbacv1.PolicyRule{
					APIGroups: []string{""}, Resources: []string{"secrets"},
					ResourceNames: []string{"*"}, Verbs: []string{"get"},
				})
				addRoleBinding(t, s, "demo", "alice-read-any", "Role", "read-any", userSubject("alice"))
			},
			want: false,
		},
		{
			name:  "rolebinding to clusterrole is limited to its namespace",
			user:  alice,
			attrs: resourceAttrs("", "other", "secrets", "", "demo-tls", "get"),
			setup: func(t *testing.T, s *Server) {
				addClusterRole(t, s, "read-secrets", nil, allowRule("", "secrets", "get"))
				addRoleBinding(t, s, "demo", "alice-read-secrets", "ClusterRole", "read-secrets", userSubject("alice"))
			},
			want: false,
		},
		{
			name:  "rolebinding to clusterrole applies in its namespace",
			user:  alice,
			attrs: resourceAttrs("", "demo", "secrets", "", "demo-tls", "get"),
			setup: func(t *testing.T, s *Server) {
				addClusterRole(t, s, "read-secrets", nil, allowRule("", "secrets", "get"))
				addRoleBinding(t, s, "demo", "alice-read-secrets", "ClusterRole", "read-secrets", userSubject("alice"))
			},
			want: true,
		},
		{
			name:  "rolebinding to clusterrole does not grant cluster-scoped resources",
			user:  alice,
			attrs: resourceAttrs("", "", "nodes", "", "", "list"),
			setup: func(t *testing.T, s *Server) {
				addClusterRole(t, s, "read-nodes", nil, allowRule("", "nodes", "list"))
				addRoleBinding(t, s, "demo", "alice-read-nodes", "ClusterRole", "read-nodes", userSubject("alice"))
			},
			want: false,
		},
		{
			name:  "clusterrolebinding grants cluster-wide access",
			user:  alice,
			attrs: resourceAttrs("", "", "nodes", "", "", "list"),
			setup: func(t *testing.T, s *Server) {
				addClusterRole(t, s, "read-nodes", nil, allowRule("", "nodes", "list"))
				addClusterRoleBinding(t, s, "alice-read-nodes", "read-nodes", userSubject("alice"))
			},
			want: true,
		},
		{
			name:  "group subject matches",
			user:  userInfo{Name: "alice", Groups: []string{"developers", "system:authenticated"}},
			attrs: resourceAttrs("", "demo", "pods", "", "", "list"),
			setup: func(t *testing.T, s *Server) {
				addClusterRole(t, s, "read-pods", nil, allowRule("", "pods", "list"))
				addClusterRoleBinding(t, s, "developers-read-pods", "read-pods", groupSubject("developers"))
			},
			want: true,
		},
		{
			name:  "group subject does not match another group",
			user:  bob,
			attrs: resourceAttrs("", "demo", "pods", "", "", "list"),
			setup: func(t *testing.T, s *Server) {
				addClusterRole(t, s, "read-pods", nil, allowRule("", "pods", "list"))
				addClusterRoleBinding(t, s, "developers-read-pods", "read-pods", groupSubject("developers"))
			},
			want: false,
		},
		{
			name:  "service account subject never matches a certificate user",
			user:  alice,
			attrs: resourceAttrs("", "demo", "pods", "", "", "list"),
			setup: func(t *testing.T, s *Server) {
				addRole(t, s, "demo", "read-pods", allowRule("", "pods", "list"))
				addRoleBinding(t, s, "demo", "alice-sa", "Role", "read-pods", rbacv1.Subject{
					Kind: "ServiceAccount", Name: "alice", Namespace: "demo",
				})
			},
			want: false,
		},
		{
			name:  "wildcard api group and resource",
			user:  alice,
			attrs: resourceAttrs("apps", "demo", "deployments", "", "web", "get"),
			setup: func(t *testing.T, s *Server) {
				addClusterRole(t, s, "all", nil, rbacv1.PolicyRule{APIGroups: []string{"*"}, Resources: []string{"*"}, Verbs: []string{"*"}})
				addClusterRoleBinding(t, s, "alice-all", "all", userSubject("alice"))
			},
			want: true,
		},
		{
			name:  "resource rule does not grant a subresource",
			user:  alice,
			attrs: resourceAttrs("", "demo", "pods", "log", "web", "get"),
			setup: func(t *testing.T, s *Server) {
				addRole(t, s, "demo", "read-pods", allowRule("", "pods", "get", "list"))
				addRoleBinding(t, s, "demo", "alice-read-pods", "Role", "read-pods", userSubject("alice"))
			},
			want: false,
		},
		{
			name:  "subresource rule grants the subresource",
			user:  alice,
			attrs: resourceAttrs("", "demo", "pods", "log", "web", "get"),
			setup: func(t *testing.T, s *Server) {
				addRole(t, s, "demo", "read-logs", allowRule("", "pods/log", "get"))
				addRoleBinding(t, s, "demo", "alice-read-logs", "Role", "read-logs", userSubject("alice"))
			},
			want: true,
		},
		{
			name:  "subresource wildcard rule grants the subresource",
			user:  alice,
			attrs: resourceAttrs("", "demo", "pods", "exec", "web", "create"),
			setup: func(t *testing.T, s *Server) {
				addRole(t, s, "demo", "pod-any-sub", allowRule("", "pods/*", "create"))
				addRoleBinding(t, s, "demo", "alice-pod-any-sub", "Role", "pod-any-sub", userSubject("alice"))
			},
			want: true,
		},
		{
			name:  "exec requires create on pods/exec",
			user:  alice,
			attrs: resourceAttrs("", "demo", "pods", "exec", "web", "create"),
			setup: func(t *testing.T, s *Server) {
				addRole(t, s, "demo", "write-pods", allowRule("", "pods", "create"))
				addRoleBinding(t, s, "demo", "alice-write-pods", "Role", "write-pods", userSubject("alice"))
			},
			want: false,
		},
		{
			name:  "rbac writes stay admin only even with wildcard rules",
			user:  alice,
			attrs: resourceAttrs(rbacAPIGroup, "demo", "roles", "", "escalate", "create"),
			setup: func(t *testing.T, s *Server) {
				addClusterRole(t, s, "all", nil, rbacv1.PolicyRule{APIGroups: []string{"*"}, Resources: []string{"*"}, Verbs: []string{"*"}})
				addClusterRoleBinding(t, s, "alice-all", "all", userSubject("alice"))
			},
			want: false,
		},
		{
			name:  "rbac reads are allowed by wildcard rules",
			user:  alice,
			attrs: resourceAttrs(rbacAPIGroup, "demo", "roles", "", "", "list"),
			setup: func(t *testing.T, s *Server) {
				addClusterRole(t, s, "all", nil, rbacv1.PolicyRule{APIGroups: []string{"*"}, Resources: []string{"*"}, Verbs: []string{"*"}})
				addClusterRoleBinding(t, s, "alice-all", "all", userSubject("alice"))
			},
			want: true,
		},
		{
			name:  "aggregation picks up a labeled stored clusterrole into view",
			user:  alice,
			attrs: resourceAttrs("example.com", "demo", "widgets", "", "", "get"),
			setup: func(t *testing.T, s *Server) {
				addClusterRole(t, s, "widget-reader", map[string]string{aggregateToViewLabel: "true"}, allowRule("example.com", "widgets", "get"))
				addClusterRoleBinding(t, s, "alice-view", "view", userSubject("alice"))
			},
			want: true,
		},
		{
			name:  "aggregation ignores an unlabeled clusterrole",
			user:  alice,
			attrs: resourceAttrs("example.com", "demo", "widgets", "", "", "get"),
			setup: func(t *testing.T, s *Server) {
				addClusterRole(t, s, "widget-reader", nil, allowRule("example.com", "widgets", "get"))
				addClusterRoleBinding(t, s, "alice-view", "view", userSubject("alice"))
			},
			want: false,
		},
		{
			name:  "missing roleRef target contributes nothing",
			user:  alice,
			attrs: resourceAttrs("", "demo", "pods", "", "", "list"),
			setup: func(t *testing.T, s *Server) {
				addRoleBinding(t, s, "demo", "alice-missing", "Role", "does-not-exist", userSubject("alice"))
			},
			want: false,
		},
		{
			name:  "missing clusterrole roleRef target contributes nothing",
			user:  alice,
			attrs: resourceAttrs("", "demo", "pods", "", "", "list"),
			setup: func(t *testing.T, s *Server) {
				addClusterRoleBinding(t, s, "alice-missing", "does-not-exist", userSubject("alice"))
			},
			want: false,
		},
		{
			name:  "built-in view reads pods but not secrets",
			user:  alice,
			attrs: resourceAttrs("", "demo", "pods", "", "", "list"),
			setup: func(t *testing.T, s *Server) {
				addClusterRoleBinding(t, s, "alice-view", "view", userSubject("alice"))
			},
			want: true,
		},
		{
			name:  "built-in view cannot read secrets",
			user:  alice,
			attrs: resourceAttrs("", "demo", "secrets", "", "demo-tls", "get"),
			setup: func(t *testing.T, s *Server) {
				addClusterRoleBinding(t, s, "alice-view", "view", userSubject("alice"))
			},
			want: false,
		},
		{
			name:  "built-in view reads nodes",
			user:  alice,
			attrs: resourceAttrs("", "", "nodes", "", "", "list"),
			setup: func(t *testing.T, s *Server) {
				addClusterRoleBinding(t, s, "alice-view", "view", userSubject("alice"))
			},
			want: true,
		},
		{
			name:  "built-in view has no non-resource access",
			user:  alice,
			attrs: nonResourceAttrs("/metrics", "get"),
			setup: func(t *testing.T, s *Server) {
				addClusterRoleBinding(t, s, "alice-view", "view", userSubject("alice"))
			},
			want: false,
		},
		{
			name:  "built-in edit writes pods",
			user:  alice,
			attrs: resourceAttrs("", "demo", "pods", "", "", "create"),
			setup: func(t *testing.T, s *Server) {
				addClusterRoleBinding(t, s, "alice-edit", "edit", userSubject("alice"))
			},
			want: true,
		},
		{
			name:  "built-in edit gets pods/log",
			user:  alice,
			attrs: resourceAttrs("", "demo", "pods", "log", "web", "get"),
			setup: func(t *testing.T, s *Server) {
				addClusterRoleBinding(t, s, "alice-edit", "edit", userSubject("alice"))
			},
			want: true,
		},
		{
			name:  "built-in edit lists secrets, as in Kubernetes",
			user:  alice,
			attrs: resourceAttrs("", "demo", "secrets", "", "", "list"),
			setup: func(t *testing.T, s *Server) {
				addClusterRoleBinding(t, s, "alice-edit", "edit", userSubject("alice"))
			},
			want: true,
		},
		{
			name:  "built-in view cannot list secrets",
			user:  alice,
			attrs: resourceAttrs("", "demo", "secrets", "", "", "list"),
			setup: func(t *testing.T, s *Server) {
				addClusterRoleBinding(t, s, "alice-view", "view", userSubject("alice"))
			},
			want: false,
		},
		{
			name:  "built-in view reads pod logs",
			user:  alice,
			attrs: resourceAttrs("", "demo", "pods", "log", "web-0", "get"),
			setup: func(t *testing.T, s *Server) {
				addClusterRoleBinding(t, s, "alice-view", "view", userSubject("alice"))
			},
			want: true,
		},
		{
			name:  "built-in admin reads rolebindings",
			user:  alice,
			attrs: resourceAttrs(rbacAPIGroup, "demo", "rolebindings", "", "", "list"),
			setup: func(t *testing.T, s *Server) {
				addClusterRoleBinding(t, s, "alice-admin", "admin", userSubject("alice"))
			},
			want: true,
		},
		{
			name:  "built-in admin cannot write rolebindings",
			user:  alice,
			attrs: resourceAttrs(rbacAPIGroup, "demo", "rolebindings", "", "escalate", "create"),
			setup: func(t *testing.T, s *Server) {
				addClusterRoleBinding(t, s, "alice-admin", "admin", userSubject("alice"))
			},
			want: false,
		},
		{
			name:  "built-in cluster-admin grants everything",
			user:  alice,
			attrs: nonResourceAttrs("/metrics", "get"),
			setup: func(t *testing.T, s *Server) {
				addClusterRoleBinding(t, s, "alice-cluster-admin", "cluster-admin", userSubject("alice"))
			},
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newAuthorizerTestServer()
			if tc.setup != nil {
				tc.setup(t, s)
			}
			got, reason := s.authorize(tc.user, tc.attrs)
			if got != tc.want {
				t.Fatalf("authorize() = %v (%s), want %v", got, reason, tc.want)
			}
			if got && reason == "" {
				t.Fatal("authorize() allowed the request without a reason")
			}
			if !got && reason == "" {
				t.Fatal("authorize() denied the request without a reason")
			}
		})
	}
}

func TestAuthorizeAggregationCycle(t *testing.T) {
	s := newAuthorizerTestServer()
	// Two stored ClusterRoles that select each other must not recurse forever.
	alpha := &rbacv1.ClusterRole{
		TypeMeta:   metav1.TypeMeta{APIVersion: rbacAPIVersion, Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{Name: "alpha", Labels: map[string]string{"cycle": "beta"}},
		Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get"}}},
		AggregationRule: &rbacv1.AggregationRule{ClusterRoleSelectors: []metav1.LabelSelector{
			{MatchLabels: map[string]string{"cycle": "alpha"}},
		}},
	}
	beta := &rbacv1.ClusterRole{
		TypeMeta:   metav1.TypeMeta{APIVersion: rbacAPIVersion, Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{Name: "beta", Labels: map[string]string{"cycle": "alpha"}},
		Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"services"}, Verbs: []string{"get"}}},
		AggregationRule: &rbacv1.AggregationRule{ClusterRoleSelectors: []metav1.LabelSelector{
			{MatchLabels: map[string]string{"cycle": "beta"}},
		}},
	}
	if _, err := s.config.Store.CreateClusterRole(alpha); err != nil {
		t.Fatal(err)
	}
	if _, err := s.config.Store.CreateClusterRole(beta); err != nil {
		t.Fatal(err)
	}
	addClusterRoleBinding(t, s, "alice-alpha", "alpha", userSubject("alice"))
	rules := s.effectiveClusterRoleRules("alpha", make(map[string]bool))
	if len(rules) != 2 {
		t.Fatalf("effective rules = %d, want 2 (alpha + beta)", len(rules))
	}
}
