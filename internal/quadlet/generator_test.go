package quadlet_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"q8s/internal/quadlet"
)

// --- helpers ---

func mustContainer(t *testing.T, pod *corev1.Pod, configDir string) string {
	t.Helper()
	b, err := quadlet.Container(pod.Name, pod, configDir, nil, nil, "", nil)
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

func quantityPtr(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
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
	// Ordered after q8s-api so a reboot doesn't race the --env-file target
	// q8s re-renders on startup (tic-1507).
	assertContains(t, out, "After=q8s-api.service")
	assertContains(t, out, "[Install]")
	assertContains(t, out, "WantedBy=default.target")
	// No explicit restartPolicy → k8s defaulting → Always → restarts.
	// (The old assertion — no [Service] section at all — pinned the quadlet
	// default on-failure fallback, which restarted "run once" pods.)
	assertContains(t, out, "[Service]")
	assertContains(t, out, "Restart=always")
}

// TestContainerPullPolicy pins tic-864b: q8s now emits a Pull= line derived
// from imagePullPolicy so a user who asks for Always actually gets image
// refresh on restart (previously no Pull= was emitted at all). An explicit
// policy is honored verbatim; unset maps to the neutral "missing".
func TestContainerPullPolicy(t *testing.T) {
	cases := []struct {
		policy corev1.PullPolicy
		want   string
	}{
		{corev1.PullAlways, "Pull=always"},
		{corev1.PullNever, "Pull=never"},
		{corev1.PullIfNotPresent, "Pull=missing"},
		{"", "Pull=missing"}, // unset → neutral default, no q8s-imposed policy
	}
	for _, tc := range cases {
		pod := simplePod("default", "app", "myimage:latest")
		pod.Spec.Containers[0].ImagePullPolicy = tc.policy
		out := mustContainer(t, pod, "")
		assertContains(t, out, tc.want)
	}
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

func TestContainerEnvValueWithSpacesQuoted(t *testing.T) {
	pod := simplePod("default", "app", "myimage:1.0")
	pod.Spec.Containers[0].Env = []corev1.EnvVar{{
		Name:  "STRATA_SET",
		Value: "conversation-cache-mib=65536 conversation-cache-slots=24 prompt-cache=12",
	}}
	out := mustContainer(t, pod, "")

	assertContains(t, out, `Environment="STRATA_SET=conversation-cache-mib=65536 conversation-cache-slots=24 prompt-cache=12"`)
	assertNotContains(t, out, "Environment=STRATA_SET=conversation-cache-mib=65536 conversation-cache-slots=24 prompt-cache=12\n")
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
	if _, err := quadlet.Container(pod.Name, pod, "", nil, nil, "", nil); err == nil {
		t.Fatal("expected error for label value containing control characters")
	}
}

func TestContainerNetworkAlias(t *testing.T) {
	pod := simplePod("default", "app", "myimage")
	out, err := quadlet.Container(pod.Name, pod, "", []string{"myservice", "other-svc"}, nil, "", nil)
	if err != nil {
		t.Fatalf("Container: %v", err)
	}
	assertContains(t, string(out), "NetworkAlias=myservice")
	assertContains(t, string(out), "NetworkAlias=other-svc")
}

func TestContainerNoNetworkAliasWithHostNetwork(t *testing.T) {
	pod := simplePod("default", "app", "myimage")
	pod.Spec.HostNetwork = true
	out, err := quadlet.Container(pod.Name, pod, "", []string{"myservice"}, nil, "", nil)
	if err != nil {
		t.Fatalf("Container: %v", err)
	}
	assertNotContains(t, string(out), "NetworkAlias=")
}

func TestContainerRejectsInvalidNetworkAlias(t *testing.T) {
	pod := simplePod("default", "app", "myimage")
	if _, err := quadlet.Container(pod.Name, pod, "", []string{"bad alias!"}, nil, "", nil); err == nil {
		t.Fatal("expected error for invalid service alias")
	}
}

func networkAliasLines(out string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "NetworkAlias=") {
			lines = append(lines, strings.TrimPrefix(line, "NetworkAlias="))
		}
	}
	return lines
}

func TestContainerNetworkAliasExpansion(t *testing.T) {
	pod := simplePod("default", "app", "myimage")
	out, err := quadlet.Container("app", pod, "", []string{"myservice"}, nil, "", nil)
	if err != nil {
		t.Fatalf("Container: %v", err)
	}
	got := networkAliasLines(string(out))
	want := []string{
		"app.default",
		"myservice",
		"myservice.default",
		"myservice.default.svc.cluster.local",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("aliases = %v, want %v", got, want)
	}
}

func TestContainerNetworkAliasWithoutServices(t *testing.T) {
	pod := simplePod("default", "app", "myimage")
	out := mustContainer(t, pod, "")
	got := networkAliasLines(out)
	if len(got) != 1 || got[0] != "app.default" {
		t.Errorf("aliases without Services = %v, want [app.default]", got)
	}
}

func TestContainerNetworkAliasHostNetworkHasNone(t *testing.T) {
	pod := simplePod("default", "app", "myimage")
	pod.Spec.HostNetwork = true
	out, err := quadlet.Container("app", pod, "", []string{"myservice"}, nil, "", nil)
	if err != nil {
		t.Fatalf("Container: %v", err)
	}
	if got := networkAliasLines(string(out)); len(got) != 0 {
		t.Errorf("hostNetwork pod got aliases %v", got)
	}
}

func networkLines(out string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "Network=") {
			lines = append(lines, line)
		}
	}
	return lines
}

// An empty attachment list must render exactly today's single network line.
func TestContainerNetworksEmptyList(t *testing.T) {
	pod := simplePod("default", "app", "myimage")
	withNil, err := quadlet.Container("app", pod, "", nil, nil, "", nil)
	if err != nil {
		t.Fatalf("Container(nil): %v", err)
	}
	withEmpty, err := quadlet.Container("app", pod, "", nil, nil, "", []quadlet.NetworkAttachment{})
	if err != nil {
		t.Fatalf("Container(empty): %v", err)
	}
	if string(withNil) != string(withEmpty) {
		t.Errorf("nil and empty networks differ:\n%s\n%s", withNil, withEmpty)
	}
	if got := networkLines(string(withNil)); len(got) != 1 || got[0] != "Network=q8s-default.network" {
		t.Errorf("network lines = %v, want [Network=q8s-default.network]", got)
	}
}

func TestContainerNetworksOwnNamespaceMissing(t *testing.T) {
	pod := simplePod("default", "app", "myimage")
	out, err := quadlet.Container("app", pod, "", nil, nil, "", []quadlet.NetworkAttachment{
		{Namespace: "other"},
	})
	if err != nil {
		t.Fatalf("Container: %v", err)
	}
	want := []string{"Network=q8s-default.network", "Network=q8s-other.network"}
	if got := networkLines(string(out)); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("network lines = %v, want %v", got, want)
	}
}

