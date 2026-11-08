package hostcheck

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

type cmdResult struct {
	out string
	err error
}

type fake struct {
	files     map[string][]byte
	cmds      map[string]cmdResult
	fs        map[string]string
	writable  map[string]bool
	lingerOn  bool
	lingerErr error
	env       Env
}

func cmdKey(name string, args ...string) string {
	return strings.Join(append([]string{name}, args...), " ")
}

func (f *fake) setCmd(out string, err error, name string, args ...string) {
	f.cmds[cmdKey(name, args...)] = cmdResult{out: out, err: err}
}

func newFake(t *testing.T, rootful bool) *fake {
	t.Helper()
	f := &fake{}
	var dirs Dirs
	if rootful {
		dirs = Dirs{
			Data:    "/etc/q8s",
			Quadlet: "/etc/containers/systemd",
			Config:  "/run/q8s/configmaps",
			Secret:  "/run/q8s/secrets",
			Systemd: "/etc/systemd/system",
			Traefik: "/etc/q8s/traefik",
		}
	} else {
		dirs = Dirs{
			Data:    "/home/alice/.local/share/q8s",
			Quadlet: "/home/alice/.config/containers/systemd",
			Config:  "/run/user/1000/q8s/configmaps",
			Secret:  "/run/user/1000/q8s/secrets",
			Systemd: "/home/alice/.config/systemd/user",
			Traefik: "/home/alice/.local/share/q8s/traefik",
		}
	}

	f.files = map[string][]byte{
		dirs.Data + "/certs/server.crt": makeCert(t, testNow.Add(365*24*time.Hour)),
	}
	if rootful {
		f.files["/etc/subuid"] = []byte("containers:2147483647:2147483648\n")
		f.files["/etc/subgid"] = []byte("containers:2147483647:2147483648\n")
		f.files["/sys/fs/cgroup/cgroup.controllers"] = []byte("cpu memory pids\n")
	} else {
		f.files["/etc/subuid"] = []byte("alice:100000:65536\n")
		f.files["/etc/subgid"] = []byte("alice:100000:65536\n")
		f.files["/sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/cgroup.controllers"] = []byte("cpu memory pids\n")
	}

	f.cmds = make(map[string]cmdResult)
	f.setCmd("5.8.2\n", nil, "podman", "version", "--format", "{{.Client.Version}}")
	f.setCmd("/var/lib/containers/storage\n", nil, "podman", "info", "--format", "{{.Store.GraphRoot}}")
	f.setCmd("pasta\n", nil, "podman", "info", "--format", "{{.Host.RootlessNetworkCmd}}")

	f.fs = map[string]string{
		"/var/lib/containers/storage": "ext4",
		dirs.Data:                     "ext4",
		dirs.Traefik:                  "ext4",
	}
	if rootful {
		f.fs["/run/q8s"] = "ext4"
	} else {
		f.fs["/run/user/1000/q8s"] = "ext4"
	}
	f.writable = map[string]bool{dirs.Traefik: true}
	f.lingerOn = true

	f.env = Env{
		Rootful: rootful,
		UID:     1000,
		User:    "alice",
		Kernel:  "6.12.0-160100.8-default",
		ReadFile: func(path string) ([]byte, error) {
			data, ok := f.files[path]
			if !ok {
				return nil, os.ErrNotExist
			}
			return data, nil
		},
		Command: func(name string, args ...string) ([]byte, error) {
			key := cmdKey(name, args...)
			r, ok := f.cmds[key]
			if !ok {
				return nil, fmt.Errorf("unexpected command: %s", key)
			}
			if r.err != nil {
				return nil, r.err
			}
			return []byte(r.out), nil
		},
		FSType: func(path string) (string, error) {
			fs, ok := f.fs[path]
			if !ok {
				return "", os.ErrNotExist
			}
			return fs, nil
		},
		Writable: func(path string) bool {
			return f.writable[path]
		},
		Linger: func() (bool, error) {
			return f.lingerOn, f.lingerErr
		},
		Now:  testNow,
		Dirs: dirs,
	}
	return f
}

