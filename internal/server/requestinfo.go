package server

import (
	"net/http"
	"strconv"
	"strings"
)

// requestAttributes is the subset of Kubernetes RequestInfo that the RBAC
// authorizer needs for one HTTP request.
type requestAttributes struct {
	IsResourceRequest bool
	Path              string // for non-resource requests, the URL path
	Verb              string // get, list, watch, create, update, patch, delete, deletecollection,
	// or for non-resource requests the lowercased HTTP method (get, post, ...)
	APIGroup    string // "" for the core group
	APIVersion  string
	Namespace   string
	Resource    string // plural, e.g. "pods"
	Subresource string // e.g. "exec", "log", "status", "scale"
	Name        string
}

// parseRequestAttributes maps an HTTP request to RBAC request attributes. It
// mirrors the Kubernetes RequestInfoFactory for the paths q8s serves. A path
// that is not a valid resource shape stays a non-resource request and keeps
// its Path, so the authorizer can deny it for non-admins.
func parseRequestAttributes(r *http.Request) requestAttributes {
	attrs := requestAttributes{
		Path: r.URL.Path,
		Verb: strings.ToLower(r.Method),
	}

	parts, ok := splitResourcePath(r.URL.Path)
	if !ok {
		return attrs
	}

	switch parts[0] {
	case "api":
		if len(parts) < 3 {
			return attrs
		}
		attrs.APIVersion = parts[1]
		parts = parts[2:]
	case "apis":
		if len(parts) < 4 {
			return attrs
		}
		attrs.APIGroup = parts[1]
		attrs.APIVersion = parts[2]
		parts = parts[3:]
	default:
		return attrs
	}

	// /namespaces/{name} is the Namespace object itself; its status and
	// finalize subresources keep "namespaces" as the resource. Any other
	// third segment starts the actual namespaced resource path.
	if parts[0] == "namespaces" && len(parts) > 1 {
		attrs.Namespace = parts[1]
		if len(parts) > 2 && !isNamespaceSubresource(parts[2]) {
			parts = parts[2:]
		}
	}

	attrs.IsResourceRequest = true
	attrs.Resource = parts[0]
	if len(parts) > 1 {
		attrs.Name = parts[1]
	}
	if len(parts) > 2 {
		attrs.Subresource = parts[2]
	}
	attrs.Verb = resourceVerb(r, attrs.Resource, attrs.Subresource, attrs.Name)
	return attrs
}

// splitResourcePath splits a URL path into segments. One leading slash and one
// trailing slash are allowed. Empty segments, "." and ".." make the path
// invalid so traversal can never be mistaken for an API resource.
func splitResourcePath(path string) ([]string, bool) {
	path = strings.TrimPrefix(path, "/")
	path = strings.TrimSuffix(path, "/")
	if path == "" {
		return nil, false
	}
	parts := strings.Split(path, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, false
		}
	}
	return parts, true
}

// isNamespaceSubresource reports whether a segment after a Namespace name is a
// subresource of the Namespace object rather than a namespaced resource.
func isNamespaceSubresource(segment string) bool {
	switch segment {
	case "status", "finalize":
		return true
	default:
		return false
	}
}

// resourceVerb maps an HTTP method to an RBAC verb, taking the collection
// versus named object and the pod streaming subresources into account.
func resourceVerb(r *http.Request, resource, subresource, name string) string {
	if resource == "pods" {
		switch subresource {
		case "exec", "attach", "portforward":
			return "create"
		case "log":
			return "get"
		}
	}

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		// watch=true makes a GET a watch even on a named object, as in
		// Kubernetes.
		if watchRequested(r) {
			return "watch"
		}
		if name != "" {
			return "get"
		}
		return "list"
	case http.MethodPost:
		return "create"
	case http.MethodPut:
		return "update"
	case http.MethodPatch:
		return "patch"
	case http.MethodDelete:
		if name != "" {
			return "delete"
		}
		return "deletecollection"
	default:
		return ""
	}
}

// watchRequested reports whether the request asks for a watch. strconv.ParseBool
// accepts the Kubernetes boolean forms, including "true" and "1".
func watchRequested(r *http.Request) bool {
	watch, err := strconv.ParseBool(r.URL.Query().Get("watch"))
	return err == nil && watch
}
