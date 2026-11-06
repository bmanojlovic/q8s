// Command gen-openapi regenerates the API schema files embedded by q8s:
//
//	internal/server/openapi_v2.json  trimmed Swagger 2.0 document
//	internal/server/crds.json        the read-only Traefik CRD list
//
// The upstream inputs are not committed. Fetch the swagger document that
// matches the k8s.io/api version in go.mod, plus the Traefik CRD YAML, and run:
//
//	go run ./hack/gen-openapi \
//	    -swagger /path/to/k8s-v1.36.2-swagger.json \
//	    -crds /path/to/traefik-v3.6-crds.yml
//
// When bumping k8s.io/api, download the matching
// https://github.com/kubernetes/kubernetes/blob/<tag>/api/openapi-spec/swagger.json
// and update openapiVersion below. When bumping Traefik, point -crds at the
// new crds.yaml and adjust wantedCRDs if the served CRD names changed.
//
// Only definitions for kinds q8s actually serves are kept, plus the special
// definitions clients ask for outside a kind (Status, DeleteOptions, Patch),
// plus the transitive $ref closure of all of them. The output is compact and
// deterministic (encoding/json sorts map keys).
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

// openapiVersion is reported in the generated document's info section. It
// tracks the upstream swagger used to generate it (the Kubernetes version
// whose API definitions match the k8s.io/api module in go.mod).
const openapiVersion = "v1.36.2"

// gvk identifies a Kubernetes type as it appears in
// x-kubernetes-group-version-kind.
type gvk struct {
	Group   string
	Version string
	Kind    string
}

// servedGVKs is the single place listing the resource kinds q8s serves. It
// must stay in sync with discovery: handleAPIV1, handleAppsV1, handleBatchV1,
// handleNetworkingV1, handleStorageV1, handleCoordinationV1 and the
// deployments/scale subresource. autoscaling/v1 Scale is served as that
// subresource even though q8s has no autoscaling group root.
var servedGVKs = []gvk{
	{Group: "", Version: "v1", Kind: "Pod"},
	{Group: "", Version: "v1", Kind: "Service"},
	{Group: "", Version: "v1", Kind: "Namespace"},
	{Group: "", Version: "v1", Kind: "PersistentVolumeClaim"},
	{Group: "", Version: "v1", Kind: "ConfigMap"},
	{Group: "", Version: "v1", Kind: "Secret"},
	{Group: "", Version: "v1", Kind: "Event"},
	{Group: "", Version: "v1", Kind: "Node"},
	{Group: "apps", Version: "v1", Kind: "Deployment"},
	{Group: "apps", Version: "v1", Kind: "DaemonSet"},
	{Group: "apps", Version: "v1", Kind: "ReplicaSet"},
	{Group: "apps", Version: "v1", Kind: "StatefulSet"},
	{Group: "batch", Version: "v1", Kind: "Job"},
	{Group: "batch", Version: "v1", Kind: "CronJob"},
	{Group: "networking.k8s.io", Version: "v1", Kind: "Ingress"},
	{Group: "storage.k8s.io", Version: "v1", Kind: "StorageClass"},
	{Group: "autoscaling", Version: "v1", Kind: "Scale"},
	{Group: "coordination.k8s.io", Version: "v1", Kind: "Lease"},
}

// extraDefinitions are schemas clients request without a kind match. Status
// is every error/response envelope, DeleteOptions is the delete body, and
// Patch names PATCH request bodies.
var extraDefinitions = []string{
	"io.k8s.apimachinery.pkg.apis.meta.v1.Status",
	"io.k8s.apimachinery.pkg.apis.meta.v1.DeleteOptions",
	"io.k8s.apimachinery.pkg.apis.meta.v1.Patch",
}

