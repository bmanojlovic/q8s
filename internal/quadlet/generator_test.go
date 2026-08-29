package quadlet_test

import (
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"q8s/internal/quadlet"
)

// --- helpers ---

func mustContainer(t *testing.T, pod *corev1.Pod, configDir string) string {
	t.Helper()
	b, err := quadlet.Container(pod.Name, pod, configDir, nil, nil, "")
	if err != nil {
		t.Fatalf("Container: %v", err)
	}
	return string(b)
}

func assertContains(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Errorf("expected output to contain %q\ngot:\n%s", want, got)
	}
}

func assertNotContains(t *testing.T, got, want string) {
	t.Helper()
	if strings.Contains(got, want) {
		t.Errorf("expected output NOT to contain %q\ngot:\n%s", want, got)
	}
}

func simplePod(ns, name, image string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: name, Image: image}},
		},
	}
}

// --- Container ---

func TestContainerBasic(t *testing.T) {
	pod := simplePod("default", "nginx", "nginx:latest")
	out := mustContainer(t, pod, "")

	assertContains(t, out, "[Container]")
	assertContains(t, out, "Image=nginx:latest")
	assertContains(t, out, "ContainerName=default-nginx")
	assertContains(t, out, "Network=q8s-default.network")
	assertContains(t, out, "Label=io.kubernetes.pod.name=nginx")
	assertContains(t, out, "Label=io.kubernetes.pod.namespace=default")
	assertContains(t, out, "[Unit]")
	assertContains(t, out, "Description=Pod nginx")
	assertContains(t, out, "[Install]")
	assertContains(t, out, "WantedBy=default.target")
	assertNotContains(t, out, "[Service]")
}

func TestContainerEnvVars(t *testing.T) {
	pod := simplePod("default", "app", "myimage:1.0")
	pod.Spec.Containers[0].Env = []corev1.EnvVar{
		{Name: "FOO", Value: "bar"},
		{Name: "DB_HOST", Value: "localhost"},
	}
	out := mustContainer(t, pod, "")

	assertContains(t, out, "Environment=FOO=bar")
	assertContains(t, out, "Environment=DB_HOST=localhost")
}

func TestContainerPorts(t *testing.T) {
	pod := simplePod("default", "web", "nginx")
	pod.Spec.Containers[0].Ports = []corev1.ContainerPort{
		{ContainerPort: 80, Protocol: corev1.ProtocolTCP},
		{ContainerPort: 53, Protocol: corev1.ProtocolUDP, HostPort: 5353},
	}
	out := mustContainer(t, pod, "")

	// containerPort without hostPort is internal-only — no PublishPort
	assertNotContains(t, out, "PublishPort=80")
	// hostPort explicitly set → published
	assertContains(t, out, "PublishPort=5353:53/udp")
}

func TestContainerPortContainerPortOnly(t *testing.T) {
	pod := simplePod("default", "web", "nginx")
	pod.Spec.Containers[0].Ports = []corev1.ContainerPort{
		{ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
	}
	out := mustContainer(t, pod, "")

	assertNotContains(t, out, "PublishPort")
}

func TestContainerPortHostPort(t *testing.T) {
	pod := simplePod("default", "web", "nginx")
	pod.Spec.Containers[0].Ports = []corev1.ContainerPort{
		{ContainerPort: 8080, HostPort: 80, Protocol: corev1.ProtocolTCP},
	}
	out := mustContainer(t, pod, "")

	assertContains(t, out, "PublishPort=80:8080/tcp")
}

func TestContainerExecArgs(t *testing.T) {
	pod := simplePod("default", "app", "myimage")
	pod.Spec.Containers[0].Command = []string{"/bin/sh"}
	pod.Spec.Containers[0].Args = []string{"-c", "echo hello"}
	out := mustContainer(t, pod, "")

	assertContains(t, out, `Exec=/bin/sh -c "echo hello"`)
}

func TestContainerWorkingDir(t *testing.T) {
	pod := simplePod("default", "app", "myimage")
	pod.Spec.Containers[0].WorkingDir = "/app"
	out := mustContainer(t, pod, "")

	assertContains(t, out, "WorkingDir=/app")
}

func TestContainerRunAsUser(t *testing.T) {
	uid := int64(1000)
	pod := simplePod("default", "app", "myimage")
	pod.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{
		RunAsUser: &uid,
	}
	out := mustContainer(t, pod, "")

	assertContains(t, out, "User=1000")
}

func TestContainerPVCVolume(t *testing.T) {
	pod := simplePod("default", "app", "myimage")
	pod.Spec.Volumes = []corev1.Volume{{
		Name:         "data",
		VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "my-pvc"}},
	}}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: "/data"}}
	out := mustContainer(t, pod, "")

	// Without a PVC map the fallback adds :Z (SELinux exclusive relabel).
	assertContains(t, out, "Volume=default-my-pvc.volume:/data:Z")
}

