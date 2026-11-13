package quadlet_test

import (
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"q8s/internal/quadlet"
)

func privilegedContainer(name, image string) corev1.Container {
	return corev1.Container{
		Name:  name,
		Image: image,
		SecurityContext: &corev1.SecurityContext{
			Privileged: boolPtr(true),
		},
	}
}

func TestContainerPrivileged(t *testing.T) {
	pod := simplePod("default", "app", "myimage")
	pod.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: boolPtr(true)}

	out := mustContainer(t, pod, "")
	assertContains(t, out, "PodmanArgs=--privileged\n")
}

func TestContainerHostPID(t *testing.T) {
	pod := simplePod("default", "app", "myimage")
	pod.Spec.HostPID = true

	out := mustContainer(t, pod, "")
	assertContains(t, out, "PodmanArgs=--pid=host\n")
	assertNotContains(t, out, "PodmanArgs=--ipc=host")
}

func TestContainerHostIPC(t *testing.T) {
	pod := simplePod("default", "app", "myimage")
	pod.Spec.HostIPC = true

	out := mustContainer(t, pod, "")
	assertContains(t, out, "PodmanArgs=--ipc=host\n")
	assertNotContains(t, out, "PodmanArgs=--pid=host")
}

func TestContainerStdinTTY(t *testing.T) {
	pod := simplePod("default", "app", "myimage")
	pod.Spec.Containers[0].Stdin = true
	pod.Spec.Containers[0].TTY = true

	out := mustContainer(t, pod, "")
	assertContains(t, out, "PodmanArgs=--interactive\n")
	assertContains(t, out, "PodmanArgs=--tty\n")
}

func TestContainerStdinWithoutTTY(t *testing.T) {
	pod := simplePod("default", "app", "myimage")
	pod.Spec.Containers[0].Stdin = true

	out := mustContainer(t, pod, "")
	assertContains(t, out, "PodmanArgs=--interactive\n")
	assertNotContains(t, out, "PodmanArgs=--tty")
}

func TestPodUnitsHostNamespaceAndPrivilegedFlags(t *testing.T) {
	pod := multiPod()
	pod.Spec.HostPID = true
	pod.Spec.HostIPC = true
	pod.Spec.Containers[1].SecurityContext = &corev1.SecurityContext{Privileged: boolPtr(true)}
	pod.Spec.Containers[1].Stdin = true
	pod.Spec.Containers[1].TTY = true

	files, err := quadlet.PodUnits(pod.Name, pod, "", nil, nil, "", nil)
	if err != nil {
		t.Fatalf("PodUnits: %v", err)
	}

	podOut := string(files["default-multi.pod"])
	assertNotContains(t, podOut, "PodmanArgs=")

	for _, name := range []string{"default-multi-web.container", "default-multi-sidecar.container"} {
		out := string(files[name])
		// hostPID and hostIPC are pod-wide, so every member carries them.
		assertContains(t, out, "PodmanArgs=--pid=host\n")
		assertContains(t, out, "PodmanArgs=--ipc=host\n")
	}

	webOut := string(files["default-multi-web.container"])
	assertNotContains(t, webOut, "PodmanArgs=--privileged")
	assertNotContains(t, webOut, "PodmanArgs=--interactive")
	assertNotContains(t, webOut, "PodmanArgs=--tty")

	sidecarOut := string(files["default-multi-sidecar.container"])
	assertContains(t, sidecarOut, "PodmanArgs=--privileged\n")
	assertContains(t, sidecarOut, "PodmanArgs=--interactive\n")
	assertContains(t, sidecarOut, "PodmanArgs=--tty\n")
}

func TestJobContainerRuntimeFlags(t *testing.T) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "default"},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				HostPID: true,
				HostIPC: true,
				Containers: []corev1.Container{{
					Name:            "c",
					Image:           "myimage",
					Stdin:           true,
					TTY:             true,
					SecurityContext: &corev1.SecurityContext{Privileged: boolPtr(true)},
				}},
			},
		}},
	}
	b, err := quadlet.JobContainer("j", job, "", nil, "")
	if err != nil {
		t.Fatalf("JobContainer: %v", err)
	}
	out := string(b)
	assertContains(t, out, "PodmanArgs=--privileged\n")
	assertContains(t, out, "PodmanArgs=--pid=host\n")
	assertContains(t, out, "PodmanArgs=--ipc=host\n")
	assertContains(t, out, "PodmanArgs=--interactive\n")
	assertContains(t, out, "PodmanArgs=--tty\n")
}

func TestCronContainerRuntimeFlags(t *testing.T) {
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default"},
		Spec: batchv1.CronJobSpec{
			Schedule: "* * * * *",
			JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{
				Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					HostPID: true,
					HostIPC: true,
					Containers: []corev1.Container{{
						Name:            "c",
						Image:           "myimage",
						Stdin:           true,
						TTY:             true,
						SecurityContext: &corev1.SecurityContext{Privileged: boolPtr(true)},
					}},
				}},
			}},
		},
	}
	b, err := quadlet.CronContainer("c", cj, "", nil, "")
	if err != nil {
		t.Fatalf("CronContainer: %v", err)
	}
	out := string(b)
	assertContains(t, out, "PodmanArgs=--privileged\n")
	assertContains(t, out, "PodmanArgs=--pid=host\n")
	assertContains(t, out, "PodmanArgs=--ipc=host\n")
	assertContains(t, out, "PodmanArgs=--interactive\n")
	assertContains(t, out, "PodmanArgs=--tty\n")
}

func TestContainerWithoutRuntimeFlags(t *testing.T) {
	pod := simplePod("default", "app", "myimage")
	out := mustContainer(t, pod, "")
	assertNotContains(t, out, "PodmanArgs=--privileged")
	assertNotContains(t, out, "PodmanArgs=--pid=host")
	assertNotContains(t, out, "PodmanArgs=--ipc=host")
	assertNotContains(t, out, "PodmanArgs=--interactive")
	assertNotContains(t, out, "PodmanArgs=--tty")
}

func TestPodUsesUserNSHostNamespacesAndPrivileged(t *testing.T) {
	restore := quadlet.SetUserNS(true)
	defer restore()

	plain := simplePod("default", "app", "myimage")
	plain.Spec.Volumes = []corev1.Volume{{
		Name:         "scratch",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}}
	plain.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "scratch", MountPath: "/scratch"}}

	assertContains(t, mustContainer(t, plain, ""), "UserNS=auto:size=65536\n")

	for _, tc := range []struct {
		name   string
		modify func(*corev1.Pod)
	}{
		{
			name:   "hostPID",
			modify: func(p *corev1.Pod) { p.Spec.HostPID = true },
		},
		{
			name:   "hostIPC",
			modify: func(p *corev1.Pod) { p.Spec.HostIPC = true },
		},
		{
			name: "privileged container",
			modify: func(p *corev1.Pod) {
				p.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: boolPtr(true)}
			},
		},
		{
			name: "privileged init container",
			modify: func(p *corev1.Pod) {
				p.Spec.InitContainers = []corev1.Container{privilegedContainer("init", "myimage")}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := plain.DeepCopy()
			tc.modify(pod)
			out := mustContainer(t, pod, "")
			assertNotContains(t, out, "UserNS")
			assertNotContains(t, out, "idmap")
		})
	}
}