// wantedCRDs is the subset of the Traefik CRD file q8s serves read-only. The
// Middleware and IngressRouteTCP storage comes with a later change; the CRD
// schemas are already served so clients can plan objects of those kinds.
var wantedCRDs = map[string]bool{
	"middlewares.traefik.io":      true,
	"ingressroutetcps.traefik.io": true,
}

func main() {
	swaggerPath := flag.String("swagger", "", "path to the upstream Kubernetes swagger.json")
	crdPath := flag.String("crds", "", "path to the Traefik CRD YAML file")
	openapiOut := flag.String("openapi-out", "internal/server/openapi_v2.json", "output path for the trimmed Swagger 2.0 document")
	crdsOut := flag.String("crds-out", "internal/server/crds.json", "output path for the CustomResourceDefinitionList")
	flag.Parse()

	if *swaggerPath == "" || *crdPath == "" {
		fmt.Fprintln(os.Stderr, "gen-openapi: -swagger and -crds are required")
		flag.Usage()
		os.Exit(2)
	}

	swagger, err := os.ReadFile(*swaggerPath)
	if err != nil {
		fatalf("read swagger: %v", err)
	}
	openapi, err := trimSwagger(swagger)
	if err != nil {
		fatalf("trim swagger: %v", err)
	}
	if err := os.WriteFile(*openapiOut, append(openapi, '\n'), 0644); err != nil {
		fatalf("write %s: %v", *openapiOut, err)
	}

	crdYAML, err := os.ReadFile(*crdPath)
	if err != nil {
		fatalf("read CRDs: %v", err)
	}
	crds, err := buildCRDList(crdYAML)
	if err != nil {
		fatalf("build CRD list: %v", err)
	}
	if err := os.WriteFile(*crdsOut, append(crds, '\n'), 0644); err != nil {
		fatalf("write %s: %v", *crdsOut, err)
	}
}

func fatalf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "gen-openapi: "+format+"\n", args...)
	os.Exit(1)
}

