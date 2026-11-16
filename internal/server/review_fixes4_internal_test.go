package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"q8s/internal/quadlet"
)

// reviewPlainDeployment is a Deployment with no host access, used as the base
// object for the update, patch and apply tests.
func reviewPlainDeployment(name, namespace string) *appsv1.Deployment {
	return &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "nginx:1.25"}},
				},
			},
		},
	}
}

func reviewAttachAllPod(name, namespace string) *corev1.Pod {
	pod := hostAccessPod(name, namespace)
	pod.Annotations = map[string]string{quadlet.AnnotationAttachAllNetworks: "true"}
	return pod
}

func reviewAttachAllDeployment(name, namespace string) *appsv1.Deployment {
	dep := reviewPlainDeployment(name, namespace)
	dep.Spec.Template.Annotations = map[string]string{quadlet.AnnotationAttachAllNetworks: "true"}
	return dep
}

func reviewAttachAllPodYAML(name, namespace string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
  annotations:
    %s: "true"
spec:
  containers:
  - name: main
    image: nginx:1.25
`, name, namespace, quadlet.AnnotationAttachAllNetworks)
}

func reviewAttachAllDeploymentYAML(name, namespace string) string {
	return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
spec:
  replicas: 1
  selector:
    matchLabels:
      app: %s
  template:
    metadata:
      labels:
        app: %s
      annotations:
        %s: "true"
    spec:
      containers:
      - name: main
        image: nginx:1.25
`, name, namespace, name, name, quadlet.AnnotationAttachAllNetworks)
}

