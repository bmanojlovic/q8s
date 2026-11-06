package server_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestCRDDiscovery(t *testing.T) {
	ts, _ := newTestServer(t)

	resp := get(t, ts.URL+"/apis")
	assertStatus(t, resp, http.StatusOK)
	var groups struct {
		Groups []struct {
			Name             string `json:"name"`
			PreferredVersion struct {
				GroupVersion string `json:"groupVersion"`
				Version      string `json:"version"`
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
		if group.Name == "apiextensions.k8s.io" {
			found = true
			if group.PreferredVersion.GroupVersion != "apiextensions.k8s.io/v1" {
				t.Fatalf("preferred version = %q", group.PreferredVersion.GroupVersion)
			}
		}
	}
	if !found {
		t.Fatal("/apis does not list apiextensions.k8s.io")
	}

	resp = get(t, ts.URL+"/apis/apiextensions.k8s.io")
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
	if group.Kind != "APIGroup" || group.Name != "apiextensions.k8s.io" {
		t.Fatalf("group document = %+v", group)
	}

	resp = get(t, ts.URL+"/apis/apiextensions.k8s.io/v1")
	assertStatus(t, resp, http.StatusOK)
	var resources struct {
		APIResources []struct {
			Name         string   `json:"name"`
			SingularName string   `json:"singularName"`
			Namespaced   bool     `json:"namespaced"`
			Kind         string   `json:"kind"`
			Verbs        []string `json:"verbs"`
			ShortNames   []string `json:"shortNames"`
		} `json:"resources"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&resources); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(resources.APIResources) != 1 {
		t.Fatalf("got %d resources, want 1", len(resources.APIResources))
	}
	crd := resources.APIResources[0]
	if crd.Name != "customresourcedefinitions" || crd.SingularName != "customresourcedefinition" ||
		crd.Namespaced || crd.Kind != "CustomResourceDefinition" {
		t.Fatalf("CRD resource = %+v", crd)
	}
	if strings.Join(crd.Verbs, ",") != "get,list" {
		t.Fatalf("CRD verbs = %v", crd.Verbs)
	}
	if strings.Join(crd.ShortNames, ",") != "crd,crds" {
		t.Fatalf("CRD short names = %v", crd.ShortNames)
	}
}

func TestCRDList(t *testing.T) {
	ts, _ := newTestServer(t)

	resp := get(t, ts.URL+"/apis/apiextensions.k8s.io/v1/customresourcedefinitions")
	assertStatus(t, resp, http.StatusOK)
	var list struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Group string `json:"group"`
				Names struct {
					Kind string `json:"kind"`
				} `json:"names"`
				Versions []struct {
					Name   string                 `json:"name"`
					Schema map[string]interface{} `json:"schema"`
				} `json:"versions"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()

	if list.APIVersion != "apiextensions.k8s.io/v1" || list.Kind != "CustomResourceDefinitionList" {
		t.Fatalf("list type = %s %s", list.APIVersion, list.Kind)
	}
	if list.Metadata.ResourceVersion == "" {
		t.Fatal("list metadata.resourceVersion is empty")
	}
	if len(list.Items) != 2 {
		t.Fatalf("got %d CRDs, want 2", len(list.Items))
	}
	kinds := map[string]string{}
	for _, item := range list.Items {
		kinds[item.Metadata.Name] = item.Spec.Names.Kind
		if item.Spec.Group != "traefik.io" {
			t.Errorf("%s group = %q", item.Metadata.Name, item.Spec.Group)
		}
		if len(item.Spec.Versions) == 0 || len(item.Spec.Versions[0].Schema) == 0 {
			t.Errorf("%s has no schema", item.Metadata.Name)
		}
	}
	if kinds["middlewares.traefik.io"] != "Middleware" {
		t.Errorf("middlewares.traefik.io kind = %q", kinds["middlewares.traefik.io"])
	}
	if kinds["ingressroutetcps.traefik.io"] != "IngressRouteTCP" {
		t.Errorf("ingressroutetcps.traefik.io kind = %q", kinds["ingressroutetcps.traefik.io"])
	}
}

func TestCRDGet(t *testing.T) {
	ts, _ := newTestServer(t)

	resp := get(t, ts.URL+"/apis/apiextensions.k8s.io/v1/customresourcedefinitions/middlewares.traefik.io")
	assertStatus(t, resp, http.StatusOK)
	var crd struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			Group string `json:"group"`
			Names struct {
				Kind string `json:"kind"`
			} `json:"names"`
		} `json:"spec"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&crd); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if crd.Kind != "CustomResourceDefinition" || crd.Metadata.Name != "middlewares.traefik.io" ||
		crd.Spec.Group != "traefik.io" || crd.Spec.Names.Kind != "Middleware" {
		t.Fatalf("unexpected CRD: %+v", crd)
	}
}

func TestCRDGetNotFound(t *testing.T) {
	ts, _ := newTestServer(t)

	resp := get(t, ts.URL+"/apis/apiextensions.k8s.io/v1/customresourcedefinitions/nope.example.com")
	defer resp.Body.Close()
	assertStatus(t, resp, http.StatusNotFound)
	var status struct {
		Kind   string `json:"kind"`
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if status.Kind != "Status" || status.Status != "Failure" || status.Reason != "NotFound" {
		t.Fatalf("unexpected Status: %+v", status)
	}
}

func TestCRDMethodNotAllowed(t *testing.T) {
	ts, _ := newTestServer(t)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		url := ts.URL + "/apis/apiextensions.k8s.io/v1/customresourcedefinitions"
		var resp *http.Response
		if method == http.MethodDelete {
			resp = do(t, method, url, nil)
		} else {
			resp = do(t, method, url, map[string]interface{}{"apiVersion": "apiextensions.k8s.io/v1"})
		}
		assertStatus(t, resp, http.StatusMethodNotAllowed)
		var status struct {
			Kind   string `json:"kind"`
			Reason string `json:"reason"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
			resp.Body.Close()
			t.Fatalf("%s: decode Status: %v", method, err)
		}
		resp.Body.Close()
		if status.Kind != "Status" || status.Reason != "MethodNotAllowed" {
			t.Fatalf("%s: unexpected Status: %+v", method, status)
		}
	}
}