func makeCert(t *testing.T, notAfter time.Time) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "q8s"},
		NotBefore:    testNow.Add(-48 * time.Hour),
		NotAfter:     notAfter,
		DNSNames:     []string{"localhost", "q8s"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func getCheck(t *testing.T, r Report, id string) Check {
	t.Helper()
	for _, c := range r.Checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("check %q not found in %v", id, checkIDs(r))
	return Check{}
}

func checkIDs(r Report) []string {
	ids := make([]string, 0, len(r.Checks))
	for _, c := range r.Checks {
		ids = append(ids, c.ID)
	}
	return ids
}

func TestUserNamespaceCheck(t *testing.T) {
	tests := []struct {
		name     string
		rootful  bool
		subuid   string
		subgid   string
		want     Status
		wantFix  string
		wantGain bool
		wantLoss bool
	}{
		{
			name:     "rootful with containers in both files",
			rootful:  true,
			subuid:   "containers:2147483647:2147483648\n",
			subgid:   "containers:2147483647:2147483648\n",
			want:     StatusOK,
			wantGain: true,
		},
		{
			name:     "rootful without containers",
			rootful:  true,
			subuid:   "",
			subgid:   "",
			want:     StatusWarn,
			wantFix:  "q8s serve adds it on its next start",
			wantLoss: true,
		},
		{
			name:     "rootful present in subuid but missing in subgid",
			rootful:  true,
			subuid:   "containers:2147483647:2147483648\n",
			subgid:   "# no containers here\n",
			want:     StatusWarn,
			wantFix:  ">> /etc/subgid",
			wantLoss: true,
		},
		{
			name:     "rootful count too small",
			rootful:  true,
			subuid:   "containers:100000:1000\n",
			subgid:   "containers:100000:1000\n",
			want:     StatusWarn,
			wantFix:  "replace the 'containers' lines",
			wantLoss: true,
		},
		{
			name:     "rootful comments and blank lines are ignored",
			rootful:  true,
			subuid:   "\n# a comment\n\ncontainers:2147483647:2147483648\n",
			subgid:   "# another comment\ncontainers:2147483647:2147483648\n",
			want:     StatusOK,
			wantGain: true,
		},
		{
			name:     "rootless matched by name",
			rootful:  false,
			subuid:   "alice:100000:65536\n",
			subgid:   "alice:100000:65536\n",
			want:     StatusOK,
			wantGain: true,
		},
		{
			name:     "rootless matched by numeric uid",
			rootful:  false,
			subuid:   "1000:100000:65536\n",
			subgid:   "1000:100000:65536\n",
			want:     StatusOK,
			wantGain: true,
		},
		{
			name:     "rootless missing entry",
			rootful:  false,
			subuid:   "",
			subgid:   "",
			want:     StatusFail,
			wantFix:  "usermod --add-subuids",
			wantLoss: true,
		},
		{
			name:     "rootless count too small",
			rootful:  false,
			subuid:   "alice:100000:1000\n",
			subgid:   "alice:100000:1000\n",
			want:     StatusFail,
			wantFix:  "usermod --add-subuids",
			wantLoss: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, tc.rootful)
			f.files["/etc/subuid"] = []byte(tc.subuid)
			f.files["/etc/subgid"] = []byte(tc.subgid)
			c := getCheck(t, Run(f.env), "userns")
			if c.Status != tc.want {
				t.Fatalf("status = %q, want %q (detail %q)", c.Status, tc.want, c.Detail)
			}
			if tc.wantFix != "" && !strings.Contains(c.Fix, tc.wantFix) {
				t.Errorf("fix %q does not contain %q", c.Fix, tc.wantFix)
			}
			if tc.wantGain && c.Gain == "" {
				t.Errorf("expected a gain line")
			}
			if tc.wantLoss && c.Loss == "" {
				t.Errorf("expected a loss line")
			}
		})
	}
}

