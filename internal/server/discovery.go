package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	openapi_v2 "github.com/google/gnostic-models/openapiv2"
	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// openapiV2Proto holds the lazily-computed protobuf encoding of openapiV2Schema.
var (
	openapiV2ProtoOnce  sync.Once
	openapiV2ProtoBytes []byte
	openapiV2ProtoErr   error
)

func getOpenapiV2Proto() ([]byte, error) {
	openapiV2ProtoOnce.Do(func() {
		doc, err := openapi_v2.ParseDocument([]byte(openapiV2Schema))
		if err != nil {
			openapiV2ProtoErr = err
			return
		}
		openapiV2ProtoBytes, openapiV2ProtoErr = proto.Marshal(doc)
	})
	return openapiV2ProtoBytes, openapiV2ProtoErr
}

func (s *Server) handleAPIRoot(w http.ResponseWriter, r *http.Request) {
	data, _ := json.Marshal(map[string]interface{}{
		"kind":       "APIVersions",
		"apiVersion": "v1",
		"versions":   []string{"v1"},
		"serverAddressByClientCIDRs": []map[string]string{
			{"serverAddress": "127.0.0.1:6443", "clientCIDR": "0.0.0.0/0"},
		},
	})
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func (s *Server) handleAPIV1(w http.ResponseWriter, r *http.Request) {
	data, _ := json.Marshal(metav1.APIResourceList{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "APIResourceList",
		},
		GroupVersion: "v1",
		APIResources: []metav1.APIResource{
			{Name: "pods", SingularName: "pod", Namespaced: true, Kind: "Pod", Verbs: verbList(), ShortNames: []string{"po"}, Categories: []string{"all"}},
			{Name: "services", SingularName: "service", Namespaced: true, Kind: "Service", Verbs: verbList(), ShortNames: []string{"svc"}, Categories: []string{"all"}},
			{Name: "namespaces", SingularName: "namespace", Namespaced: false, Kind: "Namespace", Verbs: verbList(), ShortNames: []string{"ns"}},
			{Name: "persistentvolumeclaims", SingularName: "persistentvolumeclaim", Namespaced: true, Kind: "PersistentVolumeClaim", Verbs: verbList(), ShortNames: []string{"pvc"}, Categories: []string{"all"}},
			{Name: "configmaps", SingularName: "configmap", Namespaced: true, Kind: "ConfigMap", Verbs: verbList(), ShortNames: []string{"cm"}},
			{Name: "secrets", SingularName: "secret", Namespaced: true, Kind: "Secret", Verbs: verbList()},
		},
	})
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func (s *Server) handleAPIsRoot(w http.ResponseWriter, r *http.Request) {
	data, _ := json.Marshal(metav1.APIGroupList{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "APIGroupList",
		},
		Groups: []metav1.APIGroup{
			{
				Name: "apps",
				Versions: []metav1.GroupVersionForDiscovery{
					{GroupVersion: "apps/v1", Version: "v1"},
				},
				PreferredVersion: metav1.GroupVersionForDiscovery{
					GroupVersion: "apps/v1",
					Version:      "v1",
				},
			},
			{
				Name: "batch",
				Versions: []metav1.GroupVersionForDiscovery{
					{GroupVersion: "batch/v1", Version: "v1"},
				},
				PreferredVersion: metav1.GroupVersionForDiscovery{
					GroupVersion: "batch/v1",
					Version:      "v1",
				},
			},
		},
	})
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func (s *Server) handleAppsRoot(w http.ResponseWriter, r *http.Request) {
	data, _ := json.Marshal(metav1.APIGroup{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "APIGroup",
		},
		Name: "apps",
		Versions: []metav1.GroupVersionForDiscovery{
			{GroupVersion: "apps/v1", Version: "v1"},
		},
		PreferredVersion: metav1.GroupVersionForDiscovery{
			GroupVersion: "apps/v1",
			Version:      "v1",
		},
	})
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func (s *Server) handleAppsV1(w http.ResponseWriter, r *http.Request) {
	data, _ := json.Marshal(metav1.APIResourceList{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "APIResourceList",
		},
		GroupVersion: "apps/v1",
		APIResources: []metav1.APIResource{
			{Name: "deployments", SingularName: "deployment", Namespaced: true, Kind: "Deployment", Verbs: verbList(), ShortNames: []string{"deploy"}, Categories: []string{"all"}},
			{Name: "deployments/scale", SingularName: "", Namespaced: true, Kind: "Scale", Verbs: []string{"get", "patch", "update"}},
		},
	})
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func (s *Server) handleBatchRoot(w http.ResponseWriter, r *http.Request) {
	data, _ := json.Marshal(metav1.APIGroup{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "APIGroup",
		},
		Name: "batch",
		Versions: []metav1.GroupVersionForDiscovery{
			{GroupVersion: "batch/v1", Version: "v1"},
		},
		PreferredVersion: metav1.GroupVersionForDiscovery{
			GroupVersion: "batch/v1",
			Version:      "v1",
		},
	})
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func (s *Server) handleBatchV1(w http.ResponseWriter, r *http.Request) {
	data, _ := json.Marshal(metav1.APIResourceList{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "APIResourceList",
		},
		GroupVersion: "batch/v1",
		APIResources: []metav1.APIResource{
			{Name: "jobs", SingularName: "job", Namespaced: true, Kind: "Job", Verbs: verbList(), Categories: []string{"all"}},
			{Name: "cronjobs", SingularName: "cronjob", Namespaced: true, Kind: "CronJob", Verbs: verbList(), ShortNames: []string{"cj"}, Categories: []string{"all"}},
		},
	})
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	data, _ := json.Marshal(map[string]interface{}{
		"major":        "1",
		"minor":        "36",
		"gitVersion":   "v1.36.0-q8s",
		"gitCommit":    "q8s",
		"gitTreeState": "clean",
		"buildDate":    "2026-06-29T00:00:00Z",
		"goVersion":    "go1.26",
		"compiler":     "gc",
		"platform":     "linux/amd64",
	})
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

// handleOpenAPIv2 serves the Swagger 2.0 schema for client-side validation.
// client-go sends Accept: protobuf,json — we serve native protobuf when asked
// so kubectl doesn't need --validate=false.
func (s *Server) handleOpenAPIv2(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.Header.Get("Accept"), "protobuf") {
		pb, err := getOpenapiV2Proto()
		if err != nil {
			http.Error(w, "openapi proto: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/com.github.proto-openapi.spec.v2.v1.0+protobuf")
		w.Write(pb)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(openapiV2Schema))
}

// handleOpenAPIv3 returns 404 so kubectl falls back to the v2 endpoint.
// Serving our v3 with 406 caused per-group schema fetches to fail without
// retry; 404 is the clean "not supported" signal kubectl handles gracefully.
func (s *Server) handleOpenAPIv3(w http.ResponseWriter, r *http.Request) {
	http.NotFound(w, r)
}

// openapiV2Schema is a Swagger 2.0 document with permissive definitions for all
// resource types supported by q8s. {"type":"object"} with no additionalProperties:false
// means any object structure is valid — kubectl validation passes for all fields.
const openapiV2Schema = `{
  "swagger": "2.0",
  "info": {"title": "q8s", "version": "v0"},
  "paths": {},
  "definitions": {
    "io.k8s.api.core.v1.Pod":                          {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.core.v1.PodList":                      {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.core.v1.Service":                      {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.core.v1.ServiceList":                  {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.core.v1.Namespace":                    {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.core.v1.NamespaceList":                {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.core.v1.PersistentVolumeClaim":        {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.core.v1.PersistentVolumeClaimList":    {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.core.v1.ConfigMap":                    {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.core.v1.ConfigMapList":                {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.core.v1.Secret":                       {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.core.v1.SecretList":                   {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.apps.v1.Deployment":                   {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.apps.v1.DeploymentList":               {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.batch.v1.Job":                         {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.batch.v1.JobList":                     {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.batch.v1.CronJob":                     {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.batch.v1.CronJobList":                 {"type": "object", "x-kubernetes-preserve-unknown-fields": true}
  }
}`

func verbList() []string {
	return []string{"get", "list", "watch", "create", "update", "patch", "delete", "deletecollection"}
}

// DiscoveryResources returns the API resources for kubectl discovery.
func DiscoveryResources() []schema.GroupVersionResource {
	return []schema.GroupVersionResource{
		{Group: "", Version: "v1", Resource: "pods"},
		{Group: "", Version: "v1", Resource: "services"},
		{Group: "", Version: "v1", Resource: "namespaces"},
		{Group: "", Version: "v1", Resource: "persistentvolumeclaims"},
		{Group: "", Version: "v1", Resource: "configmaps"},
		{Group: "", Version: "v1", Resource: "secrets"},
		{Group: "batch", Version: "v1", Resource: "jobs"},
		{Group: "batch", Version: "v1", Resource: "cronjobs"},
	}
}
