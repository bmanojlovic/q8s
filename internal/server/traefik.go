package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"

	"q8s/internal/traefik"
)

// --- strict decoding ---

// decodeTraefikStrict decodes a create/update body into one of the Traefik
// subset types. DisallowUnknownFields makes a field outside the subset a 422
// naming it, rather than silently dropping configuration the user asked for.
func (s *Server) decodeTraefikStrict(w http.ResponseWriter, r *http.Request, obj interface{}) bool {
	body, ok := s.readBody(w, r)
	if !ok {
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(obj); err != nil {
		s.respondStatus(w, http.StatusUnprocessableEntity, "Invalid", "failed to decode %T: %s", obj, err.Error())
		return false
	}
	return true
}

func unmarshalTraefikStrict(data []byte, obj interface{}) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(obj)
}

// --- Middleware API ---

func (s *Server) handleMiddlewares(w http.ResponseWriter, r *http.Request, ns, name string) {
	switch r.Method {
	case http.MethodGet:
		if name == "" {
			respondList(w, r, s, func() []*traefik.Middleware { return s.config.Store.Middlewares(ns) }, middlewaresToTable,
				func(items []traefik.Middleware) *traefik.MiddlewareList {
					return &traefik.MiddlewareList{TypeMeta: metav1.TypeMeta{APIVersion: traefik.APIVersion, Kind: "MiddlewareList"}, Items: items}
				})
			return
		}
		mw, err := s.config.Store.GetMiddleware(ns, name)
		if err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		if isTableRequest(r) {
			encodeTable(w, middlewaresToTable([]*traefik.Middleware{mw}, s.rv()))
			return
		}
		encode(w, mw, http.StatusOK)
	case http.MethodPost:
		var mw traefik.Middleware
		if !s.decodeTraefikStrict(w, r, &mw) {
			return
		}
		mw.APIVersion = traefik.APIVersion
		mw.Kind = "Middleware"
		if mw.Namespace == "" {
			mw.Namespace = ns
		}
		if err := validateName("namespace", mw.Namespace); err != nil {
			s.respondStatus(w, http.StatusUnprocessableEntity, "Invalid", "%s", err.Error())
			return
		}
		if err := validateName("name", mw.Name); err != nil {
			s.respondStatus(w, http.StatusUnprocessableEntity, "Invalid", "%s", err.Error())
			return
		}
		if err := validateMiddleware(&mw); err != nil {
			s.respondStatus(w, http.StatusUnprocessableEntity, "Invalid", "%s", err.Error())
			return
		}
		created, err := s.config.Store.CreateMiddleware(&mw)
		if err != nil {
			s.respondStatus(w, http.StatusConflict, "AlreadyExists", "%s", err.Error())
			return
		}
		s.generateMiddlewareConfig(created)
		encode(w, created, http.StatusCreated)
	case http.MethodPut:
		var mw traefik.Middleware
		if !s.decodeTraefikStrict(w, r, &mw) {
			return
		}
		mw.APIVersion = traefik.APIVersion
		mw.Kind = "Middleware"
		if mw.Namespace == "" {
			mw.Namespace = ns
		}
		if mw.Name == "" {
			mw.Name = name
		}
		if err := validatePatchedIdentity("middleware", mw.Namespace, mw.Name, ns, name); err != nil {
			s.respondStatus(w, http.StatusUnprocessableEntity, "Invalid", "%s", err.Error())
			return
		}
		if err := validateMiddleware(&mw); err != nil {
			s.respondStatus(w, http.StatusUnprocessableEntity, "Invalid", "%s", err.Error())
			return
		}
		updated, err := s.config.Store.UpdateMiddleware(&mw)
		if err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		s.generateMiddlewareConfig(updated)
		encode(w, updated, http.StatusOK)
	case http.MethodPatch:
		existing, err := s.config.Store.GetMiddleware(ns, name)
		if err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		patched, ok := s.patchTraefikObject(w, r, existing, ns, name, "middleware")
		if !ok {
			return
		}
		mw, ok := patched.(*traefik.Middleware)
		if !ok {
			s.respondStatus(w, http.StatusInternalServerError, "InternalError", "unexpected patched Middleware type")
			return
		}
		mw.APIVersion = traefik.APIVersion
		mw.Kind = "Middleware"
		if err := validateMiddleware(mw); err != nil {
			s.respondStatus(w, http.StatusUnprocessableEntity, "Invalid", "%s", err.Error())
			return
		}
		updated, err := s.config.Store.UpdateMiddleware(mw)
		if err != nil {
			s.respondStatus(w, http.StatusInternalServerError, "InternalError", "%s", err.Error())
			return
		}
		s.generateMiddlewareConfig(updated)
		encode(w, updated, http.StatusOK)
	case http.MethodDelete:
		if name == "" {
			s.deleteCollection(w,
				func() []string {
					var names []string
					for _, mw := range s.config.Store.Middlewares(ns) {
						names = append(names, mw.Name)
					}
					return names
				},
				func(n string) error { return s.deleteMiddlewareByName(ns, n) })
			return
		}
		if err := s.deleteMiddlewareByName(ns, name); err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		encode(w, &metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Success"}, http.StatusOK)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// --- IngressRouteTCP API ---

func (s *Server) handleIngressRouteTCPs(w http.ResponseWriter, r *http.Request, ns, name string) {
	switch r.Method {
	case http.MethodGet:
		if name == "" {
			respondList(w, r, s, func() []*traefik.IngressRouteTCP { return s.config.Store.IngressRouteTCPs(ns) }, ingressRouteTCPsToTable,
				func(items []traefik.IngressRouteTCP) *traefik.IngressRouteTCPList {
					return &traefik.IngressRouteTCPList{TypeMeta: metav1.TypeMeta{APIVersion: traefik.APIVersion, Kind: "IngressRouteTCPList"}, Items: items}
				})
			return
		}
		irt, err := s.config.Store.GetIngressRouteTCP(ns, name)
		if err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		if isTableRequest(r) {
			encodeTable(w, ingressRouteTCPsToTable([]*traefik.IngressRouteTCP{irt}, s.rv()))
			return
		}
		encode(w, irt, http.StatusOK)
	case http.MethodPost:
		var irt traefik.IngressRouteTCP
		if !s.decodeTraefikStrict(w, r, &irt) {
			return
		}
		irt.APIVersion = traefik.APIVersion
		irt.Kind = "IngressRouteTCP"
		if irt.Namespace == "" {
			irt.Namespace = ns
		}
		if err := validateName("namespace", irt.Namespace); err != nil {
			s.respondStatus(w, http.StatusUnprocessableEntity, "Invalid", "%s", err.Error())
			return
		}
		if err := validateName("name", irt.Name); err != nil {
			s.respondStatus(w, http.StatusUnprocessableEntity, "Invalid", "%s", err.Error())
			return
		}
		if err := validateIngressRouteTCP(&irt); err != nil {
			s.respondStatus(w, http.StatusUnprocessableEntity, "Invalid", "%s", err.Error())
			return
		}
		if err := s.ingressRouteTCPConflict(&irt); err != nil {
			s.respondStatus(w, http.StatusConflict, "Conflict", "%s", err.Error())
			return
		}
		created, err := s.config.Store.CreateIngressRouteTCP(&irt)
		if err != nil {
			s.respondStatus(w, http.StatusConflict, "AlreadyExists", "%s", err.Error())
			return
		}
		s.generateTCPConfig(created)
		encode(w, created, http.StatusCreated)
	case http.MethodPut:
		var irt traefik.IngressRouteTCP
		if !s.decodeTraefikStrict(w, r, &irt) {
			return
		}
		irt.APIVersion = traefik.APIVersion
		irt.Kind = "IngressRouteTCP"
		if irt.Namespace == "" {
			irt.Namespace = ns
		}
		if irt.Name == "" {
			irt.Name = name
		}
		if err := validatePatchedIdentity("ingressroutetcp", irt.Namespace, irt.Name, ns, name); err != nil {
			s.respondStatus(w, http.StatusUnprocessableEntity, "Invalid", "%s", err.Error())
			return
		}
		if err := validateIngressRouteTCP(&irt); err != nil {
			s.respondStatus(w, http.StatusUnprocessableEntity, "Invalid", "%s", err.Error())
			return
		}
		if err := s.ingressRouteTCPConflict(&irt); err != nil {
			s.respondStatus(w, http.StatusConflict, "Conflict", "%s", err.Error())
			return
		}
		updated, err := s.config.Store.UpdateIngressRouteTCP(&irt)
		if err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		s.generateTCPConfig(updated)
		encode(w, updated, http.StatusOK)
	case http.MethodPatch:
		existing, err := s.config.Store.GetIngressRouteTCP(ns, name)
		if err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		patched, ok := s.patchTraefikObject(w, r, existing, ns, name, "ingressroutetcp")
		if !ok {
			return
		}
		irt, ok := patched.(*traefik.IngressRouteTCP)
		if !ok {
			s.respondStatus(w, http.StatusInternalServerError, "InternalError", "unexpected patched IngressRouteTCP type")
			return
		}
		irt.APIVersion = traefik.APIVersion
		irt.Kind = "IngressRouteTCP"
		if err := validateIngressRouteTCP(irt); err != nil {
			s.respondStatus(w, http.StatusUnprocessableEntity, "Invalid", "%s", err.Error())
			return
		}
		if err := s.ingressRouteTCPConflict(irt); err != nil {
			s.respondStatus(w, http.StatusConflict, "Conflict", "%s", err.Error())
			return
		}
		updated, err := s.config.Store.UpdateIngressRouteTCP(irt)
		if err != nil {
			s.respondStatus(w, http.StatusInternalServerError, "InternalError", "%s", err.Error())
			return
		}
		s.generateTCPConfig(updated)
		encode(w, updated, http.StatusOK)
	case http.MethodDelete:
		if name == "" {
			s.deleteCollection(w,
				func() []string {
					var names []string
					for _, irt := range s.config.Store.IngressRouteTCPs(ns) {
						names = append(names, irt.Name)
					}
					return names
				},
				func(n string) error { return s.deleteIngressRouteTCPByName(ns, n) })
			return
		}
		if err := s.deleteIngressRouteTCPByName(ns, name); err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		encode(w, &metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Success"}, http.StatusOK)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// patchTraefikObject applies a merge or JSON patch to an object's JSON view,
// decodes the result strictly into a fresh value of the same type, and checks
// that the patch did not change its identity. It returns the patched object
// as an interface; the caller type-asserts it back.
func (s *Server) patchTraefikObject(w http.ResponseWriter, r *http.Request, existing interface{}, ns, name, kind string) (interface{}, bool) {
	body, ok := s.readBody(w, r)
	if !ok {
		return nil, false
	}
	base, err := toJSONMap(existing)
	if err != nil {
		s.respondStatus(w, http.StatusInternalServerError, "InternalError", "%s", err.Error())
		return nil, false
	}
	if isJSONPatch(r) {
		var ops []map[string]interface{}
		if err := json.Unmarshal(body, &ops); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
			return nil, false
		}
		if err := applyJSONPatch(base, ops); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
			return nil, false
		}
	} else {
		var overlay map[string]interface{}
		if err := json.Unmarshal(body, &overlay); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
			return nil, false
		}
		jsonMerge(base, overlay)
	}
	merged, err := json.Marshal(base)
	if err != nil {
		s.respondStatus(w, http.StatusInternalServerError, "InternalError", "%s", err.Error())
		return nil, false
	}

	var patched interface{}
	switch kind {
	case "middleware":
		patched = &traefik.Middleware{}
	case "ingressroutetcp":
		patched = &traefik.IngressRouteTCP{}
	default:
		s.respondStatus(w, http.StatusInternalServerError, "InternalError", "unknown object kind %q", kind)
		return nil, false
	}
	if err := unmarshalTraefikStrict(merged, patched); err != nil {
		s.respondStatus(w, http.StatusUnprocessableEntity, "Invalid", "%s", err.Error())
		return nil, false
	}
	meta, ok := patched.(metav1.Object)
	if !ok {
		s.respondStatus(w, http.StatusInternalServerError, "InternalError", "patched object has no metadata")
		return nil, false
	}
	if err := validatePatchedIdentity(kind, meta.GetNamespace(), meta.GetName(), ns, name); err != nil {
		s.respondStatus(w, http.StatusUnprocessableEntity, "Invalid", "%s", err.Error())
		return nil, false
	}
	return patched, true
}

func toJSONMap(obj interface{}) (map[string]interface{}, error) {
	data, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	var base map[string]interface{}
	if err := json.Unmarshal(data, &base); err != nil {
		return nil, err
	}
	return base, nil
}

// --- routing entry points ---

func (s *Server) handleTraefikNamespaced(w http.ResponseWriter, r *http.Request) {
	ns, resource, name, ok := parseTraefikNamespaceResource(r.URL.Path)
	if !ok {
		s.respondStatus(w, http.StatusBadRequest, "BadRequest", "invalid traefik.io namespace resource path")
		return
	}
	switch resource {
	case "middlewares":
		s.handleMiddlewares(w, r, ns, name)
	case "ingressroutetcps":
		s.handleIngressRouteTCPs(w, r, ns, name)
	default:
		s.respondStatus(w, http.StatusNotFound, "NotFound", "resource %q not found", resource)
	}
}

func (s *Server) handleTraefikClusterList(w http.ResponseWriter, r *http.Request, resource string) {
	switch resource {
	case "middlewares":
		respondList(w, r, s, s.config.Store.AllMiddlewares, middlewaresToTable,
			func(items []traefik.Middleware) *traefik.MiddlewareList {
				return &traefik.MiddlewareList{TypeMeta: metav1.TypeMeta{APIVersion: traefik.APIVersion, Kind: "MiddlewareList"}, Items: items}
			})
	case "ingressroutetcps":
		respondList(w, r, s, s.config.Store.AllIngressRouteTCPs, ingressRouteTCPsToTable,
			func(items []traefik.IngressRouteTCP) *traefik.IngressRouteTCPList {
				return &traefik.IngressRouteTCPList{TypeMeta: metav1.TypeMeta{APIVersion: traefik.APIVersion, Kind: "IngressRouteTCPList"}, Items: items}
			})
	default:
		s.respondStatus(w, http.StatusNotFound, "NotFound", "resource %q not found", resource)
	}
}

// --- Traefik dynamic config rendering ---

type middlewareDocument struct {
	HTTP middlewareHTTP `json:"http"`
}

type middlewareHTTP struct {
	Middlewares map[string]middlewareBody `json:"middlewares"`
}

type middlewareBody struct {
	IPAllowList      *traefik.IPAllowList      `json:"ipAllowList,omitempty"`
	RedirectRegex    *traefik.RedirectRegex    `json:"redirectRegex,omitempty"`
	RedirectScheme   *traefik.RedirectScheme   `json:"redirectScheme,omitempty"`
	Headers          *traefik.Headers          `json:"headers,omitempty"`
	ReplacePath      *traefik.ReplacePath      `json:"replacePath,omitempty"`
	ReplacePathRegex *traefik.ReplacePathRegex `json:"replacePathRegex,omitempty"`
	StripPrefix      *traefik.StripPrefix      `json:"stripPrefix,omitempty"`
	StripPrefixRegex *traefik.StripPrefixRegex `json:"stripPrefixRegex,omitempty"`
	AddPrefix        *traefik.AddPrefix        `json:"addPrefix,omitempty"`
}

type tcpDocument struct {
	TCP tcpConfig `json:"tcp"`
}

type tcpConfig struct {
	Routers           map[string]tcpRouter           `json:"routers"`
	Services          map[string]tcpService          `json:"services"`
	ServersTransports map[string]tcpServersTransport `json:"serversTransports,omitempty"`
}

// tcpServersTransport carries the PROXY protocol setting for a TCP service.
// Traefik v3 deprecates loadBalancer.proxyProtocol in favour of a
// serversTransport that the service references.
type tcpServersTransport struct {
	ProxyProtocol *traefik.ProxyProtocol `json:"proxyProtocol,omitempty"`
}

type tcpRouter struct {
	EntryPoints []string      `json:"entryPoints,omitempty"`
	Rule        string        `json:"rule"`
	Service     string        `json:"service"`
	TLS         *tcpRouterTLS `json:"tls,omitempty"`
}

type tcpRouterTLS struct {
	Passthrough bool `json:"passthrough,omitempty"`
}

type tcpService struct {
	LoadBalancer tcpLoadBalancer `json:"loadBalancer"`
}

type tcpLoadBalancer struct {
	ServersTransport string      `json:"serversTransport,omitempty"`
	Servers          []tcpServer `json:"servers"`
}

type tcpServer struct {
	Address string `json:"address"`
}

func middlewareFilename(ns, name string) string {
	return fmt.Sprintf("middleware-%s-%s.yaml", ns, name)
}

func tcpFilename(ns, name string) string {
	return fmt.Sprintf("tcp-%s-%s.yaml", ns, name)
}

// generateMiddlewareConfig writes the file-provider config for one
// Middleware. Traefik's file provider names the object <namespace>-<name>,
// which is also what an Ingress middleware reference is rewritten to.
func (s *Server) generateMiddlewareConfig(mw *traefik.Middleware) {
	if s.config.TraefikDir == "" {
		return
	}
	body := middlewareBody{
		IPAllowList:      mw.Spec.IPAllowList,
		RedirectRegex:    mw.Spec.RedirectRegex,
		RedirectScheme:   mw.Spec.RedirectScheme,
		Headers:          mw.Spec.Headers,
		ReplacePath:      mw.Spec.ReplacePath,
		ReplacePathRegex: mw.Spec.ReplacePathRegex,
		StripPrefix:      mw.Spec.StripPrefix,
		StripPrefixRegex: mw.Spec.StripPrefixRegex,
		AddPrefix:        mw.Spec.AddPrefix,
	}
	doc := middlewareDocument{HTTP: middlewareHTTP{
		Middlewares: map[string]middlewareBody{
			fmt.Sprintf("%s-%s", mw.Namespace, mw.Name): body,
		},
	}}
	content, err := yaml.Marshal(doc)
	if err != nil {
		fmt.Printf("traefik config: marshal middleware %s/%s: %v\n", mw.Namespace, mw.Name, err)
		return
	}
	s.writeTraefikConfigFile(middlewareFilename(mw.Namespace, mw.Name), content)
}

// generateTCPConfig writes the file-provider config for one IngressRouteTCP:
// one router and one load-balancer service per route, named <ns>-<name>-<i>
// exactly like the Ingress router naming.
func (s *Server) generateTCPConfig(irt *traefik.IngressRouteTCP) {
	if s.config.TraefikDir == "" {
		return
	}
	doc := tcpDocument{TCP: tcpConfig{
		Routers:  make(map[string]tcpRouter),
		Services: make(map[string]tcpService),
	}}
	for i, route := range irt.Spec.Routes {
		name := fmt.Sprintf("%s-%s-%d", irt.Namespace, irt.Name, i)
		router := tcpRouter{
			EntryPoints: irt.Spec.EntryPoints,
			Rule:        route.Match,
			Service:     name,
		}
		if irt.Spec.TLS != nil {
			router.TLS = &tcpRouterTLS{Passthrough: irt.Spec.TLS.Passthrough}
		}
		doc.TCP.Routers[name] = router

		svc := tcpService{LoadBalancer: tcpLoadBalancer{Servers: []tcpServer{}}}
		if len(route.Services) == 1 {
			backend := route.Services[0]
			svcPort := backend.Port.IntVal
			portName := ""
			if backend.Port.Type == intstr.String {
				svcPort = 0
				portName = backend.Port.StrVal
			}
			if backend.ProxyProtocol != nil {
				// The IngressRouteTCP field stays as Traefik's CRD defines
				// it; the file provider gets the non-deprecated form, a
				// serversTransport of the same name in this file.
				if doc.TCP.ServersTransports == nil {
					doc.TCP.ServersTransports = make(map[string]tcpServersTransport)
				}
				doc.TCP.ServersTransports[name] = tcpServersTransport{ProxyProtocol: backend.ProxyProtocol}
				svc.LoadBalancer.ServersTransport = name
			}
			for _, address := range s.tcpBackendAddresses(irt.Namespace, backend.Name, svcPort, portName) {
				svc.LoadBalancer.Servers = append(svc.LoadBalancer.Servers, tcpServer{Address: address})
			}
		}
		doc.TCP.Services[name] = svc
	}
	content, err := yaml.Marshal(doc)
	if err != nil {
		fmt.Printf("traefik config: marshal ingressroutetcp %s/%s: %v\n", irt.Namespace, irt.Name, err)
		return
	}
	s.writeTraefikConfigFile(tcpFilename(irt.Namespace, irt.Name), content)
}

// tcpBackendAddresses resolves a TCP backend's pods the same way the Ingress
// resolver does: a non-hostNetwork pod is dialed by cluster DNS on its
// container port, a hostNetwork pod only through an explicit hostPort.
func (s *Server) tcpBackendAddresses(ns, svcName string, svcPort int32, portName string) []string {
	var addresses []string
	for _, target := range s.serviceBackendTargets(ns, svcName, svcPort, portName) {
		switch {
		case !target.hostNetwork:
			addresses = append(addresses, net.JoinHostPort(fmt.Sprintf("%s.%s", target.podName, ns), strconv.Itoa(int(target.containerPort))))
		case target.hostPort != 0:
			addresses = append(addresses, backendHostPort(target.hostIP, target.hostPort))
		}
	}
	return addresses
}

// writeTraefikConfigFile writes content atomically and leaves an unchanged
// file alone: Traefik's file watcher reloads on every write.
func (s *Server) writeTraefikConfigFile(filename string, content []byte) {
	if s.config.TraefikDir == "" {
		return
	}
	path := filepath.Join(s.config.TraefikDir, filename)
	if err := os.MkdirAll(s.config.TraefikDir, 0755); err != nil {
		fmt.Printf("traefik config: mkdir %s: %v\n", s.config.TraefikDir, err)
		return
	}
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, content) {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, content, 0644); err != nil {
		fmt.Printf("traefik config: write %s: %v\n", tmp, err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		fmt.Printf("traefik config: rename %s: %v\n", path, err)
		return
	}
	fmt.Printf("traefik config: wrote %s\n", filename)
}

func (s *Server) removeTraefikConfigFile(filename string) {
	if s.config.TraefikDir == "" {
		return
	}
	path := filepath.Join(s.config.TraefikDir, filename)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		fmt.Printf("traefik config: remove %s: %v\n", path, err)
	}
}

func (s *Server) regenerateMiddlewareConfigs(ns string) {
	if s.config.TraefikDir == "" {
		return
	}
	for _, mw := range s.config.Store.Middlewares(ns) {
		s.generateMiddlewareConfig(mw)
	}
}

func (s *Server) regenerateTCPConfigs(ns string) {
	if s.config.TraefikDir == "" {
		return
	}
	for _, irt := range s.config.Store.IngressRouteTCPs(ns) {
		s.generateTCPConfig(irt)
	}
}

func (s *Server) deleteMiddlewareByName(ns, name string) error {
	if err := s.config.Store.DeleteMiddleware(ns, name); err != nil {
		return err
	}
	s.removeTraefikConfigFile(middlewareFilename(ns, name))
	return nil
}

func (s *Server) deleteIngressRouteTCPByName(ns, name string) error {
	if err := s.config.Store.DeleteIngressRouteTCP(ns, name); err != nil {
		return err
	}
	s.removeTraefikConfigFile(tcpFilename(ns, name))
	return nil
}

// --- tables ---

var middlewareColumns = []columnDef{
	{Name: "Name", Type: "string", Format: "name", Description: "Middleware name", Priority: 0},
	{Name: "Age", Type: "string", Description: "Time since creation", Priority: 0},
}

func middlewaresToTable(items []*traefik.Middleware, rv string) *table {
	t := newTable(rv, middlewareColumns)
	for _, mw := range items {
		t.Rows = append(t.Rows, tableRow{
			Cells:  []interface{}{mw.Name, age(mw.CreationTimestamp.Time)},
			Object: partialMeta{Kind: "Middleware", APIVersion: traefik.APIVersion, Metadata: mw.ObjectMeta},
		})
	}
	return t
}

var ingressRouteTCPColumns = []columnDef{
	{Name: "Name", Type: "string", Format: "name", Description: "IngressRouteTCP name", Priority: 0},
	{Name: "Age", Type: "string", Description: "Time since creation", Priority: 0},
}

func ingressRouteTCPsToTable(items []*traefik.IngressRouteTCP, rv string) *table {
	t := newTable(rv, ingressRouteTCPColumns)
	for _, irt := range items {
		t.Rows = append(t.Rows, tableRow{
			Cells:  []interface{}{irt.Name, age(irt.CreationTimestamp.Time)},
			Object: partialMeta{Kind: "IngressRouteTCP", APIVersion: traefik.APIVersion, Metadata: irt.ObjectMeta},
		})
	}
	return t
}
