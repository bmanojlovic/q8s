package server

import (
	"fmt"
	"net/http"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	authorizationAPIGroup    = "authorization.k8s.io"
	authenticationAPIGroup   = "authentication.k8s.io"
	authorizationAPIVersion  = authorizationAPIGroup + "/v1"
	authenticationAPIVersion = authenticationAPIGroup + "/v1"
)

func writeAPIGroupDiscovery(w http.ResponseWriter, group, version string) {
	encode(w, &metav1.APIGroup{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "APIGroup"},
		Name:     group,
		Versions: []metav1.GroupVersionForDiscovery{
			{GroupVersion: group + "/" + version, Version: version},
		},
		PreferredVersion: metav1.GroupVersionForDiscovery{
			GroupVersion: group + "/" + version,
			Version:      version,
		},
	}, http.StatusOK)
}

func (s *Server) handleAuthorizationRoot(w http.ResponseWriter, r *http.Request) {
	writeAPIGroupDiscovery(w, authorizationAPIGroup, "v1")
}

func (s *Server) handleAuthorizationV1(w http.ResponseWriter, r *http.Request) {
	encode(w, &metav1.APIResourceList{
		TypeMeta:     metav1.TypeMeta{APIVersion: "v1", Kind: "APIResourceList"},
		GroupVersion: authorizationAPIVersion,
		APIResources: []metav1.APIResource{
			{Name: "selfsubjectaccessreviews", SingularName: "selfsubjectaccessreview", Namespaced: false, Kind: "SelfSubjectAccessReview", Verbs: []string{"create"}},
			{Name: "selfsubjectrulesreviews", SingularName: "selfsubjectrulesreview", Namespaced: false, Kind: "SelfSubjectRulesReview", Verbs: []string{"create"}},
			{Name: "subjectaccessreviews", SingularName: "subjectaccessreview", Namespaced: false, Kind: "SubjectAccessReview", Verbs: []string{"create"}},
		},
	}, http.StatusOK)
}

func (s *Server) handleAuthenticationRoot(w http.ResponseWriter, r *http.Request) {
	writeAPIGroupDiscovery(w, authenticationAPIGroup, "v1")
}

func (s *Server) handleAuthenticationV1(w http.ResponseWriter, r *http.Request) {
	encode(w, &metav1.APIResourceList{
		TypeMeta:     metav1.TypeMeta{APIVersion: "v1", Kind: "APIResourceList"},
		GroupVersion: authenticationAPIVersion,
		APIResources: []metav1.APIResource{
			{Name: "selfsubjectreviews", SingularName: "selfsubjectreview", Namespaced: false, Kind: "SelfSubjectReview", Verbs: []string{"create"}},
		},
	}, http.StatusOK)
}

// callerInfo returns the identity authenticated for this request after
// impersonation. The boolean is false in development mode, where there is no
// client certificate and authorization is disabled.
func callerInfo(r *http.Request) (userInfo, bool) {
	return userFromContext(r.Context())
}

// handleSelfSubjectAccessReview evaluates the caller against the requested
// resource or non-resource attributes. The review is create-only and is
// never stored.
func (s *Server) handleSelfSubjectAccessReview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var review authorizationv1.SelfSubjectAccessReview
	if !s.decodeOrRespond(w, r, &review) {
		return
	}
	attrs, err := reviewRequestAttributes(review.Spec.ResourceAttributes, review.Spec.NonResourceAttributes)
	if err != nil {
		s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
		return
	}

	u, ok := callerInfo(r)
	if !ok {
		// Authentication is disabled: every request is allowed, so the
		// caller's own access review is allowed too.
		review.APIVersion = authorizationAPIVersion
		review.Kind = "SelfSubjectAccessReview"
		review.Status = authorizationv1.SubjectAccessReviewStatus{
			Allowed: true,
			Reason:  "authentication is disabled; all requests are allowed",
		}
		encode(w, &review, http.StatusCreated)
		return
	}

	allowed, reason := s.authorize(u, attrs)
	review.APIVersion = authorizationAPIVersion
	review.Kind = "SelfSubjectAccessReview"
	review.Status = authorizationv1.SubjectAccessReviewStatus{Allowed: allowed, Reason: reason}
	encode(w, &review, http.StatusCreated)
}