func reviewPrivilegedPodYAML(name, namespace string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
spec:
  hostPID: true
  containers:
  - name: main
    image: nginx:1.25
`, name, namespace)
}

// TestResourceNamesGrantNamedSubresourceCreate pins the fix that a
// resourceNames rule grants create on a named pod subresource such as exec,
// while a request without a name is still denied. The self subject access
// review must agree with the authorizer.
func TestResourceNamesGrantNamedSubresourceCreate(t *testing.T) {
	env, ca := newHostAccessTestEnv(t)
	bob := tlsClient(ca.issue(t, "bob", nil))
	createRoleForTest(t, env.srv, "team", "named-exec", rbacv1.PolicyRule{
		APIGroups: []string{""}, Resources: []string{"pods/exec"},
		ResourceNames: []string{"debug-pod"}, Verbs: []string{"create"},
	})
	createRoleBindingForTest(t, env.srv, "team", "bob-named-exec", "Role", "named-exec",
		rbacv1.Subject{Kind: "User", Name: "bob"})
	createRoleForTest(t, env.srv, "team", "named-create", rbacv1.PolicyRule{
		APIGroups: []string{""}, Resources: []string{"pods"},
		ResourceNames: []string{"debug-pod"}, Verbs: []string{"create"},
	})
	createRoleBindingForTest(t, env.srv, "team", "bob-named-create", "Role", "named-create",
		rbacv1.Subject{Kind: "User", Name: "bob"})

	user := userInfo{Name: "bob", Groups: []string{"system:authenticated"}}
	execAttrs := func(name string) requestAttributes {
		return requestAttributes{
			IsResourceRequest: true,
			Verb:              "create",
			Namespace:         "team",
			Resource:          "pods",
			Subresource:       "exec",
			Name:              name,
		}
	}
	if allowed, _ := env.srv.authorize(user, execAttrs("debug-pod")); !allowed {
		t.Fatal("a resourceNames rule did not grant exec on the listed pod")
	}
	if allowed, _ := env.srv.authorize(user, execAttrs("other-pod")); allowed {
		t.Fatal("a resourceNames rule granted exec on another pod")
	}
	collection := requestAttributes{
		IsResourceRequest: true,
		Verb:              "create",
		Namespace:         "team",
		Resource:          "pods",
	}
	if allowed, _ := env.srv.authorize(user, collection); allowed {
		t.Fatal("a resourceNames rule granted collection create")
	}

	canI := func(attrs map[string]interface{}) bool {
		resp := doAs(t, bob, http.MethodPost, env.URL("/apis/authorization.k8s.io/v1/selfsubjectaccessreviews"),
			map[string]interface{}{
				"apiVersion": "authorization.k8s.io/v1",
				"kind":       "SelfSubjectAccessReview",
				"spec":       map[string]interface{}{"resourceAttributes": attrs},
			})
		if resp.StatusCode != http.StatusCreated {
			resp.Body.Close()
			t.Fatalf("SelfSubjectAccessReview status = %d, want 201", resp.StatusCode)
		}
		body := statusBody(t, resp)
		status, _ := body["status"].(map[string]interface{})
		allowed, _ := status["allowed"].(bool)
		return allowed
	}
	if !canI(map[string]interface{}{
		"namespace": "team", "verb": "create", "group": "", "resource": "pods",
		"subresource": "exec", "name": "debug-pod",
	}) {
		t.Fatal("SelfSubjectAccessReview denied exec on the listed pod")
	}
	if canI(map[string]interface{}{
		"namespace": "team", "verb": "create", "group": "", "resource": "pods",
		"subresource": "exec", "name": "other-pod",
	}) {
		t.Fatal("SelfSubjectAccessReview allowed exec on another pod")
	}
	if canI(map[string]interface{}{
		"namespace": "team", "verb": "create", "group": "", "resource": "pods",
	}) {
		t.Fatal("SelfSubjectAccessReview allowed collection create")
	}

	resp := doAs(t, bob, http.MethodPost, env.URL("/api/v1/namespaces/team/pods/debug-pod/exec"), nil)
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusMethodNotAllowed {
		resp.Body.Close()
		t.Fatalf("exec on the listed pod was refused with %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAs(t, bob, http.MethodPost, env.URL("/api/v1/namespaces/team/pods/other-pod/exec"), nil)
	requireForbidden(t, resp, `pods "other-pod" is forbidden: User "bob" cannot create resource "pods/exec" in API group "" in the namespace "team"`)
}

// TestScaleHostAccessDeploymentToZero pins that scaling, including to zero,
// changes only replicas and is not a host-access write.
func TestScaleHostAccessDeploymentToZero(t *testing.T) {
	env, ca := newHostAccessTestEnv(t)
	alice := tlsClient(ca.issue(t, "alice", nil))
	admin := tlsClient(ca.issue(t, "root", []string{"system:masters"}))
	createRoleForTest(t, env.srv, "team", "deployment-scale", rbacv1.PolicyRule{
		APIGroups: []string{"apps"}, Resources: []string{"deployments/scale"}, Verbs: []string{"patch", "update"},
	})
	createRoleBindingForTest(t, env.srv, "team", "alice-deployment-scale", "Role", "deployment-scale",
		rbacv1.Subject{Kind: "User", Name: "alice"})

	requireStatus(t, doAs(t, admin, http.MethodPost,
		env.URL("/apis/apps/v1/namespaces/team/deployments"), privilegedDeployment("host-dep", "team")),
		http.StatusCreated)

	body := map[string]interface{}{"spec": map[string]interface{}{"replicas": 0}}
	requireStatus(t, doAs(t, alice, http.MethodPatch,
		env.URL("/apis/apps/v1/namespaces/team/deployments/host-dep/scale"), body), http.StatusOK)
	dep, err := env.st.GetDeployment("team", "host-dep")
	if err != nil {
		t.Fatal(err)
	}
	if got := deploymentReplicas(dep); got != 0 {
		t.Fatalf("replicas = %d, want 0", got)
	}
}

// TestApplyDeploymentScaleSubresource pins that server-side apply on the scale
// subresource runs as the PATCH it becomes, and that only the create fallback
// is refused with 405.
func TestApplyDeploymentScaleSubresource(t *testing.T) {
	env, ca := newHostAccessTestEnv(t)
	alice := tlsClient(ca.issue(t, "alice", nil))
	admin := tlsClient(ca.issue(t, "root", []string{"system:masters"}))
	createRoleForTest(t, env.srv, "team", "deployment-scale", rbacv1.PolicyRule{
		APIGroups: []string{"apps"}, Resources: []string{"deployments/scale"}, Verbs: []string{"patch", "update"},
	})
	createRoleBindingForTest(t, env.srv, "team", "alice-deployment-scale", "Role", "deployment-scale",
		rbacv1.Subject{Kind: "User", Name: "alice"})

	requireStatus(t, doAs(t, admin, http.MethodPost,
		env.URL("/apis/apps/v1/namespaces/team/deployments"), reviewPlainDeployment("web", "team")),
		http.StatusCreated)

	body := `apiVersion: autoscaling/v1
kind: Scale
metadata:
  name: web
  namespace: team
spec:
  replicas: 3
`
	requireStatus(t, doRawAs(t, alice, http.MethodPatch,
		env.URL("/apis/apps/v1/namespaces/team/deployments/web/scale"),
		"application/apply-patch+yaml", body), http.StatusOK)
	dep, err := env.st.GetDeployment("team", "web")
	if err != nil {
		t.Fatal(err)
	}
	if got := deploymentReplicas(dep); got != 3 {
		t.Fatalf("replicas = %d after apply, want 3", got)
	}

	missing := `apiVersion: autoscaling/v1
kind: Scale
metadata:
  name: missing
  namespace: team
spec:
  replicas: 1
`
	requireStatus(t, doRawAs(t, alice, http.MethodPatch,
		env.URL("/apis/apps/v1/namespaces/team/deployments/missing/scale"),
		"application/apply-patch+yaml", missing), http.StatusMethodNotAllowed)
	if _, err := env.st.GetDeployment("team", "missing"); err == nil {
		t.Fatal("apply on a missing subresource target created a deployment")
	}
}

// TestPVCAnnotationWithoutHostPathClass covers the annotation on a PVC whose
// resolved storage class is not hostpath: it is not host access.
func TestPVCAnnotationWithoutHostPathClass(t *testing.T) {
	env, ca := newHostAccessTestEnv(t)
	alice := tlsClient(ca.issue(t, "alice", nil))
	createRoleBindingForTest(t, env.srv, "team", "alice-edit", "ClusterRole", "edit",
		rbacv1.Subject{Kind: "User", Name: "alice"})

	standard := quadlet.StorageClassStandard
	cases := []struct {
		name  string
		class *string
	}{
		{name: "explicit standard", class: &standard},
		{name: "default class", class: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pvc := &corev1.PersistentVolumeClaim{
				TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
				ObjectMeta: metav1.ObjectMeta{
					Name:        "annotated-" + strings.ReplaceAll(tc.name, " ", "-"),
					Namespace:   "team",
					Annotations: map[string]string{quadlet.AnnotationHostPath: "/srv/data"},
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					StorageClassName: tc.class,
					AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				},
			}
			requireStatus(t, doAs(t, alice, http.MethodPost,
				env.URL("/api/v1/namespaces/team/persistentvolumeclaims"), pvc), http.StatusCreated)
		})
	}
}

// TestAttachAllNetworksIsHostAccess covers every Pod and Deployment write
// path, including server-side apply, so a non-admin cannot opt into every
// namespace network.
func TestAttachAllNetworksIsHostAccess(t *testing.T) {
	env, ca := newHostAccessTestEnv(t)
	alice := tlsClient(ca.issue(t, "alice", nil))
	admin := tlsClient(ca.issue(t, "root", []string{"system:masters"}))
	createRoleBindingForTest(t, env.srv, "team", "alice-edit", "ClusterRole", "edit",
		rbacv1.Subject{Kind: "User", Name: "alice"})

	requireStatus(t, doAs(t, admin, http.MethodPost,
		env.URL("/api/v1/namespaces/team/pods"), hostAccessPod("pod-base", "team")), http.StatusCreated)
	requireStatus(t, doAs(t, admin, http.MethodPost,
		env.URL("/apis/apps/v1/namespaces/team/deployments"), reviewPlainDeployment("dep-base", "team")),
		http.StatusCreated)

	podApplyPath := func(name string) string {
		return env.URL("/api/v1/namespaces/team/pods/" + name)
	}
	depApplyPath := func(name string) string {
		return env.URL("/apis/apps/v1/namespaces/team/deployments/" + name)
	}

	cases := []struct {
		name   string
		object string
		do     func() *http.Response
	}{
		{
			name:   "pod create",
			object: "pod-created",
			do: func() *http.Response {
				return doAs(t, alice, http.MethodPost, env.URL("/api/v1/namespaces/team/pods"),
					reviewAttachAllPod("pod-created", "team"))
			},
		},
		{
			name:   "pod patch",
			object: "pod-base",
			do: func() *http.Response {
				return doAs(t, alice, http.MethodPatch, env.URL("/api/v1/namespaces/team/pods/pod-base"),
					map[string]interface{}{"metadata": map[string]interface{}{
						"annotations": map[string]interface{}{quadlet.AnnotationAttachAllNetworks: "true"},
					}})
			},
		},
		{
			name:   "pod apply new",
			object: "pod-applied-new",
			do: func() *http.Response {
				return doRawAs(t, alice, http.MethodPatch, podApplyPath("pod-applied-new"),
					"application/apply-patch+yaml", reviewAttachAllPodYAML("pod-applied-new", "team"))
			},
		},
		{
			name:   "pod apply existing",
			object: "pod-base",
			do: func() *http.Response {
				return doRawAs(t, alice, http.MethodPatch, podApplyPath("pod-base"),
					"application/apply-patch+yaml", reviewAttachAllPodYAML("pod-base", "team"))
			},
		},
		{
			name:   "deployment create",
			object: "dep-created",
			do: func() *http.Response {
				return doAs(t, alice, http.MethodPost, env.URL("/apis/apps/v1/namespaces/team/deployments"),
					reviewAttachAllDeployment("dep-created", "team"))
			},
		},
		{
			name:   "deployment update",
			object: "dep-base",
			do: func() *http.Response {
				return doAs(t, alice, http.MethodPut, env.URL("/apis/apps/v1/namespaces/team/deployments/dep-base"),
					reviewAttachAllDeployment("dep-base", "team"))
			},
		},
		{
			name:   "deployment patch",
			object: "dep-base",
			do: func() *http.Response {
				return doAs(t, alice, http.MethodPatch, env.URL("/apis/apps/v1/namespaces/team/deployments/dep-base"),
					map[string]interface{}{"spec": map[string]interface{}{"template": map[string]interface{}{
						"metadata": map[string]interface{}{
							"annotations": map[string]interface{}{quadlet.AnnotationAttachAllNetworks: "true"},
						},
					}}})
			},
		},
		{
			name:   "deployment apply new",
			object: "dep-applied-new",
			do: func() *http.Response {
				return doRawAs(t, alice, http.MethodPatch, depApplyPath("dep-applied-new"),
					"application/apply-patch+yaml", reviewAttachAllDeploymentYAML("dep-applied-new", "team"))
			},
		},
		{
			name:   "deployment apply existing",
			object: "dep-base",
			do: func() *http.Response {
				return doRawAs(t, alice, http.MethodPatch, depApplyPath("dep-base"),
					"application/apply-patch+yaml", reviewAttachAllDeploymentYAML("dep-base", "team"))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := tc.do()
			requireHostAccessForbidden(t, resp, tc.object, "attach-all-networks")
		})
	}

	// An administrator may set the annotation on both kinds.
	requireStatus(t, doAs(t, admin, http.MethodPost,
		env.URL("/api/v1/namespaces/team/pods"), reviewAttachAllPod("pod-admin", "team")), http.StatusCreated)
	requireStatus(t, doAs(t, admin, http.MethodPost,
		env.URL("/apis/apps/v1/namespaces/team/deployments"), reviewAttachAllDeployment("dep-admin", "team")),
		http.StatusCreated)
}

// TestApplyHostAccessCheckedByHandlers pins that removing the early apply
// decode does not lose the host-access refusal: the create and patch handlers
// still check the resulting object.
func TestApplyHostAccessCheckedByHandlers(t *testing.T) {
	env, ca := newHostAccessTestEnv(t)
	alice := tlsClient(ca.issue(t, "alice", nil))
	admin := tlsClient(ca.issue(t, "root", []string{"system:masters"}))
	createRoleBindingForTest(t, env.srv, "team", "alice-edit", "ClusterRole", "edit",
		rbacv1.Subject{Kind: "User", Name: "alice"})

	requireStatus(t, doAs(t, admin, http.MethodPost,
		env.URL("/api/v1/namespaces/team/pods"), hostAccessPod("apply-existing", "team")),
		http.StatusCreated)

	for _, name := range []string{"apply-privileged-new", "apply-existing"} {
		t.Run(name, func(t *testing.T) {
			resp := doRawAs(t, alice, http.MethodPatch,
				env.URL("/api/v1/namespaces/team/pods/"+name),
				"application/apply-patch+yaml", reviewPrivilegedPodYAML(name, "team"))
			requireHostAccessForbidden(t, resp, name, "spec.hostPID")
		})
	}
	if _, err := env.st.GetPod("team", "apply-privileged-new"); err == nil {
		t.Fatal("a refused apply created the pod")
	}
	pod, err := env.st.GetPod("team", "apply-existing")
	if err != nil {
		t.Fatal(err)
	}
	if pod.Spec.HostPID {
		t.Fatal("a refused apply set hostPID on the existing pod")
	}
}

// TestNamedPOSTStatusOrder pins that 401 wins over 405, 403 wins over 405, and
// an authorized named POST is the only case answered with 405.
func TestNamedPOSTStatusOrder(t *testing.T) {
	ca := newTestCA(t)
	ts, srv, _ := newEnforceTestServer(t, ca)
	alice := tlsClient(ca.issue(t, "alice", nil))
	bob := tlsClient(ca.issue(t, "bob", nil))
	admin := tlsClient(ca.issue(t, "root", []string{"system:masters"}))

	createRoleForTest(t, srv, "team", "named-create", rbacv1.PolicyRule{
		APIGroups: []string{""}, Resources: []string{"pods"},
		ResourceNames: []string{"debug"}, Verbs: []string{"create"},
	})
	createRoleBindingForTest(t, srv, "team", "bob-named-create", "Role", "named-create",
		rbacv1.Subject{Kind: "User", Name: "bob"})

	const path = "/api/v1/namespaces/team/pods/debug"
	body := hostAccessPod("debug", "team")

	t.Run("unauthenticated is 401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("unauthorized is 403", func(t *testing.T) {
		resp := doAs(t, alice, http.MethodPost, ts.URL+path, body)
		requireForbidden(t, resp, `pods "debug" is forbidden: User "alice" cannot create resource "pods" in API group "" in the namespace "team"`)
	})

	t.Run("authorized is 405", func(t *testing.T) {
		requireStatus(t, doAs(t, admin, http.MethodPost, ts.URL+path, body), http.StatusMethodNotAllowed)
		requireStatus(t, doAs(t, bob, http.MethodPost, ts.URL+path, body), http.StatusMethodNotAllowed)
	})
}

func TestApplyRefusesNonScaleSubresource(t *testing.T) {
	ca := newTestCA(t)
	ts, _, _ := newEnforceTestServer(t, ca)
	admin := tlsClient(ca.issue(t, "root", []string{"system:masters"}))
	for _, p := range []string{
		"/api/v1/namespaces/default/pods/web/exec",
		"/api/v1/namespaces/default/pods/web/status",
	} {
		req, err := http.NewRequest(http.MethodPatch, ts.URL+p, strings.NewReader("apiVersion: v1\nkind: Pod\nmetadata:\n  name: web\n  namespace: default\n"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/apply-patch+yaml")
		resp, err := admin.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("%s: got %d, want 405", p, resp.StatusCode)
		}
	}
}

func TestEditAndViewIncludeScaleSubresources(t *testing.T) {
	for _, tc := range []struct {
		role *rbacv1.ClusterRole
		verb string
	}{{editClusterRole(), "patch"}, {viewClusterRole(), "get"}} {
		for _, res := range []string{"deployments", "replicasets", "statefulsets"} {
			a := requestAttributes{IsResourceRequest: true, Verb: tc.verb, APIGroup: "apps", Resource: res, Subresource: "scale", Name: "x", Namespace: "t"}
			found := false
			for _, rule := range tc.role.Rules {
				if resourceRuleMatches(rule, a) {
					found = true
				}
			}
			if !found {
				t.Errorf("%s: %s %s/scale not granted", tc.role.Name, tc.verb, res)
			}
		}
	}
}
