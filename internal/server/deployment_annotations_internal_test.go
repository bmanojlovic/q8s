package server

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Pod-template annotations are pod-level in Kubernetes. Deployment instances
// are materialized from the template, so they have to carry them through for
// the quadlet generator to see q8s.io/ulimits and similar annotations.
func TestMaterializeDeploymentInstanceCopiesPodTemplateAnnotations(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "llm", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
					"q8s.io/ulimits": "memlock=-1:-1",
				}},
				Spec: corev1.PodSpec{
					HostNetwork: true,
					Containers:  []corev1.Container{{Name: "llm", Image: "llm:latest"}},
				},
			},
		},
	}

	// Instance 1 avoids the replica-port allocator and node-port publish
	// paths, which need a populated Store; neither touches annotations.
	pod := (&Server{}).materializeDeploymentInstance(dep, 1)

	if got := pod.Annotations["q8s.io/ulimits"]; got != "memlock=-1:-1" {
		t.Errorf("pod annotations = %v, want q8s.io/ulimits copied from the pod template", pod.Annotations)
	}
}
