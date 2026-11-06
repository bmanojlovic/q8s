package server

import (
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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
