package server

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"q8s/internal/store"
)

func TestDeploymentInstancePodKeepsUIDAcrossTemplateSync(t *testing.T) {
	st := store.New()
	s := &Server{config: Config{Store: st}}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "web", Image: "nginx:1"}},
				},
			},
		},
	}

	first := s.materializeDeploymentInstance(dep, 0)
	if first.UID != "" {
		t.Fatalf("new instance materialized with UID %q", first.UID)
	}
	created, err := st.CreatePod(first)
	if err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	if created.UID == "" {
		t.Fatal("created instance has no UID")
	}

	dep.Spec.Template.Spec.Containers[0].Image = "nginx:2"
	incoming := s.materializeDeploymentInstance(dep, 0)
	if incoming.UID != created.UID {
		t.Fatalf("materialized instance UID = %q, want the stored %q", incoming.UID, created.UID)
	}
	s.syncInstancePod(incoming)

	stored, err := st.GetPod("default", "web-0")
	if err != nil {
		t.Fatalf("GetPod: %v", err)
	}
	if stored.UID != created.UID {
		t.Fatalf("instance pod UID changed from %q to %q on template sync", created.UID, stored.UID)
	}
	if got := stored.Spec.Containers[0].Image; got != "nginx:2" {
		t.Fatalf("instance pod image = %q, want nginx:2", got)
	}
}
