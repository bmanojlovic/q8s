package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const rbacAPIGroup = "rbac.authorization.k8s.io"
const rbacAPIVersion = rbacAPIGroup + "/v1"

// --- validation ---

// validatePolicyRules checks the rules shared by Roles and ClusterRoles.
// nonResourceURLs are only meaningful on a ClusterRole, matching Kubernetes.
func validatePolicyRules(rules []rbacv1.PolicyRule, clusterRole bool) error {
	for i, rule := range rules {
		if len(rule.Verbs) == 0 {
			return fmt.Errorf("rules[%d].verbs: must have at least one verb", i)
		}
		if len(rule.NonResourceURLs) > 0 {
			if !clusterRole {
				return fmt.Errorf("rules[%d].nonResourceURLs: non-resource URL rules are only allowed in ClusterRoles", i)
			}
			if len(rule.APIGroups) > 0 || len(rule.Resources) > 0 || len(rule.ResourceNames) > 0 {
				return fmt.Errorf("rules[%d]: nonResourceURLs cannot be combined with apiGroups, resources or resourceNames", i)
			}
			continue
		}
		if len(rule.APIGroups) == 0 {
			return fmt.Errorf("rules[%d].apiGroups: must have at least one apiGroup (use \"\" for the core group)", i)
		}
		if len(rule.Resources) == 0 {
			return fmt.Errorf("rules[%d].resources: must have at least one resource", i)
		}
	}
	return nil
}

func validateRole(role *rbacv1.Role) error {
	return validatePolicyRules(role.Rules, false)
}

func validateClusterRole(role *rbacv1.ClusterRole) error {
	return validatePolicyRules(role.Rules, true)
}

func validateRoleRef(ref rbacv1.RoleRef, clusterRoleBinding bool) error {
	if ref.APIGroup != rbacAPIGroup {
		return fmt.Errorf("roleRef.apiGroup must be %q, got %q", rbacAPIGroup, ref.APIGroup)
	}
	switch ref.Kind {
	case "Role":
		if clusterRoleBinding {
			return fmt.Errorf("roleRef.kind must be ClusterRole in a ClusterRoleBinding, got %q", ref.Kind)
		}
	case "ClusterRole":
	default:
		return fmt.Errorf("roleRef.kind must be Role or ClusterRole, got %q", ref.Kind)
	}
	if ref.Name == "" {
		return fmt.Errorf("roleRef.name must not be empty")
	}
	if err := validateName("roleRef.name", ref.Name); err != nil {
		return err
	}
	return nil
}

func validateSubjects(subjects []rbacv1.Subject) error {
	for i, subject := range subjects {
		switch subject.Kind {
		case "User", "Group", "ServiceAccount":
		default:
			return fmt.Errorf("subjects[%d].kind must be User, Group or ServiceAccount, got %q", i, subject.Kind)
		}
	}
	return nil
}

// validateRBACIdentity is validatePatchedIdentity for both namespaced and
// cluster-scoped RBAC objects. Cluster-scoped objects carry no namespace, so
// only the name is checked there.
func validateRBACIdentity(kind, patchedNS, patchedName, urlNS, urlName string) error {
	if urlNS != "" {
		return validatePatchedIdentity(kind, patchedNS, patchedName, urlNS, urlName)
	}
	if patchedNS != "" {
		return fmt.Errorf("%s %q: namespace is not allowed on a cluster-scoped object", kind, urlName)
	}
	if err := validateName("name", patchedName); err != nil {
		return err
	}
	if patchedName != urlName {
		return fmt.Errorf("%s %q: name is immutable", kind, urlName)
	}
	return nil
}

// validateRoleRefUnchanged rejects an update that changes roleRef. Kubernetes
// treats roleRef as immutable once a binding exists.
func validateRoleRefUnchanged(oldRef, newRef rbacv1.RoleRef) error {
	if !reflect.DeepEqual(oldRef, newRef) {
		return fmt.Errorf("cannot change roleRef")
	}
	return nil
}

