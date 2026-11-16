package quadlet

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func hostAccessBoolPtr(v bool) *bool { return &v }

func TestHostAccessReasons(t *testing.T) {
	base := corev1.PodSpec{
		Containers: []corev1.Container{{Name: "main", Image: "nginx:1.25"}},
	}
	if got := HostAccessReasons(base, nil); len(got) != 0 {
		t.Fatalf("reasons = %v, want none", got)
	}

	cases := []struct {
		name        string
		spec        func(corev1.PodSpec) corev1.PodSpec
		annotations map[string]string
		want        string
	}{
		{
			name: "privileged container",
			spec: func(spec corev1.PodSpec) corev1.PodSpec {
				spec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: hostAccessBoolPtr(true)}
				return spec
			},
			want: "spec.containers[0].securityContext.privileged",
		},
		{
			name: "privileged init container",
			spec: func(spec corev1.PodSpec) corev1.PodSpec {
				spec.InitContainers = []corev1.Container{{
					Name:            "init",
					Image:           "busybox",
					SecurityContext: &corev1.SecurityContext{Privileged: hostAccessBoolPtr(true)},
				}}
				return spec
			},
			want: "spec.initContainers[0].securityContext.privileged",
		},
		{
			name: "hostPID",
			spec: func(spec corev1.PodSpec) corev1.PodSpec {
				spec.HostPID = true
				return spec
			},
			want: "spec.hostPID",
		},
		{
			name: "hostIPC",
			spec: func(spec corev1.PodSpec) corev1.PodSpec {
				spec.HostIPC = true
				return spec
			},
			want: "spec.hostIPC",
		},
		{
			name: "hostNetwork",
			spec: func(spec corev1.PodSpec) corev1.PodSpec {
				spec.HostNetwork = true
				return spec
			},
			want: "spec.hostNetwork",
		},
		{
			name: "hostUsers true",
			spec: func(spec corev1.PodSpec) corev1.PodSpec {
				spec.HostUsers = hostAccessBoolPtr(true)
				return spec
			},
			want: "spec.hostUsers",
		},
		{
			name:        "attach-all-networks true",
			annotations: map[string]string{AnnotationAttachAllNetworks: "true"},
			want:        "attach-all-networks",
		},
		{
			name:        "attach-all-networks 1",
			annotations: map[string]string{AnnotationAttachAllNetworks: "1"},
			want:        "attach-all-networks",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := *base.DeepCopy()
			if tc.spec != nil {
				spec = tc.spec(spec)
			}
			got := HostAccessReasons(spec, tc.annotations)
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("reasons = %v, want [%s]", got, tc.want)
			}
		})
	}

	if got := HostAccessReasons(base, map[string]string{AnnotationAttachAllNetworks: "false"}); len(got) != 0 {
		t.Fatalf("attach-all-networks false produced reasons %v", got)
	}
	if got := HostAccessReasons(base, map[string]string{AnnotationAttachAllNetworks: "sometimes"}); len(got) != 0 {
		t.Fatalf("invalid attach-all-networks produced reasons %v", got)
	}
}
