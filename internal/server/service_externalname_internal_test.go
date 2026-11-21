package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"q8s/internal/names"
	"q8s/internal/store"
)

func extNameService(ns, name, externalName string, annotations map[string]string, ports ...corev1.ServicePort) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Annotations: annotations},
		Spec: corev1.ServiceSpec{
			Type:         corev1.ServiceTypeExternalName,
			ExternalName: externalName,
			Ports:        ports,
		},
	}
}

func extNameIngress(ns, name, svcName string, port networkingv1.ServiceBackendPort) *networkingv1.Ingress {
	prefix := networkingv1.PathTypePrefix
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{
			Host: "app.example.com",
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
				Paths: []networkingv1.HTTPIngressPath{{
					Path: "/", PathType: &prefix,
					Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
						Name: svcName, Port: port,
					}},
				}},
			}},
		}}},
	}
}

func readExtNameTraefikFile(t *testing.T, dir, ns, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, names.Host(names.KindIngressFile, ns, name)))
	if err != nil {
		t.Fatalf("read traefik file: %v", err)
	}
	return string(b)
}

func doExtNameServiceRequest(t *testing.T, s *Server, method, ns, name string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	url := "/api/v1/namespaces/" + ns + "/services"
	if name != "" {
		url += "/" + name
	}
	req := httptest.NewRequest(method, url, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleServices(rec, req, ns, name)
	return rec
}

// TestExternalNameBackendRendering pins the single server URL an ExternalName
// backend produces: <scheme>://<externalName>:<port>, with the Ingress port
// number, a resolved named port, an https serversscheme, and an IPv6 literal
// bracketed.
func TestExternalNameBackendRendering(t *testing.T) {
	for _, tc := range []struct {
		name         string
		externalName string
		annotations  map[string]string
		port         networkingv1.ServiceBackendPort
		want         string
	}{
		{
			name:         "number",
			externalName: "backend.example.com",
			port:         networkingv1.ServiceBackendPort{Number: 8443},
			want:         `- url: "http://backend.example.com:8443"`,
		},
		{
			name:         "named port",
			externalName: "backend.example.com",
			port:         networkingv1.ServiceBackendPort{Name: "https"},
			want:         `- url: "http://backend.example.com:8443"`,
		},
		{
			name:         "https scheme",
			externalName: "backend.example.com",
			annotations:  map[string]string{annServiceServersScheme: "https"},
			port:         networkingv1.ServiceBackendPort{Number: 8443},
			want:         `- url: "https://backend.example.com:8443"`,
		},
		{
			name:         "h2c scheme",
			externalName: "backend.example.com",
			annotations:  map[string]string{annServiceServersScheme: "h2c"},
			port:         networkingv1.ServiceBackendPort{Number: 8080},
			want:         `- url: "h2c://backend.example.com:8080"`,
		},
		{
			name:         "ipv6 literal",
			externalName: "2001:db8::10",
			port:         networkingv1.ServiceBackendPort{Number: 8443},
			want:         `- url: "http://[2001:db8::10]:8443"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := store.New()
			dir := t.TempDir()
			s := &Server{config: Config{Store: st, TraefikDir: dir}}
			if _, err := st.CreateService(extNameService("default", "backend", tc.externalName, tc.annotations,
				corev1.ServicePort{Name: "https", Port: 8443})); err != nil {
				t.Fatal(err)
			}
			ing := extNameIngress("default", "app", "backend", tc.port)
			s.generateTraefikConfig(ing)

			cfg := readExtNameTraefikFile(t, dir, "default", "app")
			if !strings.Contains(cfg, tc.want) {
				t.Fatalf("missing %s in:\n%s", tc.want, cfg)
			}
			if strings.Contains(cfg, "localhost") {
				t.Fatalf("ExternalName backend fell back to localhost:\n%s", cfg)
			}
		})
	}
}

// TestExternalNameBackendNeverFallsBackToLocalhost: a valid ExternalName
// Service always yields its external URL, even with no selector and no pods;
// a named Ingress port the Service does not declare skips the path instead of
// dialing Traefik's own loopback.
func TestExternalNameBackendNeverFallsBackToLocalhost(t *testing.T) {
	t.Run("no pods", func(t *testing.T) {
		st := store.New()
		dir := t.TempDir()
		s := &Server{config: Config{Store: st, TraefikDir: dir}}
		if _, err := st.CreateService(extNameService("default", "backend", "backend.example.com", nil)); err != nil {
			t.Fatal(err)
		}
		s.generateTraefikConfig(extNameIngress("default", "app", "backend",
			networkingv1.ServiceBackendPort{Number: 8443}))

		cfg := readExtNameTraefikFile(t, dir, "default", "app")
		if !strings.Contains(cfg, `- url: "http://backend.example.com:8443"`) {
			t.Fatalf("external backend missing:\n%s", cfg)
		}
		if strings.Contains(cfg, "localhost") || strings.Count(cfg, "- url:") != 1 {
			t.Fatalf("unexpected backend list:\n%s", cfg)
		}
	})

	t.Run("unresolvable named port", func(t *testing.T) {
		st := store.New()
		dir := t.TempDir()
		s := &Server{config: Config{Store: st, TraefikDir: dir}}
		if _, err := st.CreateService(extNameService("default", "backend", "backend.example.com", nil,
			corev1.ServicePort{Name: "web", Port: 8080})); err != nil {
			t.Fatal(err)
		}
		s.generateTraefikConfig(extNameIngress("default", "app", "backend",
			networkingv1.ServiceBackendPort{Name: "missing"}))

		cfg := readExtNameTraefikFile(t, dir, "default", "app")
		if strings.Contains(cfg, "localhost") {
			t.Fatalf("broken ExternalName port fell back to localhost:\n%s", cfg)
		}
		if strings.Contains(cfg, "- url:") {
			t.Fatalf("skipped path still has a server:\n%s", cfg)
		}
	})
}

// TestExternalNameServersTransport: a valid serversTransport reference is
// written into the generated service, while an invalid value stored behind
// the API's back is ignored when rendering.
func TestExternalNameServersTransport(t *testing.T) {
	st := store.New()
	dir := t.TempDir()
	s := &Server{config: Config{Store: st, TraefikDir: dir}}
	svc := extNameService("default", "backend", "backend.example.com", map[string]string{
		annServiceServersScheme:    "https",
		annServiceServersTransport: "backend-ca@file",
	}, corev1.ServicePort{Port: 8443})
	created, err := st.CreateService(svc)
	if err != nil {
		t.Fatal(err)
	}
	s.generateTraefikConfig(extNameIngress("default", "app", "backend",
		networkingv1.ServiceBackendPort{Number: 8443}))

	cfg := readExtNameTraefikFile(t, dir, "default", "app")
	for _, want := range []string{
		`- url: "https://backend.example.com:8443"`,
		`serversTransport: "backend-ca@file"`,
	} {
		if !strings.Contains(cfg, want) {
			t.Fatalf("missing %s in:\n%s", want, cfg)
		}
	}

	// The store accepts this shape (it bypasses the API validation), but the
	// renderer must not splice it into the config Traefik loads.
	stored, err := st.GetService("default", created.Name)
	if err != nil {
		t.Fatal(err)
	}
	stored.Annotations[annServiceServersTransport] = `bad"transport@file`
	stored.Annotations[annServiceServersScheme] = "ftp"
	if _, err := st.UpdateService(stored); err != nil {
		t.Fatal(err)
	}
	s.generateTraefikConfig(extNameIngress("default", "app", "backend",
		networkingv1.ServiceBackendPort{Number: 8443}))
	cfg = readExtNameTraefikFile(t, dir, "default", "app")
	if strings.Contains(cfg, "serversTransport") {
		t.Fatalf("invalid stored transport rendered:\n%s", cfg)
	}
	if !strings.Contains(cfg, `- url: "http://backend.example.com:8443"`) {
		t.Fatalf("invalid stored scheme did not fall back to http:\n%s", cfg)
	}
}

// TestServiceValidationRejectsBadAnnotationsAndExternalName: Service writes
// fail with 422 before the object is stored when an annotation or an
// ExternalName is invalid, and a valid ExternalName Service is accepted.
func TestServiceValidationRejectsBadAnnotationsAndExternalName(t *testing.T) {
	st := store.New()
	s := &Server{config: Config{Store: st, TraefikDir: t.TempDir()}}

	serviceBody := func(name string, spec map[string]interface{}, annotations map[string]interface{}) map[string]interface{} {
		metadata := map[string]interface{}{"name": name, "namespace": "default"}
		if annotations != nil {
			metadata["annotations"] = annotations
		}
		return map[string]interface{}{
			"apiVersion": "v1", "kind": "Service",
			"metadata": metadata,
			"spec":     spec,
		}
	}
	externalNameSpec := func(externalName interface{}) map[string]interface{} {
		spec := map[string]interface{}{
			"type": "ExternalName",
			"ports": []interface{}{map[string]interface{}{
				"port": 8443, "targetPort": 8443,
			}},
		}
		if externalName != nil {
			spec["externalName"] = externalName
		}
		return spec
	}

	for _, tc := range []struct {
		name       string
		body       map[string]interface{}
		wantCode   int
		wantStored bool
	}{
		{
			name:     "missing externalName",
			body:     serviceBody("missing", externalNameSpec(nil), nil),
			wantCode: http.StatusUnprocessableEntity,
		},
		{
			name:     "invalid externalName",
			body:     serviceBody("upper", externalNameSpec("Backend.Example.com"), nil),
			wantCode: http.StatusUnprocessableEntity,
		},
		{
			name: "invalid scheme",
			body: serviceBody("scheme", externalNameSpec("backend.example.com"), map[string]interface{}{
				annServiceServersScheme: "ftp",
			}),
			wantCode: http.StatusUnprocessableEntity,
		},
		{
			name: "transport without provider",
			body: serviceBody("transport", externalNameSpec("backend.example.com"), map[string]interface{}{
				annServiceServersTransport: "backend-ca",
			}),
			wantCode: http.StatusUnprocessableEntity,
		},
		{
			name: "transport with bad provider",
			body: serviceBody("provider", externalNameSpec("backend.example.com"), map[string]interface{}{
				annServiceServersTransport: "backend-ca@File",
			}),
			wantCode: http.StatusUnprocessableEntity,
		},
		{
			name: "valid",
			body: serviceBody("valid", externalNameSpec("backend.example.com"), map[string]interface{}{
				annServiceServersScheme:    "https",
				annServiceServersTransport: "backend-ca@file",
			}),
			wantCode:   http.StatusCreated,
			wantStored: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doExtNameServiceRequest(t, s, http.MethodPost, "default", "", tc.body)
			if rec.Code != tc.wantCode {
				t.Fatalf("POST status = %d, want %d: %s", rec.Code, tc.wantCode, rec.Body.String())
			}
			name := tc.body["metadata"].(map[string]interface{})["name"].(string)
			_, err := st.GetService("default", name)
			if stored := err == nil; stored != tc.wantStored {
				t.Fatalf("Service stored = %v, want %v (err=%v)", stored, tc.wantStored, err)
			}
		})
	}

	// A PATCH that produces an invalid ExternalName is rejected too.
	rec := doExtNameServiceRequest(t, s, http.MethodPatch, "default", "valid", map[string]interface{}{
		"spec": map[string]interface{}{"externalName": "Bad_Example.com"},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("PATCH status = %d, want 422: %s", rec.Code, rec.Body.String())
	}
	got, err := st.GetService("default", "valid")
	if err != nil {
		t.Fatal(err)
	}
	if got.Spec.ExternalName != "backend.example.com" {
		t.Fatalf("rejected patch changed the stored externalName to %q", got.Spec.ExternalName)
	}
}

// TestSelectorServiceSchemeAnnotation: serversscheme also changes the scheme
// of pod URLs generated from a selector Service.
func TestSelectorServiceSchemeAnnotation(t *testing.T) {
	st := store.New()
	dir := t.TempDir()
	s := &Server{config: Config{Store: st, TraefikDir: dir}}
	if _, err := st.CreatePod(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "default", Labels: map[string]string{"app": "web"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "web", Image: "nginx",
			Ports: []corev1.ContainerPort{{ContainerPort: 8080}},
		}}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateService(&corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default", Annotations: map[string]string{
			annServiceServersScheme: "https",
		}},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "web"},
			Ports:    []corev1.ServicePort{{Port: 80, TargetPort: intstr.FromInt(8080)}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	s.generateTraefikConfig(extNameIngress("default", "app", "web",
		networkingv1.ServiceBackendPort{Number: 80}))

	cfg := readExtNameTraefikFile(t, dir, "default", "app")
	if !strings.Contains(cfg, `- url: "https://web-0.default:8080"`) {
		t.Fatalf("selector backend did not use https:\n%s", cfg)
	}
}

// TestServiceUpdateRerendersIngress: changing spec.externalName through the
// Service PATCH handler rewrites the Ingress file that references it.
func TestServiceUpdateRerendersIngress(t *testing.T) {
	st := store.New()
	dir := t.TempDir()
	s := &Server{config: Config{Store: st, TraefikDir: dir}}
	if _, err := st.CreateService(extNameService("default", "backend", "old.example.com", nil)); err != nil {
		t.Fatal(err)
	}
	ing := extNameIngress("default", "app", "backend",
		networkingv1.ServiceBackendPort{Number: 8443})
	if _, err := st.CreateIngress(ing); err != nil {
		t.Fatal(err)
	}
	s.generateTraefikConfig(ing)
	if cfg := readExtNameTraefikFile(t, dir, "default", "app"); !strings.Contains(cfg, "old.example.com:8443") {
		t.Fatalf("initial render missing old externalName:\n%s", cfg)
	}

	rec := doExtNameServiceRequest(t, s, http.MethodPatch, "default", "backend", map[string]interface{}{
		"spec": map[string]interface{}{"externalName": "new.example.com"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	cfg := readExtNameTraefikFile(t, dir, "default", "app")
	if !strings.Contains(cfg, `- url: "http://new.example.com:8443"`) {
		t.Fatalf("Service update did not re-render the Ingress:\n%s", cfg)
	}
	if strings.Contains(cfg, "old.example.com") {
		t.Fatalf("stale externalName left in the Ingress file:\n%s", cfg)
	}
}

// TestExternalNameServiceCreateDoesNotPublishPorts: creating an ExternalName
// Service must not allocate nodePorts, touch the quadlet directory, or
// otherwise run selector/port publishing code.
func TestExternalNameServiceCreateDoesNotPublishPorts(t *testing.T) {
	st := store.New()
	quadletDir := t.TempDir()
	s := &Server{config: Config{Store: st, TraefikDir: t.TempDir(), QuadletDir: quadletDir}}

	body := map[string]interface{}{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]interface{}{
			"name": "backend", "namespace": "default",
			"annotations": map[string]interface{}{
				annServiceServersScheme: "https",
			},
		},
		"spec": map[string]interface{}{
			"type":         "ExternalName",
			"externalName": "backend.example.com",
			"selector":     map[string]string{"app": "backend"},
			"ports": []interface{}{map[string]interface{}{
				"port": 8443, "targetPort": 8443,
			}},
		},
	}
	rec := doExtNameServiceRequest(t, s, http.MethodPost, "default", "", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	created, err := st.GetService("default", "backend")
	if err != nil {
		t.Fatal(err)
	}
	if got := created.Spec.Ports[0].NodePort; got != 0 {
		t.Fatalf("ExternalName Service was assigned nodePort %d", got)
	}
	entries, err := os.ReadDir(quadletDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("ExternalName Service create wrote quadlet files: %v", names)
	}
}
