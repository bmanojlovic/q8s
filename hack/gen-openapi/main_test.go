package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// tinySwagger has one served kind (Pod), its list kind, a $ref closure, the
// three extra definitions and an unrelated kind with its own child schema.
const tinySwagger = `{
  "swagger": "2.0",
  "info": {"title": "upstream", "version": "v9"},
  "paths": {"/ignored": {}},
  "definitions": {
    "io.k8s.api.core.v1.Pod": {
      "type": "object",
      "x-kubernetes-group-version-kind": [{"group": "", "version": "v1", "kind": "Pod"}],
      "properties": {
        "metadata": {"$ref": "#/definitions/io.k8s.apimachinery.pkg.apis.meta.v1.ObjectMeta"},
        "spec": {"$ref": "#/definitions/io.k8s.api.core.v1.PodSpec"}
      }
    },
    "io.k8s.api.core.v1.PodList": {
      "type": "object",
      "x-kubernetes-group-version-kind": [{"group": "", "version": "v1", "kind": "PodList"}],
      "properties": {
        "items": {"type": "array", "items": {"$ref": "#/definitions/io.k8s.api.core.v1.Pod"}}
      }
    },
    "io.k8s.api.core.v1.PodSpec": {
      "type": "object",
      "properties": {
        "nodeName": {"type": "string"}
      }
    },
    "io.k8s.apimachinery.pkg.apis.meta.v1.ObjectMeta": {
      "type": "object",
      "properties": {
        "name": {"type": "string"}
      }
    },
    "io.k8s.apimachinery.pkg.apis.meta.v1.Status": {"type": "object"},
    "io.k8s.apimachinery.pkg.apis.meta.v1.DeleteOptions": {"type": "object"},
    "io.k8s.apimachinery.pkg.apis.meta.v1.Patch": {"type": "object"},
    "io.k8s.api.other.v1.Unrelated": {
      "type": "object",
      "x-kubernetes-group-version-kind": [{"group": "other", "version": "v1", "kind": "Unrelated"}],
      "properties": {
        "child": {"$ref": "#/definitions/io.k8s.api.other.v1.UnrelatedChild"}
      }
    },
    "io.k8s.api.other.v1.UnrelatedChild": {"type": "object"}
  }
}`