func TestUserNamespaceDetailNamesMissingFile(t *testing.T) {
	f := newFake(t, true)
	f.files["/etc/subuid"] = []byte("containers:2147483647:2147483648\n")
	f.files["/etc/subgid"] = []byte("")
	c := getCheck(t, Run(f.env), "userns")
	if !strings.Contains(c.Detail, "/etc/subgid") {
		t.Errorf("detail %q should name /etc/subgid", c.Detail)
	}
	if strings.Contains(c.Detail, "/etc/subuid") {
		t.Errorf("detail %q should not name /etc/subuid", c.Detail)
	}
}

func TestUserNSAvailable(t *testing.T) {
	t.Run("rootful with containers", func(t *testing.T) {
		if !UserNSAvailable(newFake(t, true).env) {
			t.Fatal("expected userns to be available")
		}
	})
	t.Run("rootful missing subgid", func(t *testing.T) {
		f := newFake(t, true)
		f.files["/etc/subgid"] = []byte("")
		if UserNSAvailable(f.env) {
			t.Fatal("expected userns to be unavailable")
		}
	})
	t.Run("rootful count too small", func(t *testing.T) {
		f := newFake(t, true)
		f.files["/etc/subuid"] = []byte("containers:1000:1000\n")
		if UserNSAvailable(f.env) {
			t.Fatal("expected userns to be unavailable")
		}
	})
	t.Run("rootless is always false here", func(t *testing.T) {
		if UserNSAvailable(newFake(t, false).env) {
			t.Fatal("rootless should not report userns auto as available")
		}
	})
}

func TestIDMapCheck(t *testing.T) {
	tests := []struct {
		name    string
		rootful bool
		kernel  string
		fs      map[string]string
		want    Status
	}{
		{
			name:    "rootless not needed",
			rootful: false,
			kernel:  "6.12.0-default",
			want:    StatusInfo,
		},
		{
			name:    "6.12 ext4 and tmpfs",
			rootful: true,
			kernel:  "6.12.0-160100.8-default",
			fs: map[string]string{
				"/var/lib/containers/storage": "ext4",
				"/etc/q8s":                    "ext4",
				"/run/q8s":                    "tmpfs",
				"/etc/q8s/traefik":            "tmpfs",
			},
			want: StatusOK,
		},
		{
			name:    "kernel 6.1 with tmpfs",
			rootful: true,
			kernel:  "6.1.0-default",
			fs: map[string]string{
				"/var/lib/containers/storage": "ext4",
				"/etc/q8s":                    "ext4",
				"/run/q8s":                    "tmpfs",
				"/etc/q8s/traefik":            "tmpfs",
			},
			want: StatusWarn,
		},
		{
			name:    "overlay path",
			rootful: true,
			kernel:  "6.12.0-default",
			fs: map[string]string{
				"/var/lib/containers/storage": "ext4",
				"/etc/q8s":                    "overlay",
				"/run/q8s":                    "ext4",
				"/etc/q8s/traefik":            "ext4",
			},
			want: StatusWarn,
		},
		{
			name:    "unknown filesystem",
			rootful: true,
			kernel:  "6.12.0-default",
			fs: map[string]string{
				"/var/lib/containers/storage": "other:0x1234",
				"/etc/q8s":                    "ext4",
				"/run/q8s":                    "ext4",
				"/etc/q8s/traefik":            "ext4",
			},
			want: StatusWarn,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, tc.rootful)
			f.env.Kernel = tc.kernel
			if tc.fs != nil {
				f.fs = tc.fs
			}
			c := getCheck(t, Run(f.env), "idmap")
			if c.Status != tc.want {
				t.Fatalf("status = %q, want %q (detail %q)", c.Status, tc.want, c.Detail)
			}
			if tc.want == StatusOK && !strings.Contains(c.Detail, "=") {
				t.Errorf("detail %q should list path=fstype entries", c.Detail)
			}
		})
	}
}

