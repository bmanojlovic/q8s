package server

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"q8s/internal/store"
)

func nodePortPublishService(name string, nodePort int32, app string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeNodePort,
			Selector: map[string]string{"app": app},
			Ports: []corev1.ServicePort{{
				Port:       80,
				TargetPort: intstr.FromInt(80),
				NodePort:   nodePort,
			}},
		},
	}
}

func nodePortPublishPod(app string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pod-" + app, Namespace: "default",
			Labels: map[string]string{"app": app},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:  "c",
			Image: "demo-app",
			Ports: []corev1.ContainerPort{{ContainerPort: 80}},
		}}},
	}
}

// A stored nodePort that is out of range or claimed by a different Service
// must not turn into a published host port, even when the object reached the
// store through a path that skipped the normal checks.
func TestNodePortPublishSkipsInvalidClaims(t *testing.T) {
	srv, _, st := hostPortTestServer(t)

	const validPort = store.NodePortMin + 630
	const duplicatePort = store.NodePortMin + 631
	for _, svc := range []*corev1.Service{
		nodePortPublishService("valid", validPort, "valid"),
		nodePortPublishService("dup-a", duplicatePort, "dup-a"),
		nodePortPublishService("dup-b", duplicatePort, "dup-b"),
		nodePortPublishService("out-of-range", 8080, "out-of-range"),
	} {
		if _, err := st.CreateService(svc); err != nil {
			t.Fatalf("CreateService %s: %v", svc.Name, err)
		}
	}

	for _, tc := range []struct {
		app  string
		want int32
	}{
		{"valid", validPort},
		{"dup-a", 0},
		{"dup-b", 0},
		{"out-of-range", 0},
	} {
		pod := srv.applyNodePorts(nodePortPublishPod(tc.app))
		got := pod.Spec.Containers[0].Ports[0].HostPort
		if got != tc.want {
			t.Fatalf("pod selected by Service for app %s published hostPort %d, want %d",
				tc.app, got, tc.want)
		}
	}
}