func TestCRDTable(t *testing.T) {
	ts, _ := newTestServer(t)

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/apis/apiextensions.k8s.io/v1/customresourcedefinitions", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "application/json;as=Table;v=v1;g=meta.k8s.io")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET CRD table: %v", err)
	}
	defer resp.Body.Close()
	assertStatus(t, resp, http.StatusOK)

	var table struct {
		Kind              string `json:"kind"`
		ColumnDefinitions []struct {
			Name string `json:"name"`
		} `json:"columnDefinitions"`
		Rows []struct {
			Cells []interface{} `json:"cells"`
		} `json:"rows"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&table); err != nil {
		t.Fatal(err)
	}
	if table.Kind != "Table" {
		t.Fatalf("kind = %q, want Table", table.Kind)
	}
	if len(table.ColumnDefinitions) != 2 ||
		table.ColumnDefinitions[0].Name != "Name" ||
		table.ColumnDefinitions[1].Name != "Created At" {
		t.Fatalf("columns = %+v", table.ColumnDefinitions)
	}
	if len(table.Rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(table.Rows))
	}
	names := map[string]bool{}
	for _, row := range table.Rows {
		if len(row.Cells) != 2 {
			t.Fatalf("row has %d cells, want 2", len(row.Cells))
		}
		name, _ := row.Cells[0].(string)
		names[name] = true
	}
	if !names["middlewares.traefik.io"] || !names["ingressroutetcps.traefik.io"] {
		t.Fatalf("table names = %v", names)
	}
}