func TestContainerConfigMapVolume(t *testing.T) {
	pod := simplePod("default", "app", "myimage")
	pod.Spec.Volumes = []corev1.Volume{{
		Name:         "cfg",
		VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "my-config"}}},
	}}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "cfg", MountPath: "/etc/config"}}
	out := mustContainer(t, pod, "/run/q8s/configmaps")

	assertContains(t, out, "Volume=/run/q8s/configmaps/default/my-config:/etc/config:ro,z")
}

func TestContainerConfigMapVolumeNoConfigDir(t *testing.T) {
	pod := simplePod("default", "app", "myimage")
	pod.Spec.Volumes = []corev1.Volume{{
		Name:         "cfg",
		VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "my-config"}}},
	}}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "cfg", MountPath: "/etc/config"}}
	out := mustContainer(t, pod, "")

	// No configDir → no Volume line for ConfigMap
	assertNotContains(t, out, "Volume=")
}

func TestContainerHostNetwork(t *testing.T) {
	pod := simplePod("default", "app", "myimage")
	pod.Spec.HostNetwork = true
	out := mustContainer(t, pod, "")

	assertContains(t, out, "Network=host")
	assertNotContains(t, out, "Network=q8s-default.network")
}

func TestContainerPropagatesPodLabels(t *testing.T) {
	pod := simplePod("default", "app", "myimage")
	pod.Labels = map[string]string{"app": "web", "tier": "frontend"}
	out := mustContainer(t, pod, "")

	assertContains(t, out, "Label=app=web")
	assertContains(t, out, "Label=tier=frontend")
}

func TestContainerRejectsInvalidLabelValue(t *testing.T) {
	pod := simplePod("default", "app", "myimage")
	pod.Labels = map[string]string{"app": "web\n\n[Service]\nExecStartPre=/bin/evil"}
	if _, err := quadlet.Container(pod.Name, pod, "", nil, nil, ""); err == nil {
		t.Fatal("expected error for label value containing control characters")
	}
}

func TestContainerNetworkAlias(t *testing.T) {
	pod := simplePod("default", "app", "myimage")
	out, err := quadlet.Container(pod.Name, pod, "", []string{"myservice", "other-svc"}, nil, "")
	if err != nil {
		t.Fatalf("Container: %v", err)
	}
	assertContains(t, string(out), "NetworkAlias=myservice")
	assertContains(t, string(out), "NetworkAlias=other-svc")
}

func TestContainerNoNetworkAliasWithHostNetwork(t *testing.T) {
	pod := simplePod("default", "app", "myimage")
	pod.Spec.HostNetwork = true
	out, err := quadlet.Container(pod.Name, pod, "", []string{"myservice"}, nil, "")
	if err != nil {
		t.Fatalf("Container: %v", err)
	}
	assertNotContains(t, string(out), "NetworkAlias=")
}

func TestContainerRejectsInvalidNetworkAlias(t *testing.T) {
	pod := simplePod("default", "app", "myimage")
	if _, err := quadlet.Container(pod.Name, pod, "", []string{"bad alias!"}, nil, ""); err == nil {
		t.Fatal("expected error for invalid service alias")
	}
}

func TestContainerRestartPolicyAlways(t *testing.T) {
	pod := simplePod("default", "app", "myimage")
	pod.Spec.RestartPolicy = corev1.RestartPolicyAlways
	out := mustContainer(t, pod, "")

	assertContains(t, out, "[Service]")
	assertContains(t, out, "Restart=always")
	assertContains(t, out, "RestartSec=5")
}

func TestContainerRestartPolicyOnFailure(t *testing.T) {
	pod := simplePod("default", "app", "myimage")
	pod.Spec.RestartPolicy = corev1.RestartPolicyOnFailure
	out := mustContainer(t, pod, "")

	assertContains(t, out, "Restart=on-failure")
}

func TestContainerRestartPolicyNever(t *testing.T) {
	pod := simplePod("default", "app", "myimage")
	pod.Spec.RestartPolicy = corev1.RestartPolicyNever
	out := mustContainer(t, pod, "")

	assertNotContains(t, out, "Restart=on-failure")
}

