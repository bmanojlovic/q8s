package quadlet_test

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"q8s/internal/quadlet"
)

func multiPod() *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "multi",
			Namespace: "default",
			Labels:    map[string]string{"app": "multi"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "web",
					Image: "nginx:latest",
					Env:   []corev1.EnvVar{{Name: "ROLE", Value: "web"}},
					Ports: []corev1.ContainerPort{{ContainerPort: 80, HostPort: 8080, Protocol: corev1.ProtocolTCP}},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "cfg", MountPath: "/etc/config"},
						{Name: "shm", MountPath: "/dev/shm"},
					},
				},
				{
					Name:  "sidecar",
					Image: "busybox",
					Env:   []corev1.EnvVar{{Name: "ROLE", Value: "sidecar"}},
					ReadinessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							Exec: &corev1.ExecAction{Command: []string{"test", "-f", "/tmp/ready"}},
						},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "cfg",
					VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "app-config"}},
					},
				},
				{
					Name: "shm",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{
							Medium:    corev1.StorageMediumMemory,
							SizeLimit: quantityPtr("64Mi"),
						},
					},
				},
			},
		},
	}
	return pod
}

func TestPodUnitsTwoContainers(t *testing.T) {
	pod := multiPod()
	files, err := quadlet.PodUnits(pod.Name, pod, "/run/q8s/configmaps", []string{"myservice"}, nil, "")
	if err != nil {
		t.Fatalf("PodUnits: %v", err)
	}
	if len(files) != 3 {
		t.Fatalf("expected 3 files (.pod plus 2 .container), got %d: %v", len(files), keys(files))
	}

	podOut := string(files["default-multi.pod"])
	for _, want := range []string{
		"[Pod]",
		"PodName=default-multi",
		"Network=q8s-default.network",
		"NetworkAlias=myservice",
		"PublishPort=8080:80/tcp",
		"ShmSize=67108864",
		"Label=io.kubernetes.pod.name=multi",
		"Label=io.kubernetes.pod.namespace=default",
		"Label=app=multi",
	} {
		assertContains(t, podOut, want)
	}
	assertNotContains(t, podOut, "Image=")
	assertNotContains(t, podOut, "io.kubernetes.container.name")

	webOut := string(files["default-multi-web.container"])
	for _, want := range []string{
		"Image=nginx:latest",
		"ContainerName=default-multi-web",
		"Pod=default-multi.pod",
		"Environment=ROLE=web",
		"Volume=/run/q8s/configmaps/default/app-config:/etc/config:ro,z",
		"Label=io.kubernetes.container.name=web",
		"Label=app=multi",
		"[Service]",
	} {
		assertContains(t, webOut, want)
	}
	for _, unwanted := range []string{"Network=", "NetworkAlias=", "PublishPort=", "ShmSize=", "HealthCmd="} {
		assertNotContains(t, webOut, unwanted)
	}
	// The /dev/shm emptyDir is a pod-level ShmSize, never a Volume= line.
	assertNotContains(t, webOut, "shm")

	sidecarOut := string(files["default-multi-sidecar.container"])
	for _, want := range []string{
		"Image=busybox",
		"ContainerName=default-multi-sidecar",
		"Pod=default-multi.pod",
		"Environment=ROLE=sidecar",
		"HealthCmd=test -f /tmp/ready",
		"Label=io.kubernetes.container.name=sidecar",
	} {
		assertContains(t, sidecarOut, want)
	}
	// Mounts belong to the container that declared them.
	assertNotContains(t, sidecarOut, "Volume=")
}

func TestPodUnitsHostNetwork(t *testing.T) {
	pod := multiPod()
	pod.Spec.HostNetwork = true
	files, err := quadlet.PodUnits(pod.Name, pod, "", []string{"myservice"}, nil, "")
	if err != nil {
		t.Fatalf("PodUnits: %v", err)
	}
	podOut := string(files["default-multi.pod"])
	assertContains(t, podOut, "Network=host")
	assertNotContains(t, podOut, "Network=q8s-default.network")
	assertNotContains(t, podOut, "NetworkAlias=")
}

func TestPodUnitsRejectsInvalidContainerName(t *testing.T) {
	pod := multiPod()
	pod.Spec.Containers[1].Name = "bad name"
	if _, err := quadlet.PodUnits(pod.Name, pod, "", nil, nil, ""); err == nil {
		t.Fatal("expected error for invalid container name")
	}
}

func keys(files map[string][]byte) string {
	var out []string
	for name := range files {
		out = append(out, name)
	}
	return strings.Join(out, ", ")
}

func TestPodUnitsOwnershipAndHashLabels(t *testing.T) {
	pod := multiPod()
	files, err := quadlet.PodUnits(pod.Name, pod, "", nil, nil, "")
	if err != nil {
		t.Fatalf("PodUnits: %v", err)
	}

	podOut := string(files["default-multi.pod"])
	assertContains(t, podOut, quadlet.ManagedByLabel)
	assertContains(t, podOut, "Label=q8s.io/containers=web,sidecar")
	hash := labelValue(podOut, quadlet.TemplateHashLabel)
	if len(hash) != 12 {
		t.Fatalf("pod template hash = %q, want 12 hex chars", hash)
	}

	// Equal input renders the same pod hash.
	again, err := quadlet.PodUnits(pod.Name, pod.DeepCopy(), "", nil, nil, "")
	if err != nil {
		t.Fatalf("PodUnits second render: %v", err)
	}
	assertContains(t, string(again["default-multi.pod"]), "Label="+quadlet.TemplateHashLabel+"="+hash)

	for name, content := range files {
		if !strings.HasSuffix(name, ".container") {
			continue
		}
		assertContains(t, string(content), quadlet.ManagedByLabel)
		if got := labelValue(string(content), quadlet.TemplateHashLabel); len(got) != 12 {
			t.Errorf("%s template hash = %q, want 12 hex chars", name, got)
		}
	}
}