func TestContainerNetworksSortedAndIPOption(t *testing.T) {
	pod := simplePod("default", "app", "myimage")
	out, err := quadlet.Container("app", pod, "", nil, nil, "", []quadlet.NetworkAttachment{
		{Namespace: "other"},
		{Namespace: "default", IP: "10.89.3.254"},
	})
	if err != nil {
		t.Fatalf("Container: %v", err)
	}
	want := []string{
		"Network=q8s-default.network:ip=10.89.3.254",
		"Network=q8s-other.network",
	}
	if got := networkLines(string(out)); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("network lines = %v, want %v", got, want)
	}
}

func TestContainerNetworksRejectsBadIP(t *testing.T) {
	for _, ip := range []string{"not-an-ip", "fd00::1", "10.89.3.256"} {
		pod := simplePod("default", "app", "myimage")
		if _, err := quadlet.Container("app", pod, "", nil, nil, "", []quadlet.NetworkAttachment{
			{Namespace: "default", IP: ip},
		}); err == nil {
			t.Errorf("expected error for network IP %q", ip)
		}
	}
}

func TestLastUsableIPv4(t *testing.T) {
	tests := []struct {
		cidr    string
		want    string
		wantErr bool
	}{
		{cidr: "10.89.3.0/24", want: "10.89.3.254"},
		{cidr: "10.89.0.0/16", want: "10.89.255.254"},
		{cidr: "10.0.0.0/8", want: "10.255.255.254"},
		{cidr: "198.51.100.0/30", want: "198.51.100.2"},
		{cidr: "10.89.3.7/24", want: "10.89.3.254"},
		{cidr: "10.0.0.0/31", wantErr: true},
		{cidr: "10.0.0.0/32", wantErr: true},
		{cidr: "fd00::/64", wantErr: true},
		{cidr: "not-a-cidr", wantErr: true},
		{cidr: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.cidr, func(t *testing.T) {
			got, err := quadlet.LastUsableIPv4(tt.cidr)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("LastUsableIPv4(%q): %v", tt.cidr, err)
			}
			if got != tt.want {
				t.Errorf("LastUsableIPv4(%q) = %q, want %q", tt.cidr, got, tt.want)
			}
		})
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

	// Never must be explicit: emitting nothing would fall back to the
	// quadlet default on-failure, restarting a "run once" pod.
	assertContains(t, out, "Restart=no")
	assertNotContains(t, out, "Restart=on-failure")
	assertNotContains(t, out, "Restart=always")
}

func TestContainerRestartPolicyEmptyIsAlways(t *testing.T) {
	// Defensive: the API boundary defaults empty to Always, but the
	// generator must not strand an empty policy as Never.
	pod := simplePod("default", "app", "myimage")
	pod.Spec.RestartPolicy = ""
	out := mustContainer(t, pod, "")
	assertContains(t, out, "Restart=always")
}

// healthLines returns every directive writeHealthCheck emits, in order, so a
// test can assert the exact rendering rather than just substrings.
func healthLines(out string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "Health") {
			lines = append(lines, line)
		}
	}
	return lines
}

func assertHealthLines(t *testing.T, out string, want ...string) {
	t.Helper()
	got := healthLines(out)
	if len(got) != len(want) {
		t.Fatalf("expected %d health check lines, got %d:\n%v\nfull output:\n%s", len(want), len(got), got, out)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("health line %d: expected %q, got %q", i, want[i], got[i])
		}
	}
}

// healthCmdArgv decodes a HealthCmd= or HealthStartupCmd= line back into
// argv, undoing the systemd escaping healthCmdLine applies.
func healthCmdArgv(t *testing.T, out, directive string) []string {
	t.Helper()
	prefix := directive + "="
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		raw := strings.TrimPrefix(line, prefix)
		raw = strings.ReplaceAll(raw, "$$", "$")
		raw = strings.ReplaceAll(raw, "%%", "%")
		var argv []string
		if err := json.Unmarshal([]byte(raw), &argv); err != nil {
			t.Fatalf("decode %s: %v (value %q)", directive, err, raw)
		}
		return argv
	}
	t.Fatalf("no %s line in output:\n%s", directive, out)
	return nil
}

// assertHTTPProbeArgv checks the positional shape of an httpGet health
// command and returns the in-container script for optional marker checks.
func assertHTTPProbeArgv(t *testing.T, out, directive, insecure, url string, headers ...string) string {
	t.Helper()
	argv := healthCmdArgv(t, out, directive)
	wantLen := 6 + len(headers)
	if len(argv) != wantLen {
		t.Fatalf("%s: expected %d argv entries, got %d: %v", directive, wantLen, len(argv), argv)
	}
	if argv[0] != "sh" || argv[1] != "-c" || argv[3] != "q8s-probe" {
		t.Errorf("%s: unexpected prefix: %v", directive, argv[:4])
	}
	if argv[4] != insecure {
		t.Errorf("%s: insecure flag: expected %q, got %q", directive, insecure, argv[4])
	}
	if argv[5] != url {
		t.Errorf("%s: URL: expected %q, got %q", directive, url, argv[5])
	}
	got := argv[6:]
	if len(got) != len(headers) {
		t.Errorf("%s: headers: expected %v, got %v", directive, headers, got)
	} else {
		for i := range headers {
			if got[i] != headers[i] {
				t.Errorf("%s: header %d: expected %q, got %q", directive, i, headers[i], got[i])
			}
		}
	}
	return argv[2]
}

// assertHealthTiming checks every health directive except the command lines.
func assertHealthTiming(t *testing.T, out string, want ...string) {
	t.Helper()
	var got []string
	for _, line := range healthLines(out) {
		if strings.HasPrefix(line, "HealthCmd=") || strings.HasPrefix(line, "HealthStartupCmd=") {
			continue
		}
		got = append(got, line)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("health timing lines: expected %v, got %v", want, got)
	}
}

func mustContainerErr(t *testing.T, pod *corev1.Pod) error {
	t.Helper()
	_, err := quadlet.Container(pod.Name, pod, "", nil, nil, "", nil)
	return err
}

// TestContainerLivenessExecExact: a liveness probe is a JSON exec health
// command with podman's restart-on-failure hook and the Kubernetes timing
// defaults filled in.
func TestContainerLivenessExecExact(t *testing.T) {
	pod := simplePod("default", "app", "myimage")
	pod.Spec.Containers[0].LivenessProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{
			Command: []string{"sh", "-c", "test -f /tmp/ok && echo fine"},
		}},
	}
	out := mustContainer(t, pod, "")

	assertHealthLines(t, out,
		`HealthCmd=["sh","-c","test -f /tmp/ok && echo fine"]`,
		"HealthInterval=10s",
		"HealthTimeout=1s",
		"HealthRetries=3",
		"HealthOnFailure=kill",
	)
}

