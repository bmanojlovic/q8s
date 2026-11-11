package quadlet

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// probeCmdArgv decodes a rendered HealthCmd value back into argv, undoing the
// systemd escaping healthCmdLine applies.
func probeCmdArgv(t *testing.T, cmd string) []string {
	t.Helper()
	raw := strings.ReplaceAll(cmd, "$$", "$")
	raw = strings.ReplaceAll(raw, "%%", "%")
	var argv []string
	if err := json.Unmarshal([]byte(raw), &argv); err != nil {
		t.Fatalf("decode health command: %v (value %q)", err, raw)
	}
	return argv
}

// assertSystemdEscaped fails on a single $ or % that systemd would expand.
func assertSystemdEscaped(t *testing.T, value string) {
	t.Helper()
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '$':
			if i+1 >= len(value) || value[i+1] != '$' {
				t.Fatalf("unescaped $ at offset %d in %q", i, value)
			}
			i++
		case '%':
			if i+1 >= len(value) || value[i+1] != '%' {
				t.Fatalf("unescaped %% at offset %d in %q", i, value)
			}
			i++
		}
	}
}

// TestHTTPProbeHealthCmdHTTP: an HTTP probe is the fixed sh script with the
// insecure flag empty and the URL as the last fixed argument.
func TestHTTPProbeHealthCmdHTTP(t *testing.T) {
	get := &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(8080)}
	cmd, err := httpProbeHealthCmd(corev1.Container{}, "livenessProbe", get)
	if err != nil {
		t.Fatalf("httpProbeHealthCmd: %v", err)
	}
	want := []string{
		"sh", "-c", httpProbeScript, "q8s-probe", "",
		"http://127.0.0.1:8080/healthz",
	}
	if got := probeCmdArgv(t, cmd); !reflect.DeepEqual(got, want) {
		t.Errorf("argv: expected %v, got %v", want, got)
	}
}

// TestHTTPProbeHealthCmdHTTPSHeaders: HTTPS sets the insecure flag and every
// header is its own "Name: value" argument after the URL.
func TestHTTPProbeHealthCmdHTTPSHeaders(t *testing.T) {
	get := &corev1.HTTPGetAction{
		Path:   "/healthz",
		Port:   intstr.FromInt32(8443),
		Scheme: corev1.URISchemeHTTPS,
		Host:   "192.0.2.5",
		HTTPHeaders: []corev1.HTTPHeader{
			{Name: "X-Token", Value: "secret"},
			{Name: "Accept", Value: "application/json"},
		},
	}
	cmd, err := httpProbeHealthCmd(corev1.Container{}, "livenessProbe", get)
	if err != nil {
		t.Fatalf("httpProbeHealthCmd: %v", err)
	}
	want := []string{
		"sh", "-c", httpProbeScript, "q8s-probe", "1",
		"https://192.0.2.5:8443/healthz",
		"X-Token: secret", "Accept: application/json",
	}
	if got := probeCmdArgv(t, cmd); !reflect.DeepEqual(got, want) {
		t.Errorf("argv: expected %v, got %v", want, got)
	}
}

// TestHTTPProbeScriptNoUnsafeText: the single script constant must be safe to
// embed in a single-quoted sh string and, after healthCmdLine, in a systemd
// ExecStart line.
func TestHTTPProbeScriptNoUnsafeText(t *testing.T) {
	if strings.Contains(httpProbeScript, "%") {
		t.Error("the probe script contains a percent sign, which systemd would expand")
	}
	// The two quotes delimit the python program passed to python3 -c.
	if got := strings.Count(httpProbeScript, "'"); got != 2 {
		t.Errorf("the probe script contains %d single quotes, expected 2", got)
	}

	get := &corev1.HTTPGetAction{
		Path:        "/healthz",
		Port:        intstr.FromInt32(8080),
		HTTPHeaders: []corev1.HTTPHeader{{Name: "X-Test", Value: "1"}},
	}
	cmd, err := httpProbeHealthCmd(corev1.Container{}, "livenessProbe", get)
	if err != nil {
		t.Fatalf("httpProbeHealthCmd: %v", err)
	}
	line, err := healthCmdLine("HealthCmd", cmd)
	if err != nil {
		t.Fatalf("healthCmdLine: %v", err)
	}
	assertSystemdEscaped(t, line)
}

// TestHealthCmdLineEscapesSystemdVars: a dollar and a percent in an exec
// probe reach the container's shell unchanged.
func TestHealthCmdLineEscapesSystemdVars(t *testing.T) {
	line, err := healthCmdLine("HealthCmd", `["sh","-c","echo ${HOME} 100%"]`)
	if err != nil {
		t.Fatalf("healthCmdLine: %v", err)
	}
	want := `HealthCmd=["sh","-c","echo $${HOME} 100%%"]` + "\n"
	if line != want {
		t.Errorf("expected %q, got %q", want, line)
	}
}

