package server

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
)

const rulesReviewPath = "/apis/authorization.k8s.io/v1/selfsubjectrulesreviews"

func decodeRulesReview(t *testing.T, resp *http.Response) authorizationv1.SelfSubjectRulesReview {
	t.Helper()
	defer resp.Body.Close()
	var review authorizationv1.SelfSubjectRulesReview
	if err := json.NewDecoder(resp.Body).Decode(&review); err != nil {
		t.Fatalf("decode rules review: %v", err)
	}
	return review
}

func postRulesReview(t *testing.T, client *http.Client, url, namespace string, headers map[string][]string) authorizationv1.SelfSubjectRulesReview {
	t.Helper()
	resp := doAsHeaders(t, client, http.MethodPost, url, map[string]interface{}{
		"apiVersion": "authorization.k8s.io/v1",
		"kind":       "SelfSubjectRulesReview",
		"spec":       map[string]interface{}{"namespace": namespace},
	}, headers)
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("status = %d, want 201: %s", resp.StatusCode, body)
	}
	return decodeRulesReview(t, resp)
}

// ruleListContains reports whether list contains want or the "*" wildcard.
func ruleListContains(list []string, want string) bool {
	for _, value := range list {
		if value == want || value == "*" {
			return true
		}
	}
	return false
}

// hasResourceRule reports whether rules grants verb on group/resource through
// one rule. Restricted rules with resourceNames do not count as a full grant.
func hasResourceRule(rules []authorizationv1.ResourceRule, group, resource, verb string) bool {
	for _, rule := range rules {
		if !ruleListContains(rule.APIGroups, group) ||
			!ruleListContains(rule.Resources, resource) ||
			!ruleListContains(rule.Verbs, verb) {
			continue
		}
		if len(rule.ResourceNames) > 0 {
			continue
		}
		return true
	}
	return false
}

func requireSelfReviewRules(t *testing.T, review authorizationv1.SelfSubjectRulesReview) {
	t.Helper()
	for _, tc := range []struct {
		group    string
		resource string
	}{
		{authorizationAPIGroup, "selfsubjectaccessreviews"},
		{authorizationAPIGroup, "selfsubjectrulesreviews"},
		{authenticationAPIGroup, "selfsubjectreviews"},
	} {
		if !hasResourceRule(review.Status.ResourceRules, tc.group, tc.resource, "create") {
			t.Errorf("missing self-review rule create %s/%s in %+v", tc.group, tc.resource, review.Status.ResourceRules)
		}
	}
}