// TestContainerLivenessExecSpecialChars: argument boundaries survive spaces,
// quotes and percent signs, and the value is quoted for systemd so the JSON
// reaches podman intact.
func TestContainerLivenessExecSpecialChars(t *testing.T) {
	pod := simplePod("default", "app", "myimage")
	pod.Spec.Containers[0].LivenessProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{
			Command: []string{"printf", "%s", `a "quoted" arg`},
		}},
	}
	out := mustContainer(t, pod, "")

	assertHealthLines(t, out,
		`HealthCmd=["printf","%%s","a \"quoted\" arg"]`,
		"HealthInterval=10s",
		"HealthTimeout=1s",
		"HealthRetries=3",
		"HealthOnFailure=kill",
	)
}

// TestContainerLivenessHTTPGetNamedPort: an httpGet liveness probe becomes an
// in-container sh health command, killing the container on failure.
func TestContainerLivenessHTTPGetNamedPort(t *testing.T) {
	pod := simplePod("default", "llm", "llm:latest")
	pod.Spec.Containers[0].Ports = []corev1.ContainerPort{{Name: "http", ContainerPort: 8080}}
	pod.Spec.Containers[0].LivenessProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path: "/live",
			Port: intstr.FromString("http"),
		}},
	}
	out := mustContainer(t, pod, "")

	script := assertHTTPProbeArgv(t, out, "HealthCmd", "", "http://127.0.0.1:8080/live")
	assertContains(t, script, "command -v curl")
	assertHealthTiming(t, out,
		"HealthInterval=10s",
		"HealthTimeout=1s",
		"HealthRetries=3",
		"HealthOnFailure=kill",
	)
}

// TestContainerLivenessHTTPGetHTTPSHeadersHost: HTTPS sets the insecure flag,
// headers become separate "Name: value" arguments, and an IP literal host
// replaces 127.0.0.1.
func TestContainerLivenessHTTPGetHTTPSHeadersHost(t *testing.T) {
	pod := simplePod("default", "app", "myimage")
	pod.Spec.Containers[0].LivenessProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path:   "/healthz",
			Port:   intstr.FromInt32(8443),
			Scheme: corev1.URISchemeHTTPS,
			Host:   "192.0.2.5",
			HTTPHeaders: []corev1.HTTPHeader{
				{Name: "X-Token", Value: "secret"},
				{Name: "Accept", Value: "application/json"},
			},
		}},
	}
	out := mustContainer(t, pod, "")

	script := assertHTTPProbeArgv(t, out, "HealthCmd", "1", "https://192.0.2.5:8443/healthz",
		"X-Token: secret", "Accept: application/json")
	assertContains(t, script, "command -v wget")
	assertHealthTiming(t, out,
		"HealthInterval=10s",
		"HealthTimeout=1s",
		"HealthRetries=3",
		"HealthOnFailure=kill",
	)
}

// TestContainerReadinessHTTPGet: a readiness probe is status-only; no
// HealthOnFailure means podman never restarts the container for it.
func TestContainerReadinessHTTPGet(t *testing.T) {
	pod := simplePod("default", "llm", "llm:latest")
	pod.Spec.Containers[0].Ports = []corev1.ContainerPort{{Name: "http", ContainerPort: 8080}}
	pod.Spec.Containers[0].ReadinessProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path: "/healthz",
			Port: intstr.FromInt32(8080),
		}},
		PeriodSeconds:       10,
		InitialDelaySeconds: 30,
	}
	out := mustContainer(t, pod, "")

	script := assertHTTPProbeArgv(t, out, "HealthCmd", "", "http://127.0.0.1:8080/healthz")
	assertContains(t, script, "command -v python3")
	assertHealthTiming(t, out,
		"HealthInterval=10s",
		"HealthTimeout=1s",
		"HealthRetries=3",
		"HealthStartPeriod=30s",
	)
	assertNotContains(t, out, "HealthOnFailure=")
}

func TestContainerReadinessHTTPGetNamedPort(t *testing.T) {
	pod := simplePod("default", "llm", "llm:latest")
	pod.Spec.Containers[0].Ports = []corev1.ContainerPort{{Name: "http", ContainerPort: 8080}}
	pod.Spec.Containers[0].ReadinessProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path: "/ready",
			Port: intstr.FromString("http"),
		}},
	}
	out := mustContainer(t, pod, "")

	assertHTTPProbeArgv(t, out, "HealthCmd", "", "http://127.0.0.1:8080/ready")
}

func TestContainerReadinessExec(t *testing.T) {
	pod := simplePod("default", "llm", "llm:latest")
	pod.Spec.Containers[0].ReadinessProbe = &corev1.Probe{
		ProbeHandler:        corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"cat", "/tmp/ready"}}},
		InitialDelaySeconds: 5,
	}
	out := mustContainer(t, pod, "")

	assertHealthLines(t, out,
		`HealthCmd=["cat","/tmp/ready"]`,
		"HealthInterval=10s",
		"HealthTimeout=1s",
		"HealthRetries=3",
		"HealthStartPeriod=5s",
	)
	assertNotContains(t, out, "HealthOnFailure=")
}

// TestContainerLivenessProbeWinsOverReadiness: podman has one health check;
// liveness owns it and the readiness probe is deliberately not represented.
func TestContainerLivenessProbeWinsOverReadiness(t *testing.T) {
	pod := simplePod("default", "app", "myimage")
	pod.Spec.Containers[0].LivenessProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"cat", "/tmp/live"}}},
	}
	pod.Spec.Containers[0].ReadinessProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path: "/ready",
			Port: intstr.FromInt32(8080),
		}},
	}
	out := mustContainer(t, pod, "")

	assertHealthLines(t, out,
		`HealthCmd=["cat","/tmp/live"]`,
		"HealthInterval=10s",
		"HealthTimeout=1s",
		"HealthRetries=3",
		"HealthOnFailure=kill",
	)
	assertNotContains(t, out, "curl")
}