func TestPodmanCheck(t *testing.T) {
	tests := []struct {
		name    string
		version string
		missing bool
		want    Status
	}{
		{name: "podman 5.8.2", version: "5.8.2", want: StatusOK},
		{name: "podman 5.0.0", version: "5.0.0", want: StatusOK},
		{name: "podman 4.9", version: "4.9.3", want: StatusFail},
		{name: "podman missing", missing: true, want: StatusFail},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, true)
			key := cmdKey("podman", "version", "--format", "{{.Client.Version}}")
			if tc.missing {
				delete(f.cmds, key)
			} else {
				f.setCmd(tc.version+"\n", nil, "podman", "version", "--format", "{{.Client.Version}}")
			}
			r := Run(f.env)
			c := getCheck(t, r, "podman")
			if c.Status != tc.want {
				t.Fatalf("status = %q, want %q", c.Status, tc.want)
			}
			if !tc.missing && !strings.Contains(c.Detail, tc.version) {
				t.Errorf("detail %q should contain version %q", c.Detail, tc.version)
			}
			if !tc.missing && r.Podman != tc.version {
				t.Errorf("report podman = %q, want %q", r.Podman, tc.version)
			}
		})
	}
}

func TestCgroupsCheck(t *testing.T) {
	tests := []struct {
		name         string
		rootful      bool
		controllers  string
		unreadable   bool
		want         Status
		wantDetail   string
		wantLossPart string
	}{
		{
			name:        "all delegated",
			rootful:     false,
			controllers: "cpu memory pids\n",
			want:        StatusOK,
		},
		{
			name:         "missing memory",
			rootful:      false,
			controllers:  "cpu pids\n",
			want:         StatusWarn,
			wantDetail:   "memory",
			wantLossPart: "memory",
		},
		{
			name:       "cgroup v2 missing",
			rootful:    false,
			unreadable: true,
			want:       StatusWarn,
			wantDetail: "cgroup v2 not detected",
		},
		{
			name:         "rootful missing cpu",
			rootful:      true,
			controllers:  "memory pids\n",
			want:         StatusWarn,
			wantDetail:   "cpu",
			wantLossPart: "cpu",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, tc.rootful)
			var path string
			if tc.rootful {
				path = "/sys/fs/cgroup/cgroup.controllers"
			} else {
				path = "/sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/cgroup.controllers"
			}
			if tc.unreadable {
				delete(f.files, path)
			} else {
				f.files[path] = []byte(tc.controllers)
			}
			c := getCheck(t, Run(f.env), "cgroups")
			if c.Status != tc.want {
				t.Fatalf("status = %q, want %q (detail %q)", c.Status, tc.want, c.Detail)
			}
			if tc.wantDetail != "" && !strings.Contains(c.Detail, tc.wantDetail) {
				t.Errorf("detail %q should contain %q", c.Detail, tc.wantDetail)
			}
			if tc.wantLossPart != "" && !strings.Contains(c.Loss, tc.wantLossPart) {
				t.Errorf("loss %q should contain %q", c.Loss, tc.wantLossPart)
			}
		})
	}
}

func TestCertificatesCheck(t *testing.T) {
	tests := []struct {
		name     string
		notAfter time.Time
		missing  bool
		want     Status
	}{
		{name: "expired", notAfter: testNow.Add(-24 * time.Hour), want: StatusFail},
		{name: "expires in 10 days", notAfter: testNow.Add(10 * 24 * time.Hour), want: StatusWarn},
		{name: "valid for a year", notAfter: testNow.Add(365 * 24 * time.Hour), want: StatusOK},
		{name: "missing", missing: true, want: StatusFail},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, true)
			path := f.env.Dirs.Data + "/certs/server.crt"
			if tc.missing {
				delete(f.files, path)
			} else {
				f.files[path] = makeCert(t, tc.notAfter)
			}
			r := Run(f.env)
			c := getCheck(t, r, "certificates")
			if c.Status != tc.want {
				t.Fatalf("status = %q, want %q (detail %q)", c.Status, tc.want, c.Detail)
			}
			if tc.want == StatusWarn && !strings.Contains(c.Fix, "q8s install --regenerate-certs") {
				t.Errorf("fix %q should regenerate certs", c.Fix)
			}
			if !tc.missing && !strings.Contains(c.Detail, tc.notAfter.UTC().Format(time.RFC3339)) {
				t.Errorf("detail %q should contain not-after %q", c.Detail, tc.notAfter.UTC().Format(time.RFC3339))
			}
		})
	}
}

