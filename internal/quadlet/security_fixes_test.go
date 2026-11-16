package quadlet_test

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"q8s/internal/quadlet"
)

// TestEmptyDirVolumeNamesAreNamespacedAndWorkloadScoped pins that an emptyDir
// gets a Podman volume name that is unique per namespace and workload, and
// can never equal a PVC volume name.
func TestEmptyDirVolumeNamesAreNamespacedAndWorkloadScoped(t *testing.T) {
	first := simplePod("team", "app", "img")
	first.Spec.Volumes = []corev1.Volume{{
		Name:         "data",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}}
	first.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: "/data"}}
	second := simplePod("team-prod", "app", "img")
	second.Spec.Volumes = first.Spec.Volumes
	second.Spec.Containers[0].VolumeMounts = first.Spec.Containers[0].VolumeMounts

	firstOut := string(mustContainer(t, first, ""))
	secondOut := string(mustContainer(t, second, ""))

	assertContains(t, firstOut, "Volume=q8s_team_app_data:/data")
	assertContains(t, secondOut, "Volume=q8s_team-prod_app_data:/data")
	if strings.Contains(firstOut, "Volume=data:/data") || strings.Contains(secondOut, "Volume=data:/data") {
		t.Fatalf("emptyDir fell back to a bare host-global volume name:\n%s\n%s", firstOut, secondOut)
	}

	if got, want := quadlet.EmptyDirVolumeName("default", "app", "prod-data"), quadlet.PVCVolumeName("prod", "data"); got == want {
		t.Fatalf("emptyDir volume name %q equals PVC volume name %q", got, want)
	}
}

// TestEmptyDirVolumeIsPerDeploymentReplica: replica i of Deployment web gets
// its own volume q8s_<namespace>_web_<i>_<volume>, as Kubernetes gives each pod
// its own emptyDir. A bare pod keeps the bare-pod form, so the two forms can
// never collide.
func TestEmptyDirVolumeIsPerDeploymentReplica(t *testing.T) {
	replica := func(name string) *corev1.Pod {
		pod := simplePod("default", name, "img")
		pod.Spec.Volumes = []corev1.Volume{{
			Name:         "cache",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		}}
		pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "cache", MountPath: "/cache"}}
		return pod
	}
	first := replica("web-0")
	first.OwnerReferences = []metav1.OwnerReference{{Kind: "Deployment", Name: "web"}}
	second := replica("web-1")
	second.OwnerReferences = []metav1.OwnerReference{{Kind: "Deployment", Name: "web"}}

	firstOut := string(mustContainer(t, first, ""))
	secondOut := string(mustContainer(t, second, ""))
	assertContains(t, firstOut, "Volume=q8s_default_web_0_cache:/cache")
	assertContains(t, secondOut, "Volume=q8s_default_web_1_cache:/cache")

	bare := replica("web-0")
	bareOut := string(mustContainer(t, bare, ""))
	assertContains(t, bareOut, "Volume=q8s_default_web-0_cache:/cache")
	if strings.Contains(bareOut, "q8s_default_web_0_cache") {
		t.Fatalf("bare pod volume name collides with a Deployment replica name:\n%s", bareOut)
	}
}

