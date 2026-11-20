package server

import (
	"net/http"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func externalNameServiceObject(ns, name, externalName string) *corev1.Service {
	return &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.ServiceSpec{
			Type:         corev1.ServiceTypeExternalName,
			ExternalName: externalName,
			Ports:        []corev1.ServicePort{{Port: 80}},
		},
	}
}

func clusterIPServiceObject(ns, name string) *corev1.Service {
	return &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.ServiceSpec{
			Type:  corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{{Port: 80}},
		},
	}
}

func TestExternalNameServiceCreateIsAdminOnly(t *testing.T) {
	env, ca := newHostAccessTestEnv(t)
	alice := tlsClient(ca.issue(t, "alice", nil))
	admin := tlsClient(ca.issue(t, "root", []string{"system:masters"}))
	createRoleBindingForTest(t, env.srv, "team", "alice-edit", "ClusterRole", "edit",
		rbacv1.Subject{Kind: "User", Name: "alice"})
	collection := env.URL("/api/v1/namespaces/team/services")

	requireHostAccessForbidden(t,
		doAs(t, alice, http.MethodPost, collection, externalNameServiceObject("team", "denied", "web.example.com")),
		"denied", "spec.type: ExternalName")
	if _, err := env.st.GetService("team", "denied"); err == nil {
		t.Fatal("forbidden create stored the ExternalName Service")
	}

	requireHostAccessForbidden(t,
		doAs(t, alice, http.MethodPost, collection+"?dryRun=All", externalNameServiceObject("team", "dry-run", "web.example.com")),
		"dry-run", "spec.type: ExternalName")
	if _, err := env.st.GetService("team", "dry-run"); err == nil {
		t.Fatal("forbidden dry-run create stored the ExternalName Service")
	}

	requireStatus(t,
		doAs(t, admin, http.MethodPost, collection, externalNameServiceObject("team", "allowed", "web.example.com")),
		http.StatusCreated)
	created, err := env.st.GetService("team", "allowed")
	if err != nil {
		t.Fatalf("admin create did not store the Service: %v", err)
	}
	if created.Spec.Type != corev1.ServiceTypeExternalName {
		t.Fatalf("admin created Service has type %q", created.Spec.Type)
	}

	requireStatus(t,
		doAs(t, alice, http.MethodPost, collection, clusterIPServiceObject("team", "plain")),
		http.StatusCreated)
}

func TestExternalNameServicePatchIsAdminOnly(t *testing.T) {
	env, ca := newHostAccessTestEnv(t)
	alice := tlsClient(ca.issue(t, "alice", nil))
	admin := tlsClient(ca.issue(t, "root", []string{"system:masters"}))
	createRoleBindingForTest(t, env.srv, "team", "alice-edit", "ClusterRole", "edit",
		rbacv1.Subject{Kind: "User", Name: "alice"})
	collection := env.URL("/api/v1/namespaces/team/services")

	requireStatus(t, doAs(t, admin, http.MethodPost, collection, clusterIPServiceObject("team", "cluster")), http.StatusCreated)
	requireStatus(t, doAs(t, admin, http.MethodPost, collection, externalNameServiceObject("team", "backend", "old.example.com")), http.StatusCreated)

	clusterURL := collection + "/cluster"
	requireHostAccessForbidden(t,
		doAs(t, alice, http.MethodPatch, clusterURL, map[string]interface{}{
			"spec": map[string]interface{}{"type": "ExternalName", "externalName": "web.example.com"},
		}),
		"cluster", "spec.type: ExternalName")
	stored, err := env.st.GetService("team", "cluster")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Spec.Type == corev1.ServiceTypeExternalName {
		t.Fatal("forbidden patch changed the Service to ExternalName")
	}

	backendURL := collection + "/backend"
	requireHostAccessForbidden(t,
		doAs(t, alice, http.MethodPatch, backendURL, map[string]interface{}{
			"spec": map[string]interface{}{"externalName": "new.example.com"},
		}),
		"backend", "spec.type: ExternalName")
	stored, err = env.st.GetService("team", "backend")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Spec.ExternalName != "old.example.com" {
		t.Fatalf("forbidden patch changed externalName to %q", stored.Spec.ExternalName)
	}

	requireStatus(t,
		doAs(t, alice, http.MethodPatch, backendURL, map[string]interface{}{
			"spec": map[string]interface{}{"type": "ClusterIP"},
		}),
		http.StatusOK)
	stored, err = env.st.GetService("team", "backend")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Fatalf("changing away from ExternalName left type %q", stored.Spec.Type)
	}

	requireStatus(t, doAs(t, admin, http.MethodPost, collection, clusterIPServiceObject("team", "json-cluster")), http.StatusCreated)
	jsonPatch := `[{"op":"replace","path":"/spec/type","value":"ExternalName"},` +
		`{"op":"add","path":"/spec/externalName","value":"web.example.com"}]`
	requireHostAccessForbidden(t,
		doRawAs(t, alice, http.MethodPatch, collection+"/json-cluster", "application/json-patch+json", jsonPatch),
		"json-cluster", "spec.type: ExternalName")
}

