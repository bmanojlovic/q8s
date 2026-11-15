package server_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

const rbacBase = "/apis/rbac.authorization.k8s.io/v1"

func rbacRule(apiGroup, resource, verb string) []interface{} {
	return []interface{}{map[string]interface{}{
		"apiGroups": []interface{}{apiGroup},
		"resources": []interface{}{resource},
		"verbs":     []interface{}{verb},
	}}
}

func rbacSubject(kind, name string) []interface{} {
	return []interface{}{map[string]interface{}{"kind": kind, "name": name}}
}

func rbacObject(kind, ns, name string) map[string]interface{} {
	metadata := map[string]interface{}{"name": name}
	if ns != "" {
		metadata["namespace"] = ns
	}
	obj := map[string]interface{}{
		"apiVersion": "rbac.authorization.k8s.io/v1",
		"kind":       kind,
		"metadata":   metadata,
	}
	switch kind {
	case "Role", "ClusterRole":
		obj["rules"] = rbacRule("", "pods", "get")
	case "RoleBinding":
		obj["roleRef"] = map[string]interface{}{
			"apiGroup": "rbac.authorization.k8s.io",
			"kind":     "Role",
			"name":     "view",
		}
		obj["subjects"] = rbacSubject("User", "alice")
	case "ClusterRoleBinding":
		obj["roleRef"] = map[string]interface{}{
			"apiGroup": "rbac.authorization.k8s.io",
			"kind":     "ClusterRole",
			"name":     "view",
		}
		obj["subjects"] = rbacSubject("Group", "developers")
	}
	return obj
}

func rbacCollection(kind, ns string) string {
	switch kind {
	case "Role":
		return rbacBase + "/namespaces/" + ns + "/roles"
	case "RoleBinding":
		return rbacBase + "/namespaces/" + ns + "/rolebindings"
	case "ClusterRole":
		return rbacBase + "/clusterroles"
	case "ClusterRoleBinding":
		return rbacBase + "/clusterrolebindings"
	}
	panic("unknown RBAC kind " + kind)
}

func rbacObjectURL(kind, ns, name string) string {
	return rbacCollection(kind, ns) + "/" + name
}

