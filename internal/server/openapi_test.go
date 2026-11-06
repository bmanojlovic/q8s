package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	openapi_v2 "github.com/google/gnostic-models/openapiv2"
	"google.golang.org/protobuf/proto"
)

type openAPIDefinition struct {
	GVKs []struct {
		Group   string `json:"group"`
		Version string `json:"version"`
		Kind    string `json:"kind"`
	} `json:"x-kubernetes-group-version-kind"`
}

type discoveredGVK struct {
	Group   string
	Version string
	Kind    string
}

func TestOpenAPIV2CoversDiscovery(t *testing.T) {
	ts, _ := newTestServer(t)

	resp := get(t, ts.URL+"/openapi/v2")
	assertStatus(t, resp, http.StatusOK)
	var doc struct {
		Swagger string `json:"swagger"`
		Info    struct {
			Title   string `json:"title"`
			Version string `json:"version"`
		} `json:"info"`
		Definitions map[string]openAPIDefinition `json:"definitions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		resp.Body.Close()
		t.Fatalf("decode openapi: %v", err)
	}
	resp.Body.Close()
	if doc.Swagger != "2.0" {
		t.Fatalf("swagger = %q, want 2.0", doc.Swagger)
	}
	if doc.Info.Title != "q8s" || doc.Info.Version != "v1.36.2" {
		t.Fatalf("info = %+v", doc.Info)
	}
	if _, ok := doc.Definitions["io.k8s.apimachinery.pkg.apis.meta.v1.ObjectMeta"]; !ok {
		t.Fatal("OpenAPI document is missing ObjectMeta")
	}

	for _, want := range discoveredGVKs(t, ts.URL) {
		found := false
		for _, def := range doc.Definitions {
			for _, actual := range def.GVKs {
				if actual.Group == want.Group && actual.Version == want.Version && actual.Kind == want.Kind {
					found = true
					break
				}
			}
		}
		if !found {
			t.Errorf("no OpenAPI definition for served kind %+v", want)
		}
	}
}

// discoveredGVKs reads the same discovery endpoints a client uses and returns
// the GVKs q8s advertises. apiextensions.k8s.io is deliberately excluded:
// the CRD resource is read-only metadata whose schema clients never validate
// through /openapi/v2, and its full JSONSchemaProps closure is large. The
// metrics group has no resource schemas either.
func discoveredGVKs(t *testing.T, baseURL string) []discoveredGVK {
	t.Helper()
	var result []discoveredGVK
	seen := map[discoveredGVK]bool{}
	addList := func(path, group, version string) {
		resp := get(t, baseURL+path)
		assertStatus(t, resp, http.StatusOK)
		var list struct {
			APIResources []struct {
				Name string `json:"name"`
				Kind string `json:"kind"`
			} `json:"resources"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
			resp.Body.Close()
			t.Fatalf("decode %s: %v", path, err)
		}
		resp.Body.Close()
		for _, resource := range list.APIResources {
			if strings.Contains(resource.Name, "/") {
				// deployments/scale advertises Kind Scale, whose schema lives
				// under the autoscaling group rather than apps/v1.
				if resource.Kind == "Scale" {
					gvk := discoveredGVK{Group: "autoscaling", Version: "v1", Kind: "Scale"}
					if !seen[gvk] {
						seen[gvk] = true
						result = append(result, gvk)
					}
				}
				continue
			}
			gvk := discoveredGVK{Group: group, Version: version, Kind: resource.Kind}
			if !seen[gvk] {
				seen[gvk] = true
				result = append(result, gvk)
			}
		}
	}

	addList("/api/v1", "", "v1")

	resp := get(t, baseURL+"/apis")
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
		t.Fatalf("decode /apis: %v", err)
	}
	resp.Body.Close()
	for _, group := range groups.Groups {
		if group.Name == "apiextensions.k8s.io" || group.Name == "metrics.k8s.io" {
			continue
		}
		groupName, version, ok := strings.Cut(group.PreferredVersion.GroupVersion, "/")
		if !ok {
			t.Fatalf("bad groupVersion %q", group.PreferredVersion.GroupVersion)
		}
		addList("/apis/"+group.PreferredVersion.GroupVersion, groupName, version)
	}
	return result
}

func TestOpenAPIV2Protobuf(t *testing.T) {
	ts, _ := newTestServer(t)

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/openapi/v2", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "application/com.github.proto-openapi.spec.v2.v1.0+protobuf")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET openapi protobuf: %v", err)
	}
	defer resp.Body.Close()
	assertStatus(t, resp, http.StatusOK)
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "protobuf") {
		t.Fatalf("Content-Type = %q, want protobuf", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var document openapi_v2.Document
	if err := proto.Unmarshal(body, &document); err != nil {
		t.Fatalf("parse protobuf OpenAPI document: %v", err)
	}
	if len(document.GetDefinitions().GetAdditionalProperties()) == 0 {
		t.Fatal("protobuf OpenAPI document has no definitions")
	}
}
