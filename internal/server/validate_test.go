package server

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestValidateSingleContainer(t *testing.T) {
	container := corev1.Container{Name: "app", Image: "nginx"}

	tests := []struct {
		name    string
		spec    *corev1.PodSpec
		allow   bool
		wantErr string
	}{
		{
			name:  "one container",
			spec:  &corev1.PodSpec{Containers: []corev1.Container{container}},
			allow: true,
		},
		{
			name:    "no containers",
			spec:    &corev1.PodSpec{},
			allow:   true,
			wantErr: "pod spec has no containers",
		},
		{
			name:    "two containers in a Job",
			spec:    &corev1.PodSpec{Containers: []corev1.Container{container, {Name: "sidecar", Image: "busybox"}}},
			allow:   false,
			wantErr: "pod spec has 2 containers: sidecars in Jobs/CronJobs are not supported yet",
		},
		{
			name:  "two containers in a Pod or Deployment",
			spec:  &corev1.PodSpec{Containers: []corev1.Container{container, {Name: "sidecar", Image: "busybox"}}},
			allow: true,
		},
		{
			name: "one container plus init container",
			spec: &corev1.PodSpec{
				Containers:     []corev1.Container{container},
				InitContainers: []corev1.Container{{Name: "init", Image: "busybox"}},
			},
			allow:   true,
			wantErr: "pod spec has 1 initContainers: initContainers are not supported",
		},
		{
			name: "one container plus ephemeral container",
			spec: &corev1.PodSpec{
				Containers:          []corev1.Container{container},
				EphemeralContainers: []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debug", Image: "busybox"}}},
			},
			allow:   true,
			wantErr: "pod spec has 1 ephemeralContainers: ephemeralContainers are not supported",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSingleContainer("pod spec", tt.spec, tt.allow)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("expected nil error, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error %q, got nil", tt.wantErr)
			}
			if err.Error() != tt.wantErr {
				t.Fatalf("expected error %q, got %q", tt.wantErr, err.Error())
			}
		})
	}
}