func TestLingerCheck(t *testing.T) {
	t.Run("enabled", func(t *testing.T) {
		f := newFake(t, false)
		c := getCheck(t, Run(f.env), "linger")
		if c.Status != StatusOK {
			t.Fatalf("status = %q, want ok", c.Status)
		}
	})
	t.Run("disabled", func(t *testing.T) {
		f := newFake(t, false)
		f.lingerOn = false
		c := getCheck(t, Run(f.env), "linger")
		if c.Status != StatusWarn {
			t.Fatalf("status = %q, want warn", c.Status)
		}
		if !strings.Contains(c.Fix, "sudo loginctl enable-linger alice") {
			t.Errorf("fix %q should enable linger", c.Fix)
		}
	})
	t.Run("loginctl unavailable", func(t *testing.T) {
		f := newFake(t, false)
		f.lingerErr = fmt.Errorf("no loginctl")
		c := getCheck(t, Run(f.env), "linger")
		if c.Status != StatusInfo {
			t.Fatalf("status = %q, want info", c.Status)
		}
		if !strings.Contains(c.Detail, "unknown") {
			t.Errorf("detail %q should say unknown", c.Detail)
		}
	})
}

func TestCheckOrderAndRootfulSkipsLinger(t *testing.T) {
	rootful := newFake(t, true)
	got := checkIDs(Run(rootful.env))
	want := []string{"podman", "kernel", "userns", "idmap", "cgroups", "network", "certificates", "traefik-dir"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rootful check ids = %v, want %v", got, want)
	}

	rootless := newFake(t, false)
	got = checkIDs(Run(rootless.env))
	want = []string{"podman", "kernel", "userns", "idmap", "linger", "cgroups", "network", "certificates", "traefik-dir"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rootless check ids = %v, want %v", got, want)
	}
}

func TestTraefikDirCheck(t *testing.T) {
	t.Run("exists and writable", func(t *testing.T) {
		f := newFake(t, true)
		c := getCheck(t, Run(f.env), "traefik-dir")
		if c.Status != StatusOK {
			t.Fatalf("status = %q, want ok", c.Status)
		}
	})
	t.Run("missing", func(t *testing.T) {
		f := newFake(t, true)
		delete(f.fs, f.env.Dirs.Traefik)
		c := getCheck(t, Run(f.env), "traefik-dir")
		if c.Status != StatusInfo {
			t.Fatalf("status = %q, want info (detail %q)", c.Status, c.Detail)
		}
	})
	t.Run("not writable", func(t *testing.T) {
		f := newFake(t, true)
		f.writable[f.env.Dirs.Traefik] = false
		c := getCheck(t, Run(f.env), "traefik-dir")
		if c.Status != StatusFail {
			t.Fatalf("status = %q, want fail", c.Status)
		}
		if !strings.Contains(c.Loss, "Ingress changes cannot be written") {
			t.Errorf("loss %q should explain the failure", c.Loss)
		}
	})
}

func TestNetworkCheckAndSummary(t *testing.T) {
	rootful := newFake(t, true)
	r := Run(rootful.env)
	c := getCheck(t, r, "network")
	if c.Status != StatusOK {
		t.Fatalf("rootful network status = %q, want ok", c.Status)
	}
	if !strings.Contains(summaryText(r, "networking"), "kernel NAT") {
		t.Errorf("rootful networking summary = %q", summaryText(r, "networking"))
	}

	rootless := newFake(t, false)
	r = Run(rootless.env)
	c = getCheck(t, r, "network")
	if c.Status != StatusInfo || c.Detail != "pasta" {
		t.Fatalf("rootless network = %+v, want info with pasta", c)
	}
	if !strings.Contains(summaryText(r, "networking"), "pasta") {
		t.Errorf("rootless networking summary = %q", summaryText(r, "networking"))
	}
}