// TestRBACCRUD exercises the common create, read, list, table, update, patch
// and delete path for all four RBAC kinds.
func TestRBACCRUD(t *testing.T) {
	cases := []struct {
		kind      string
		ns        string
		listKind  string
		tableCols []string
	}{
		{kind: "Role", ns: "default", listKind: "RoleList", tableCols: []string{"Name", "Created At"}},
		{kind: "RoleBinding", ns: "default", listKind: "RoleBindingList", tableCols: []string{"Name", "Role", "Age"}},
		{kind: "ClusterRole", listKind: "ClusterRoleList", tableCols: []string{"Name", "Created At"}},
		{kind: "ClusterRoleBinding", listKind: "ClusterRoleBindingList", tableCols: []string{"Name", "Role", "Age"}},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			ts, _ := newTestServer(t)
			collection := ts.URL + rbacCollection(tc.kind, tc.ns)
			objectURL := ts.URL + rbacObjectURL(tc.kind, tc.ns, "demo")

			resp := post(t, collection, rbacObject(tc.kind, tc.ns, "demo"))
			assertStatus(t, resp, http.StatusCreated)
			created := decodeBody(t, resp)
			assertKind(t, created, tc.kind)
			metadata, _ := created["metadata"].(map[string]interface{})
			if metadata["uid"] == nil || metadata["resourceVersion"] == nil || metadata["creationTimestamp"] == nil {
				t.Fatalf("server-owned metadata missing: %v", metadata)
			}

			resp = get(t, collection)
			assertStatus(t, resp, http.StatusOK)
			list := decodeBody(t, resp)
			assertKind(t, list, tc.listKind)
			if items, _ := list["items"].([]interface{}); len(items) != 1 {
				t.Fatalf("expected one item, got %v", list["items"])
			}

			resp = get(t, objectURL)
			assertStatus(t, resp, http.StatusOK)
			resp.Body.Close()

			resp = getWithAccept(t, collection, "application/json;as=Table;v=v1;g=meta.k8s.io, application/json")
			assertStatus(t, resp, http.StatusOK)
			table := decodeBody(t, resp)
			assertKind(t, table, "Table")
			cols := tableColumnNames(t, table)
			if len(cols) != len(tc.tableCols) {
				t.Fatalf("table columns = %v, want %v", cols, tc.tableCols)
			}
			for i, want := range tc.tableCols {
				if cols[i] != want {
					t.Fatalf("table columns = %v, want %v", cols, tc.tableCols)
				}
			}

			if tc.kind == "RoleBinding" || tc.kind == "ClusterRoleBinding" {
				cells := table["rows"].([]interface{})[0].(map[string]interface{})["cells"].([]interface{})
				if cells[1] != "Role/view" && cells[1] != "ClusterRole/view" {
					t.Fatalf("ROLE cell = %v, want kind/name", cells[1])
				}
			}

			updatedObj := rbacObject(tc.kind, tc.ns, "demo")
			updatedObj["metadata"].(map[string]interface{})["labels"] = map[string]interface{}{"updated": "yes"}
			if rules, ok := updatedObj["rules"]; ok {
				rules.([]interface{})[0].(map[string]interface{})["verbs"] = []interface{}{"list"}
			}
			resp = do(t, http.MethodPut, objectURL, updatedObj)
			assertStatus(t, resp, http.StatusOK)
			resp.Body.Close()

			resp = do(t, http.MethodPatch, objectURL, map[string]interface{}{
				"metadata": map[string]interface{}{"labels": map[string]interface{}{"patched": "yes"}},
			})
			assertStatus(t, resp, http.StatusOK)
			resp.Body.Close()

			resp = get(t, objectURL)
			assertStatus(t, resp, http.StatusOK)
			got := decodeBody(t, resp)
			labels, _ := got["metadata"].(map[string]interface{})["labels"].(map[string]interface{})
			if labels["updated"] != "yes" || labels["patched"] != "yes" {
				t.Fatalf("labels after update/patch = %v", labels)
			}

			resp = do(t, http.MethodDelete, objectURL, nil)
			assertStatus(t, resp, http.StatusOK)
			resp.Body.Close()

			resp = get(t, objectURL)
			assertStatus(t, resp, http.StatusNotFound)
			resp.Body.Close()
		})
	}
}

func TestRBACAllNamespaceRoleList(t *testing.T) {
	ts, _ := newTestServer(t)
	for _, ns := range []string{"default", "other"} {
		resp := post(t, ts.URL+rbacCollection("Role", ns), rbacObject("Role", ns, "reader"))
		assertStatus(t, resp, http.StatusCreated)
		resp.Body.Close()
	}
	resp := get(t, ts.URL+rbacBase+"/roles")
	assertStatus(t, resp, http.StatusOK)
	list := decodeBody(t, resp)
	assertKind(t, list, "RoleList")
	if items, _ := list["items"].([]interface{}); len(items) != 2 {
		t.Fatalf("all-namespace role list = %v, want 2 items", list["items"])
	}

	resp = do(t, http.MethodPost, ts.URL+rbacBase+"/roles", rbacObject("Role", "default", "nope"))
	assertStatus(t, resp, http.StatusMethodNotAllowed)
	resp.Body.Close()
}

func TestRBACWatch(t *testing.T) {
	cases := []struct {
		kind string
		ns   string
	}{
		{kind: "Role", ns: "default"},
		{kind: "RoleBinding", ns: "default"},
		{kind: "ClusterRole"},
		{kind: "ClusterRoleBinding"},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			ts, _ := newTestServer(t)
			collection := ts.URL + rbacCollection(tc.kind, tc.ns)
			objectURL := ts.URL + rbacObjectURL(tc.kind, tc.ns, "watched")
			events := startWatch(t, collection+"?watch=true", "")

			// Let serveWatch take its initial, empty snapshot.
			time.Sleep(150 * time.Millisecond)

			resp := post(t, collection, rbacObject(tc.kind, tc.ns, "watched"))
			assertStatus(t, resp, http.StatusCreated)
			resp.Body.Close()
			time.Sleep(50 * time.Millisecond)

			resp = do(t, http.MethodPatch, objectURL, map[string]interface{}{
				"metadata": map[string]interface{}{"labels": map[string]interface{}{"watched": "yes"}},
			})
			assertStatus(t, resp, http.StatusOK)
			resp.Body.Close()
			time.Sleep(50 * time.Millisecond)

			resp = do(t, http.MethodDelete, objectURL, nil)
			assertStatus(t, resp, http.StatusOK)
			resp.Body.Close()

			seen := map[string]int{}
			deadline := time.After(4 * time.Second)
			for seen["ADDED"] == 0 || seen["MODIFIED"] == 0 || seen["DELETED"] == 0 {
				select {
				case ev, ok := <-events:
					if !ok {
						t.Fatalf("watch stream closed early; events so far: %v", seen)
					}
					meta, _ := ev.Object["metadata"].(map[string]interface{})
					if meta["name"] == "watched" {
						seen[ev.Type]++
					}
				case <-deadline:
					t.Fatalf("timed out waiting for watch events; got %v", seen)
				}
			}
		})
	}
}