// handleSelfSubjectRulesReview returns the rules that apply to the caller in
// the requested namespace. It is create-only and is never stored.
func (s *Server) handleSelfSubjectRulesReview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var review authorizationv1.SelfSubjectRulesReview
	if !s.decodeOrRespond(w, r, &review) {
		return
	}

	u, ok := callerInfo(r)
	review.APIVersion = authorizationAPIVersion
	review.Kind = "SelfSubjectRulesReview"
	if !ok || isAdmin(u) {
		// Authentication is disabled, so every request is allowed, or the
		// caller is an administrator. Either way the caller is unrestricted.
		review.Status = authorizationv1.SubjectRulesReviewStatus{
			ResourceRules: []authorizationv1.ResourceRule{{
				Verbs: []string{"*"}, APIGroups: []string{"*"}, Resources: []string{"*"},
			}},
			NonResourceRules: []authorizationv1.NonResourceRule{{
				Verbs: []string{"*"}, NonResourceURLs: []string{"*"},
			}},
		}
		encode(w, &review, http.StatusCreated)
		return
	}

	namespaced, cluster := s.boundPolicyRules(u, review.Spec.Namespace)
	all := make([]rbacv1.PolicyRule, 0, len(namespaced)+len(cluster))
	all = append(all, namespaced...)
	all = append(all, cluster...)

	resourceRules := make([]authorizationv1.ResourceRule, 0, len(all)+2)
	nonResourceRules := make([]authorizationv1.NonResourceRule, 0, len(cluster))
	for _, rule := range all {
		if len(rule.NonResourceURLs) > 0 {
			nonResourceRules = append(nonResourceRules, authorizationv1.NonResourceRule{
				Verbs:           append([]string(nil), rule.Verbs...),
				NonResourceURLs: append([]string(nil), rule.NonResourceURLs...),
			})
			continue
		}
		resourceRules = append(resourceRules, authorizationv1.ResourceRule{
			Verbs:         append([]string(nil), rule.Verbs...),
			APIGroups:     append([]string(nil), rule.APIGroups...),
			Resources:     append([]string(nil), rule.Resources...),
			ResourceNames: append([]string(nil), rule.ResourceNames...),
		})
	}
	resourceRules = append(resourceRules, selfReviewResourceRules()...)

	review.Status = authorizationv1.SubjectRulesReviewStatus{
		ResourceRules:    resourceRules,
		NonResourceRules: nonResourceRules,
		Incomplete:       false,
	}
	encode(w, &review, http.StatusCreated)
}

// selfReviewResourceRules returns the rules every authenticated user holds:
// create on the self-review resources, matching the system:basic-user
// ClusterRole in Kubernetes. q8s serves all three resources.
func selfReviewResourceRules() []authorizationv1.ResourceRule {
	return []authorizationv1.ResourceRule{
		{
			Verbs:     []string{"create"},
			APIGroups: []string{authorizationAPIGroup},
			Resources: []string{"selfsubjectaccessreviews", "selfsubjectrulesreviews"},
		},
		{
			Verbs:     []string{"create"},
			APIGroups: []string{authenticationAPIGroup},
			Resources: []string{"selfsubjectreviews"},
		},
	}
}

// handleSubjectAccessReview evaluates an arbitrary user and group list. It is
// admin-only. The review is create-only and is never stored.
func (s *Server) handleSubjectAccessReview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if u, ok := callerInfo(r); ok && !isAdmin(u) {
		s.respondStatus(w, http.StatusForbidden, "Forbidden",
			"subjectaccessreviews is forbidden: User %q may not review the access of other users", u.Name)
		return
	}

	var review authorizationv1.SubjectAccessReview
	if !s.decodeOrRespond(w, r, &review) {
		return
	}
	attrs, err := reviewRequestAttributes(review.Spec.ResourceAttributes, review.Spec.NonResourceAttributes)
	if err != nil {
		s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
		return
	}

	subject := userInfo{Name: review.Spec.User, Groups: append([]string(nil), review.Spec.Groups...)}
	allowed, reason := s.authorize(subject, attrs)
	review.APIVersion = authorizationAPIVersion
	review.Kind = "SubjectAccessReview"
	review.Status = authorizationv1.SubjectAccessReviewStatus{Allowed: allowed, Reason: reason}
	encode(w, &review, http.StatusCreated)
}

// handleSelfSubjectReview returns the caller's identity, including groups and
// any impersonation already applied by the middleware. It is create-only and
// never stored.
func (s *Server) handleSelfSubjectReview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var review authenticationv1.SelfSubjectReview
	if !s.decodeOrRespond(w, r, &review) {
		return
	}

	u, ok := callerInfo(r)
	if !ok {
		u = userInfo{Name: "system:anonymous", Groups: []string{"system:unauthenticated"}}
	}
	review.APIVersion = authenticationAPIVersion
	review.Kind = "SelfSubjectReview"
	review.Status = authenticationv1.SelfSubjectReviewStatus{
		UserInfo: authenticationv1.UserInfo{
			Username: u.Name,
			Groups:   append([]string(nil), u.Groups...),
		},
	}
	encode(w, &review, http.StatusCreated)
}

// reviewRequestAttributes converts a review spec to the request attributes
// the authorizer consumes. Kubernetes requires exactly one of the two
// attribute forms.
func reviewRequestAttributes(resource *authorizationv1.ResourceAttributes, nonResource *authorizationv1.NonResourceAttributes) (requestAttributes, error) {
	if resource == nil && nonResource == nil {
		return requestAttributes{}, fmt.Errorf("exactly one of resourceAttributes or nonResourceAttributes is required")
	}
	if resource != nil && nonResource != nil {
		return requestAttributes{}, fmt.Errorf("resourceAttributes and nonResourceAttributes are mutually exclusive")
	}
	if resource != nil {
		return requestAttributes{
			IsResourceRequest: true,
			Verb:              resource.Verb,
			APIGroup:          resource.Group,
			APIVersion:        resource.Version,
			Namespace:         resource.Namespace,
			Resource:          resource.Resource,
			Subresource:       resource.Subresource,
			Name:              resource.Name,
		}, nil
	}
	return requestAttributes{
		IsResourceRequest: false,
		Path:              nonResource.Path,
		Verb:              nonResource.Verb,
	}, nil
}
