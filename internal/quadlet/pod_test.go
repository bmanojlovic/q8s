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
	files, err := quadlet.PodUnits(pod.Name, pod, "/run/q8s/configmaps", []string{"myservice"}, nil, "", nil)
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
		`HealthCmd=["CMD","test","-f","/tmp/ready"]`,
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
	files, err := quadlet.PodUnits(pod.Name, pod, "", []string{"myservice"}, nil, "", nil)
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
	if _, err := quadlet.PodUnits(pod.Name, pod, "", nil, nil, "", nil); err == nil {
		t.Fatal("expected error for invalid container name")
	}
}

func TestPodUnitsRejectsSocketActivation(t *testing.T) {
	pod := multiPod()
	pod.Annotations = map[string]string{quadlet.AnnotationSocketActivation: "web=80"}
	if _, err := quadlet.PodUnits(pod.Name, pod, "", nil, nil, "", nil); err == nil {
		t.Fatal("expected error for socket activation on a multi-container pod")
	}
}

func TestPodUnitsNetworkAttachments(t *testing.T) {
	pod := multiPod()
	files, err := quadlet.PodUnits(pod.Name, pod, "", nil, nil, "", []quadlet.NetworkAttachment{
		{Namespace: "other"},
		{Namespace: "default", IP: "10.89.3.254"},
	})
	if err != nil {
		t.Fatalf("PodUnits: %v", err)
	}
	podOut := string(files["default-multi.pod"])
	want := "Network=q8s-default.network:ip=10.89.3.254\nNetwork=q8s-other.network\n"
	if !strings.Contains(podOut, want) {
		t.Errorf("pod networks missing:\nwant:\n%s\ngot:\n%s", want, podOut)
	}
	assertContains(t, podOut, "NetworkAlias=multi.default")
}

func keys(files map[string][]byte) string {
	var out []string
	for name := range files {
		out = append(out, name)
	}
	return strings.Join(out, ", ")
}

// deploymentOwnedPod is a single-container pod owned by a Deployment, the
// only shape that gets Annotation= lines.
func deploymentOwnedPod(annotations map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "app-0",
			Namespace:   "default",
			Annotations: annotations,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       "app",
			}},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "nginx:latest"}},
		},
	}
}