func TestRBACNamespaceDeleteRemovesRoleBindings(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := post(t, ts.URL+"/api/v1/namespaces", nsBody("rbac-team"))
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()
	resp = post(t, ts.URL+"/api/v1/namespaces", nsBody("rbac-other"))
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	for _, ns := range []string{"rbac-team", "rbac-other"} {
		resp = post(t, ts.URL+rbacCollection("Role", ns), rbacObject("Role", ns, "reader"))
		assertStatus(t, resp, http.StatusCreated)
		resp.Body.Close()
		resp = post(t, ts.URL+rbacCollection("RoleBinding", ns), rbacObject("RoleBinding", ns, "readers"))
		assertStatus(t, resp, http.StatusCreated)
		resp.Body.Close()
	}

	resp = do(t, http.MethodDelete, ts.URL+"/api/v1/namespaces/rbac-team", nil)
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	for _, kind := range []string{"Role", "RoleBinding"} {
		name := "reader"
		if kind == "RoleBinding" {
			name = "readers"
		}
		resp = get(t, ts.URL+rbacObjectURL(kind, "rbac-team", name))
		assertStatus(t, resp, http.StatusNotFound)
		resp.Body.Close()
		resp = get(t, ts.URL+rbacObjectURL(kind, "rbac-other", name))
		assertStatus(t, resp, http.StatusOK)
		resp.Body.Close()
	}
}

func TestRBACRoleRefValidation(t *testing.T) {
	ts, _ := newTestServer(t)
	collection := ts.URL + rbacCollection("RoleBinding", "default")
	objectURL := ts.URL + rbacObjectURL("RoleBinding", "default", "bound")

	resp := post(t, collection, rbacObject("RoleBinding", "default", "bound"))
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	// roleRef is immutable.
	changed := rbacObject("RoleBinding", "default", "bound")
	changed["roleRef"] = map[string]interface{}{
		"apiGroup": "rbac.authorization.k8s.io",
		"kind":     "ClusterRole",
		"name":     "view",
	}
	resp = do(t, http.MethodPut, objectURL, changed)
	assertStatus(t, resp, http.StatusUnprocessableEntity)
	body := decodeBody(t, resp)
	if msg, _ := body["message"].(string); !strings.Contains(msg, "cannot change roleRef") {
		t.Fatalf("PUT message = %q, want cannot change roleRef", msg)
	}

	resp = do(t, http.MethodPatch, objectURL, map[string]interface{}{
		"roleRef": map[string]interface{}{
			"apiGroup": "rbac.authorization.k8s.io",
			"kind":     "ClusterRole",
			"name":     "view",
		},
	})
	assertStatus(t, resp, http.StatusUnprocessableEntity)
	body = decodeBody(t, resp)
	if msg, _ := body["message"].(string); !strings.Contains(msg, "cannot change roleRef") {
		t.Fatalf("PATCH message = %q, want cannot change roleRef", msg)
	}

	// Invalid roleRef kind.
	invalid := rbacObject("RoleBinding", "default", "invalid")
	invalid["roleRef"] = map[string]interface{}{
		"apiGroup": "rbac.authorization.k8s.io",
		"kind":     "Deployment",
		"name":     "view",
	}
	resp = post(t, collection, invalid)
	assertStatus(t, resp, http.StatusUnprocessableEntity)
	resp.Body.Close()

	// A ClusterRoleBinding may only reference a ClusterRole.
	binding := rbacObject("ClusterRoleBinding", "", "invalid")
	binding["roleRef"] = map[string]interface{}{
		"apiGroup": "rbac.authorization.k8s.io",
		"kind":     "Role",
		"name":     "view",
	}
	resp = post(t, ts.URL+rbacCollection("ClusterRoleBinding", ""), binding)
	assertStatus(t, resp, http.StatusUnprocessableEntity)
	resp.Body.Close()

	// Subjects must be User, Group or ServiceAccount.
	badSubject := rbacObject("RoleBinding", "default", "bad-subject")
	badSubject["subjects"] = rbacSubject("Robot", "alice")
	resp = post(t, collection, badSubject)
	assertStatus(t, resp, http.StatusUnprocessableEntity)
	resp.Body.Close()
}