// TestContainerStartupProbe: startup gets podman's separate startup health
// check with its own interval/timeout/retries/success keys.
func TestContainerStartupProbe(t *testing.T) {
	pod := simplePod("default", "llm", "llm:latest")
	pod.Spec.Containers[0].LivenessProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"test", "-f", "/tmp/live"}}},
	}
	pod.Spec.Containers[0].StartupProbe = &corev1.Probe{
		ProbeHandler:     corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"test", "-f", "/tmp/started"}}},
		PeriodSeconds:    5,
		TimeoutSeconds:   2,
		FailureThreshold: 30,
		SuccessThreshold: 2,
	}
	out := mustContainer(t, pod, "")

	assertHealthLines(t, out,
		`HealthCmd=["test","-f","/tmp/live"]`,
		"HealthInterval=10s",
		"HealthTimeout=1s",
		"HealthRetries=3",
		"HealthOnFailure=kill",
		`HealthStartupCmd=["test","-f","/tmp/started"]`,
		"HealthStartupInterval=5s",
		"HealthStartupTimeout=2s",
		"HealthStartupRetries=30",
		"HealthStartupSuccess=2",
	)
}

// TestContainerStartupProbeDefaults: an empty startupProbe timing block gets
// the Kubernetes defaults, including successThreshold 1.
func TestContainerStartupProbeDefaults(t *testing.T) {
	pod := simplePod("default", "llm", "llm:latest")
	pod.Spec.Containers[0].ReadinessProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"true"}}},
	}
	pod.Spec.Containers[0].StartupProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Port: intstr.FromInt32(8080),
		}},
	}
	out := mustContainer(t, pod, "")

	assertContains(t, out, `HealthCmd=["true"]`)
	assertHTTPProbeArgv(t, out, "HealthStartupCmd", "", "http://127.0.0.1:8080/")
	assertHealthTiming(t, out,
		"HealthInterval=10s",
		"HealthTimeout=1s",
		"HealthRetries=3",
		"HealthStartupInterval=10s",
		"HealthStartupTimeout=1s",
		"HealthStartupRetries=3",
		"HealthStartupSuccess=1",
	)
}

// TestContainerLivenessExplicitTiming: explicit probe timing values are used
// verbatim (seconds suffix on the duration keys).
func TestContainerLivenessExplicitTiming(t *testing.T) {
	pod := simplePod("default", "app", "myimage")
	pod.Spec.Containers[0].LivenessProbe = &corev1.Probe{
		ProbeHandler:        corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"true"}}},
		PeriodSeconds:       7,
		TimeoutSeconds:      2,
		FailureThreshold:    5,
		InitialDelaySeconds: 11,
	}
	out := mustContainer(t, pod, "")

	assertHealthLines(t, out,
		`HealthCmd=["true"]`,
		"HealthInterval=7s",
		"HealthTimeout=2s",
		"HealthRetries=5",
		"HealthStartPeriod=11s",
		"HealthOnFailure=kill",
	)
}

func TestContainerNoReadinessProbe(t *testing.T) {
	pod := simplePod("default", "app", "myimage")
	pod.Spec.Containers[0].ReadinessProbe = nil
	out := mustContainer(t, pod, "")

	assertNotContains(t, out, "HealthCmd=")
}

// TestContainerProbeErrors covers every unsupported probe shape: none may be
// silently dropped.
func TestContainerProbeErrors(t *testing.T) {
	tests := []struct {
		name  string
		probe func(*corev1.Container)
	}{
		{
			name: "tcpSocket",
			probe: func(c *corev1.Container) {
				c.LivenessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
					TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(80)},
				}}
			},
		},
		{
			name: "grpc",
			probe: func(c *corev1.Container) {
				c.ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
					GRPC: &corev1.GRPCAction{Port: 50051},
				}}
			},
		},
		{
			name: "unresolved named port",
			probe: func(c *corev1.Container) {
				c.ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{Path: "/ready", Port: intstr.FromString("missing")},
				}}
			},
		},
		{
			name: "header name with space",
			probe: func(c *corev1.Container) {
				c.LivenessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{
						Port:        intstr.FromInt32(80),
						HTTPHeaders: []corev1.HTTPHeader{{Name: "Bad Header", Value: "x"}},
					},
				}}
			},
		},
		{
			name: "header value with newline",
			probe: func(c *corev1.Container) {
				c.LivenessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{
						Port:        intstr.FromInt32(80),
						HTTPHeaders: []corev1.HTTPHeader{{Name: "X-Test", Value: "a\r\nInjected: yes"}},
					},
				}}
			},
		},
		{
			name: "hostname host",
			probe: func(c *corev1.Container) {
				c.LivenessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromInt32(80), Host: "example.com"},
				}}
			},
		},
		{
			name: "startup alone",
			probe: func(c *corev1.Container) {
				c.StartupProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
					Exec: &corev1.ExecAction{Command: []string{"true"}},
				}}
			},
		},
		{
			name: "empty exec command",
			probe: func(c *corev1.Container) {
				c.LivenessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
					Exec: &corev1.ExecAction{},
				}}
			},
		},
		{
			name: "empty handler",
			probe: func(c *corev1.Container) {
				c.LivenessProbe = &corev1.Probe{}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := simplePod("default", "app", "myimage")
			tt.probe(&pod.Spec.Containers[0])
			if err := mustContainerErr(t, pod); err == nil {
				t.Fatal("expected an error, got none")
			}
		})
	}
}

func TestContainerGPUDevice(t *testing.T) {
	pod := simplePod("default", "llm", "llm:latest")
	pod.Spec.Containers[0].Resources.Limits = corev1.ResourceList{
		quadlet.GPULimitResource: resource.MustParse("2"),
	}
	out := mustContainer(t, pod, "")

	assertContains(t, out, "AddDevice=nvidia.com/gpu=all")
}

func TestContainerNoGPUDevice(t *testing.T) {
	for _, value := range []string{"", "0"} {
		pod := simplePod("default", "llm", "llm:latest")
		if value != "" {
			pod.Spec.Containers[0].Resources.Limits = corev1.ResourceList{
				quadlet.GPULimitResource: resource.MustParse(value),
			}
		}
		out := mustContainer(t, pod, "")
		assertNotContains(t, out, "AddDevice=")
	}
}

func TestContainerShmEmptyDir(t *testing.T) {
	pod := simplePod("default", "llm", "llm:latest")
	pod.Spec.Volumes = []corev1.Volume{{
		Name: "shm",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
			Medium:    corev1.StorageMediumMemory,
			SizeLimit: quantityPtr("1Gi"),
		}},
	}}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "shm", MountPath: "/dev/shm"}}
	out := mustContainer(t, pod, "")

	assertContains(t, out, "ShmSize=1073741824")
	// Podman owns /dev/shm; it must not also be a Volume= mount.
	assertNotContains(t, out, "Volume=shm:/dev/shm")
}

func TestContainerShmEmptyDirNoSizeLimit(t *testing.T) {
	pod := simplePod("default", "llm", "llm:latest")
	pod.Spec.Volumes = []corev1.Volume{{
		Name:         "shm",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}},
	}}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "shm", MountPath: "/dev/shm"}}
	out := mustContainer(t, pod, "")

	assertNotContains(t, out, "ShmSize=")
	assertNotContains(t, out, "Volume=shm:/dev/shm")
}

