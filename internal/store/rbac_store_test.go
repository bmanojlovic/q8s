package store_test

import (
	"path/filepath"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"q8s/internal/store"
)

func makeRole(ns, name string) *rbacv1.Role {
	return &rbacv1.Role{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{"app": name}},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{"pods"},
			Verbs:     []string{"get", "list"},
		}},
	}
}

func makeRoleBinding(ns, name string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     "view",
		},
		Subjects: []rbacv1.Subject{{Kind: "User", Name: "alice"}},
	}
}

func makeClusterRole(name string) *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Rules: []rbacv1.PolicyRule{{
			NonResourceURLs: []string{"/healthz"},
			Verbs:           []string{"get"},
		}},
	}
}

func makeClusterRoleBinding(name string) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     "view",
		},
		Subjects: []rbacv1.Subject{{Kind: "Group", Name: "developers"}},
	}
}

func TestRBACKindsPersistenceRoundTrip(t *testing.T) {
	file := filepath.Join(settledTempDir(t), "store.json")
	st1, err := store.Load(file)
	if err != nil {
		t.Fatalf("Load (new file): %v", err)
	}
	createdRole, err := st1.CreateRole(makeRole("default", "reader"))
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	createdRB, err := st1.CreateRoleBinding(makeRoleBinding("default", "readers"))
	if err != nil {
		t.Fatalf("CreateRoleBinding: %v", err)
	}
	createdCR, err := st1.CreateClusterRole(makeClusterRole("viewer"))
	if err != nil {
		t.Fatalf("CreateClusterRole: %v", err)
	}
	createdCRB, err := st1.CreateClusterRoleBinding(makeClusterRoleBinding("viewers"))
	if err != nil {
		t.Fatalf("CreateClusterRoleBinding: %v", err)
	}

	st2 := reloadUntil(t, file, func(st *store.Store) bool {
		_, errRole := st.GetRole("default", "reader")
		_, errRB := st.GetRoleBinding("default", "readers")
		_, errCR := st.GetClusterRole("viewer")
		_, errCRB := st.GetClusterRoleBinding("viewers")
		return errRole == nil && errRB == nil && errCR == nil && errCRB == nil
	})

	role, err := st2.GetRole("default", "reader")
	if err != nil {
		t.Fatalf("Role not restored: %v", err)
	}
	if len(role.Rules) != 1 || len(role.Rules[0].Verbs) != 2 || role.UID != createdRole.UID {
		t.Fatalf("Role not round-tripped: %+v", role)
	}

	rb, err := st2.GetRoleBinding("default", "readers")
	if err != nil {
		t.Fatalf("RoleBinding not restored: %v", err)
	}
	if rb.RoleRef.Kind != "Role" || rb.RoleRef.Name != "view" || len(rb.Subjects) != 1 ||
		rb.Subjects[0].Name != "alice" || rb.UID != createdRB.UID {
		t.Fatalf("RoleBinding not round-tripped: %+v", rb)
	}

	cr, err := st2.GetClusterRole("viewer")
	if err != nil {
		t.Fatalf("ClusterRole not restored: %v", err)
	}
	if len(cr.Rules) != 1 || len(cr.Rules[0].NonResourceURLs) != 1 ||
		cr.Rules[0].NonResourceURLs[0] != "/healthz" || cr.UID != createdCR.UID {
		t.Fatalf("ClusterRole not round-tripped: %+v", cr)
	}

	crb, err := st2.GetClusterRoleBinding("viewers")
	if err != nil {
		t.Fatalf("ClusterRoleBinding not restored: %v", err)
	}
	if crb.RoleRef.Kind != "ClusterRole" || crb.RoleRef.Name != "view" ||
		len(crb.Subjects) != 1 || crb.Subjects[0].Name != "developers" || crb.UID != createdCRB.UID {
		t.Fatalf("ClusterRoleBinding not round-tripped: %+v", crb)
	}

	// Updates keep the UID and get a new resource version.
	updated := role.DeepCopy()
	updated.Rules[0].Verbs = []string{"watch"}
	got, err := st2.UpdateRole(updated)
	if err != nil {
		t.Fatalf("UpdateRole: %v", err)
	}
	if got.UID != role.UID {
		t.Fatalf("UpdateRole changed UID: %q -> %q", role.UID, got.UID)
	}
	if got.ResourceVersion == role.ResourceVersion {
		t.Fatal("UpdateRole did not bump resourceVersion")
	}
}

func TestPurgeNamespaceRemovesRBACKinds(t *testing.T) {
	st := store.New()
	if _, err := st.CreateRole(makeRole("default", "reader")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRole(makeRole("other", "reader2")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRoleBinding(makeRoleBinding("default", "readers")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRoleBinding(makeRoleBinding("other", "readers2")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateClusterRole(makeClusterRole("viewer")); err != nil {
		t.Fatal(err)
	}

	st.PurgeNamespace("default")

	if _, err := st.GetRole("default", "reader"); err == nil {
		t.Fatal("default Role survived namespace purge")
	}
	if _, err := st.GetRoleBinding("default", "readers"); err == nil {
		t.Fatal("default RoleBinding survived namespace purge")
	}
	if _, err := st.GetRole("other", "reader2"); err != nil {
		t.Fatalf("other namespace Role was purged: %v", err)
	}
	if _, err := st.GetRoleBinding("other", "readers2"); err != nil {
		t.Fatalf("other namespace RoleBinding was purged: %v", err)
	}
	if _, err := st.GetClusterRole("viewer"); err != nil {
		t.Fatalf("ClusterRole was purged with a namespace: %v", err)
	}
}