func TestRBACRuleValidation(t *testing.T) {
	ts, _ := newTestServer(t)
	roleURL := ts.URL + rbacCollection("Role", "default")
	clusterURL := ts.URL + rbacCollection("ClusterRole", "")

	noVerbs := rbacObject("Role", "default", "no-verbs")
	noVerbs["rules"] = []interface{}{map[string]interface{}{
		"apiGroups": []interface{}{""},
		"resources": []interface{}{"pods"},
		"verbs":     []interface{}{},
	}}
	resp := post(t, roleURL, noVerbs)
	assertStatus(t, resp, http.StatusUnprocessableEntity)
	resp.Body.Close()

	noResources := rbacObject("Role", "default", "no-resources")
	noResources["rules"] = []interface{}{map[string]interface{}{
		"apiGroups": []interface{}{""},
		"verbs":     []interface{}{"get"},
	}}
	resp = post(t, roleURL, noResources)
	assertStatus(t, resp, http.StatusUnprocessableEntity)
	resp.Body.Close()

	nonResourceRole := rbacObject("Role", "default", "non-resource")
	nonResourceRole["rules"] = []interface{}{map[string]interface{}{
		"nonResourceURLs": []interface{}{"/healthz"},
		"verbs":           []interface{}{"get"},
	}}
	resp = post(t, roleURL, nonResourceRole)
	assertStatus(t, resp, http.StatusUnprocessableEntity)
	resp.Body.Close()

	nonResourceCluster := rbacObject("ClusterRole", "", "non-resource")
	nonResourceCluster["rules"] = []interface{}{map[string]interface{}{
		"nonResourceURLs": []interface{}{"/healthz"},
		"verbs":           []interface{}{"get"},
	}}
	resp = post(t, clusterURL, nonResourceCluster)
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()
}