func TestContainerOrdinaryEmptyDirKeepsVolumeLine(t *testing.T) {
	pod := simplePod("default", "app", "myimage")
	pod.Spec.Volumes = []corev1.Volume{{
		Name: "scratch",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
			SizeLimit: quantityPtr("1Gi"),
		}},
	}}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "scratch", MountPath: "/scratch"}}
	out := mustContainer(t, pod, "")

	assertContains(t, out, "Volume=scratch:/scratch")
	assertNotContains(t, out, "ShmSize=")
}

func TestContainerUlimits(t *testing.T) {
	pod := simplePod("default", "llm", "llm:latest")
	pod.Annotations = map[string]string{
		quadlet.AnnotationUlimits: "memlock=-1:-1,nofile=65536",
	}
	out := mustContainer(t, pod, "")

	assertContains(t, out, "Ulimit=memlock=-1:-1")
	assertContains(t, out, "Ulimit=nofile=65536")
}

func TestContainerNoUlimits(t *testing.T) {
	pod := simplePod("default", "app", "myimage")
	out := mustContainer(t, pod, "")

	assertNotContains(t, out, "Ulimit=")
}

func TestContainerRejectsInvalidUlimits(t *testing.T) {
	for _, value := range []string{
		"memlock=abc",
		"Memlock=-1:-1",
		"memlock=-1:-1,nofile=1\n\n[Service]\nExecStartPre=/bin/evil",
	} {
		pod := simplePod("default", "llm", "llm:latest")
		pod.Annotations = map[string]string{quadlet.AnnotationUlimits: value}
		if _, err := quadlet.Container(pod.Name, pod, "", nil, nil, "", nil); err == nil {
			t.Errorf("expected error for q8s.io/ulimits %q", value)
		}
	}
}

func TestContainerNoContainersError(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "empty", Namespace: "default"},
		Spec:       corev1.PodSpec{},
	}
	_, err := quadlet.Container(pod.Name, pod, "", nil, nil, "", nil)
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

func TestJobContainerPodTemplateAnnotations(t *testing.T) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "ns"},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				quadlet.AnnotationUlimits: "memlock=-1:-1",
			}},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name:  "c",
				Image: "img",
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{quadlet.GPULimitResource: resource.MustParse("1")},
				},
				ReadinessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"true"}}},
				},
			}}},
		}},
	}
	b, err := quadlet.JobContainer(job.Name, job, "", nil, "")
	if err != nil {
		t.Fatalf("JobContainer: %v", err)
	}
	out := string(b)

	assertContains(t, out, "Ulimit=memlock=-1:-1")
	assertContains(t, out, "AddDevice=nvidia.com/gpu=all")
	assertContains(t, out, `HealthCmd=["true"]`)
}

func TestJobContainerReadOnlyPVC(t *testing.T) {
	sc := "standard"
	pvcMap := map[string]*corev1.PersistentVolumeClaim{
		"my-pvc": {
			ObjectMeta: metav1.ObjectMeta{Name: "my-pvc", Namespace: "ns"},
			Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: &sc},
		},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "ns"},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:         "c",
					Image:        "img",
					VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/data", ReadOnly: true}},
				}},
				Volumes: []corev1.Volume{{
					Name:         "data",
					VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "my-pvc"}},
				}},
			},
		}},
	}
	b, err := quadlet.JobContainer(job.Name, job, "", pvcMap, "")
	if err != nil {
		t.Fatalf("JobContainer: %v", err)
	}
	assertContains(t, string(b), "Volume=ns-my-pvc.volume:/data:ro,Z")
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