func TestExternalNameServicePutIsAdminOnly(t *testing.T) {
	env, ca := newHostAccessTestEnv(t)
	alice := tlsClient(ca.issue(t, "alice", nil))
	admin := tlsClient(ca.issue(t, "root", []string{"system:masters"}))
	createRoleBindingForTest(t, env.srv, "team", "alice-edit", "ClusterRole", "edit",
		rbacv1.Subject{Kind: "User", Name: "alice"})
	collection := env.URL("/api/v1/namespaces/team/services")

	requireStatus(t, doAs(t, admin, http.MethodPost, collection, externalNameServiceObject("team", "backend", "old.example.com")), http.StatusCreated)
	requireStatus(t, doAs(t, admin, http.MethodPost, collection, clusterIPServiceObject("team", "cluster")), http.StatusCreated)

	requireHostAccessForbidden(t,
		doAs(t, alice, http.MethodPut, collection+"/backend", externalNameServiceObject("team", "backend", "new.example.com")),
		"backend", "spec.type: ExternalName")

	requireHostAccessForbidden(t,
		doAs(t, alice, http.MethodPut, collection+"/cluster", externalNameServiceObject("team", "cluster", "web.example.com")),
		"cluster", "spec.type: ExternalName")

	requireStatus(t,
		doAs(t, alice, http.MethodPut, collection+"/backend", clusterIPServiceObject("team", "backend")),
		http.StatusOK)
	stored, err := env.st.GetService("team", "backend")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Fatalf("changing away from ExternalName left type %q", stored.Spec.Type)
	}
}

func TestExternalNameServiceApplyIsAdminOnly(t *testing.T) {
	env, ca := newHostAccessTestEnv(t)
	alice := tlsClient(ca.issue(t, "alice", nil))
	admin := tlsClient(ca.issue(t, "root", []string{"system:masters"}))
	createRoleBindingForTest(t, env.srv, "team", "alice-edit", "ClusterRole", "edit",
		rbacv1.Subject{Kind: "User", Name: "alice"})
	collection := env.URL("/api/v1/namespaces/team/services")

	applyYAML := `apiVersion: v1
kind: Service
metadata:
  name: applied
  namespace: team
spec:
  type: ExternalName
  externalName: web.example.com
  ports:
    - port: 80
`
	requireHostAccessForbidden(t,
		doRawAs(t, alice, http.MethodPatch, collection+"/applied", "application/apply-patch+yaml", applyYAML),
		"applied", "spec.type: ExternalName")
	if _, err := env.st.GetService("team", "applied"); err == nil {
		t.Fatal("forbidden apply created the ExternalName Service")
	}

	requireHostAccessForbidden(t,
		doRawAs(t, alice, http.MethodPatch, collection+"/applied?dryRun=All", "application/apply-patch+yaml", applyYAML),
		"applied", "spec.type: ExternalName")
	if _, err := env.st.GetService("team", "applied"); err == nil {
		t.Fatal("forbidden dry-run apply created the ExternalName Service")
	}

	adminYAML := `apiVersion: v1
kind: Service
metadata:
  name: admin-dry
  namespace: team
spec:
  type: ExternalName
  externalName: web.example.com
  ports:
    - port: 80
`
	requireStatus(t,
		doRawAs(t, admin, http.MethodPatch, collection+"/admin-dry?dryRun=All", "application/apply-patch+yaml", adminYAML),
		http.StatusCreated)
	if _, err := env.st.GetService("team", "admin-dry"); err == nil {
		t.Fatal("admin dry-run apply stored the ExternalName Service")
	}

	requireStatus(t, doAs(t, admin, http.MethodPost, collection, clusterIPServiceObject("team", "target")), http.StatusCreated)
	updateYAML := `apiVersion: v1
kind: Service
metadata:
  name: target
  namespace: team
spec:
  type: ExternalName
  externalName: web.example.com
  ports:
    - port: 80
`
	requireHostAccessForbidden(t,
		doRawAs(t, alice, http.MethodPatch, collection+"/target", "application/apply-patch+yaml", updateYAML),
		"target", "spec.type: ExternalName")
	stored, err := env.st.GetService("team", "target")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Spec.Type == corev1.ServiceTypeExternalName {
		t.Fatal("forbidden apply changed the Service to ExternalName")
	}
}
