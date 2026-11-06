package server

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"q8s/internal/store"
)

func webService(ns string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "web"},
			Ports: []corev1.ServicePort{{
				Port:       80,
				TargetPort: intstr.FromInt(8080),
			}},
		},
	}
}

// TestIngressBackendServersDNSForDeployment: a Deployment instance has no
// host publish, so its backend is its cluster DNS name on the container
// port; nothing invents a hostPort for it.
func TestIngressBackendServersDNSForDeployment(t *testing.T) {
	st := store.New()
	s := &Server{config: Config{Store: st}}

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "web"}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:  "web",
					Image: "nginx",
					Ports: []corev1.ContainerPort{{ContainerPort: 8080}},
				}}},
			},
		},
	}
	pod := s.materializeDeploymentInstance(dep, 0)
	if _, err := st.CreatePod(pod); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateService(webService("default")); err != nil {
		t.Fatal(err)
	}
	if cp := pod.Spec.Containers[0].Ports[0]; cp.HostPort != 0 || cp.HostIP != "" {
		t.Fatalf("materialized instance invented a host publish: %+v", cp)
	}

	got := s.ingressBackendServers("default", "web", 80, "")
	want := []string{"http://web-0.default:8080"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("servers = %v, want %v", got, want)
	}

	// The store pod (not just the materialized one) must keep hostPort 0 so
	// the ingress resolver sees the same thing on later passes.
	stored, err := st.GetPod("default", "web-0")
	if err != nil {
		t.Fatal(err)
	}
	if cp := stored.Spec.Containers[0].Ports[0]; cp.HostPort != 0 {
		t.Fatalf("stored instance has invented hostPort %d", cp.HostPort)
	}
}

// TestIngressBackendServersNonHostNetworkIgnoresHostPort: a pod on the
// namespace networks is dialed by its cluster DNS name on the container
// port even when it declares a hostPort. Only a hostNetwork pod is reached
// through the host (tic-090e).
func TestIngressBackendServersNonHostNetworkIgnoresHostPort(t *testing.T) {
	st := store.New()
	s := &Server{config: Config{Store: st}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pub", Namespace: "default", Labels: map[string]string{"app": "web"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:  "web",
			Image: "nginx",
			Ports: []corev1.ContainerPort{{ContainerPort: 8080, HostPort: 20001, HostIP: "127.25.0.1"}},
		}}},
	}
	if _, err := st.CreatePod(pod); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateService(webService("default")); err != nil {
		t.Fatal(err)
	}
	got := s.ingressBackendServers("default", "web", 80, "")
	want := []string{"http://pub.default:8080"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("servers = %v, want %v", got, want)
	}
}

// TestIngressBackendServersHostNetworkWithoutHostPort: a hostNetwork pod
// with no publish has no addressable backend.
func TestIngressBackendServersHostNetworkWithoutHostPort(t *testing.T) {
	st := store.New()
	s := &Server{config: Config{Store: st}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "host", Namespace: "default", Labels: map[string]string{"app": "web"}},
		Spec: corev1.PodSpec{
			HostNetwork: true,
			Containers: []corev1.Container{{
				Name:  "web",
				Image: "nginx",
				Ports: []corev1.ContainerPort{{ContainerPort: 8080}},
			}},
		},
	}
	if _, err := st.CreatePod(pod); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateService(webService("default")); err != nil {
		t.Fatal(err)
	}
	if got := s.ingressBackendServers("default", "web", 80, ""); len(got) != 0 {
		t.Fatalf("hostNetwork pod without hostPort yielded %v", got)
	}
}

// TestReconcileIngressConfigsRewritesStaleFiles: after an upgrade an existing
// Ingress file may carry backends rendered by an older q8s. Startup must
// rewrite it from the store, and must leave an already-current file alone
// (Traefik reloads on every write).
func TestReconcileIngressConfigsRewritesStaleFiles(t *testing.T) {
	st := store.New()
	dir := t.TempDir()
	s := &Server{config: Config{Store: st, TraefikDir: dir}}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "default", Labels: map[string]string{"app": "web"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "web", Image: "nginx",
			Ports: []corev1.ContainerPort{{ContainerPort: 8080, HostPort: 8080, HostIP: "127.25.0.1"}},
		}}},
	}
	if _, err := st.CreatePod(pod); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateService(webService("default")); err != nil {
		t.Fatal(err)
	}
	prefix := networkingv1.PathTypePrefix
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{
			Host: "web.example.com",
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
				Paths: []networkingv1.HTTPIngressPath{{
					Path: "/", PathType: &prefix,
					Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
						Name: "web", Port: networkingv1.ServiceBackendPort{Number: 80},
					}},
				}},
			}},
		}}},
	}
	if _, err := st.CreateIngress(ing); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "default-web.yaml")
	stale := "http:\n  services:\n    x:\n      loadBalancer:\n        servers:\n        - url: \"http://127.25.0.1:8080\"\n"
	if err := os.WriteFile(path, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}

	s.ReconcileIngressConfigs()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "http://web-0.default:8080") || strings.Contains(string(got), "127.25.0.1") {
		t.Fatalf("stale Ingress file not regenerated:\n%s", got)
	}

	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	s.ReconcileIngressConfigs()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().Equal(old) {
		t.Fatalf("unchanged Ingress file was rewritten (mtime %v)", fi.ModTime())
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temporary file left behind: %v", err)
	}
}