func TestCronContainerReadOnlyEmptyDir(t *testing.T) {
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"},
		Spec: batchv1.CronJobSpec{
			Schedule: "* * * * *",
			JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{
				Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:         "c",
						Image:        "img",
						VolumeMounts: []corev1.VolumeMount{{Name: "scratch", MountPath: "/scratch", ReadOnly: true}},
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
	assertContains(t, string(b), "Volume=scratch:/scratch:ro")
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

// TestCronTimerTimeZone pins the systemd >= 235 zone suffix. It is appended
// to every OnCalendar= line, including both lines of the day-of-month/day-of-
// week union.
func TestCronTimerTimeZone(t *testing.T) {
	zone := "Europe/Belgrade"
	tests := []struct {
		name     string
		schedule string
		timeZone *string
		want     []string
	}{
		{
			name:     "single expression",
			schedule: "34 0 * * *",
			timeZone: &zone,
			want:     []string{"OnCalendar=*-*-* 0:34:00 Europe/Belgrade\n"},
		},
		{
			name:     "both day fields restricted",
			schedule: "34 0 1 * 1",
			timeZone: &zone,
			want: []string{
				"OnCalendar=1 *-*-* 0:34:00 Europe/Belgrade\n",
				"OnCalendar=*-*-1 0:34:00 Europe/Belgrade\n",
			},
		},
		{
			name:     "unset",
			schedule: "34 0 * * *",
			want:     []string{"OnCalendar=*-*-* 0:34:00\n"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cj := &batchv1.CronJob{
				ObjectMeta: metav1.ObjectMeta{Name: "zoned", Namespace: "default"},
				Spec:       batchv1.CronJobSpec{Schedule: tt.schedule, TimeZone: tt.timeZone},
			}
			b, err := quadlet.CronTimer(cj.Name, cj)
			if err != nil {
				t.Fatalf("CronTimer: %v", err)
			}
			out := string(b)
			for _, want := range tt.want {
				assertContains(t, out, want)
			}
			if got, want := strings.Count(out, "OnCalendar="), len(tt.want); got != want {
				t.Errorf("got %d OnCalendar lines, want %d:\n%s", got, want, out)
			}
			if tt.timeZone != nil {
				if got := strings.Count(out, "Europe/Belgrade"); got != len(tt.want) {
					t.Errorf("zone appears %d times, want %d:\n%s", got, len(tt.want), out)
				}
			}
		})
	}
}

// An explicitly empty spec.timeZone means unset, so it must render exactly
// like an absent one.
func TestCronTimerTimeZoneEmptyIsUnset(t *testing.T) {
	empty := ""
	spec := batchv1.CronJobSpec{Schedule: "34 0 * * *"}
	unset := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"}, Spec: spec}
	spec.TimeZone = &empty
	withEmpty := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"}, Spec: spec}

	unsetOut, err := quadlet.CronTimer(unset.Name, unset)
	if err != nil {
		t.Fatalf("CronTimer unset: %v", err)
	}
	emptyOut, err := quadlet.CronTimer(withEmpty.Name, withEmpty)
	if err != nil {
		t.Fatalf("CronTimer empty: %v", err)
	}
	if string(unsetOut) != string(emptyOut) {
		t.Errorf("empty timeZone changed the timer output:\nunset:\n%s\nempty:\n%s", unsetOut, emptyOut)
	}
}

// CronTimer renders objects loaded from disk, which never passed API
// validation, so an invalid zone is dropped instead of written.
func TestCronTimerDropsInvalidTimeZone(t *testing.T) {
	for _, zone := range []string{"Mars/Phobos", "Local", "Europe/Belgrade\nX", "../etc"} {
		t.Run(zone, func(t *testing.T) {
			invalid := zone
			cj := &batchv1.CronJob{
				ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"},
				Spec:       batchv1.CronJobSpec{Schedule: "34 0 * * *", TimeZone: &invalid},
			}
			b, err := quadlet.CronTimer(cj.Name, cj)
			if err != nil {
				t.Fatalf("CronTimer: %v", err)
			}
			out := string(b)
			assertContains(t, out, "OnCalendar=*-*-* 0:34:00\n")
			assertNotContains(t, out, zone)
		})
	}
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

		// day-of-week: cron 0-6 (0=Sun) maps to systemd 1-7 (1=Mon, 7=Sun)
		{"30 2 * * 6", "OnCalendar=6 *-*-* 2:30:00"},         // Saturday
		{"0 0 * * 1", "OnCalendar=1 *-*-* 0:0:00"},           // Monday
		{"0 0 * * 0", "OnCalendar=7 *-*-* 0:0:00"},           // Sunday via 0
		{"0 0 * * 7", "OnCalendar=7 *-*-* 0:0:00"},           // Sunday via 7
		{"0 0 * * 1-5", "OnCalendar=1,2,3,4,5 *-*-* 0:0:00"}, // weekdays
		{"0 0 * * */2", "OnCalendar=2,4,6,7 *-*-* 0:0:00"},   // 0,2,4,6 → Sun,Tue,Thu,Sat

		// date fields expand to lists (1-based, so no 0/N steps)
		{"0 0 */2 * *", "OnCalendar=*-*-1,3,5,7,9,11,13,15,17,19,21,23,25,27,29,31 0:0:00"},
		{"0 0 * */2 *", "OnCalendar=*-1,3,5,7,9,11-* 0:0:00"},
		{"0 0 1,15 * *", "OnCalendar=*-*-1,15 0:0:00"},

		// dom+dow both restricted: cron ORs, so two OnCalendar lines
		{"0 0 1 * 1", "OnCalendar=1 *-*-* 0:0:00\nOnCalendar=*-*-1 0:0:00"},
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
	b, err := quadlet.Container(pod.Name, pod, "", nil, pvcMap, "", nil)
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
	b, err := quadlet.Container(pod.Name, pod, "", nil, pvcMap, "", nil)
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
	b, err := quadlet.Container(pod.Name, pod, "", nil, pvcMap, "", nil)
	if err != nil {
		t.Fatalf("Container: %v", err)
	}
	out := string(b)
	assertContains(t, out, "Volume=/srv/data:/data:Z")
	assertNotContains(t, out, ".volume")
}

func TestContainerPVCHostpathSharedSELinuxLabel(t *testing.T) {
	sc := "hostpath"
	pvcMap := map[string]*corev1.PersistentVolumeClaim{
		"hp": {
			ObjectMeta: metav1.ObjectMeta{Name: "hp", Namespace: "default",
				Annotations: map[string]string{
					"q8s.io/host-path":     "/srv/models",
					"q8s.io/selinux-label": "shared",
				}},
			Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: &sc},
		},
	}
	pod := simplePod("default", "app", "myimage")
	pod.Spec.Volumes = []corev1.Volume{{
		Name:         "data",
		VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "hp"}},
	}}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: "/models", ReadOnly: true}}
	b, err := quadlet.Container(pod.Name, pod, "", nil, pvcMap, "", nil)
	if err != nil {
		t.Fatalf("Container: %v", err)
	}
	assertContains(t, string(b), "Volume=/srv/models:/models:ro,z")
}

func TestContainerPVCHostpathReadOnly(t *testing.T) {
	sc := "hostpath"
	pvcMap := map[string]*corev1.PersistentVolumeClaim{
		"hp": {
			ObjectMeta: metav1.ObjectMeta{Name: "hp", Namespace: "default",
				Annotations: map[string]string{
					"q8s.io/host-path":     "/srv/models",
					"q8s.io/selinux-label": "private",
				}},
			Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: &sc},
		},
	}
	pod := simplePod("default", "app", "myimage")
	pod.Spec.Volumes = []corev1.Volume{{
		Name:         "data",
		VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "hp"}},
	}}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: "/models", ReadOnly: true}}
	b, err := quadlet.Container(pod.Name, pod, "", nil, pvcMap, "", nil)
	if err != nil {
		t.Fatalf("Container: %v", err)
	}
	assertContains(t, string(b), "Volume=/srv/models:/models:ro,Z")
}

func TestContainerPVCVolumeSourceReadOnly(t *testing.T) {
	sc := "standard"
	pvcMap := map[string]*corev1.PersistentVolumeClaim{
		"my-pvc": {
			ObjectMeta: metav1.ObjectMeta{Name: "my-pvc", Namespace: "default"},
			Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: &sc},
		},
	}
	for _, tc := range []struct {
		name     string
		readOnly bool
		want     string
	}{
		{"read-only", true, "Volume=default-my-pvc.volume:/data:ro,Z"},
		{"read-write", false, "Volume=default-my-pvc.volume:/data:Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := simplePod("default", "app", "myimage")
			pod.Spec.Volumes = []corev1.Volume{{
				Name: "data",
				VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: "my-pvc",
					ReadOnly:  tc.readOnly,
				}},
			}}
			pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: "/data"}}
			b, err := quadlet.Container(pod.Name, pod, "", nil, pvcMap, "", nil)
			if err != nil {
				t.Fatalf("Container: %v", err)
			}
			assertContains(t, string(b), tc.want)
		})
	}
}

func TestContainerPVCFallbackReadOnly(t *testing.T) {
	pod := simplePod("default", "app", "myimage")
	pod.Spec.Volumes = []corev1.Volume{{
		Name: "data",
		VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
			ClaimName: "my-pvc",
			ReadOnly:  true,
		}},
	}}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: "/data"}}
	out := mustContainer(t, pod, "")
	assertContains(t, out, "Volume=default-my-pvc.volume:/data:ro,Z")
}

