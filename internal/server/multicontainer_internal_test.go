package server

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"q8s/internal/store"
)

func TestPodContainerName(t *testing.T) {
	st := store.New()
	srv := &Server{config: Config{Store: st}}

	if _, err := st.CreatePod(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "multi", Namespace: "default"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "web", Image: "nginx"},
			{Name: "sidecar", Image: "busybox"},
		}},
	}); err != nil {
		t.Fatalf("CreatePod multi: %v", err)
	}
	if _, err := st.CreatePod(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "single", Namespace: "default"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
	}); err != nil {
		t.Fatalf("CreatePod single: %v", err)
	}

	cases := []struct {
		name      string
		pod       string
		requested string
		want      string
	}{
		{"multi default container", "multi", "", "default-multi-web"},
		{"multi explicit container", "multi", "sidecar", "default-multi-sidecar"},
		{"single unchanged", "single", "", "default-single"},
		{"single explicit container unchanged", "single", "app", "default-single"},
		{"unknown pod falls back", "missing", "", "default-missing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := srv.podContainerName("default", tc.pod, tc.requested); got != tc.want {
				t.Fatalf("podContainerName(%q, %q) = %q, want %q", tc.pod, tc.requested, got, tc.want)
			}
		})
	}
}