func summaryText(r Report, topic string) string {
	for _, s := range r.Summary {
		if s.Topic == topic {
			return s.Text
		}
	}
	return ""
}

func TestExitCode(t *testing.T) {
	tests := []struct {
		name   string
		checks []Check
		want   int
	}{
		{name: "all ok and info", checks: []Check{{Status: StatusOK}, {Status: StatusInfo}}, want: 0},
		{name: "one warn", checks: []Check{{Status: StatusOK}, {Status: StatusWarn}}, want: 1},
		{name: "one fail", checks: []Check{{Status: StatusWarn}, {Status: StatusFail}}, want: 2},
		{name: "no checks", checks: nil, want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExitCode(Report{Checks: tc.checks}); got != tc.want {
				t.Errorf("ExitCode = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestJSONRoundTrip(t *testing.T) {
	f := newFake(t, true)
	original := Run(f.env)
	data, err := json.MarshalIndent(original, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Report
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(original, got) {
		t.Fatalf("round trip mismatch:\noriginal: %+v\ngot:      %+v", original, got)
	}
}

func TestRenderText(t *testing.T) {
	f := newFake(t, true)
	delete(f.files, "/etc/subuid")
	delete(f.files, "/etc/subgid")
	r := Run(f.env)

	var buf bytes.Buffer
	RenderText(&buf, r)
	out := buf.String()

	for _, want := range []string{
		"q8s doctor: rootful, podman 5.8.2",
		"[warn] userns",
		"lose: container uids are host uids",
		"fix:  q8s serve adds it on its next start (or by hand: echo containers:2147483647:2147483648 >> /etc/subuid",
		"\nsummary\n",
		"  isolation",
		"  certificates",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}
}

func TestRenderTextHidesGainOnWarn(t *testing.T) {
	r := Report{
		Mode:   "rootful",
		Podman: "5.8.2",
		Kernel: "6.12.0",
		Checks: []Check{{
			ID:     "userns",
			Status: StatusWarn,
			Detail: "broken",
			Gain:   "should not be printed",
			Loss:   "the loss",
			Fix:    "the fix",
		}},
	}
	var buf bytes.Buffer
	RenderText(&buf, r)
	out := buf.String()
	if strings.Contains(out, "gain:") {
		t.Errorf("warn check should not print a gain line:\n%s", out)
	}
	if !strings.Contains(out, "lose: the loss") || !strings.Contains(out, "fix:  the fix") {
		t.Errorf("warn check should print loss and fix lines:\n%s", out)
	}
}

func TestKernelVersionParsing(t *testing.T) {
	tests := []struct {
		release string
		major   int
		minor   int
		ok      bool
	}{
		{"6.12.0-160100.8-default", 6, 12, true},
		{"6.1.0-default", 6, 1, true},
		{"5.12", 5, 12, true},
		{"weird", 0, 0, false},
	}
	for _, tc := range tests {
		major, minor, ok := parseKernelVersion(tc.release)
		if major != tc.major || minor != tc.minor || ok != tc.ok {
			t.Errorf("parseKernelVersion(%q) = (%d, %d, %v), want (%d, %d, %v)",
				tc.release, major, minor, ok, tc.major, tc.minor, tc.ok)
		}
	}
}

// Rootless pods share the user's single subordinate range, so the summary must
// not promise per-pod isolation the way the rootful 'containers' range does.
func TestIsolationSummary(t *testing.T) {
	if got := summaryText(Run(newFake(t, true).env), "isolation"); got != "own uid range per pod" {
		t.Errorf("rootful isolation = %q", got)
	}
	got := summaryText(Run(newFake(t, false).env), "isolation")
	if !strings.Contains(got, "shared by all pods") {
		t.Errorf("rootless isolation = %q, want shared range", got)
	}
}