func TestContainerPVCUnknownClassReadOnly(t *testing.T) {
	sc := "custom"
	pvcMap := map[string]*corev1.PersistentVolumeClaim{
		"odd-vol": {
			ObjectMeta: metav1.ObjectMeta{Name: "odd-vol", Namespace: "default"},
			Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: &sc},
		},
	}
	for _, tc := range []struct {
		name     string
		readOnly bool
		want     string
	}{
		{"read-only", true, "Volume=default-odd-vol.volume:/data:ro"},
		{"read-write", false, "Volume=default-odd-vol.volume:/data\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := simplePod("default", "app", "myimage")
			pod.Spec.Volumes = []corev1.Volume{{
				Name:         "data",
				VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "odd-vol"}},
			}}
			pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: "/data", ReadOnly: tc.readOnly}}
			b, err := quadlet.Container(pod.Name, pod, "", nil, pvcMap, "", nil)
			if err != nil {
				t.Fatalf("Container: %v", err)
			}
			assertContains(t, string(b), tc.want)
		})
	}
}

func TestContainerEmptyDirReadOnly(t *testing.T) {
	pod := simplePod("default", "app", "myimage")
	pod.Spec.Volumes = []corev1.Volume{{
		Name:         "scratch",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "scratch", MountPath: "/scratch", ReadOnly: true}}
	out := mustContainer(t, pod, "")
	assertContains(t, out, "Volume=scratch:/scratch:ro")
}

func TestContainerPVCHostpathPrivateSELinuxLabel(t *testing.T) {
	sc := "hostpath"
	pvcMap := map[string]*corev1.PersistentVolumeClaim{
		"hp": {
			ObjectMeta: metav1.ObjectMeta{Name: "hp", Namespace: "default",
				Annotations: map[string]string{
					"q8s.io/host-path":     "/srv/models",
					"q8s.io/selinux-label": "private",
				}},
			Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: &sc},
		},
	}
	pod := simplePod("default", "app", "myimage")
	pod.Spec.Volumes = []corev1.Volume{{
		Name:         "data",
		VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "hp"}},
	}}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: "/models"}}
	b, err := quadlet.Container(pod.Name, pod, "", nil, pvcMap, "", nil)
	if err != nil {
		t.Fatalf("Container: %v", err)
	}
	assertContains(t, string(b), "Volume=/srv/models:/models:Z")
}

func TestContainerPVCHostpathInvalidSELinuxLabel(t *testing.T) {
	for _, value := range []string{"world", "private\n[Service]\nExecStartPre=/bin/evil"} {
		sc := "hostpath"
		pvcMap := map[string]*corev1.PersistentVolumeClaim{
			"hp": {
				ObjectMeta: metav1.ObjectMeta{Name: "hp", Namespace: "default",
					Annotations: map[string]string{
						"q8s.io/host-path":     "/srv/models",
						"q8s.io/selinux-label": value,
					}},
				Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: &sc},
			},
		}
		pod := simplePod("default", "app", "myimage")
		pod.Spec.Volumes = []corev1.Volume{{
			Name:         "data",
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "hp"}},
		}}
		pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: "/models"}}
		if _, err := quadlet.Container(pod.Name, pod, "", nil, pvcMap, "", nil); err == nil {
			t.Errorf("expected error for q8s.io/selinux-label %q", value)
		}
	}
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
	_, err := quadlet.Container(pod.Name, pod, "", nil, pvcMap, "", nil)
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
	b, err := quadlet.Container(pod.Name, pod, "", nil, pvcMap, "", nil)
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
	if _, err := quadlet.Container(pod.Name, pod, "", nil, nil, "", nil); err == nil {
		t.Fatal("expected error for image containing embedded unit-file directives, got nil")
	}
}

func TestContainerRejectsNewlineInWorkingDir(t *testing.T) {
	pod := simplePod("default", "victim", "nginx:latest")
	pod.Spec.Containers[0].WorkingDir = "/app\n\n[Service]\nExecStartPre=/bin/touch /tmp/pwned"
	if _, err := quadlet.Container(pod.Name, pod, "", nil, nil, "", nil); err == nil {
		t.Fatal("expected error for WorkingDir containing embedded unit-file directives, got nil")
	}
}

func TestContainerRejectsNewlineInEnvValue(t *testing.T) {
	pod := simplePod("default", "victim", "nginx:latest")
	pod.Spec.Containers[0].Env = []corev1.EnvVar{
		{Name: "FOO", Value: "bar\n\n[Service]\nExecStartPre=/bin/touch /tmp/pwned"},
	}
	if _, err := quadlet.Container(pod.Name, pod, "", nil, nil, "", nil); err == nil {
		t.Fatal("expected error for env value containing embedded unit-file directives, got nil")
	}
}

func TestContainerRejectsInvalidEnvName(t *testing.T) {
	pod := simplePod("default", "victim", "nginx:latest")
	pod.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "not a valid name", Value: "x"}}
	if _, err := quadlet.Container(pod.Name, pod, "", nil, nil, "", nil); err == nil {
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
	if _, err := quadlet.Container(pod.Name, pod, "", nil, nil, "", nil); err == nil {
		t.Fatal("expected error for PVC claim name containing embedded unit-file directives, got nil")
	}
}