func TestContainerLivenessProbe(t *testing.T) {
	pod := simplePod("default", "app", "myimage")
	pod.Spec.Containers[0].LivenessProbe = &corev1.Probe{
		ProbeHandler:        corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"cat", "/tmp/healthy"}}},
		InitialDelaySeconds: 5,
	}
	out := mustContainer(t, pod, "")

	assertContains(t, out, "HealthCmd=cat /tmp/healthy")
	assertContains(t, out, "HealthStartPeriod=5")
}

func TestContainerNoContainersError(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "empty", Namespace: "default"},
		Spec:       corev1.PodSpec{},
	}
	_, err := quadlet.Container(pod.Name, pod, "", nil, nil, "")
	if err == nil {
		t.Fatal("expected error for pod with no containers")
	}
}

// --- JobContainer ---

func TestJobContainerBasic(t *testing.T) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "myjob", Namespace: "default"},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "worker", Image: "busybox:latest"}},
				},
			},
		},
	}
	b, err := quadlet.JobContainer(job.Name, job, "", nil, "")
	if err != nil {
		t.Fatalf("JobContainer: %v", err)
	}
	out := string(b)

	assertContains(t, out, "Image=busybox:latest")
	assertContains(t, out, "ContainerName=default-myjob-job")
	assertContains(t, out, "Network=q8s-default.network")
	assertContains(t, out, "Restart=no")
	assertContains(t, out, "Description=Job default/myjob")
	assertNotContains(t, out, "[Install]")
}

func TestJobContainerEnvVars(t *testing.T) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "ns"},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "c",
						Image: "img",
						Env:   []corev1.EnvVar{{Name: "X", Value: "1"}},
					}},
				},
			},
		},
	}
	b, _ := quadlet.JobContainer(job.Name, job, "", nil, "")
	assertContains(t, string(b), "Environment=X=1")
}

func TestJobContainerNoContainersError(t *testing.T) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "ns"},
		Spec:       batchv1.JobSpec{Template: corev1.PodTemplateSpec{}},
	}
	_, err := quadlet.JobContainer(job.Name, job, "", nil, "")
	if err == nil {
		t.Fatal("expected error for job with no containers")
	}
}

// --- CronContainer ---

func TestCronContainerBasic(t *testing.T) {
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "mycron", Namespace: "default"},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 3 * * *",
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "task", Image: "alpine"}},
						},
					},
				},
			},
		},
	}
	b, err := quadlet.CronContainer(cj.Name, cj, "", nil, "")
	if err != nil {
		t.Fatalf("CronContainer: %v", err)
	}
	out := string(b)

	assertContains(t, out, "Image=alpine")
	assertContains(t, out, "ContainerName=default-mycron-cron")
	assertContains(t, out, "Restart=no")
	assertContains(t, out, "Description=CronJob default/mycron")
	assertNotContains(t, out, "[Install]")
}

func TestCronContainerNoContainersError(t *testing.T) {
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"},
		Spec: batchv1.CronJobSpec{
			Schedule:    "* * * * *",
			JobTemplate: batchv1.JobTemplateSpec{},
		},
	}
	_, err := quadlet.CronContainer(cj.Name, cj, "", nil, "")
	if err == nil {
		t.Fatal("expected error for cronjob with no containers")
	}
}

// --- CronTimer ---

func TestCronTimerBasic(t *testing.T) {
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "mycron", Namespace: "default"},
		Spec:       batchv1.CronJobSpec{Schedule: "0 3 * * *"},
	}
	b, err := quadlet.CronTimer(cj.Name, cj)
	if err != nil {
		t.Fatalf("CronTimer: %v", err)
	}
	out := string(b)

	assertContains(t, out, "[Timer]")
	assertContains(t, out, "OnCalendar=")
	assertContains(t, out, "Persistent=true")
	assertContains(t, out, "[Install]")
	assertContains(t, out, "WantedBy=timers.target")
}

// --- cronToOnCalendar (via CronTimer output) ---