func validateRoleBinding(rb *rbacv1.RoleBinding, oldRef *rbacv1.RoleRef) error {
	if err := validateRoleRef(rb.RoleRef, false); err != nil {
		return err
	}
	if err := validateSubjects(rb.Subjects); err != nil {
		return err
	}
	if oldRef != nil {
		return validateRoleRefUnchanged(*oldRef, rb.RoleRef)
	}
	return nil
}

func validateClusterRoleBinding(crb *rbacv1.ClusterRoleBinding, oldRef *rbacv1.RoleRef) error {
	if err := validateRoleRef(crb.RoleRef, true); err != nil {
		return err
	}
	if err := validateSubjects(crb.Subjects); err != nil {
		return err
	}
	if oldRef != nil {
		return validateRoleRefUnchanged(*oldRef, crb.RoleRef)
	}
	return nil
}

// --- routing ---

func (s *Server) handleRBACNamespaced(w http.ResponseWriter, r *http.Request) {
	ns, resource, name, ok := parseRBACNamespaceResource(r.URL.Path)
	if !ok {
		s.respondStatus(w, http.StatusBadRequest, "BadRequest", "invalid rbac.authorization.k8s.io namespace resource path")
		return
	}
	switch resource {
	case "roles":
		s.handleRoles(w, r, ns, name)
	case "rolebindings":
		s.handleRoleBindings(w, r, ns, name)
	default:
		s.respondStatus(w, http.StatusNotFound, "NotFound", "resource %q not found", resource)
	}
}

func (s *Server) handleRBACAllNamespaced(w http.ResponseWriter, r *http.Request, resource string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	switch resource {
	case "roles":
		respondList(w, r, s, s.config.Store.AllRoles, rolesToTable,
			func(items []rbacv1.Role) *rbacv1.RoleList {
				return &rbacv1.RoleList{TypeMeta: metav1.TypeMeta{APIVersion: rbacAPIVersion, Kind: "RoleList"}, Items: items}
			})
	case "rolebindings":
		respondList(w, r, s, s.config.Store.AllRoleBindings, roleBindingsToTable,
			func(items []rbacv1.RoleBinding) *rbacv1.RoleBindingList {
				return &rbacv1.RoleBindingList{TypeMeta: metav1.TypeMeta{APIVersion: rbacAPIVersion, Kind: "RoleBindingList"}, Items: items}
			})
	default:
		s.respondStatus(w, http.StatusNotFound, "NotFound", "resource %q not found", resource)
	}
}

// --- Roles ---

