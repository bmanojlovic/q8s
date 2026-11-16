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

// TestEmptyDirVolumeIsSharedByWorkloadReplicas: every pod of one Deployment
// resolves its owner to the Deployment name, not the instance pod name, so
// all replicas keep sharing one volume as before.
func TestEmptyDirVolumeIsSharedByWorkloadReplicas(t *testing.T) {
	pod := simplePod("default", "web-0", "img")
	pod.OwnerReferences = []metav1.OwnerReference{{Kind: "Deployment", Name: "web"}}
	pod.Spec.Volumes = []corev1.Volume{{
		Name:         "cache",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "cache", MountPath: "/cache"}}
	first := string(mustContainer(t, pod, ""))

	second := pod.DeepCopy()
	second.Name = "web-1"
	secondOut := string(mustContainer(t, second, ""))

	assertContains(t, first, "Volume=q8s_default_web_cache:/cache")
	assertContains(t, secondOut, "Volume=q8s_default_web_cache:/cache")
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

// TestPodIdentityLabelsComeAfterUserLabels pins the label order: podman keeps
// the last value of a repeated label key, so q8s identity labels must be
// written after the user labels they protect.
func TestPodIdentityLabelsComeAfterUserLabels(t *testing.T) {
	pod := simplePod("default", "app", "img")
	pod.Labels = map[string]string{"zebra": "last", "app": "demo"}

	out := string(mustContainer(t, pod, ""))
	user := strings.Index(out, "Label=app=demo")
	identity := []int{
		strings.Index(out, "Label=io.kubernetes.pod.name=app"),
		strings.Index(out, "Label=io.kubernetes.pod.namespace=default"),
		strings.Index(out, "Label=app.kubernetes.io/managed-by=q8s"),
	}
	if user < 0 {
		t.Fatalf("user label not found:\n%s", out)
	}
	for _, i := range identity {
		if i < 0 || i < user {
			t.Fatalf("identity label is missing or comes before the user labels:\n%s", out)
		}
	}
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
