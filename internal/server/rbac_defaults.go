package server

import (
	"sort"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Built-in ClusterRoles are synthetic: they are served by GET and LIST but are
// never stored, and every write to one is refused. The rules mirror the
// Kubernetes defaults, limited to the resources q8s actually serves.
const (
	builtinClusterRoleBootstrappingLabel = "kubernetes.io/bootstrapping"
	builtinClusterRoleBootstrappingValue = "rbac-defaults"

	aggregateToAdminLabel = "rbac.authorization.k8s.io/aggregate-to-admin"
	aggregateToEditLabel  = "rbac.authorization.k8s.io/aggregate-to-edit"
	aggregateToViewLabel  = "rbac.authorization.k8s.io/aggregate-to-view"
)

var (
	readVerbs  = []string{"get", "list", "watch"}
	writeVerbs = []string{"create", "update", "patch", "delete", "deletecollection"}
)

func newBuiltinClusterRole(name string, rules []rbacv1.PolicyRule) *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		TypeMeta: metav1.TypeMeta{APIVersion: rbacAPIVersion, Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{builtinClusterRoleBootstrappingLabel: builtinClusterRoleBootstrappingValue},
		},
		Rules: rules,
	}
}

// aggregationRule selects the labels Kubernetes uses to extend a built-in role.
func aggregationRule(label string) *rbacv1.AggregationRule {
	return &rbacv1.AggregationRule{
		ClusterRoleSelectors: []metav1.LabelSelector{{
			MatchLabels: map[string]string{label: "true"},
		}},
	}
}

func clusterAdminClusterRole() *rbacv1.ClusterRole {
	return newBuiltinClusterRole("cluster-admin", []rbacv1.PolicyRule{
		{APIGroups: []string{"*"}, Resources: []string{"*"}, Verbs: []string{"*"}},
		{NonResourceURLs: []string{"*"}, Verbs: []string{"*"}},
	})
}

func editClusterRole() *rbacv1.ClusterRole {
	role := newBuiltinClusterRole("edit", viewClusterRole().Rules)
	role.Rules = append(role.Rules,
		rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"configmaps", "persistentvolumeclaims", "pods", "secrets", "services"}, Verbs: writeVerbs},
		rbacv1.PolicyRule{APIGroups: []string{"apps"}, Resources: []string{"daemonsets", "deployments", "replicasets", "statefulsets"}, Verbs: writeVerbs},
		rbacv1.PolicyRule{APIGroups: []string{"batch"}, Resources: []string{"cronjobs", "jobs"}, Verbs: writeVerbs},
		rbacv1.PolicyRule{APIGroups: []string{"networking.k8s.io"}, Resources: []string{"ingresses"}, Verbs: writeVerbs},
		rbacv1.PolicyRule{APIGroups: []string{"traefik.io"}, Resources: []string{"ingressroutetcps", "middlewares"}, Verbs: writeVerbs},
		rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"pods/attach", "pods/exec", "pods/portforward"}, Verbs: []string{"create"}},
		rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"pods/log"}, Verbs: []string{"get"}},
		// Kubernetes edit reads Secrets in full (view deliberately does not).
		rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: readVerbs},
	)
	role.AggregationRule = aggregationRule(aggregateToEditLabel)
	return role
}

func adminClusterRole() *rbacv1.ClusterRole {
	role := newBuiltinClusterRole("admin", editClusterRole().Rules)
	role.Rules = append(role.Rules,
		rbacv1.PolicyRule{APIGroups: []string{rbacAPIGroup}, Resources: []string{"roles", "rolebindings"}, Verbs: readVerbs},
	)
	role.AggregationRule = aggregationRule(aggregateToAdminLabel)
	return role
}

func viewClusterRole() *rbacv1.ClusterRole {
	role := newBuiltinClusterRole("view", []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"configmaps", "events", "namespaces", "nodes", "persistentvolumeclaims", "pods", "services"}, Verbs: readVerbs},
		{APIGroups: []string{""}, Resources: []string{"pods/log", "pods/status"}, Verbs: readVerbs},
		{APIGroups: []string{"apps"}, Resources: []string{"daemonsets", "deployments", "replicasets", "statefulsets"}, Verbs: readVerbs},
		{APIGroups: []string{"batch"}, Resources: []string{"cronjobs", "jobs"}, Verbs: readVerbs},
		{APIGroups: []string{"networking.k8s.io"}, Resources: []string{"ingresses"}, Verbs: readVerbs},
		{APIGroups: []string{"traefik.io"}, Resources: []string{"ingressroutetcps", "middlewares"}, Verbs: readVerbs},
		{APIGroups: []string{"storage.k8s.io"}, Resources: []string{"storageclasses"}, Verbs: readVerbs},
		{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, Verbs: readVerbs},
		{APIGroups: []string{"metrics.k8s.io"}, Resources: []string{"nodes", "pods"}, Verbs: readVerbs},
		{APIGroups: []string{"apiextensions.k8s.io"}, Resources: []string{"customresourcedefinitions"}, Verbs: readVerbs},
	})
	role.AggregationRule = aggregationRule(aggregateToViewLabel)
	return role
}

// builtinClusterRoles returns fresh copies of the synthetic ClusterRoles, in
// the Kubernetes bootstrap order.
func builtinClusterRoles() []*rbacv1.ClusterRole {
	return []*rbacv1.ClusterRole{
		clusterAdminClusterRole(),
		adminClusterRole(),
		editClusterRole(),
		viewClusterRole(),
	}
}

// builtinClusterRole returns a fresh copy of the named built-in ClusterRole.
func builtinClusterRole(name string) (*rbacv1.ClusterRole, bool) {
	switch name {
	case "cluster-admin":
		return clusterAdminClusterRole(), true
	case "admin":
		return adminClusterRole(), true
	case "edit":
		return editClusterRole(), true
	case "view":
		return viewClusterRole(), true
	default:
		return nil, false
	}
}

// isBuiltinClusterRole reports whether name belongs to a synthetic ClusterRole.
func isBuiltinClusterRole(name string) bool {
	_, ok := builtinClusterRole(name)
	return ok
}

// allClusterRoles merges the synthetic ClusterRoles with the stored ones,
// sorted by name so LIST responses are stable.
func (s *Server) allClusterRoles() []*rbacv1.ClusterRole {
	roles := builtinClusterRoles()
	roles = append(roles, s.config.Store.AllClusterRoles()...)
	sort.Slice(roles, func(i, j int) bool { return roles[i].Name < roles[j].Name })
	return roles
}

// getClusterRole resolves a ClusterRole by name, built-in or stored.
func (s *Server) getClusterRole(name string) (*rbacv1.ClusterRole, error) {
	if role, ok := builtinClusterRole(name); ok {
		return role, nil
	}
	return s.config.Store.GetClusterRole(name)
}