func TestCronToOnCalendar(t *testing.T) {
	tests := []struct {
		schedule string
		want     string
	}{
		{"0 3 * * *", "OnCalendar=*-*-* 3:0:00"},
		{"*/5 * * * *", "OnCalendar=*-*-* *:0/5:00"},
		{"*/1 * * * *", "OnCalendar=*-*-* *:*:00"},
		{"0 0 1 * *", "OnCalendar=*-*-1 0:0:00"},
		{"30 6 * * *", "OnCalendar=*-*-* 6:30:00"},
		{"0 0 * 1 *", "OnCalendar=*-1-* 0:0:00"},
	}
	for _, tt := range tests {
		t.Run(tt.schedule, func(t *testing.T) {
			cj := &batchv1.CronJob{
				ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"},
				Spec:       batchv1.CronJobSpec{Schedule: tt.schedule},
			}
			b, _ := quadlet.CronTimer(cj.Name, cj)
			if !strings.Contains(string(b), tt.want) {
				t.Errorf("schedule %q: expected %q in:\n%s", tt.schedule, tt.want, b)
			}
		})
	}
}

// --- Volume ---

func TestVolume(t *testing.T) {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "my-pvc", Namespace: "default"},
	}
	b, err := quadlet.Volume(pvc)
	if err != nil {
		t.Fatalf("Volume: %v", err)
	}
	out := string(b)

	assertContains(t, out, "[Volume]")
	assertContains(t, out, "VolumeName=default-my-pvc")
}

func TestVolumeHostpathReturnsNil(t *testing.T) {
	sc := "hostpath"
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "hp", Namespace: "default",
			Annotations: map[string]string{"q8s.io/host-path": "/data/hp"}},
		Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: &sc},
	}
	b, err := quadlet.Volume(pvc)
	if err != nil {
		t.Fatalf("Volume: %v", err)
	}
	if b != nil {
		t.Fatalf("expected nil for hostpath PVC, got:\n%s", b)
	}
}

func TestVolumeSharedClass(t *testing.T) {
	sc := "standard-shared"
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-vol", Namespace: "default"},
		Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: &sc},
	}
	b, err := quadlet.Volume(pvc)
	if err != nil {
		t.Fatalf("Volume: %v", err)
	}
	out := string(b)
	assertContains(t, out, "[Volume]")
	assertContains(t, out, "VolumeName=default-shared-vol")
}

// --- StorageClass volume mounts in Container ---

func TestContainerPVCStandardClass(t *testing.T) {
	sc := "standard"
	pvcMap := map[string]*corev1.PersistentVolumeClaim{
		"my-pvc": {
			ObjectMeta: metav1.ObjectMeta{Name: "my-pvc", Namespace: "default"},
			Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: &sc},
		},
	}
	pod := simplePod("default", "app", "myimage")
	pod.Spec.Volumes = []corev1.Volume{{
		Name:         "data",
		VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "my-pvc"}},
	}}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: "/data"}}
	b, err := quadlet.Container(pod.Name, pod, "", nil, pvcMap, "")
	if err != nil {
		t.Fatalf("Container: %v", err)
	}
	assertContains(t, string(b), "Volume=default-my-pvc.volume:/data:Z")
}

func TestContainerPVCSharedClass(t *testing.T) {
	sc := "standard-shared"
	pvcMap := map[string]*corev1.PersistentVolumeClaim{
		"shared-vol": {
			ObjectMeta: metav1.ObjectMeta{Name: "shared-vol", Namespace: "default"},
			Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: &sc},
		},
	}
	pod := simplePod("default", "app", "myimage")
	pod.Spec.Volumes = []corev1.Volume{{
		Name:         "data",
		VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "shared-vol"}},
	}}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: "/data"}}
	b, err := quadlet.Container(pod.Name, pod, "", nil, pvcMap, "")
	if err != nil {
		t.Fatalf("Container: %v", err)
	}
	assertContains(t, string(b), "Volume=default-shared-vol.volume:/data:z")
}

func TestContainerPVCHostpathClass(t *testing.T) {
	sc := "hostpath"
	pvcMap := map[string]*corev1.PersistentVolumeClaim{
		"hp": {
			ObjectMeta: metav1.ObjectMeta{Name: "hp", Namespace: "default",
				Annotations: map[string]string{"q8s.io/host-path": "/srv/data"}},
			Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: &sc},
		},
	}
	pod := simplePod("default", "app", "myimage")
	pod.Spec.Volumes = []corev1.Volume{{
		Name:         "data",
		VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "hp"}},
	}}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: "/data"}}
	b, err := quadlet.Container(pod.Name, pod, "", nil, pvcMap, "")
	if err != nil {
		t.Fatalf("Container: %v", err)
	}
	out := string(b)
	assertContains(t, out, "Volume=/srv/data:/data:Z")
	assertNotContains(t, out, ".volume")
}