func (s *Server) handleRoles(w http.ResponseWriter, r *http.Request, ns, name string) {
	switch r.Method {
	case http.MethodGet:
		if name == "" {
			respondList(w, r, s, func() []*rbacv1.Role { return s.config.Store.Roles(ns) }, rolesToTable,
				func(items []rbacv1.Role) *rbacv1.RoleList {
					return &rbacv1.RoleList{TypeMeta: metav1.TypeMeta{APIVersion: rbacAPIVersion, Kind: "RoleList"}, Items: items}
				})
			return
		}
		role, err := s.config.Store.GetRole(ns, name)
		if err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		if isTableRequest(r) {
			encodeTable(w, rolesToTable([]*rbacv1.Role{role}, s.rv()))
			return
		}
		encode(w, role, http.StatusOK)
	case http.MethodPost:
		var role rbacv1.Role
		if !s.decodeOrRespond(w, r, &role) {
			return
		}
		role.APIVersion = rbacAPIVersion
		role.Kind = "Role"
		if !s.objectIdentityMatchesURL(w, &role, ns, "") {
			return
		}
		if err := validateName("namespace", role.Namespace); err != nil {
			s.respondStatus(w, http.StatusUnprocessableEntity, "Invalid", "%s", err.Error())
			return
		}
		if err := validateName("name", role.Name); err != nil {
			s.respondStatus(w, http.StatusUnprocessableEntity, "Invalid", "%s", err.Error())
			return
		}
		if err := validateRole(&role); err != nil {
			s.respondStatus(w, http.StatusUnprocessableEntity, "Invalid", "%s", err.Error())
			return
		}
		created, err := s.config.Store.CreateRole(&role)
		if err != nil {
			s.respondStoreError(w, err, http.StatusConflict, "AlreadyExists")
			return
		}
		encode(w, created, http.StatusCreated)
	case http.MethodPut:
		var role rbacv1.Role
		if !s.decodeOrRespond(w, r, &role) {
			return
		}
		role.APIVersion = rbacAPIVersion
		role.Kind = "Role"
		if !s.objectIdentityMatchesURL(w, &role, ns, name) {
			return
		}
		if err := validateName("namespace", role.Namespace); err != nil {
			s.respondStatus(w, http.StatusUnprocessableEntity, "Invalid", "%s", err.Error())
			return
		}
		if err := validateName("name", role.Name); err != nil {
			s.respondStatus(w, http.StatusUnprocessableEntity, "Invalid", "%s", err.Error())
			return
		}
		if err := validateRole(&role); err != nil {
			s.respondStatus(w, http.StatusUnprocessableEntity, "Invalid", "%s", err.Error())
			return
		}
		updated, err := s.config.Store.UpdateRole(&role)
		if err != nil {
			s.respondStoreError(w, err, http.StatusNotFound, "NotFound")
			return
		}
		encode(w, updated, http.StatusOK)
	case http.MethodPatch:
		existing, err := s.config.Store.GetRole(ns, name)
		if err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		var patched rbacv1.Role
		if !s.patchRBACObject(w, r, existing, &patched, ns, name, "role") {
			return
		}
		patched.APIVersion = rbacAPIVersion
		patched.Kind = "Role"
		if err := validateRole(&patched); err != nil {
			s.respondStatus(w, http.StatusUnprocessableEntity, "Invalid", "%s", err.Error())
			return
		}
		updated, err := s.config.Store.UpdateRole(&patched)
		if err != nil {
			s.respondStoreError(w, err, http.StatusInternalServerError, "InternalError")
			return
		}
		encode(w, updated, http.StatusOK)
	case http.MethodDelete:
		if name == "" {
			s.deleteCollection(w,
				func() []string {
					var names []string
					for _, role := range s.config.Store.Roles(ns) {
						names = append(names, role.Name)
					}
					return names
				},
				func(n string) error { return s.config.Store.DeleteRole(ns, n) })
			return
		}
		if err := s.config.Store.DeleteRole(ns, name); err != nil {
			s.respondStoreError(w, err, http.StatusNotFound, "NotFound")
			return
		}
		encode(w, &metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Success"}, http.StatusOK)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// --- RoleBindings ---

func (s *Server) handleRoleBindings(w http.ResponseWriter, r *http.Request, ns, name string) {
	switch r.Method {
	case http.MethodGet:
		if name == "" {
			respondList(w, r, s, func() []*rbacv1.RoleBinding { return s.config.Store.RoleBindings(ns) }, roleBindingsToTable,
				func(items []rbacv1.RoleBinding) *rbacv1.RoleBindingList {
					return &rbacv1.RoleBindingList{TypeMeta: metav1.TypeMeta{APIVersion: rbacAPIVersion, Kind: "RoleBindingList"}, Items: items}
				})
			return
		}
		rb, err := s.config.Store.GetRoleBinding(ns, name)
		if err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		if isTableRequest(r) {
			encodeTable(w, roleBindingsToTable([]*rbacv1.RoleBinding{rb}, s.rv()))
			return
		}
		encode(w, rb, http.StatusOK)
	case http.MethodPost:
		var rb rbacv1.RoleBinding
		if !s.decodeOrRespond(w, r, &rb) {
			return
		}
		rb.APIVersion = rbacAPIVersion
		rb.Kind = "RoleBinding"
		if !s.objectIdentityMatchesURL(w, &rb, ns, "") {
			return
		}
		if err := validateName("namespace", rb.Namespace); err != nil {
			s.respondStatus(w, http.StatusUnprocessableEntity, "Invalid", "%s", err.Error())
			return
		}
		if err := validateName("name", rb.Name); err != nil {
			s.respondStatus(w, http.StatusUnprocessableEntity, "Invalid", "%s", err.Error())
			return
		}
		if err := validateRoleBinding(&rb, nil); err != nil {
			s.respondStatus(w, http.StatusUnprocessableEntity, "Invalid", "%s", err.Error())
			return
		}
		created, err := s.config.Store.CreateRoleBinding(&rb)
		if err != nil {
			s.respondStoreError(w, err, http.StatusConflict, "AlreadyExists")
			return
		}
		encode(w, created, http.StatusCreated)
	case http.MethodPut:
		existing, err := s.config.Store.GetRoleBinding(ns, name)
		if err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		var rb rbacv1.RoleBinding
		if !s.decodeOrRespond(w, r, &rb) {
			return
		}
		rb.APIVersion = rbacAPIVersion
		rb.Kind = "RoleBinding"
		if !s.objectIdentityMatchesURL(w, &rb, ns, name) {
			return
		}
		if err := validateName("namespace", rb.Namespace); err != nil {
			s.respondStatus(w, http.StatusUnprocessableEntity, "Invalid", "%s", err.Error())
			return
		}
		if err := validateName("name", rb.Name); err != nil {
			s.respondStatus(w, http.StatusUnprocessableEntity, "Invalid", "%s", err.Error())
			return
		}
		if err := validateRoleBinding(&rb, &existing.RoleRef); err != nil {
			s.respondStatus(w, http.StatusUnprocessableEntity, "Invalid", "%s", err.Error())
			return
		}
		updated, err := s.config.Store.UpdateRoleBinding(&rb)
		if err != nil {
			s.respondStoreError(w, err, http.StatusNotFound, "NotFound")
			return
		}
		encode(w, updated, http.StatusOK)
	case http.MethodPatch:
		existing, err := s.config.Store.GetRoleBinding(ns, name)
		if err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		var patched rbacv1.RoleBinding
		if !s.patchRBACObject(w, r, existing, &patched, ns, name, "rolebinding") {
			return
		}
		patched.APIVersion = rbacAPIVersion
		patched.Kind = "RoleBinding"
		if err := validateRoleBinding(&patched, &existing.RoleRef); err != nil {
			s.respondStatus(w, http.StatusUnprocessableEntity, "Invalid", "%s", err.Error())
			return
		}
		updated, err := s.config.Store.UpdateRoleBinding(&patched)
		if err != nil {
			s.respondStoreError(w, err, http.StatusInternalServerError, "InternalError")
			return
		}
		encode(w, updated, http.StatusOK)
	case http.MethodDelete:
		if name == "" {
			s.deleteCollection(w,
				func() []string {
					var names []string
					for _, rb := range s.config.Store.RoleBindings(ns) {
						names = append(names, rb.Name)
					}
					return names
				},
				func(n string) error { return s.config.Store.DeleteRoleBinding(ns, n) })
			return
		}
		if err := s.config.Store.DeleteRoleBinding(ns, name); err != nil {
			s.respondStoreError(w, err, http.StatusNotFound, "NotFound")
			return
		}
		encode(w, &metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Success"}, http.StatusOK)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// --- ClusterRoles ---

func (s *Server) handleClusterRoles(w http.ResponseWriter, r *http.Request) {
	name := strings.Trim(strings.TrimPrefix(r.URL.Path, "/apis/"+rbacAPIVersion+"/clusterroles"), "/")
	switch r.Method {
	case http.MethodGet:
		if name == "" {
			respondList(w, r, s, s.allClusterRoles, clusterRolesToTable,
				func(items []rbacv1.ClusterRole) *rbacv1.ClusterRoleList {
					return &rbacv1.ClusterRoleList{TypeMeta: metav1.TypeMeta{APIVersion: rbacAPIVersion, Kind: "ClusterRoleList"}, Items: items}
				})
			return
		}
		cr, err := s.getClusterRole(name)
		if err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		if isTableRequest(r) {
			encodeTable(w, clusterRolesToTable([]*rbacv1.ClusterRole{cr}, s.rv()))
			return
		}
		encode(w, cr, http.StatusOK)
	case http.MethodPost:
		var cr rbacv1.ClusterRole
		if !s.decodeOrRespond(w, r, &cr) {
			return
		}
		cr.APIVersion = rbacAPIVersion
		cr.Kind = "ClusterRole"
		if err := validateName("name", cr.Name); err != nil {
			s.respondStatus(w, http.StatusUnprocessableEntity, "Invalid", "%s", err.Error())
			return
		}
		if isBuiltinClusterRole(cr.Name) {
			s.respondStatus(w, http.StatusConflict, "AlreadyExists", "clusterroles.rbac.authorization.k8s.io %q already exists", cr.Name)
			return
		}
		if err := validateClusterRole(&cr); err != nil {
			s.respondStatus(w, http.StatusUnprocessableEntity, "Invalid", "%s", err.Error())
			return
		}
		created, err := s.config.Store.CreateClusterRole(&cr)
		if err != nil {
			s.respondStoreError(w, err, http.StatusConflict, "AlreadyExists")
			return
		}
		encode(w, created, http.StatusCreated)
	case http.MethodPut:
		if isBuiltinClusterRole(name) {
			s.respondStatus(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "clusterroles.rbac.authorization.k8s.io %q is read-only", name)
			return
		}
		if _, err := s.config.Store.GetClusterRole(name); err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		var cr rbacv1.ClusterRole
		if !s.decodeOrRespond(w, r, &cr) {
			return
		}
		cr.APIVersion = rbacAPIVersion
		cr.Kind = "ClusterRole"
		if cr.Name == "" {
			cr.Name = name
		}
		if err := validateRBACIdentity("clusterrole", cr.Namespace, cr.Name, "", name); err != nil {
			s.respondStatus(w, http.StatusUnprocessableEntity, "Invalid", "%s", err.Error())
			return
		}
		if err := validateClusterRole(&cr); err != nil {
			s.respondStatus(w, http.StatusUnprocessableEntity, "Invalid", "%s", err.Error())
			return
		}
		updated, err := s.config.Store.UpdateClusterRole(&cr)
		if err != nil {
			s.respondStoreError(w, err, http.StatusNotFound, "NotFound")
			return
		}
		encode(w, updated, http.StatusOK)
	case http.MethodPatch:
		if isBuiltinClusterRole(name) {
			s.respondStatus(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "clusterroles.rbac.authorization.k8s.io %q is read-only", name)
			return
		}
		existing, err := s.config.Store.GetClusterRole(name)
		if err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		var patched rbacv1.ClusterRole
		if !s.patchRBACObject(w, r, existing, &patched, "", name, "clusterrole") {
			return
		}
		patched.APIVersion = rbacAPIVersion
		patched.Kind = "ClusterRole"
		if err := validateClusterRole(&patched); err != nil {
			s.respondStatus(w, http.StatusUnprocessableEntity, "Invalid", "%s", err.Error())
			return
		}
		updated, err := s.config.Store.UpdateClusterRole(&patched)
		if err != nil {
			s.respondStoreError(w, err, http.StatusInternalServerError, "InternalError")
			return
		}
		encode(w, updated, http.StatusOK)
	case http.MethodDelete:
		if name == "" {
			s.deleteCollection(w,
				func() []string {
					var names []string
					for _, cr := range s.config.Store.AllClusterRoles() {
						names = append(names, cr.Name)
					}
					return names
				},
				func(n string) error { return s.config.Store.DeleteClusterRole(n) })
			return
		}
		if isBuiltinClusterRole(name) {
			s.respondStatus(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "clusterroles.rbac.authorization.k8s.io %q is read-only", name)
			return
		}
		if err := s.config.Store.DeleteClusterRole(name); err != nil {
			s.respondStoreError(w, err, http.StatusNotFound, "NotFound")
			return
		}
		encode(w, &metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Success"}, http.StatusOK)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// --- ClusterRoleBindings ---

func (s *Server) handleClusterRoleBindings(w http.ResponseWriter, r *http.Request) {
	name := strings.Trim(strings.TrimPrefix(r.URL.Path, "/apis/"+rbacAPIVersion+"/clusterrolebindings"), "/")
	switch r.Method {
	case http.MethodGet:
		if name == "" {
			respondList(w, r, s, s.config.Store.AllClusterRoleBindings, clusterRoleBindingsToTable,
				func(items []rbacv1.ClusterRoleBinding) *rbacv1.ClusterRoleBindingList {
					return &rbacv1.ClusterRoleBindingList{TypeMeta: metav1.TypeMeta{APIVersion: rbacAPIVersion, Kind: "ClusterRoleBindingList"}, Items: items}
				})
			return
		}
		crb, err := s.config.Store.GetClusterRoleBinding(name)
		if err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		if isTableRequest(r) {
			encodeTable(w, clusterRoleBindingsToTable([]*rbacv1.ClusterRoleBinding{crb}, s.rv()))
			return
		}
		encode(w, crb, http.StatusOK)
	case http.MethodPost:
		var crb rbacv1.ClusterRoleBinding
		if !s.decodeOrRespond(w, r, &crb) {
			return
		}
		crb.APIVersion = rbacAPIVersion
		crb.Kind = "ClusterRoleBinding"
		if err := validateName("name", crb.Name); err != nil {
			s.respondStatus(w, http.StatusUnprocessableEntity, "Invalid", "%s", err.Error())
			return
		}
		if err := validateClusterRoleBinding(&crb, nil); err != nil {
			s.respondStatus(w, http.StatusUnprocessableEntity, "Invalid", "%s", err.Error())
			return
		}
		created, err := s.config.Store.CreateClusterRoleBinding(&crb)
		if err != nil {
			s.respondStoreError(w, err, http.StatusConflict, "AlreadyExists")
			return
		}
		encode(w, created, http.StatusCreated)
	case http.MethodPut:
		existing, err := s.config.Store.GetClusterRoleBinding(name)
		if err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		var crb rbacv1.ClusterRoleBinding
		if !s.decodeOrRespond(w, r, &crb) {
			return
		}
		crb.APIVersion = rbacAPIVersion
		crb.Kind = "ClusterRoleBinding"
		if crb.Name == "" {
			crb.Name = name
		}
		if err := validateRBACIdentity("clusterrolebinding", crb.Namespace, crb.Name, "", name); err != nil {
			s.respondStatus(w, http.StatusUnprocessableEntity, "Invalid", "%s", err.Error())
			return
		}
		if err := validateClusterRoleBinding(&crb, &existing.RoleRef); err != nil {
			s.respondStatus(w, http.StatusUnprocessableEntity, "Invalid", "%s", err.Error())
			return
		}
		updated, err := s.config.Store.UpdateClusterRoleBinding(&crb)
		if err != nil {
			s.respondStoreError(w, err, http.StatusNotFound, "NotFound")
			return
		}
		encode(w, updated, http.StatusOK)
	case http.MethodPatch:
		existing, err := s.config.Store.GetClusterRoleBinding(name)
		if err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		var patched rbacv1.ClusterRoleBinding
		if !s.patchRBACObject(w, r, existing, &patched, "", name, "clusterrolebinding") {
			return
		}
		patched.APIVersion = rbacAPIVersion
		patched.Kind = "ClusterRoleBinding"
		if err := validateClusterRoleBinding(&patched, &existing.RoleRef); err != nil {
			s.respondStatus(w, http.StatusUnprocessableEntity, "Invalid", "%s", err.Error())
			return
		}
		updated, err := s.config.Store.UpdateClusterRoleBinding(&patched)
		if err != nil {
			s.respondStoreError(w, err, http.StatusInternalServerError, "InternalError")
			return
		}
		encode(w, updated, http.StatusOK)
	case http.MethodDelete:
		if name == "" {
			s.deleteCollection(w,
				func() []string {
					var names []string
					for _, crb := range s.config.Store.AllClusterRoleBindings() {
						names = append(names, crb.Name)
					}
					return names
				},
				func(n string) error { return s.config.Store.DeleteClusterRoleBinding(n) })
			return
		}
		if err := s.config.Store.DeleteClusterRoleBinding(name); err != nil {
			s.respondStoreError(w, err, http.StatusNotFound, "NotFound")
			return
		}
		encode(w, &metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Success"}, http.StatusOK)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// patchRBACObject applies a merge, strategic merge or JSON patch to an RBAC
// object's JSON view, decodes the result into target and checks that the patch
// did not change its identity.
func (s *Server) patchRBACObject(w http.ResponseWriter, r *http.Request, existing interface{}, target interface{}, ns, name, kind string) bool {
	body, ok := s.readBody(w, r)
	if !ok {
		return false
	}
	base, err := toJSONMap(existing)
	if err != nil {
		s.respondStatus(w, http.StatusInternalServerError, "InternalError", "%s", err.Error())
		return false
	}
	if isJSONPatch(r) {
		var ops []map[string]interface{}
		if err := json.Unmarshal(body, &ops); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
			return false
		}
		if err := applyJSONPatch(base, ops); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
			return false
		}
	} else {
		var overlay map[string]interface{}
		if err := json.Unmarshal(body, &overlay); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
			return false
		}
		jsonMerge(base, overlay)
	}
	merged, err := json.Marshal(base)
	if err != nil {
		s.respondStatus(w, http.StatusInternalServerError, "InternalError", "%s", err.Error())
		return false
	}
	if err := json.Unmarshal(merged, target); err != nil {
		s.respondStatus(w, http.StatusUnprocessableEntity, "Invalid", "%s", err.Error())
		return false
	}
	meta, ok := target.(metav1.Object)
	if !ok {
		s.respondStatus(w, http.StatusInternalServerError, "InternalError", "patched object has no metadata")
		return false
	}
	if ns != "" {
		if !s.objectIdentityMatchesURL(w, meta, ns, name) {
			return false
		}
	}
	if err := validateRBACIdentity(kind, meta.GetNamespace(), meta.GetName(), ns, name); err != nil {
		s.respondStatus(w, http.StatusUnprocessableEntity, "Invalid", "%s", err.Error())
		return false
	}
	return true
}

// parseRBACNamespaceResource parses /apis/rbac.authorization.k8s.io/v1/namespaces/{ns}/{resource}[/{name}]
func parseRBACNamespaceResource(path string) (namespace, resource, name string, ok bool) {
	parts := strings.TrimPrefix(path, "/apis/"+rbacAPIVersion+"/namespaces/")
	if parts == path {
		return "", "", "", false
	}
	parts = strings.TrimSuffix(parts, "/")
	if parts == "" {
		return "", "", "", false
	}
	pieces := strings.SplitN(parts, "/", 3)
	ns := pieces[0]
	if len(pieces) == 1 {
		return ns, "", "", true
	}
	resource = pieces[1]
	if len(pieces) == 2 {
		return ns, resource, "", true
	}
	return ns, resource, pieces[2], true
}
