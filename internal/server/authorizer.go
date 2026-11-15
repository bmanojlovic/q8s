package server

import (
	"fmt"
	"net/http"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// authorize decides whether u may perform the request described by a. It has
// no side effects and reads the store directly, so it can be unit tested
// without an HTTP request.
func (s *Server) authorize(u userInfo, a requestAttributes) (allowed bool, reason string) {
	if isAdmin(u) {
		return true, "user is an administrator"
	}

	if !a.IsResourceRequest {
		if isDiscoveryRequest(a) {
			return true, "allowed by the system:discovery role"
		}
		if s.nonResourceAllowed(u, a) {
			return true, "allowed by a bound ClusterRole"
		}
		return false, "no matching non-resource rule"
	}

	if isRBACWrite(a) {
		return false, "only administrators may modify rbac.authorization.k8s.io resources"
	}

	if isSubjectAccessReview(a) {
		return false, "only administrators may create SubjectAccessReviews"
	}

	if isBasicUserRequest(a) {
		return true, "allowed by the system:basic-user role"
	}

	if s.resourceAllowed(u, a) {
		return true, "allowed by a bound role"
	}
	return false, "no matching rule"
}

// isRBACWrite reports whether a changes an rbac.authorization.k8s.io object.
// Escalation checks are not implemented, so these writes are admin-only even
// when a bound rule would otherwise allow them.
func isRBACWrite(a requestAttributes) bool {
	if a.APIGroup != rbacAPIGroup {
		return false
	}
	switch a.Verb {
	case "create", "update", "patch", "delete", "deletecollection":
		return true
	default:
		return false
	}
}

// isSubjectAccessReview reports whether a creates a SubjectAccessReview.
// Reviewing another user's access is admin-only in q8s; there is no
// escalation path that could grant it.
func isSubjectAccessReview(a requestAttributes) bool {
	return a.APIGroup == "authorization.k8s.io" && a.Resource == "subjectaccessreviews" && a.Subresource == ""
}

// isBasicUserRequest reports whether a is a self-review every authenticated
// user may create, the way the system:basic-user ClusterRole works in
// Kubernetes.
func isBasicUserRequest(a requestAttributes) bool {
	if a.Verb != "create" || a.Subresource != "" || a.Name != "" || a.Namespace != "" {
		return false
	}
	switch a.APIGroup {
	case "authorization.k8s.io":
		return a.Resource == "selfsubjectaccessreviews"
	case "authentication.k8s.io":
		return a.Resource == "selfsubjectreviews"
	default:
		return false
	}
}

// isDiscoveryRequest reports whether a is one of the read-only discovery
// endpoints every authenticated user may access, the way the
// system:discovery ClusterRole does in Kubernetes.
func isDiscoveryRequest(a requestAttributes) bool {
	if a.Verb != "get" {
		return false
	}
	switch a.Path {
	case "/api", "/apis", "/version", "/version/", "/healthz", "/openapi/v2", "/openapi/v3":
		return true
	}
	for _, prefix := range []string{"/api/", "/apis/", "/openapi/v3/"} {
		if strings.HasPrefix(a.Path, prefix) {
			return true
		}
	}
	return false
}

// nonResourceAllowed checks the nonResourceURLs rules of every ClusterRole
// bound to u.
func (s *Server) nonResourceAllowed(u userInfo, a requestAttributes) bool {
	for _, binding := range s.config.Store.AllClusterRoleBindings() {
		if !bindingMatches(binding.Subjects, u) {
			continue
		}
		for _, rule := range s.effectiveClusterRoleRules(binding.RoleRef.Name, make(map[string]bool)) {
			if nonResourceRuleMatches(rule, a) {
				return true
			}
		}
	}
	return false
}

// resourceAllowed collects the rules granted to u by RoleBindings in the
// request namespace and by ClusterRoleBindings, and reports whether one of
// them matches the request.
func (s *Server) resourceAllowed(u userInfo, a requestAttributes) bool {
	if a.Namespace != "" {
		for _, binding := range s.config.Store.RoleBindings(a.Namespace) {
			if !bindingMatches(binding.Subjects, u) {
				continue
			}
			for _, rule := range s.roleBindingRules(binding, a.Namespace) {
				if resourceRuleMatches(rule, a) {
					return true
				}
			}
		}
	}
	for _, binding := range s.config.Store.AllClusterRoleBindings() {
		if !bindingMatches(binding.Subjects, u) {
			continue
		}
		for _, rule := range s.effectiveClusterRoleRules(binding.RoleRef.Name, make(map[string]bool)) {
			if resourceRuleMatches(rule, a) {
				return true
			}
		}
	}
	return false
}

// roleBindingRules resolves the rules of a RoleBinding's roleRef. A missing
// target contributes nothing, matching the Kubernetes authorizer.
func (s *Server) roleBindingRules(binding *rbacv1.RoleBinding, namespace string) []rbacv1.PolicyRule {
	switch binding.RoleRef.Kind {
	case "Role":
		role, err := s.config.Store.GetRole(namespace, binding.RoleRef.Name)
		if err != nil {
			return nil
		}
		return role.Rules
	case "ClusterRole":
		return s.effectiveClusterRoleRules(binding.RoleRef.Name, make(map[string]bool))
	default:
		return nil
	}
}

// effectiveClusterRoleRules returns a ClusterRole's own rules plus the rules
// of every ClusterRole whose labels match one of its aggregationRule
// selectors. Aggregation is evaluated at request time; visited stops cycles.
func (s *Server) effectiveClusterRoleRules(name string, visited map[string]bool) []rbacv1.PolicyRule {
	if visited[name] {
		return nil
	}
	visited[name] = true

	role, err := s.getClusterRole(name)
	if err != nil {
		return nil
	}

	rules := append([]rbacv1.PolicyRule(nil), role.Rules...)
	if role.AggregationRule == nil {
		return rules
	}
	for _, labelSelector := range role.AggregationRule.ClusterRoleSelectors {
		selector, err := metav1.LabelSelectorAsSelector(&labelSelector)
		if err != nil {
			continue
		}
		for _, candidate := range s.allClusterRoles() {
			if candidate.Name == name || !selector.Matches(labels.Set(candidate.Labels)) {
				continue
			}
			rules = append(rules, s.effectiveClusterRoleRules(candidate.Name, visited)...)
		}
	}
	return rules
}

// bindingMatches reports whether any subject of a binding names u. A
// ServiceAccount subject names the certificate user
// system:serviceaccount:{namespace}:{name}; group subjects for
// system:serviceaccounts and system:serviceaccounts:{namespace} therefore
// match through the certificate groups.
func bindingMatches(subjects []rbacv1.Subject, u userInfo) bool {
	for _, subject := range subjects {
		switch subject.Kind {
		case "User":
			if subject.Name == u.Name {
				return true
			}
		case "Group":
			for _, group := range u.Groups {
				if subject.Name == group {
					return true
				}
			}
		case "ServiceAccount":
			if subject.Namespace == "" {
				continue
			}
			if u.Name == "system:serviceaccount:"+subject.Namespace+":"+subject.Name {
				return true
			}
		}
	}
	return false
}

// nonResourceRuleMatches reports whether rule grants a.Verb on a.Path. A
// nonResourceURL ending in "*" is a prefix match; "*" alone matches every
// path.
func nonResourceRuleMatches(rule rbacv1.PolicyRule, a requestAttributes) bool {
	if !verbMatches(rule.Verbs, a.Verb) {
		return false
	}
	for _, url := range rule.NonResourceURLs {
		if url == "*" || url == a.Path {
			return true
		}
		if strings.HasSuffix(url, "*") && strings.HasPrefix(a.Path, strings.TrimSuffix(url, "*")) {
			return true
		}
	}
	return false
}

// resourceRuleMatches reports whether rule grants a. See the RBAC rule
// matching rules in the Kubernetes authorizer: nonResourceURLs rules never
// match resource requests, and a rule naming a resource does not grant its
// subresources.
func resourceRuleMatches(rule rbacv1.PolicyRule, a requestAttributes) bool {
	if len(rule.NonResourceURLs) > 0 {
		return false
	}
	if !verbMatches(rule.Verbs, a.Verb) {
		return false
	}
	if !stringListMatches(rule.APIGroups, a.APIGroup) {
		return false
	}
	if !resourceMatches(rule.Resources, a.Resource, a.Subresource) {
		return false
	}
	if len(rule.ResourceNames) > 0 {
		// A rule restricted to names never grants a request without a name,
		// such as list, watch or deletecollection.
		if a.Name == "" {
			return false
		}
		for _, candidate := range rule.ResourceNames {
			if candidate == a.Name {
				return true
			}
		}
		return false
	}
	return true
}

// verbMatches reports whether verbs contains verb or "*".
func verbMatches(verbs []string, verb string) bool {
	for _, candidate := range verbs {
		if candidate == "*" || candidate == verb {
			return true
		}
	}
	return false
}

// stringListMatches reports whether list contains value or "*".
func stringListMatches(list []string, value string) bool {
	for _, candidate := range list {
		if candidate == "*" || candidate == value {
			return true
		}
	}
	return false
}

// resourceMatches reports whether resources grants resource, or
// resource/subresource when subresource is set.
func resourceMatches(resources []string, resource, subresource string) bool {
	for _, candidate := range resources {
		if candidate == "*" {
			return true
		}
		if subresource == "" {
			if candidate == resource {
				return true
			}
			continue
		}
		if candidate == resource+"/"+subresource || candidate == resource+"/*" {
			return true
		}
	}
	return false
}

// authorizeMiddleware enforces authorize on every request that carries an
// authenticated identity. Authentication runs before it, so a request without
// a user is either unauthenticated (already rejected by the auth middleware)
// or a development-mode request, which stays unrestricted as before.
func (s *Server) authorizeMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok := userFromContext(r.Context())
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		impersonated, err := applyImpersonation(r, u)
		if err != nil {
			status := http.StatusBadRequest
			reason := "BadRequest"
			if err.forbidden {
				status = http.StatusForbidden
				reason = "Forbidden"
			}
			s.respondStatus(w, status, reason, "%s", err.message)
			return
		}
		u = impersonated
		r = r.WithContext(withUserInfo(r.Context(), u))
		a := parseRequestAttributes(r)
		if allowed, _ := s.authorize(u, a); allowed {
			next.ServeHTTP(w, r)
			return
		}
		s.respondStatus(w, http.StatusForbidden, "Forbidden", "%s", forbiddenMessage(u, a))
	})
}

// forbiddenMessage builds the Kubernetes-shaped message for a denied request.
func forbiddenMessage(u userInfo, a requestAttributes) string {
	if !a.IsResourceRequest {
		return fmt.Sprintf("forbidden: User %q cannot %s path %q", u.Name, a.Verb, a.Path)
	}
	resource := a.Resource
	if a.Subresource != "" {
		resource += "/" + a.Subresource
	}
	scope := "at the cluster scope"
	if a.Namespace != "" {
		scope = fmt.Sprintf("in the namespace %q", a.Namespace)
	}
	object := a.Resource
	if a.Name != "" {
		object += " " + fmt.Sprintf("%q", a.Name)
	}
	return fmt.Sprintf("%s is forbidden: User %q cannot %s resource %q in API group %q %s",
		object, u.Name, a.Verb, resource, a.APIGroup, scope)
}
