package quadlet_test

import (
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"q8s/internal/quadlet"
)

func boolPtr(v bool) *bool { return &v }

// usernsPod returns a standalone pod mounting every volume kind q8s renders,
// plus the PVC map that resolves the named and hostpath claims.
func usernsPod() (*corev1.Pod, map[string]*corev1.PersistentVolumeClaim) {
	standard := "standard"
	hostpath := "hostpath"
	pvcMap := map[string]*corev1.PersistentVolumeClaim{
		"data": {
			ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "default"},
			Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: &standard},
		},
		"data-ro": {
			ObjectMeta: metav1.ObjectMeta{Name: "data-ro", Namespace: "default"},
			Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: &standard},
		},
		"models": {
			ObjectMeta: metav1.ObjectMeta{Name: "models", Namespace: "default",
				Annotations: map[string]string{"q8s.io/host-path": "/srv/models"}},
			Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: &hostpath},
		},
	}

	pod := simplePod("default", "app", "myimage")
	pod.Spec.Volumes = []corev1.Volume{
		{
			Name:         "data",
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"}},
		},
		{
			Name:         "data-ro",
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data-ro"}},
		},
		{
			Name:         "models",
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "models"}},
		},
		{
			Name:         "cfg",
			VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "app-config"}}},
		},
		{
			Name:         "creds",
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "creds"}},
		},
		{
			Name:         "scratch",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
		{
			Name:         "scratch-ro",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
	}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{
		{Name: "data", MountPath: "/data"},
		{Name: "data-ro", MountPath: "/data-ro", ReadOnly: true},
		{Name: "models", MountPath: "/models", ReadOnly: true},
		{Name: "cfg", MountPath: "/etc/config"},
		{Name: "creds", MountPath: "/etc/creds"},
		{Name: "scratch", MountPath: "/scratch"},
		{Name: "scratch-ro", MountPath: "/scratch-ro", ReadOnly: true},
	}
	return pod, pvcMap
}

func TestUserNSStandaloneContainerAndVolumes(t *testing.T) {
	restore := quadlet.SetUserNS(true)
	defer restore()

	pod, pvcMap := usernsPod()
	b, err := quadlet.Container(pod.Name, pod, "/run/q8s/configmaps", nil, pvcMap, "", nil)
	if err != nil {
		t.Fatalf("Container: %v", err)
	}
	out := string(b)

	assertContains(t, out, "UserNS=auto")
	for _, want := range []string{
		"Volume=default-data.volume:/data:Z,idmap",
		"Volume=default-data-ro.volume:/data-ro:ro,Z,idmap",
		"Volume=/srv/models:/models:ro,Z,idmap",
		"Volume=/run/q8s/configmaps/default/app-config:/etc/config:ro,z,idmap",
		"Volume=/run/q8s/secrets/default/creds:/etc/creds:ro,z,idmap",
		"Volume=scratch:/scratch:idmap",
		"Volume=scratch-ro:/scratch-ro:ro,idmap",
	} {
		assertContains(t, out, want)
	}
}

func TestUserNSOffByDefault(t *testing.T) {
	pod, pvcMap := usernsPod()

	b, err := quadlet.Container(pod.Name, pod, "/run/q8s/configmaps", nil, pvcMap, "", nil)
	if err != nil {
		t.Fatalf("Container: %v", err)
	}
	out := string(b)
	assertNotContains(t, out, "UserNS")
	assertNotContains(t, out, "idmap")
	assertContains(t, out, "Volume=default-data.volume:/data:Z")
	assertContains(t, out, "Volume=scratch:/scratch\n")
	assertContains(t, out, "Volume=scratch-ro:/scratch-ro:ro\n")

	files, err := quadlet.PodUnits("multi", multiPod(), "/run/q8s/configmaps", nil, nil, "", nil)
	if err != nil {
		t.Fatalf("PodUnits: %v", err)
	}
	for name, content := range files {
		if strings.Contains(string(content), "UserNS") || strings.Contains(string(content), "idmap") {
			t.Errorf("%s gained userns output with the switch off:\n%s", name, content)
		}
	}
}