// TestSelfSubjectRulesReviewHTTP: a non-admin sees the rules its RoleBinding
// to the built-in view ClusterRole grants in that namespace, plus the
// self-review rules every authenticated user has.
func TestSelfSubjectRulesReviewHTTP(t *testing.T) {
	ca := newTestCA(t)
	ts, srv, _ := newEnforceTestServer(t, ca)
	admin := tlsClient(ca.issue(t, "root", []string{"system:masters"}))
	alice := tlsClient(ca.issue(t, "alice", nil))

	createRoleBindingForTest(t, srv, "demo", "alice-view", "ClusterRole", "view",
		rbacv1.Subject{Kind: "User", Name: "alice"})

	url := ts.URL + rulesReviewPath

	t.Run("bound view rules in the namespace", func(t *testing.T) {
		review := postRulesReview(t, alice, url, "demo", nil)
		if review.APIVersion != authorizationAPIVersion || review.Kind != "SelfSubjectRulesReview" {
			t.Errorf("response type = %q %q", review.APIVersion, review.Kind)
		}
		if review.Status.Incomplete {
			t.Error("incomplete = true, want false")
		}
		for _, verb := range []string{"get", "list", "watch"} {
			if !hasResourceRule(review.Status.ResourceRules, "", "pods", verb) {
				t.Errorf("missing %s on pods in %+v", verb, review.Status.ResourceRules)
			}
		}
		requireSelfReviewRules(t, review)
	})

	t.Run("no binding in another namespace", func(t *testing.T) {
		review := postRulesReview(t, alice, url, "other", nil)
		if hasResourceRule(review.Status.ResourceRules, "", "pods", "get") {
			t.Errorf("pods rule leaked into namespace other: %+v", review.Status.ResourceRules)
		}
		if len(review.Status.ResourceRules) != 2 {
			t.Errorf("resourceRules = %+v, want only the two self-review rules", review.Status.ResourceRules)
		}
		if len(review.Status.NonResourceRules) != 0 {
			t.Errorf("nonResourceRules = %+v, want none", review.Status.NonResourceRules)
		}
		requireSelfReviewRules(t, review)
	})

	t.Run("admin is unrestricted", func(t *testing.T) {
		review := postRulesReview(t, admin, url, "demo", nil)
		if len(review.Status.ResourceRules) != 1 {
			t.Fatalf("resourceRules = %+v, want one wildcard rule", review.Status.ResourceRules)
		}
		rule := review.Status.ResourceRules[0]
		if len(rule.Verbs) != 1 || rule.Verbs[0] != "*" ||
			len(rule.APIGroups) != 1 || rule.APIGroups[0] != "*" ||
			len(rule.Resources) != 1 || rule.Resources[0] != "*" {
			t.Errorf("admin resource rule = %+v, want verbs/apiGroups/resources *", rule)
		}
		if len(review.Status.NonResourceRules) != 1 {
			t.Fatalf("nonResourceRules = %+v, want one wildcard rule", review.Status.NonResourceRules)
		}
		nonResource := review.Status.NonResourceRules[0]
		if len(nonResource.Verbs) != 1 || nonResource.Verbs[0] != "*" ||
			len(nonResource.NonResourceURLs) != 1 || nonResource.NonResourceURLs[0] != "*" {
			t.Errorf("admin non-resource rule = %+v, want verbs/nonResourceURLs *", nonResource)
		}
	})

	t.Run("impersonation uses the impersonated user", func(t *testing.T) {
		review := postRulesReview(t, admin, url, "demo", map[string][]string{
			"Impersonate-User": {"alice"},
		})
		if !hasResourceRule(review.Status.ResourceRules, "", "pods", "get") {
			t.Errorf("impersonated alice is missing the view pods rule: %+v", review.Status.ResourceRules)
		}
		if hasResourceRule(review.Status.ResourceRules, "*", "*", "*") {
			t.Errorf("impersonated alice got the admin wildcard: %+v", review.Status.ResourceRules)
		}
	})
}

func TestSelfSubjectRulesReviewDiscovery(t *testing.T) {
	ca := newTestCA(t)
	ts, _, _ := newEnforceTestServer(t, ca)
	alice := tlsClient(ca.issue(t, "alice", nil))
	admin := tlsClient(ca.issue(t, "root", []string{"system:masters"}))

	resp := doAs(t, alice, http.MethodGet, ts.URL+"/apis/authorization.k8s.io/v1", nil)
	body := statusBody(t, resp)
	resources, _ := body["resources"].([]interface{})
	found := false
	for _, resource := range resources {
		obj, _ := resource.(map[string]interface{})
		if obj["kind"] != "SelfSubjectRulesReview" {
			continue
		}
		found = true
		if obj["namespaced"] != false {
			t.Errorf("namespaced = %v, want false", obj["namespaced"])
		}
		verbs, _ := obj["verbs"].([]interface{})
		if len(verbs) != 1 || verbs[0] != "create" {
			t.Errorf("verbs = %v, want [create]", verbs)
		}
	}
	if !found {
		t.Fatalf("authorization discovery is missing SelfSubjectRulesReview: %v", resources)
	}

	requireStatus(t, doAs(t, admin, http.MethodGet, ts.URL+rulesReviewPath, nil), http.StatusMethodNotAllowed)
}