// TestDeploymentReplicaWithoutEmptyDirRendersUnchanged is the golden guard for
// the per-replica emptyDir change: a Deployment replica without an emptyDir
// must render exactly as it did before, because an upgrade restarts every unit
// whose rendered file changes.
func TestDeploymentReplicaWithoutEmptyDirRendersUnchanged(t *testing.T) {
	pod := simplePod("default", "web-0", "nginx:1.0")
	pod.OwnerReferences = []metav1.OwnerReference{{
		Kind: "Deployment", Name: "web", APIVersion: "apps/v1", Controller: boolPtr(true),
	}}
	want := `[Container]
Image=nginx:1.0
Pull=missing
ContainerName=default-web-0
Network=q8s-default.network
NetworkAlias=web-0.default
Label=io.kubernetes.pod.name=web-0
Label=io.kubernetes.pod.namespace=default
Label=io.kubernetes.pod.deployment=web
Label=app.kubernetes.io/managed-by=q8s
Label=q8s.io/template-hash=ee46d0e4b71f

[Unit]
Description=Pod web-0
Wants=q8s-api.service
After=q8s-api.service
StartLimitBurst=5
StartLimitIntervalSec=60

[Service]
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
`
	if got := mustContainer(t, pod, ""); got != want {
		t.Fatalf("Deployment replica without emptyDir changed:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestUnsupportedStoredVolumeRendersNamespacedEmptyDirName: an object already
// in a store with a hostPath volume must still not fall back to a bare named
// volume. The API rejects new objects of this shape with 422.
func TestUnsupportedStoredVolumeRendersNamespacedEmptyDirName(t *testing.T) {
	pod := simplePod("default", "app", "img")
	pod.Spec.Volumes = []corev1.Volume{{
		Name:         "host",
		VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/srv/data"}},
	}}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "host", MountPath: "/host"}}

	out := string(mustContainer(t, pod, ""))
	assertContains(t, out, "Volume=q8s_default_app_host:/host")
	assertNotContains(t, out, "Volume=host:/host")
}

// TestStoredLabelValueCannotInjectLabelItems: the render-time guard rejects a
// value that would start another Label= item even when the API was bypassed.
func TestStoredLabelValueCannotInjectLabelItems(t *testing.T) {
	for _, value := range []string{
		"a io.kubernetes.pod.namespace=prod io.kubernetes.pod.name=db",
		"a\tb",
		`a"b`,
		"a'b",
		`a\b`,
	} {
		pod := simplePod("default", "app", "img")
		pod.Labels = map[string]string{"app": value}
		if _, err := quadlet.Container(pod.Name, pod, "", nil, nil, "", nil); err == nil {
			t.Errorf("label value %q was rendered without an error", value)
		}
	}
}

// TestEmptyDirVolumeNameSetsMatchTheStoredVolumes pins that delete and doctor
// compute the emptyDir names from the stored spec with the same owner and
// that PVC, ConfigMap and Secret sources are not treated as emptyDir volumes.
func TestEmptyDirVolumeNameSetsMatchTheStoredVolumes(t *testing.T) {
	pod := simplePod("default", "web-0", "img")
	pod.OwnerReferences = []metav1.OwnerReference{{Kind: "Deployment", Name: "web"}}
	pod.Spec.Volumes = []corev1.Volume{
		{Name: "cache", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "shm", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}},
		{Name: "claim", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"}}},
		{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{}}},
		{Name: "secret", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{}}},
	}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "cache", MountPath: "/cache"}}

	want := []string{"q8s_default_web_0_cache", "q8s_default_web_0_scratch", "q8s_default_web_0_shm"}
	names := quadlet.EmptyDirVolumeNames(pod)
	if len(names) != len(want) {
		t.Fatalf("EmptyDirVolumeNames = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("EmptyDirVolumeNames = %v, want %v", names, want)
		}
	}
	bases := quadlet.EmptyDirVolumeBases(&pod.Spec)
	if len(bases) != 3 || bases[0] != "cache" || bases[1] != "scratch" || bases[2] != "shm" {
		t.Fatalf("EmptyDirVolumeBases = %v, want [cache scratch shm]", bases)
	}
	fromSpec := quadlet.EmptyDirVolumeNamesForDeploymentReplica("default", "web", 0, &pod.Spec)
	if len(fromSpec) != len(names) {
		t.Fatalf("EmptyDirVolumeNamesForDeploymentReplica = %v, want %v", fromSpec, names)
	}
	for i := range names {
		if fromSpec[i] != names[i] {
			t.Fatalf("EmptyDirVolumeNamesForDeploymentReplica = %v, want %v", fromSpec, names)
		}
	}
}