func TestContainerPVCHostpathMissingAnnotation(t *testing.T) {
	sc := "hostpath"
	pvcMap := map[string]*corev1.PersistentVolumeClaim{
		"hp": {
			ObjectMeta: metav1.ObjectMeta{Name: "hp", Namespace: "default"},
			Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: &sc},
		},
	}
	pod := simplePod("default", "app", "myimage")
	pod.Spec.Volumes = []corev1.Volume{{
		Name:         "data",
		VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "hp"}},
	}}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: "/data"}}
	_, err := quadlet.Container(pod.Name, pod, "", nil, pvcMap, "")
	if err == nil {
		t.Fatal("expected error for hostpath PVC without q8s.io/host-path annotation")
	}
}

func TestContainerPVCDefaultClassWhenNil(t *testing.T) {
	// StorageClassName nil → treated as "standard" → :Z
	pvcMap := map[string]*corev1.PersistentVolumeClaim{
		"my-pvc": {
			ObjectMeta: metav1.ObjectMeta{Name: "my-pvc", Namespace: "default"},
		},
	}
	pod := simplePod("default", "app", "myimage")
	pod.Spec.Volumes = []corev1.Volume{{
		Name:         "data",
		VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "my-pvc"}},
	}}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: "/data"}}
	b, err := quadlet.Container(pod.Name, pod, "", nil, pvcMap, "")
	if err != nil {
		t.Fatalf("Container: %v", err)
	}
	assertContains(t, string(b), "Volume=default-my-pvc.volume:/data:Z")
}

// --- Network ---

func TestNetwork(t *testing.T) {
	b, err := quadlet.Network("production")
	if err != nil {
		t.Fatalf("Network: %v", err)
	}
	out := string(b)

	assertContains(t, out, "[Network]")
	assertContains(t, out, "NetworkName=q8s-production")
}

// --- ShellJoin ---

func TestShellJoin(t *testing.T) {
	// ShellJoin is not exported; test it indirectly through Container Exec
	cases := []struct {
		cmd  []string
		want string
	}{
		{[]string{"echo", "hello"}, `Exec=echo hello`},
		{[]string{"/bin/sh", "-c", "ls -la"}, `Exec=/bin/sh -c "ls -la"`},
		{[]string{"simple"}, "Exec=simple"},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.cmd, " "), func(t *testing.T) {
			pod := simplePod("default", "app", "img")
			pod.Spec.Containers[0].Command = tc.cmd
			out := mustContainer(t, pod, "")
			assertContains(t, out, tc.want)
		})
	}
}

// --- PVC volume mount in pod ---

func TestContainerPVCStorageRequest(t *testing.T) {
	q := resource.MustParse("1Gi")
	_ = q // just verify the parse works; actual PVC volume is separate from pod spec
}

// --- Injection resistance ---
//
// These generated files are plain "Key=Value" INI text loaded and executed
// by systemd. An unescaped newline in any interpolated field doesn't just
// corrupt a line — it opens a new line, and with a blank line plus
// "[Section]", a whole new section with attacker-chosen directives. Every
// case here reproduces a field that used to be spliced in unvalidated.

func TestContainerRejectsNewlineInImage(t *testing.T) {
	pod := simplePod("default", "victim", "nginx:latest\n\n[Service]\nExecStartPre=/bin/touch /tmp/pwned")
	if _, err := quadlet.Container(pod.Name, pod, "", nil, nil, ""); err == nil {
		t.Fatal("expected error for image containing embedded unit-file directives, got nil")
	}
}

func TestContainerRejectsNewlineInWorkingDir(t *testing.T) {
	pod := simplePod("default", "victim", "nginx:latest")
	pod.Spec.Containers[0].WorkingDir = "/app\n\n[Service]\nExecStartPre=/bin/touch /tmp/pwned"
	if _, err := quadlet.Container(pod.Name, pod, "", nil, nil, ""); err == nil {
		t.Fatal("expected error for WorkingDir containing embedded unit-file directives, got nil")
	}
}

func TestContainerRejectsNewlineInEnvValue(t *testing.T) {
	pod := simplePod("default", "victim", "nginx:latest")
	pod.Spec.Containers[0].Env = []corev1.EnvVar{
		{Name: "FOO", Value: "bar\n\n[Service]\nExecStartPre=/bin/touch /tmp/pwned"},
	}
	if _, err := quadlet.Container(pod.Name, pod, "", nil, nil, ""); err == nil {
		t.Fatal("expected error for env value containing embedded unit-file directives, got nil")
	}
}