func TestUserNSMultiContainerPod(t *testing.T) {
	restore := quadlet.SetUserNS(true)
	defer restore()

	standard := "standard"
	pvcMap := map[string]*corev1.PersistentVolumeClaim{
		"data": {
			ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "default"},
			Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: &standard},
		},
	}
	pod := multiPod()
	pod.Spec.Volumes = append(pod.Spec.Volumes,
		corev1.Volume{
			Name:         "data",
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"}},
		},
		corev1.Volume{
			Name:         "scratch",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
	)
	pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts,
		corev1.VolumeMount{Name: "data", MountPath: "/data"})
	pod.Spec.Containers[1].VolumeMounts = append(pod.Spec.Containers[1].VolumeMounts,
		corev1.VolumeMount{Name: "scratch", MountPath: "/scratch"})

	files, err := quadlet.PodUnits(pod.Name, pod, "/run/q8s/configmaps", nil, pvcMap, "", nil)
	if err != nil {
		t.Fatalf("PodUnits: %v", err)
	}

	podOut := string(files["default-multi.pod"])
	assertContains(t, podOut, "UserNS=auto")

	webOut := string(files["default-multi-web.container"])
	assertNotContains(t, webOut, "UserNS")
	assertContains(t, webOut, "Volume=/run/q8s/configmaps/default/app-config:/etc/config:ro,z,idmap")
	assertContains(t, webOut, "Volume=default-data.volume:/data:Z,idmap")

	sidecarOut := string(files["default-multi-sidecar.container"])
	assertNotContains(t, sidecarOut, "UserNS")
	assertContains(t, sidecarOut, "Volume=scratch:/scratch:idmap")

	// The /dev/shm emptyDir is a pod-level ShmSize, not a Volume= line.
	assertNotContains(t, podOut, "idmap")
	assertNotContains(t, webOut, "shm")
}

func TestUserNSHostNetworkOptOut(t *testing.T) {
	restore := quadlet.SetUserNS(true)
	defer restore()

	pod := simplePod("default", "app", "myimage")
	pod.Spec.HostNetwork = true
	pod.Spec.Volumes = []corev1.Volume{{
		Name:         "scratch",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "scratch", MountPath: "/scratch"}}

	out := mustContainer(t, pod, "")
	assertContains(t, out, "Network=host")
	assertNotContains(t, out, "UserNS")
	assertNotContains(t, out, "idmap")
}

func TestUserNSHostUsersOptOut(t *testing.T) {
	restore := quadlet.SetUserNS(true)
	defer restore()

	for _, tc := range []struct {
		name      string
		hostUsers *bool
		wantNS    bool
	}{
		{name: "hostUsers true opts out", hostUsers: boolPtr(true), wantNS: false},
		{name: "hostUsers false opts in", hostUsers: boolPtr(false), wantNS: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := simplePod("default", "app", "myimage")
			pod.Spec.HostUsers = tc.hostUsers
			pod.Spec.Volumes = []corev1.Volume{{
				Name:         "scratch",
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			}}
			pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "scratch", MountPath: "/scratch"}}
			out := mustContainer(t, pod, "")
			if tc.wantNS {
				assertContains(t, out, "UserNS=auto")
				assertContains(t, out, "Volume=scratch:/scratch:idmap")
			} else {
				assertNotContains(t, out, "UserNS")
				assertNotContains(t, out, "idmap")
			}
		})
	}
}

func TestUserNSJobContainer(t *testing.T) {
	restore := quadlet.SetUserNS(true)
	defer restore()

	standard := "standard"
	pvcMap := map[string]*corev1.PersistentVolumeClaim{
		"data": {
			ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "ns"},
			Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: &standard},
		},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "ns"},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:         "c",
					Image:        "img",
					VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/data"}},
				}},
				Volumes: []corev1.Volume{{
					Name:         "data",
					VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"}},
				}},
			},
		}},
	}
	b, err := quadlet.JobContainer(job.Name, job, "", pvcMap, "")
	if err != nil {
		t.Fatalf("JobContainer: %v", err)
	}
	out := string(b)
	assertContains(t, out, "UserNS=auto")
	assertContains(t, out, "Volume=ns-data.volume:/data:Z,idmap")
}

func TestUserNSCronContainer(t *testing.T) {
	restore := quadlet.SetUserNS(true)
	defer restore()

	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"},
		Spec: batchv1.CronJobSpec{
			Schedule: "* * * * *",
			JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{
				Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:         "c",
						Image:        "img",
						VolumeMounts: []corev1.VolumeMount{{Name: "scratch", MountPath: "/scratch"}},
					}},
					Volumes: []corev1.Volume{{
						Name:         "scratch",
						VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
					}},
				}},
			}},
		},
	}
	b, err := quadlet.CronContainer(cj.Name, cj, "", nil, "")
	if err != nil {
		t.Fatalf("CronContainer: %v", err)
	}
	out := string(b)
	assertContains(t, out, "UserNS=auto")
	assertContains(t, out, "Volume=scratch:/scratch:idmap")
}

func TestUserNSJobHostNetworkOptOut(t *testing.T) {
	restore := quadlet.SetUserNS(true)
	defer restore()

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "ns"},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				HostNetwork: true,
				Containers:  []corev1.Container{{Name: "c", Image: "img"}},
			},
		}},
	}
	b, err := quadlet.JobContainer(job.Name, job, "", nil, "")
	if err != nil {
		t.Fatalf("JobContainer: %v", err)
	}
	assertNotContains(t, string(b), "UserNS")
	assertNotContains(t, string(b), "idmap")
}