func TestContainerRejectsNewlineInCommand(t *testing.T) {
	pod := simplePod("default", "victim", "nginx:latest")
	pod.Spec.Containers[0].Command = []string{"echo\n\n[Service]\nExecStartPre=/bin/touch /tmp/pwned"}
	if _, err := quadlet.Container(pod.Name, pod, "", nil, nil, "", nil); err == nil {
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
	assertContains(t, out, "PodmanArgs=--memory=536870912 --memory-swap=-1")
	assertContains(t, out, "PodmanArgs=--cpus=0.5")
	// Memory must NOT use the native quadlet key — older podman rejects it.
	assertNotContains(t, out, "Memory=536870912")
	assertNotContains(t, out, "\nMemory=")
}

func TestContainerNoResourceLimits(t *testing.T) {
	restore := quadlet.SetCgroupOverride(true, true)
	defer restore()

	pod := simplePod("default", "app", "nginx:latest")
	out := mustContainer(t, pod, "")
	assertNotContains(t, out, "Memory=")
	assertNotContains(t, out, "--memory")
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
	assertNotContains(t, out, "--memory")
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

// TestCronTimerRejectsOutOfRangeFields pins the up-front range validation:
// "99 25 * * *" is not a valid cron schedule and must fail at API time, not
// surface later as an opaque systemd timer load error at daemon-reload.
func TestCronTimerRejectsOutOfRangeFields(t *testing.T) {
	for _, schedule := range []string{
		"60 * * * *",   // minute > 59
		"* 24 * * *",   // hour > 23
		"* * 32 * *",   // dom > 31
		"* * * 13 *",   // month > 12
		"* * * * 8",    // dow > 7
		"5-70 * * * *", // range endpoint out of bounds
		"10-5 * * * *", // descending range
		"*/0 * * * *",  // zero step
		"1,,2 * * * *", // empty list element
	} {
		cj := &batchv1.CronJob{
			ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"},
			Spec:       batchv1.CronJobSpec{Schedule: schedule},
		}
		if _, err := quadlet.CronTimer(cj.Name, cj); err == nil {
			t.Errorf("schedule %q: expected error, got none", schedule)
		}
	}
}

// TestContainerPublishPortHonorsHostIP: an explicit HostIP binds the
// published port to one interface (the replica allocator uses 127.0.0.1);
// without it the publish binds all interfaces, like k8s hostPort.
func TestContainerPublishPortHonorsHostIP(t *testing.T) {
	mkPod := func(hostIP string) *corev1.Pod {
		pod := simplePod("default", "web", "nginx:latest")
		pod.Spec.Containers[0].Ports = []corev1.ContainerPort{{
			ContainerPort: 8080,
			HostPort:      20000,
			HostIP:        hostIP,
		}}
		return pod
	}

	out, err := quadlet.Container("web", mkPod("127.0.0.1"), "", nil, nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, string(out), "PublishPort=127.0.0.1:20000:8080/tcp")

	out, err = quadlet.Container("web", mkPod(""), "", nil, nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, string(out), "PublishPort=20000:8080/tcp")
	if strings.Contains(string(out), "127.0.0.1:20000") {
		t.Errorf("unexpected HostIP prefix without HostIP set:\n%s", out)
	}
}

// --- generator name validation (import-path injection barrier) ---

func TestContainerRejectsUnsafeNameAndNamespace(t *testing.T) {
	// A newline in a label-derived pod name would open a new unit-file
	// line/section; "../" traverses the output filename. The API boundary
	// rejects these, but the Podman-label import path doesn't — the
	// generator is the last line of defense.
	for _, name := range []string{
		"x\n[Service]\nExecStartPre=evil",
		"../escape",
		"-leading-dash",
		"UpperCase",
		"trailing-",
	} {
		pod := simplePod("default", name, "nginx:latest")
		if _, err := quadlet.Container(name, pod, "", nil, nil, "", nil); err == nil {
			t.Errorf("Container accepted unsafe name %q", name)
		}
	}
	pod := simplePod("bad ns", "ok", "nginx:latest")
	if _, err := quadlet.Container("ok", pod, "", nil, nil, "", nil); err == nil {
		t.Error("Container accepted unsafe namespace")
	}
}

func TestJobContainerRejectsUnsafeName(t *testing.T) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "x\nExecStart=evil", Namespace: "default"},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c", Image: "busybox"}},
		}}},
	}
	if _, err := quadlet.JobContainer(job.Name, job, "", nil, ""); err == nil {
		t.Error("JobContainer accepted unsafe name")
	}
}

func TestCronContainerRejectsUnsafeName(t *testing.T) {
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "../../evil", Namespace: "default"},
		Spec: batchv1.CronJobSpec{JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "c", Image: "busybox"}},
			}},
		}}},
	}
	if _, err := quadlet.CronContainer(cj.Name, cj, "", nil, ""); err == nil {
		t.Error("CronContainer accepted unsafe name")
	}
}

// labelValue returns the value of an exact Label=<key>= line, or "".
func labelValue(out, key string) string {
	prefix := "Label=" + key + "="
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix)
		}
	}
	return ""
}

// TestManagedByLabelOnEveryGeneratedKind pins the ownership marker the server
// relies on to distinguish its own quadlet files from hand-written ones.
func TestManagedByLabelOnEveryGeneratedKind(t *testing.T) {
	pod := simplePod("default", "app", "nginx")
	if out := mustContainer(t, pod, ""); !strings.Contains(out, quadlet.ManagedByLabel) {
		t.Errorf("Container output missing %q:\n%s", quadlet.ManagedByLabel, out)
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "default"},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c", Image: "busybox"}},
		}}},
	}
	jobOut, err := quadlet.JobContainer(job.Name, job, "", nil, "")
	if err != nil {
		t.Fatalf("JobContainer: %v", err)
	}
	if !strings.Contains(string(jobOut), quadlet.ManagedByLabel) {
		t.Errorf("JobContainer output missing %q:\n%s", quadlet.ManagedByLabel, jobOut)
	}

	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "cj", Namespace: "default"},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 3 * * *",
			JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{
				Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "c", Image: "busybox"}},
				}},
			}},
		},
	}
	cronOut, err := quadlet.CronContainer(cj.Name, cj, "", nil, "")
	if err != nil {
		t.Fatalf("CronContainer: %v", err)
	}
	if !strings.Contains(string(cronOut), quadlet.ManagedByLabel) {
		t.Errorf("CronContainer output missing %q:\n%s", quadlet.ManagedByLabel, cronOut)
	}

	vol, err := quadlet.Volume(&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: "default"}})
	if err != nil {
		t.Fatalf("Volume: %v", err)
	}
	if !strings.Contains(string(vol), quadlet.ManagedByLabel) {
		t.Errorf("Volume output missing %q:\n%s", quadlet.ManagedByLabel, vol)
	}

	net, err := quadlet.Network("default")
	if err != nil {
		t.Fatalf("Network: %v", err)
	}
	if !strings.Contains(string(net), quadlet.ManagedByLabel) {
		t.Errorf("Network output missing %q:\n%s", quadlet.ManagedByLabel, net)
	}

	// A .timer has no Label key, so it carries the same marker as a comment.
	timer, err := quadlet.CronTimer(cj.Name, cj)
	if err != nil {
		t.Fatalf("CronTimer: %v", err)
	}
	if !strings.Contains(string(timer), "# app.kubernetes.io/managed-by=q8s") {
		t.Errorf("CronTimer output missing managed-by comment:\n%s", timer)
	}
}

// TestContainerTemplateHash checks the hash is present, stable for equal
// input, and changes when the rendered unit changes.
func TestContainerTemplateHash(t *testing.T) {
	pod := simplePod("default", "app", "nginx:1")
	first := mustContainer(t, pod, "")
	second := mustContainer(t, pod.DeepCopy(), "")

	hash := labelValue(first, quadlet.TemplateHashLabel)
	if len(hash) != 12 {
		t.Fatalf("template hash = %q, want 12 hex chars", hash)
	}
	if !strings.Contains(second, "Label="+quadlet.TemplateHashLabel+"="+hash) {
		t.Fatalf("same input produced a different hash:\nfirst:\n%s\nsecond:\n%s", first, second)
	}

	changed := pod.DeepCopy()
	changed.Spec.Containers[0].Image = "nginx:2"
	if got := labelValue(mustContainer(t, changed, ""), quadlet.TemplateHashLabel); got == hash {
		t.Fatalf("changed input kept the same hash %q", hash)
	}
}