// trimSwagger parses an upstream Swagger 2.0 document and returns the compact
// q8s document containing only the definitions reachable from the served
// kinds and the extra definitions.
func trimSwagger(raw []byte) ([]byte, error) {
	var doc struct {
		Definitions map[string]map[string]interface{} `json:"definitions"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse swagger: %w", err)
	}
	if len(doc.Definitions) == 0 {
		return nil, fmt.Errorf("swagger has no definitions")
	}

	wanted := make(map[gvk]bool, len(servedGVKs)*2)
	for _, g := range servedGVKs {
		wanted[g] = true
		wanted[gvk{Group: g.Group, Version: g.Version, Kind: g.Kind + "List"}] = true
	}

	selected := make(map[string]map[string]interface{})
	for _, name := range extraDefinitions {
		def, ok := doc.Definitions[name]
		if !ok {
			return nil, fmt.Errorf("swagger is missing required definition %q", name)
		}
		selected[name] = def
	}
	for name, def := range doc.Definitions {
		for _, entry := range gvkEntries(def) {
			if wanted[entry] {
				selected[name] = def
				break
			}
		}
	}

	// Follow every $ref until no new definitions are pulled in. Unknown refs
	// are an error: emitting a document with dangling refs would fail in the
	// client instead of here.
	queue := make([]string, 0, len(selected))
	for name := range selected {
		queue = append(queue, name)
	}
	sort.Strings(queue)
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		for _, ref := range definitionRefs(selected[name]) {
			const prefix = "#/definitions/"
			if !strings.HasPrefix(ref, prefix) {
				return nil, fmt.Errorf("definition %q has unsupported $ref %q", name, ref)
			}
			refName := strings.TrimPrefix(ref, prefix)
			if _, ok := doc.Definitions[refName]; !ok {
				return nil, fmt.Errorf("definition %q references missing definition %q", name, refName)
			}
			if _, ok := selected[refName]; ok {
				continue
			}
			selected[refName] = doc.Definitions[refName]
			queue = append(queue, refName)
		}
	}

	out := map[string]interface{}{
		"swagger": "2.0",
		"info": map[string]interface{}{
			"title":   "q8s",
			"version": openapiVersion,
		},
		"paths":       map[string]interface{}{},
		"definitions": selected,
	}
	return json.Marshal(out)
}

// gvkEntries returns the x-kubernetes-group-version-kind entries of a
// definition. Several entries are possible: DeleteOptions, for instance,
// appears under many group/version pairs.
func gvkEntries(def map[string]interface{}) []gvk {
	raw, ok := def["x-kubernetes-group-version-kind"].([]interface{})
	if !ok {
		return nil
	}
	entries := make([]gvk, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		group, _ := m["group"].(string)
		version, _ := m["version"].(string)
		kind, _ := m["kind"].(string)
		entries = append(entries, gvk{Group: group, Version: version, Kind: kind})
	}
	return entries
}

// definitionRefs walks a definition and returns every $ref it contains,
// including refs nested in properties, items and additionalProperties.
func definitionRefs(value interface{}) []string {
	var refs []string
	var walk func(interface{})
	walk = func(v interface{}) {
		switch node := v.(type) {
		case map[string]interface{}:
			for key, child := range node {
				if key == "$ref" {
					if ref, ok := child.(string); ok {
						refs = append(refs, ref)
					}
					continue
				}
				walk(child)
			}
		case []interface{}:
			for _, child := range node {
				walk(child)
			}
		}
	}
	walk(value)
	sort.Strings(refs)
	return refs
}

// buildCRDList converts the multi-document Traefik CRD YAML into a
// CustomResourceDefinitionList containing only wantedCRDs, sorted by name.
func buildCRDList(raw []byte) ([]byte, error) {
	var items []map[string]interface{}
	for _, document := range splitYAMLDocuments(raw) {
		js, err := yaml.YAMLToJSON(document)
		if err != nil {
			return nil, fmt.Errorf("parse CRD document: %w", err)
		}
		var obj map[string]interface{}
		if err := json.Unmarshal(js, &obj); err != nil {
			return nil, fmt.Errorf("decode CRD document: %w", err)
		}
		if obj["kind"] != "CustomResourceDefinition" {
			continue
		}
		name := nestedString(obj, "metadata", "name")
		if wantedCRDs[name] {
			items = append(items, obj)
		}
	}
	if len(items) != len(wantedCRDs) {
		return nil, fmt.Errorf("found %d matching CRDs, want %d", len(items), len(wantedCRDs))
	}
	sort.Slice(items, func(i, j int) bool {
		return nestedString(items[i], "metadata", "name") < nestedString(items[j], "metadata", "name")
	})

	list := map[string]interface{}{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinitionList",
		"metadata":   map[string]interface{}{},
		"items":      items,
	}
	return json.Marshal(list)
}

// splitYAMLDocuments splits a YAML stream on document markers at column 0.
// The marker cannot appear inside an indented block scalar, so a line-based
// split is safe for CRD files.
func splitYAMLDocuments(raw []byte) [][]byte {
	var documents [][]byte
	var current []byte
	for _, line := range bytes.SplitAfter(raw, []byte("\n")) {
		marker := bytes.TrimRight(line, " \t\r\n")
		if bytes.Equal(marker, []byte("---")) || bytes.HasPrefix(marker, []byte("--- ")) {
			if len(bytes.TrimSpace(current)) > 0 {
				documents = append(documents, current)
			}
			current = nil
			continue
		}
		current = append(current, line...)
	}
	if len(bytes.TrimSpace(current)) > 0 {
		documents = append(documents, current)
	}
	return documents
}

func nestedString(obj map[string]interface{}, keys ...string) string {
	var current interface{} = obj
	for _, key := range keys {
		m, ok := current.(map[string]interface{})
		if !ok {
			return ""
		}
		current = m[key]
	}
	s, _ := current.(string)
	return s
}
