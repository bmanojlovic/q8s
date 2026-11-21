package quadlet_test

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"q8s/internal/quadlet"
)

func pvcWithVolumeName(ns, name, volumeName string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: volumeName},
	}
}

// A claim created before the naming upgrade keeps its old volume name and its
// old .volume file even though every other generated name is new.
func TestLegacyPVCKeepsItsVolumeName(t *testing.T) {
	legacy := pvcWithVolumeName("team", "data", "team-data")
	content, err := quadlet.Volume(legacy)
	if err != nil {
		t.Fatalf("Volume: %v", err)
	}
	if want := "VolumeName=team-data\n"; !strings.Contains(string(content), want) {
		t.Fatalf("legacy volume content = %q, want %q", content, want)
	}
	if got := quadlet.PVCVolumeFileBase(legacy); got != "team-data" {
		t.Fatalf("PVCVolumeFileBase = %q, want team-data", got)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "team"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "app",
				Image: "nginx",
				VolumeMounts: []corev1.VolumeMount{{
					Name:      "data",
					MountPath: "/data",
				}},
			}},
			Volumes: []corev1.Volume{{
				Name: "data",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"},
				},
			}},
		},
	}
	rendered, err := quadlet.Container("app", pod, "", nil,
		map[string]*corev1.PersistentVolumeClaim{"data": legacy}, "", nil)
	if err != nil {
		t.Fatalf("Container: %v", err)
	}
	if want := "Volume=team-data.volume:/data:Z\n"; !strings.Contains(string(rendered), want) {
		t.Fatalf("legacy claim rendered a different mount:\n%s\nwant %q", rendered, want)
	}

	// Multi-container pods use the same effective name.
	multi := pod.DeepCopy()
	multi.Spec.Containers = append(multi.Spec.Containers, corev1.Container{Name: "sidecar", Image: "busybox"})
	files, err := quadlet.PodUnits("app", multi, "", nil,
		map[string]*corev1.PersistentVolumeClaim{"data": legacy}, "", nil)
	if err != nil {
		t.Fatalf("PodUnits: %v", err)
	}
	if member := string(files["team_app_sidecar.container"]); strings.Contains(member, "Volume=") {
		t.Fatalf("sidecar without the mount got a volume line:\n%s", member)
	}
	if main := string(files["team_app_app.container"]); !strings.Contains(main, "Volume=team-data.volume:/data:Z") {
		t.Fatalf("multi-container pod lost the legacy mount:\n%s", main)
	}
}

func TestNewPVCUsesTheNewVolumeName(t *testing.T) {
	claim := pvcWithVolumeName("team", "data", quadlet.PVCVolumeName("team", "data"))
	if got, want := claim.Spec.VolumeName, "team_data"; got != want {
		t.Fatalf("PVCVolumeName = %q, want %q", got, want)
	}
	content, err := quadlet.Volume(claim)
	if err != nil {
		t.Fatalf("Volume: %v", err)
	}
	if want := "VolumeName=team_data\n"; !strings.Contains(string(content), want) {
		t.Fatalf("new volume content = %q, want %q", content, want)
	}
	if got := quadlet.PVCVolumeFileBase(claim); got != "team_data" {
		t.Fatalf("PVCVolumeFileBase = %q, want team_data", got)
	}
}

func TestUnboundPVCWithoutLegacyFileIsNew(t *testing.T) {
	claim := pvcWithVolumeName("team", "data", "")
	content, err := quadlet.Volume(claim)
	if err != nil {
		t.Fatalf("Volume: %v", err)
	}
	if want := "VolumeName=team_data\n"; !strings.Contains(string(content), want) {
		t.Fatalf("unbound new volume content = %q, want %q", content, want)
	}
}
