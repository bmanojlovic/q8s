package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"

	"sigs.k8s.io/yaml"
)

// applyMiddleware turns server-side apply requests into the existing
// create/JSON-patch operations. q8s has no managedFields and no apply
// tracking, so apply is implemented as a single-manager model: the applier
// owns the user sections (spec/data/binaryData/stringData and
// metadata.labels/annotations), and everything else (status, uid,
// resourceVersion, creationTimestamp) stays server-owned.
//
// Terraform's kubernetes_manifest provider always sends
// Content-Type: application/apply-patch+yaml and PATCHes the object URL, even
// when the object does not exist yet. Handling it here means every resource
// handler keeps its existing validation, defaulting and quadlet side effects.
func (s *Server) applyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || !isApplyPatch(r.Header.Get("Content-Type")) {
			next.ServeHTTP(w, r)
			return
		}
		s.handleApply(w, r, next)
	})
}

// isApplyPatch reports whether contentType is the server-side apply media
// type. Parameters after ';' are ignored, matching the Content-Type parsing
// used by the resource handlers.
func isApplyPatch(contentType string) bool {
	if i := strings.IndexByte(contentType, ';'); i >= 0 {
		contentType = contentType[:i]
	}
	return strings.EqualFold(strings.TrimSpace(contentType), "application/apply-patch+yaml")
}

func (s *Server) handleApply(w http.ResponseWriter, r *http.Request, next http.Handler) {
	// fieldManager and force are accepted and ignored: q8s keeps no
	// managedFields, so there is no per-manager ownership to conflict. Apply
	// here means the applier owns the user sections, as described above.
	if _, ok := r.URL.Query()["dryRun"]; ok {
		s.respondStatus(w, http.StatusBadRequest, "BadRequest", "dryRun is not supported")
		return
	}

	body, ok := s.readBody(w, r)
	if !ok {
		return
	}
	js, err := yaml.YAMLToJSON(body)
	if err != nil {
		s.respondStatus(w, http.StatusBadRequest, "BadRequest", "invalid apply body: %s", err.Error())
		return
	}
	var applied map[string]interface{}
	if err := json.Unmarshal(js, &applied); err != nil || applied == nil {
		s.respondStatus(w, http.StatusBadRequest, "BadRequest", "apply body must be a JSON object")
		return
	}

	apiVersion, _ := applied["apiVersion"].(string)
	kind, _ := applied["kind"].(string)
	if apiVersion == "" || kind == "" {
		s.respondStatus(w, http.StatusBadRequest, "BadRequest", "apply body must contain apiVersion and kind")
		return
	}

	metadata, _ := applied["metadata"].(map[string]interface{})
	if metadata == nil {
		s.respondStatus(w, http.StatusBadRequest, "BadRequest", "apply body must contain metadata")
		return
	}
	urlPath := strings.TrimSuffix(r.URL.Path, "/")
	lastSlash := strings.LastIndex(urlPath, "/")
	if lastSlash < 0 {
		s.respondStatus(w, http.StatusBadRequest, "BadRequest", "invalid object URL %q", r.URL.Path)
		return
	}
	urlName := path.Base(urlPath)
	name, _ := metadata["name"].(string)
	if name == "" || name != urlName {
		s.respondStatus(w, http.StatusBadRequest, "BadRequest", "metadata.name %q does not match URL name %q", name, urlName)
		return
	}
	if namespace, _ := metadata["namespace"].(string); namespace != "" && namespace != namespaceFromURLPath(urlPath) {
		s.respondStatus(w, http.StatusBadRequest, "BadRequest", "metadata.namespace %q does not match URL namespace %q", namespace, namespaceFromURLPath(urlPath))
		return
	}

	// Look the object up through the normal GET handler. This also lets
	// resources q8s synthesizes (Node, StorageClass) participate without
	// apply needing to know about them.
	getRequest := r.Clone(r.Context())
	getRequest.Method = http.MethodGet
	getRequest.Body = http.NoBody
	getRequest.ContentLength = 0
	getRequest.Header = r.Header.Clone()
	getRequest.Header.Del("Content-Type")
	getRequest.Header.Set("Accept", "application/json")
	existing := httptest.NewRecorder()
	next.ServeHTTP(existing, getRequest)

	if existing.Code == http.StatusNotFound {
		collectionPath := urlPath[:lastSlash]
		createRequest := r.Clone(r.Context())
		createRequest.Method = http.MethodPost
		createRequest.URL.Path = collectionPath
		createRequest.Body = io.NopCloser(bytes.NewReader(js))
		createRequest.ContentLength = int64(len(js))
		createRequest.Header = r.Header.Clone()
		createRequest.Header.Set("Content-Type", "application/json")
		createRequest.Header.Del("Content-Length")
		next.ServeHTTP(w, createRequest)
		return
	}
	if existing.Code != http.StatusOK {
		writeRecordedResponse(w, existing)
		return
	}

	var current map[string]interface{}
	if err := json.Unmarshal(existing.Body.Bytes(), &current); err != nil {
		s.respondStatus(w, http.StatusInternalServerError, "InternalError", "decode existing object: %s", err.Error())
		return
	}
	patchBody, err := json.Marshal(applyPatchOps(current, applied))
	if err != nil {
		s.respondStatus(w, http.StatusInternalServerError, "InternalError", "build apply patch: %s", err.Error())
		return
	}

	patchRequest := r.Clone(r.Context())
	patchRequest.Method = http.MethodPatch
	patchRequest.Body = io.NopCloser(bytes.NewReader(patchBody))
	patchRequest.ContentLength = int64(len(patchBody))
	patchRequest.Header = r.Header.Clone()
	patchRequest.Header.Set("Content-Type", "application/json-patch+json")
	patchRequest.Header.Del("Content-Length")
	next.ServeHTTP(w, patchRequest)
}

