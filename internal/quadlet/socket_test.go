package quadlet_test

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"q8s/internal/quadlet"
)

func TestParseSocketActivation(t *testing.T) {
	longName := strings.Repeat("a", 65)
	tests := []struct {
		name    string
		value   string
		want    []quadlet.SocketEntry
		wantErr bool
	}{
		{
			name:  "all addresses",
			value: "web=80,websecure=443",
			want: []quadlet.SocketEntry{
				{Name: "web", Port: 80},
				{Name: "websecure", Port: 443},
			},
		},
		{
			name:  "IPv4 address and whitespace",
			value: " web=127.0.0.1:8080 , websecure=443 ",
			want: []quadlet.SocketEntry{
				{Name: "web", Addr: "127.0.0.1", Port: 8080},
				{Name: "websecure", Port: 443},
			},
		},
		{
			name:  "IPv6 address",
			value: "web=[::1]:8080",
			want: []quadlet.SocketEntry{
				{Name: "web", Addr: "::1", Port: 8080},
			},
		},
		{name: "empty value", value: "", wantErr: true},
		{name: "empty entry", value: "web=80,", wantErr: true},
		{name: "missing equals", value: "web", wantErr: true},
		{name: "empty name", value: "=80", wantErr: true},
		{name: "name starts with dash", value: "-web=80", wantErr: true},
		{name: "name with space", value: "web name=80", wantErr: true},
		{name: "name too long", value: longName + "=80", wantErr: true},
		{name: "duplicate name", value: "web=80,web=81", wantErr: true},
		{name: "duplicate listen", value: "a=80,b=80", wantErr: true},
		{name: "port zero", value: "web=0", wantErr: true},
		{name: "port too high", value: "web=65536", wantErr: true},
		{name: "port not a number", value: "web=http", wantErr: true},
		{name: "hostname instead of IP", value: "web=example.com:80", wantErr: true},
		{name: "bracketed IPv4", value: "web=[127.0.0.1]:80", wantErr: true},
		{name: "unterminated IPv6", value: "web=[::1:80", wantErr: true},
		{name: "IPv6 without brackets", value: "web=::1:80", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := quadlet.ParseSocketActivation(map[string]string{quadlet.AnnotationSocketActivation: tt.value})
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got %v", tt.value, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSocketActivation(%q): %v", tt.value, err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("entry %d = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestParseSocketActivationAbsent(t *testing.T) {
	got, err := quadlet.ParseSocketActivation(nil)
	if err != nil || got != nil {
		t.Fatalf("absent annotation = %v, %v; want nil, nil", got, err)
	}
}

func TestValidateSocketActivationRejectsHostPort(t *testing.T) {
	pod := simplePod("default", "traefik", "traefik:v3")
	pod.Annotations = map[string]string{quadlet.AnnotationSocketActivation: "web=80"}
	if err := quadlet.ValidateSocketActivation(pod); err != nil {
		t.Fatalf("no hostPort yet: %v", err)
	}
	pod.Spec.Containers[0].Ports = []corev1.ContainerPort{{ContainerPort: 8080, HostPort: 80}}
	if err := quadlet.ValidateSocketActivation(pod); err == nil {
		t.Fatal("expected error when a socket port equals a hostPort")
	}
}

func TestSocketUnitsExactOutput(t *testing.T) {
	files, err := quadlet.SocketUnits("ingress", "traefik", []quadlet.SocketEntry{
		{Name: "web", Port: 80},
		{Name: "websecure", Addr: "127.0.0.1", Port: 443},
		{Name: "tls", Addr: "::1", Port: 8443},
	})
	if err != nil {
		t.Fatalf("SocketUnits: %v", err)
	}
	if len(files) != 3 {
		t.Fatalf("expected 3 files, got %d: %v", len(files), keys(files))
	}

	wantWeb := `[Unit]
Description=q8s socket web for pod ingress/traefik
X-Q8s-Namespace=ingress
X-Q8s-Pod=traefik

[Socket]
ListenStream=80
FileDescriptorName=web
Service=ingress-traefik.service

[Install]
WantedBy=sockets.target
`
	if got := string(files["ingress-traefik-web.socket"]); got != wantWeb {
		t.Errorf("web socket unit = %q, want %q", got, wantWeb)
	}
	for filename, want := range map[string]string{
		"ingress-traefik-websecure.socket": "ListenStream=127.0.0.1:443\n",
		"ingress-traefik-tls.socket":       "ListenStream=[::1]:8443\n",
	} {
		if got := string(files[filename]); !strings.Contains(got, want) {
			t.Errorf("%s missing %q:\n%s", filename, want, got)
		}
	}
}

func TestSocketUnitsRejectsInvalidEntries(t *testing.T) {
	if _, err := quadlet.SocketUnits("bad ns", "pod", []quadlet.SocketEntry{{Name: "web", Port: 80}}); err == nil {
		t.Error("expected error for invalid namespace")
	}
	if _, err := quadlet.SocketUnits("default", "bad name", []quadlet.SocketEntry{{Name: "web", Port: 80}}); err == nil {
		t.Error("expected error for invalid pod name")
	}
	if _, err := quadlet.SocketUnits("default", "pod", []quadlet.SocketEntry{{Name: "bad name", Port: 80}}); err == nil {
		t.Error("expected error for invalid socket name")
	}
	if _, err := quadlet.SocketUnits("default", "pod", []quadlet.SocketEntry{{Name: "web", Port: 0}}); err == nil {
		t.Error("expected error for invalid port")
	}
	if _, err := quadlet.SocketUnits("default", "pod", []quadlet.SocketEntry{{Name: "web", Addr: "example.com", Port: 80}}); err == nil {
		t.Error("expected error for hostname listen address")
	}
	if _, err := quadlet.SocketUnits("default", "pod", []quadlet.SocketEntry{
		{Name: "web", Port: 80}, {Name: "web", Port: 81},
	}); err == nil {
		t.Error("expected error for duplicate socket name")
	}
	files, err := quadlet.SocketUnits("default", "pod", nil)
	if err != nil || len(files) != 0 {
		t.Errorf("no entries = %v, %v; want empty map, nil", files, err)
	}
}

func TestContainerSocketActivationUnitDependencies(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "traefik",
			Namespace: "ingress",
			Annotations: map[string]string{
				quadlet.AnnotationSocketActivation: "web=80,websecure=[::1]:443",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "traefik", Image: "traefik:v3"}}},
	}
	out, err := quadlet.Container("traefik", pod, "", nil, nil, "", nil)
	if err != nil {
		t.Fatalf("Container: %v", err)
	}
	want := "After=q8s-api.service\n" +
		"Requires=ingress-traefik-web.socket\n" +
		"After=ingress-traefik-web.socket\n" +
		"Requires=ingress-traefik-websecure.socket\n" +
		"After=ingress-traefik-websecure.socket\n"
	if !strings.Contains(string(out), want) {
		t.Fatalf("socket unit dependencies missing:\nwant:\n%s\ngot:\n%s", want, out)
	}
}
