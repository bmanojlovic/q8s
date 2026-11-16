package server

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"q8s/internal/store"
)

// TestServicePassHostHeaderRendering pins the generated loadBalancer field for
// an ExternalName backend: the annotation is rendered exactly, and an absent
// annotation leaves the file without the key.
func TestServicePassHostHeaderRendering(t *testing.T) {
	for _, tc := range []struct {
		name        string
		annotations map[string]string
		want        string
	}{
		{
			name:        "false",
			annotations: map[string]string{annServicePassHostHeader: "false"},
			want:        "        passHostHeader: false",
		},
		{
			name:        "true",
			annotations: map[string]string{annServicePassHostHeader: "true"},
			want:        "        passHostHeader: true",
		},
		{
			name:        "unset",
			annotations: nil,
			want:        "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := store.New()
			dir := t.TempDir()
			s := &Server{config: Config{Store: st, TraefikDir: dir}}
			if _, err := st.CreateService(extNameService("default", "backend", "backend.example.com",
				tc.annotations, corev1.ServicePort{Port: 8443})); err != nil {
				t.Fatal(err)
			}
			s.generateTraefikConfig(extNameIngress("default", "app", "backend",
				networkingv1.ServiceBackendPort{Number: 8443}))

			cfg := readExtNameTraefikFile(t, dir, "default", "app")
			if tc.want == "" {
				if strings.Contains(cfg, "passHostHeader") {
					t.Fatalf("unset annotation rendered passHostHeader:\n%s", cfg)
				}
				return
			}
			if !strings.Contains(cfg, tc.want) {
				t.Fatalf("missing %q in:\n%s", tc.want, cfg)
			}
			if !strings.Contains(cfg, `- url: "http://backend.example.com:8443"`) {
				t.Fatalf("backend URL missing in:\n%s", cfg)
			}
		})
	}
}

// TestSelectorServicePassHostHeader: a selector-based backend renders the
// annotation too, in the same generated loadBalancer block.
func TestSelectorServicePassHostHeader(t *testing.T) {
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
			annServicePassHostHeader: "false",
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
	for _, want := range []string{
		`- url: "http://web-0.default:8080"`,
		"passHostHeader: false",
	} {
		if !strings.Contains(cfg, want) {
			t.Fatalf("missing %q in:\n%s", want, cfg)
		}
	}
}

// TestServicePassHostHeaderInvalidStoredValue: a value written directly to
// the store, bypassing API validation, is not spliced into the YAML.
func TestServicePassHostHeaderInvalidStoredValue(t *testing.T) {
	st := store.New()
	dir := t.TempDir()
	s := &Server{config: Config{Store: st, TraefikDir: dir}}
	created, err := st.CreateService(extNameService("default", "backend", "backend.example.com",
		map[string]string{annServicePassHostHeader: "false"}, corev1.ServicePort{Port: 8443}))
	if err != nil {
		t.Fatal(err)
	}
	stored, err := st.GetService("default", created.Name)
	if err != nil {
		t.Fatal(err)
	}
	stored.Annotations[annServicePassHostHeader] = `false" on: true`
	if _, err := st.UpdateService(stored); err != nil {
		t.Fatal(err)
	}
	s.generateTraefikConfig(extNameIngress("default", "app", "backend",
		networkingv1.ServiceBackendPort{Number: 8443}))

	cfg := readExtNameTraefikFile(t, dir, "default", "app")
	if strings.Contains(cfg, "passHostHeader") || strings.Contains(cfg, "on: true") {
		t.Fatalf("invalid stored value reached the YAML:\n%s", cfg)
	}
}

// TestServicePassHostHeaderValidation: the API accepts only the exact strings
// true and false, on create and on patch, and rejects the rest with 422
// before storing anything.
func TestServicePassHostHeaderValidation(t *testing.T) {
	st := store.New()
	s := &Server{config: Config{Store: st, TraefikDir: t.TempDir()}}

	serviceBody := func(name string, annotations map[string]interface{}) map[string]interface{} {
		return map[string]interface{}{
			"apiVersion": "v1", "kind": "Service",
			"metadata": map[string]interface{}{
				"name": name, "namespace": "default", "annotations": annotations,
			},
			"spec": map[string]interface{}{
				"type": "ExternalName", "externalName": "backend.example.com",
				"ports": []interface{}{map[string]interface{}{"port": 8443}},
			},
		}
	}

	for i, value := range []string{"True", "FALSE", "yes", "1", ""} {
		name := fmt.Sprintf("invalid-%d", i)
		rec := doExtNameServiceRequest(t, s, http.MethodPost, "default", "",
			serviceBody(name, map[string]interface{}{annServicePassHostHeader: value}))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("POST %q status = %d, want 422: %s", value, rec.Code, rec.Body.String())
		}
		if _, err := st.GetService("default", name); err == nil {
			t.Fatalf("Service with passhostheader %q was stored", value)
		}
	}

	rec := doExtNameServiceRequest(t, s, http.MethodPost, "default", "",
		serviceBody("valid", map[string]interface{}{annServicePassHostHeader: "false"}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("valid POST status = %d, want 201: %s", rec.Code, rec.Body.String())
	}

	rec = doExtNameServiceRequest(t, s, http.MethodPatch, "default", "valid", map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]interface{}{annServicePassHostHeader: "off"},
		},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("PATCH status = %d, want 422: %s", rec.Code, rec.Body.String())
	}
	got, err := st.GetService("default", "valid")
	if err != nil {
		t.Fatal(err)
	}
	if got.Annotations[annServicePassHostHeader] != "false" {
		t.Fatalf("rejected patch changed the stored annotation to %q", got.Annotations[annServicePassHostHeader])
	}
}

// TestServicePassHostHeaderUpdateRerendersIngress: changing the annotation
// through the Service PATCH handler rewrites the Ingress file that references
// the backend.
func TestServicePassHostHeaderUpdateRerendersIngress(t *testing.T) {
	st := store.New()
	dir := t.TempDir()
	s := &Server{config: Config{Store: st, TraefikDir: dir}}
	if _, err := st.CreateService(extNameService("default", "backend", "backend.example.com", nil,
		corev1.ServicePort{Port: 8443})); err != nil {
		t.Fatal(err)
	}
	ing := extNameIngress("default", "app", "backend",
		networkingv1.ServiceBackendPort{Number: 8443})
	if _, err := st.CreateIngress(ing); err != nil {
		t.Fatal(err)
	}
	s.generateTraefikConfig(ing)
	if cfg := readExtNameTraefikFile(t, dir, "default", "app"); strings.Contains(cfg, "passHostHeader") {
		t.Fatalf("initial render already has passHostHeader:\n%s", cfg)
	}

	rec := doExtNameServiceRequest(t, s, http.MethodPatch, "default", "backend", map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]interface{}{annServicePassHostHeader: "false"},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	cfg := readExtNameTraefikFile(t, dir, "default", "app")
	if !strings.Contains(cfg, "passHostHeader: false") {
		t.Fatalf("Service update did not render passHostHeader:\n%s", cfg)
	}

	rec = doExtNameServiceRequest(t, s, http.MethodPatch, "default", "backend", map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]interface{}{annServicePassHostHeader: "true"},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	cfg = readExtNameTraefikFile(t, dir, "default", "app")
	if !strings.Contains(cfg, "passHostHeader: true") {
		t.Fatalf("Service update did not change passHostHeader:\n%s", cfg)
	}
	if strings.Contains(cfg, "passHostHeader: false") {
		t.Fatalf("stale passHostHeader left in the Ingress file:\n%s", cfg)
	}
}