// fakeProbeTool writes a fake HTTP client that records its argv, NUL
// separated, in $Q8S_PROBE_LOG. NUL separators keep the multi-line python
// program in one field.
func fakeProbeTool(t *testing.T, dir, name string) {
	t.Helper()
	const body = "#!/bin/sh\nprintf '%s\\0' \"$0\" \"$@\" > \"$Q8S_PROBE_LOG\"\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
}

// runProbeScript runs the probe script with the given PATH and returns the
// fake client's argv, stderr and exit code.
func runProbeScript(t *testing.T, sh, pathDir, insecure, url string, headers ...string) ([]string, string, int) {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "argv.log")
	args := []string{"-c", httpProbeScript, "q8s-probe", insecure, url}
	args = append(args, headers...)
	cmd := exec.Command(sh, args...)
	cmd.Env = []string{"PATH=" + pathDir, "Q8S_PROBE_LOG=" + logPath}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run probe script: %v", err)
		}
		code = exitErr.ExitCode()
	}

	data, readErr := os.ReadFile(logPath)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatalf("read probe log: %v", readErr)
	}
	text := strings.TrimSuffix(string(data), "\x00")
	var log []string
	if text != "" {
		log = strings.Split(text, "\x00")
	}
	return log, stderr.String(), code
}

// TestHTTPProbeScriptClientSelection runs the script with fake clients in
// PATH and checks which one it picks, with which arguments.
func TestHTTPProbeScriptClientSelection(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh is not available")
	}
	const url = "http://192.0.2.10:8080/healthz"
	header := "X-Test: 1"

	tests := []struct {
		name       string
		tools      []string
		insecure   string
		headers    []string
		want       []string
		wantCode   int
		wantStderr string
	}{
		{
			name:    "curl is preferred",
			tools:   []string{"curl", "wget", "python3"},
			headers: []string{header},
			want:    []string{"curl", "-fsS", "-o", "/dev/null", "-H", header, url},
		},
		{
			name:    "wget without curl",
			tools:   []string{"wget", "python3"},
			headers: []string{header},
			want:    []string{"wget", "-q", "-O", "/dev/null", "--header", header, url},
		},
		{
			name:    "python3 without curl or wget",
			tools:   []string{"python3"},
			headers: []string{header},
			want:    []string{"python3", "-c", httpProbePythonProgram, "", url, header},
		},
		{
			name:     "curl insecure",
			tools:    []string{"curl"},
			insecure: "1",
			want:     []string{"curl", "-fsS", "-o", "/dev/null", "-k", url},
		},
		{
			name:     "wget insecure",
			tools:    []string{"wget"},
			insecure: "1",
			want:     []string{"wget", "-q", "-O", "/dev/null", "--no-check-certificate", url},
		},
		{
			name:     "python3 insecure",
			tools:    []string{"python3"},
			insecure: "1",
			want:     []string{"python3", "-c", httpProbePythonProgram, "1", url},
		},
		{
			name:       "no client",
			wantCode:   1,
			wantStderr: "no curl, wget or python3",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, tool := range tt.tools {
				fakeProbeTool(t, dir, tool)
			}
			log, stderr, code := runProbeScript(t, sh, dir, tt.insecure, url, tt.headers...)
			if code != tt.wantCode {
				t.Fatalf("exit code: expected %d, got %d (stderr %q)", tt.wantCode, code, stderr)
			}
			if tt.wantStderr != "" && !strings.Contains(stderr, tt.wantStderr) {
				t.Errorf("stderr %q does not contain %q", stderr, tt.wantStderr)
			}
			if tt.want == nil {
				if log != nil {
					t.Errorf("expected no client invocation, got %v", log)
				}
				return
			}
			if len(log) == 0 {
				t.Fatalf("the client was not invoked (stderr %q)", stderr)
			}
			if got := filepath.Base(log[0]); got != tt.want[0] {
				t.Fatalf("client: expected %q, got %q", tt.want[0], got)
			}
			if got := log[1:]; !reflect.DeepEqual(got, tt.want[1:]) {
				t.Errorf("client args: expected %v, got %v", tt.want[1:], got)
			}
		})
	}
}

// TestHTTPProbePythonProgram: the python3 fallback succeeds on 200, fails on
// 500 and sends the header it was given.
func TestHTTPProbePythonProgram(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not available")
	}
	var seen atomic.Value
	seen.Store("")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.Header.Get("X-Test"))
		if r.URL.Path == "/ok" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	run := func(path string) (int, string) {
		cmd := exec.Command(python, "-c", httpProbePythonProgram, "", server.URL+path, "X-Test: 1")
		out, err := cmd.CombinedOutput()
		if err == nil {
			return 0, string(out)
		}
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run python3: %v", err)
		}
		return exitErr.ExitCode(), string(out)
	}

	if code, out := run("/ok"); code != 0 {
		t.Fatalf("200 response: expected exit code 0, got %d (output %q)", code, out)
	}
	if got := seen.Load(); got != "1" {
		t.Errorf("header: expected %q, got %v", "1", got)
	}
	if code, out := run("/bad"); code == 0 {
		t.Fatalf("500 response: expected a non-zero exit code, got 0 (output %q)", out)
	}
}