// TestRBACApply checks that apply treats rules, roleRef and subjects as user
// sections, not only spec, and that the second apply updates the stored object.
func TestRBACApply(t *testing.T) {
	cases := []struct {
		kind       string
		ns         string
		create     string
		update     string
		checkKey   string
		checkValue interface{}
	}{
		{
			kind: "Role", ns: "default",
			create: `apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: applied
  namespace: default
rules:
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["get"]
`,
			update: `apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: applied
  namespace: default
rules:
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["list"]
`,
			checkKey: "verbs", checkValue: "list",
		},
		{
			kind: "ClusterRole", ns: "",
			create: `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: applied
rules:
- nonResourceURLs: ["/healthz"]
  verbs: ["get"]
`,
			update: `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: applied
rules:
- nonResourceURLs: ["/version"]
  verbs: ["get"]
`,
			checkKey: "nonResourceURLs", checkValue: "/version",
		},
		{
			kind: "RoleBinding", ns: "default",
			create: `apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: applied
  namespace: default
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: view
subjects:
- kind: User
  name: alice
`,
			update: `apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: applied
  namespace: default
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: view
subjects:
- kind: User
  name: bob
`,
			checkKey: "name", checkValue: "bob",
		},
		{
			kind: "ClusterRoleBinding", ns: "",
			create: `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: applied
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: view
subjects:
- kind: Group
  name: developers
`,
			update: `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: applied
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: view
subjects:
- kind: Group
  name: auditors
`,
			checkKey: "name", checkValue: "auditors",
		},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			ts, _ := newTestServer(t)
			url := ts.URL + rbacObjectURL(tc.kind, tc.ns, "applied")

			resp := applyPatch(t, url, tc.create)
			assertStatus(t, resp, http.StatusCreated)
			resp.Body.Close()

			resp = applyPatch(t, url, tc.update)
			assertStatus(t, resp, http.StatusOK)
			resp.Body.Close()

			resp = get(t, url)
			assertStatus(t, resp, http.StatusOK)
			got := decodeBody(t, resp)
			var section interface{}
			switch tc.kind {
			case "Role", "ClusterRole":
				rules, _ := got["rules"].([]interface{})
				if len(rules) != 1 {
					t.Fatalf("rules = %v", got["rules"])
				}
				section = rules[0]
			default:
				subjects, _ := got["subjects"].([]interface{})
				if len(subjects) != 1 {
					t.Fatalf("subjects = %v", got["subjects"])
				}
				section = subjects[0]
			}
			field := section.(map[string]interface{})[tc.checkKey]
			if list, ok := field.([]interface{}); ok {
				if len(list) != 1 || list[0] != tc.checkValue {
					t.Fatalf("%s = %v, want [%v]", tc.checkKey, list, tc.checkValue)
				}
				return
			}
			if field != tc.checkValue {
				t.Fatalf("%s = %v, want %v", tc.checkKey, field, tc.checkValue)
			}
		})
	}
}

func TestRBACDiscovery(t *testing.T) {
	ts, _ := newTestServer(t)

	resp := get(t, ts.URL+"/apis")
	assertStatus(t, resp, http.StatusOK)
	var groups struct {
		Groups []struct {
			Name             string `json:"name"`
			PreferredVersion struct {
				GroupVersion string `json:"groupVersion"`
			} `json:"preferredVersion"`
		} `json:"groups"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&groups); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	found := false
	for _, group := range groups.Groups {
		if group.Name == "rbac.authorization.k8s.io" {
			found = true
			if group.PreferredVersion.GroupVersion != "rbac.authorization.k8s.io/v1" {
				t.Fatalf("preferred version = %q", group.PreferredVersion.GroupVersion)
			}
		}
	}
	if !found {
		t.Fatal("/apis does not list rbac.authorization.k8s.io")
	}

	resp = get(t, ts.URL+"/apis/rbac.authorization.k8s.io")
	assertStatus(t, resp, http.StatusOK)
	var group struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&group); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if group.Kind != "APIGroup" || group.Name != "rbac.authorization.k8s.io" {
		t.Fatalf("group document = %+v", group)
	}

	resp = get(t, ts.URL+rbacBase)
	assertStatus(t, resp, http.StatusOK)
	var resources struct {
		APIResources []struct {
			Name       string   `json:"name"`
			Namespaced bool     `json:"namespaced"`
			Kind       string   `json:"kind"`
			Verbs      []string `json:"verbs"`
		} `json:"resources"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&resources); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	want := map[string]struct {
		namespaced bool
		kind       string
	}{
		"roles":               {true, "Role"},
		"rolebindings":        {true, "RoleBinding"},
		"clusterroles":        {false, "ClusterRole"},
		"clusterrolebindings": {false, "ClusterRoleBinding"},
	}
	if len(resources.APIResources) != len(want) {
		t.Fatalf("got %d resources, want %d", len(resources.APIResources), len(want))
	}
	for _, resource := range resources.APIResources {
		expected, ok := want[resource.Name]
		if !ok {
			t.Fatalf("unexpected resource %q", resource.Name)
		}
		if resource.Namespaced != expected.namespaced || resource.Kind != expected.kind {
			t.Fatalf("resource %+v, want namespaced=%v kind=%s", resource, expected.namespaced, expected.kind)
		}
		for _, verb := range []string{"get", "list", "watch", "create", "update", "patch", "delete", "deletecollection"} {
			found := false
			for _, got := range resource.Verbs {
				if got == verb {
					found = true
				}
			}
			if !found {
				t.Fatalf("resource %q verbs %v missing %q", resource.Name, resource.Verbs, verb)
			}
		}
	}
}