// namespaceFromURLPath returns the namespace segment for namespaced resource
// URLs such as /api/v1/namespaces/ns/pods/name and
// /apis/apps/v1/namespaces/ns/deployments/name. Namespace objects themselves
// are cluster-scoped, so /api/v1/namespaces/name has no namespace segment.
func namespaceFromURLPath(urlPath string) string {
	const marker = "/namespaces/"
	i := strings.Index(urlPath, marker)
	if i < 0 {
		return ""
	}
	parts := strings.Split(strings.TrimPrefix(urlPath[i+len(marker):], "/"), "/")
	if len(parts) >= 3 {
		return parts[0]
	}
	return ""
}

// applyPatchOps builds an RFC 6902 JSON Patch that replaces current's
// user-owned sections with applied's as whole values. A JSON Patch rather
// than a merge patch: the PATCH handlers merge some lists by name (containers,
// env, volumes), so a merge patch could not remove a list element the applier
// dropped. "add" on an existing member replaces it. Metadata is limited to
// labels and annotations so status, uid, resourceVersion, creationTimestamp
// and other server-owned fields are never touched. A section the applier
// omits is removed only for data-like fields; spec is never removed.
func applyPatchOps(current, applied map[string]interface{}) []map[string]interface{} {
	var ops []map[string]interface{}
	set := func(path string, value interface{}) {
		ops = append(ops, map[string]interface{}{"op": "add", "path": path, "value": value})
	}
	remove := func(path string) {
		ops = append(ops, map[string]interface{}{"op": "remove", "path": path})
	}
	for _, field := range []string{"spec", "data", "binaryData", "stringData"} {
		appliedValue, appliedOK := applied[field]
		currentValue, currentOK := current[field]
		switch {
		case appliedOK:
			set("/"+field, appliedValue)
		case currentOK && currentValue != nil && field != "spec":
			remove("/" + field)
		}
	}
	currentMetadata, _ := current["metadata"].(map[string]interface{})
	appliedMetadata, _ := applied["metadata"].(map[string]interface{})
	if currentMetadata == nil {
		set("/metadata", map[string]interface{}{})
		currentMetadata = map[string]interface{}{}
	}
	for _, field := range []string{"labels", "annotations"} {
		appliedValue, appliedOK := appliedMetadata[field]
		currentValue, currentOK := currentMetadata[field]
		switch {
		case appliedOK && appliedValue != nil:
			set("/metadata/"+field, appliedValue)
		case currentOK && currentValue != nil:
			remove("/metadata/" + field)
		}
	}
	return ops
}

func writeRecordedResponse(w http.ResponseWriter, rec *httptest.ResponseRecorder) {
	for key, values := range rec.Header() {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(rec.Code)
	w.Write(rec.Body.Bytes())
}