func TestDeploymentTemplateAnnotations(t *testing.T) {
	t.Run("sorted and quoted", func(t *testing.T) {
		pod := deploymentOwnedPod(map[string]string{
			"b":               "2",
			"checksum/config": "abc",
			"a":               `x y %z "q"`,
		})
		out, err := quadlet.Container("app-0", pod, "", nil, nil, "", nil)
		if err != nil {
			t.Fatalf("Container: %v", err)
		}
		got := string(out)
		wantLines := []string{
			`Annotation="a=x y %%z \"q\""` + "\n",
			"Annotation=b=2\n",
			"Annotation=checksum/config=abc\n",
		}
		for _, want := range wantLines {
			assertContains(t, got, want)
		}
		// Sorted by key, whatever the map iteration order.
		if i, j, k := strings.Index(got, wantLines[0]), strings.Index(got, wantLines[1]), strings.Index(got, wantLines[2]); i < 0 || i > j || j > k {
			t.Fatalf("Annotation= lines are not sorted by key:\n%s", got)
		}
	})

	t.Run("insertion order does not matter", func(t *testing.T) {
		first := deploymentOwnedPod(map[string]string{})
		first.Annotations["b"] = "2"
		first.Annotations["checksum/config"] = "abc"
		first.Annotations["a"] = `x y %z "q"`
		second := deploymentOwnedPod(map[string]string{})
		second.Annotations["a"] = `x y %z "q"`
		second.Annotations["checksum/config"] = "abc"
		second.Annotations["b"] = "2"

		firstOut, err := quadlet.Container("app-0", first, "", nil, nil, "", nil)
		if err != nil {
			t.Fatalf("Container first: %v", err)
		}
		secondOut, err := quadlet.Container("app-0", second, "", nil, nil, "", nil)
		if err != nil {
			t.Fatalf("Container second: %v", err)
		}
		if string(firstOut) != string(secondOut) {
			t.Fatalf("same annotations rendered differently:\n--- first ---\n%s\n--- second ---\n%s", firstOut, secondOut)
		}
	})

	t.Run("last-applied-configuration skipped", func(t *testing.T) {
		pod := deploymentOwnedPod(map[string]string{
			"kubectl.kubernetes.io/last-applied-configuration": `{"huge":"json"}`,
			"keep": "v",
		})
		out, err := quadlet.Container("app-0", pod, "", nil, nil, "", nil)
		if err != nil {
			t.Fatalf("Container: %v", err)
		}
		assertNotContains(t, string(out), "Annotation=kubectl.kubernetes.io/last-applied-configuration")
		assertContains(t, string(out), "Annotation=keep=v")
	})

	t.Run("control characters are escaped", func(t *testing.T) {
		pod := deploymentOwnedPod(map[string]string{
			"multi": "line1\nline2\ttab",
			"other": "a\x01b\x7fc\rd",
		})
		out, err := quadlet.Container("app-0", pod, "", nil, nil, "", nil)
		if err != nil {
			t.Fatalf("Container: %v", err)
		}
		got := string(out)
		assertContains(t, got, `Annotation="multi=line1\nline2\ttab"`+"\n")
		assertContains(t, got, `Annotation="other=a\x01b\x7fc\rd"`+"\n")
		if strings.Contains(got, "line1\nline2") {
			t.Errorf("raw newline from the annotation value reached the unit file:\n%s", got)
		}
		for _, line := range strings.Split(got, "\n") {
			if strings.HasPrefix(line, "Annotation=") && strings.ContainsAny(line, "\t\r") {
				t.Errorf("raw control character in annotation line %q", line)
			}
		}
	})

	t.Run("no annotations keeps today's output", func(t *testing.T) {
		out, err := quadlet.Container("app-0", deploymentOwnedPod(nil), "", nil, nil, "", nil)
		if err != nil {
			t.Fatalf("Container: %v", err)
		}
		assertNotContains(t, string(out), "Annotation=")
	})

	t.Run("standalone pod ignores annotations", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "plain",
				Namespace:   "default",
				Annotations: map[string]string{"a": "b"},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "c", Image: "nginx:latest"}},
			},
		}
		out, err := quadlet.Container("plain", pod, "", nil, nil, "", nil)
		if err != nil {
			t.Fatalf("Container: %v", err)
		}
		assertNotContains(t, string(out), "Annotation=")
	})

	t.Run("multi-container writes every container only", func(t *testing.T) {
		pod := multiPod()
		pod.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: "multi"}}
		pod.Annotations = map[string]string{"b": "2", "a": "1"}
		files, err := quadlet.PodUnits(pod.Name, pod, "", nil, nil, "", nil)
		if err != nil {
			t.Fatalf("PodUnits: %v", err)
		}
		for name, content := range files {
			if strings.HasSuffix(name, ".container") {
				assertContains(t, string(content), "Annotation=a=1\n")
				assertContains(t, string(content), "Annotation=b=2\n")
				continue
			}
			assertNotContains(t, string(content), "Annotation=")
		}
	})
}

func TestDeploymentTemplateAnnotationInvalidKey(t *testing.T) {
	for _, key := range []string{"bad key", "a=b", "-x"} {
		t.Run(key, func(t *testing.T) {
			pod := deploymentOwnedPod(map[string]string{key: "v"})
			_, err := quadlet.Container("app-0", pod, "", nil, nil, "", nil)
			if err == nil {
				t.Fatalf("expected an error for annotation key %q", key)
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("error %q should name the key %q", err, key)
			}
		})
	}
}

func TestPodUnitsOwnershipAndHashLabels(t *testing.T) {
	pod := multiPod()
	files, err := quadlet.PodUnits(pod.Name, pod, "", nil, nil, "", nil)
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
	again, err := quadlet.PodUnits(pod.Name, pod.DeepCopy(), "", nil, nil, "", nil)
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