func TestTrimSwagger(t *testing.T) {
	out, err := trimSwagger([]byte(tinySwagger))
	if err != nil {
		t.Fatalf("trimSwagger: %v", err)
	}

	var doc struct {
		Swagger string `json:"swagger"`
		Info    struct {
			Title   string `json:"title"`
			Version string `json:"version"`
		} `json:"info"`
		Paths       map[string]interface{}            `json:"paths"`
		Definitions map[string]map[string]interface{} `json:"definitions"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if doc.Swagger != "2.0" || doc.Info.Title != "q8s" || doc.Info.Version != openapiVersion {
		t.Fatalf("unexpected header: %+v", doc)
	}
	if len(doc.Paths) != 0 {
		t.Fatalf("paths should be empty, got %v", doc.Paths)
	}

	for _, name := range []string{
		"io.k8s.api.core.v1.Pod",
		"io.k8s.api.core.v1.PodList",
		"io.k8s.api.core.v1.PodSpec",
		"io.k8s.apimachinery.pkg.apis.meta.v1.ObjectMeta",
		"io.k8s.apimachinery.pkg.apis.meta.v1.Status",
		"io.k8s.apimachinery.pkg.apis.meta.v1.DeleteOptions",
		"io.k8s.apimachinery.pkg.apis.meta.v1.Patch",
	} {
		if _, ok := doc.Definitions[name]; !ok {
			t.Errorf("missing definition %q", name)
		}
	}
	for _, name := range []string{
		"io.k8s.api.other.v1.Unrelated",
		"io.k8s.api.other.v1.UnrelatedChild",
	} {
		if _, ok := doc.Definitions[name]; ok {
			t.Errorf("unrelated definition %q was kept", name)
		}
	}

	// Deterministic output: same input, same bytes.
	again, err := trimSwagger([]byte(tinySwagger))
	if err != nil {
		t.Fatalf("second trimSwagger: %v", err)
	}
	if string(out) != string(again) {
		t.Fatal("trimSwagger output is not deterministic")
	}
}

func TestTrimSwaggerMissingRef(t *testing.T) {
	bad := strings.Replace(tinySwagger,
		`"spec": {"$ref": "#/definitions/io.k8s.api.core.v1.PodSpec"}`,
		`"spec": {"$ref": "#/definitions/io.k8s.api.core.v1.DoesNotExist"}`, 1)
	if _, err := trimSwagger([]byte(bad)); err == nil {
		t.Fatal("expected an error for a dangling $ref")
	}
}

const tinyCRDs = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: ingressroutes.traefik.io
spec:
  group: traefik.io
  names:
    kind: IngressRoute
  versions:
  - name: v1alpha1
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: middlewares.traefik.io
spec:
  group: traefik.io
  names:
    kind: Middleware
  versions:
  - name: v1alpha1
    schema:
      openAPIV3Schema:
        type: object
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: ingressroutetcps.traefik.io
spec:
  group: traefik.io
  names:
    kind: IngressRouteTCP
  versions:
  - name: v1alpha1
    schema:
      openAPIV3Schema:
        type: object
        properties:
          spec:
            type: object
            properties:
              routes:
                type: array
                items:
                  type: object
                  properties:
                    port:
                      anyOf:
                      - type: integer
                      - type: string
                      x-kubernetes-int-or-string: true
`

func TestBuildCRDList(t *testing.T) {
	out, err := buildCRDList([]byte(tinyCRDs))
	if err != nil {
		t.Fatalf("buildCRDList: %v", err)
	}
	var list struct {
		APIVersion string                   `json:"apiVersion"`
		Kind       string                   `json:"kind"`
		Items      []map[string]interface{} `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if list.APIVersion != "apiextensions.k8s.io/v1" || list.Kind != "CustomResourceDefinitionList" {
		t.Fatalf("unexpected type: %s %s", list.APIVersion, list.Kind)
	}
	if len(list.Items) != 2 {
		t.Fatalf("got %d items, want 2", len(list.Items))
	}
	names := []string{
		nestedString(list.Items[0], "metadata", "name"),
		nestedString(list.Items[1], "metadata", "name"),
	}
	want := []string{"ingressroutetcps.traefik.io", "middlewares.traefik.io"}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("item %d = %q, want %q (items must be sorted)", i, names[i], want[i])
		}
	}
	if got := nestedString(list.Items[0], "spec", "names", "kind"); got != "IngressRouteTCP" {
		t.Fatalf("first item kind = %q", got)
	}
	if got := nestedString(list.Items[1], "spec", "names", "kind"); got != "Middleware" {
		t.Fatalf("second item kind = %q", got)
	}
}

func TestBuildCRDListMissing(t *testing.T) {
	if _, err := buildCRDList([]byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: middlewares.traefik.io
`)); err == nil {
		t.Fatal("expected an error when wanted CRDs are absent")
	}
}
func TestAddCRDDefinitions(t *testing.T) {
	trimmed := []byte(`{"swagger":"2.0","info":{"title":"q8s","version":"v1"},"paths":{},"definitions":{"x":{"type":"object"}}}`)
	out, err := addCRDDefinitions(trimmed, []byte(tinyCRDs))
	if err != nil {
		t.Fatalf("addCRDDefinitions: %v", err)
	}
	var doc struct {
		Definitions map[string]map[string]interface{} `json:"definitions"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	for _, name := range []string{
		"io.traefik.v1alpha1.Middleware",
		"io.traefik.v1alpha1.MiddlewareList",
		"io.traefik.v1alpha1.IngressRouteTCP",
		"io.traefik.v1alpha1.IngressRouteTCPList",
	} {
		if _, ok := doc.Definitions[name]; !ok {
			t.Errorf("missing definition %q", name)
		}
	}
	raw, err := json.Marshal(doc.Definitions["io.traefik.v1alpha1.IngressRouteTCP"])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "anyOf") {
		t.Fatalf("anyOf survived sanitization: %s", raw)
	}
	if !strings.Contains(string(raw), "x-kubernetes-int-or-string") {
		t.Fatalf("int-or-string marker was dropped: %s", raw)
	}
}