func TestContainerRejectsInvalidEnvName(t *testing.T) {
	pod := simplePod("default", "victim", "nginx:latest")
	pod.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "not a valid name", Value: "x"}}
	if _, err := quadlet.Container(pod.Name, pod, "", nil, nil, ""); err == nil {
		t.Fatal("expected error for invalid environment variable name, got nil")
	}
}

func TestContainerRejectsNewlineInPVCClaimName(t *testing.T) {
	pod := simplePod("default", "victim", "nginx:latest")
	pod.Spec.Volumes = []corev1.Volume{{
		Name: "data",
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: "x\n\n[Service]\nExecStartPre=/bin/touch /tmp/pwned",
			},
		},
	}}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: "/data"}}
	if _, err := quadlet.Container(pod.Name, pod, "", nil, nil, ""); err == nil {
		t.Fatal("expected error for PVC claim name containing embedded unit-file directives, got nil")
	}
}

func TestContainerRejectsNewlineInCommand(t *testing.T) {
	pod := simplePod("default", "victim", "nginx:latest")
	pod.Spec.Containers[0].Command = []string{"echo\n\n[Service]\nExecStartPre=/bin/touch /tmp/pwned"}
	if _, err := quadlet.Container(pod.Name, pod, "", nil, nil, ""); err == nil {
		t.Fatal("expected error for command containing embedded unit-file directives, got nil")
	}
}

func TestCronTimerRejectsMalformedSchedule(t *testing.T) {
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "victim", Namespace: "default"},
		Spec:       batchv1.CronJobSpec{Schedule: "* * * * *\n\n[Service]\nExecStartPre=/bin/touch /tmp/pwned"},
	}
	if _, err := quadlet.CronTimer(cj.Name, cj); err == nil {
		t.Fatal("expected error for schedule containing embedded unit-file directives, got nil")
	}
}

// --- Resource limits ---

func TestContainerResourceLimits(t *testing.T) {
	restore := quadlet.SetCgroupOverride(true, true)
	defer restore()

	pod := simplePod("default", "app", "nginx:latest")
	pod.Spec.Containers[0].Resources.Limits = corev1.ResourceList{
		corev1.ResourceMemory: resource.MustParse("512Mi"),
		corev1.ResourceCPU:    resource.MustParse("500m"),
	}
	out := mustContainer(t, pod, "")
	assertContains(t, out, "Memory=536870912")
	assertContains(t, out, "PodmanArgs=--memory-swap=-1")
	assertContains(t, out, "PodmanArgs=--cpus=0.5")
}

func TestContainerNoResourceLimits(t *testing.T) {
	restore := quadlet.SetCgroupOverride(true, true)
	defer restore()

	pod := simplePod("default", "app", "nginx:latest")
	out := mustContainer(t, pod, "")
	assertNotContains(t, out, "Memory=")
	assertNotContains(t, out, "--memory-swap")
	assertNotContains(t, out, "--cpus")
}

func TestContainerResourceLimitsSkippedWhenCgroupUnavailable(t *testing.T) {
	restore := quadlet.SetCgroupOverride(false, false)
	defer restore()

	pod := simplePod("default", "app", "nginx:latest")
	pod.Spec.Containers[0].Resources.Limits = corev1.ResourceList{
		corev1.ResourceMemory: resource.MustParse("512Mi"),
		corev1.ResourceCPU:    resource.MustParse("500m"),
	}
	out := mustContainer(t, pod, "")
	assertNotContains(t, out, "Memory=")
	assertNotContains(t, out, "--memory-swap")
	assertNotContains(t, out, "--cpus")
}

func TestContainerAcceptsOrdinaryValues(t *testing.T) {
	// Sanity check: realistic values still work after the validation pass.
	pod := simplePod("default", "app", "docker.io/library/nginx:1.25-alpine")
	pod.Spec.Containers[0].WorkingDir = "/usr/share/nginx/html"
	pod.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "APP_ENV", Value: "production"}}
	pod.Spec.Containers[0].Command = []string{"/bin/sh", "-c", "nginx -g 'daemon off;'"}
	out := mustContainer(t, pod, "")
	assertContains(t, out, "Image=docker.io/library/nginx:1.25-alpine")
	assertContains(t, out, "WorkingDir=/usr/share/nginx/html")
	assertContains(t, out, "Environment=APP_ENV=production")
}
