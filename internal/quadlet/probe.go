package quadlet

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Kubernetes probe defaults, applied when the corresponding field is 0 (the
// same values the Kubernetes API server defaults a probe to). Without them a
// probe that leaves periodSeconds unset would render HealthInterval=0s, which
// podman rejects.
const (
	defaultProbePeriodSeconds    = 10
	defaultProbeTimeoutSeconds   = 1
	defaultProbeFailureThreshold = 3
	defaultProbeSuccessThreshold = 1
)

// Exported probe defaults for callers outside this package, such as the
// reconcile loop's health check watch window. They stay aliases of the values
// above so the generated health check and the watch window cannot drift apart.
const (
	DefaultProbePeriodSeconds    = defaultProbePeriodSeconds
	DefaultProbeFailureThreshold = defaultProbeFailureThreshold
)

// httpTokenRE is RFC 7230's token rule, the only shape an HTTP header name
// may take. The name is passed to the in-container probe script as part of a
// "Name: value" argument, so anything outside the token set (whitespace, ':',
// quotes, control characters) is rejected instead of being passed through.
var httpTokenRE = regexp.MustCompile("^[!#$%&'*+\\-.^_`|~0-9A-Za-z]+$")

// ValidateProbes rejects every probe shape q8s cannot translate faithfully.
// Both the quadlet generator and the API call it (the API through
// internal/server), so a manifest can never be accepted and then have a
// probe silently dropped. In particular:
//
//   - only exec and httpGet handlers are supported; tcpSocket and grpc are
//     errors instead of being ignored;
//   - an exec probe must carry a non-empty command;
//   - an httpGet probe's port must resolve to a number (an unknown named
//     port is an error) and its host, if set, must be an IP literal;
//   - httpHeader names must be HTTP tokens and values must be free of
//     CR/LF/NUL;
//   - a startupProbe needs a liveness or readiness probe next to it,
//     because podman's startup health check only gates the main one.
//
// The presence of both a liveness and a readiness probe is not an error:
// podman has one health check and liveness owns it (see writeHealthCheck).
func ValidateProbes(c corev1.Container) error {
	for _, item := range []struct {
		kind  string
		probe *corev1.Probe
	}{
		{"livenessProbe", c.LivenessProbe},
		{"readinessProbe", c.ReadinessProbe},
		{"startupProbe", c.StartupProbe},
	} {
		if item.probe == nil {
			continue
		}
		if _, err := probeHealthCmd(c, item.probe, item.kind); err != nil {
			return err
		}
	}
	if c.StartupProbe != nil && c.LivenessProbe == nil && c.ReadinessProbe == nil {
		return fmt.Errorf("startupProbe: requires a livenessProbe or readinessProbe (podman's startup health check only gates the main HealthCmd)")
	}
	return nil
}

// probeHealthCmd validates one probe handler and renders the command podman
// should run, as a JSON argv array (no "CMD" keyword, see execProbeHealthCmd). exec commands keep their
// argument boundaries this way: joining them with spaces turns
// ["sh", "-c", "wget -qO- http://x"] into a different command.
func probeHealthCmd(c corev1.Container, p *corev1.Probe, kind string) (string, error) {
	h := p.ProbeHandler
	switch {
	case h.TCPSocket != nil:
		return "", fmt.Errorf("%s: tcpSocket probes are not supported by q8s; use an exec probe", kind)
	case h.GRPC != nil:
		return "", fmt.Errorf("%s: grpc probes are not supported by q8s; use an exec probe", kind)
	}
	handlers := 0
	if h.Exec != nil {
		handlers++
	}
	if h.HTTPGet != nil {
		handlers++
	}
	switch {
	case handlers == 0:
		return "", fmt.Errorf("%s: probe has no supported handler (exec or httpGet)", kind)
	case handlers > 1:
		return "", fmt.Errorf("%s: probe sets more than one handler", kind)
	}
	if h.Exec != nil {
		return execProbeHealthCmd(kind, h.Exec)
	}
	return httpProbeHealthCmd(c, kind, h.HTTPGet)
}

// execProbeHealthCmd renders an exec probe as a plain JSON argv array,
// [<command...>], without the "CMD" keyword. podman 5.4 does not handle a
// leading "CMD": it either keeps the whole JSON text as one shell string or
// re-splits the arguments on whitespace, so every check fails (and a
// livenessProbe kills the container in a loop). Without the keyword podman
// 5.4 and 6 both store the array exactly as ["CMD", <command...>]. A
// one-element command is the exception: podman runs it through /bin/sh
// (CMD-SHELL), so such an image needs a shell.
func execProbeHealthCmd(kind string, exec *corev1.ExecAction) (string, error) {
	if len(exec.Command) == 0 {
		return "", fmt.Errorf("%s: exec probe has an empty command", kind)
	}
	for _, part := range exec.Command {
		if err := noControlChars(kind+" command", part); err != nil {
			return "", err
		}
	}
	return jsonExecCommand(append([]string(nil), exec.Command...))
}

// httpProbeHealthCmd renders an httpGet probe as a POSIX sh script that runs
// inside the container and uses the first HTTP client the image provides:
// curl, else wget, else python3. Nothing is interpolated into the script text:
// the insecure flag, the URL and the headers arrive as positional arguments,
// so a header value cannot break the shell command. An image with none of the
// three clients fails the check with a clear message. An image without
// /bin/sh (distroless) is not supported and needs an exec probe.
//
// The URL points at the container's own network namespace. HTTPS sets the
// insecure flag because Kubernetes does not verify probe certificates.
func httpProbeHealthCmd(c corev1.Container, kind string, get *corev1.HTTPGetAction) (string, error) {
	port, err := resolveProbePort(c, get.Port)
	if err != nil {
		return "", fmt.Errorf("%s: %w", kind, err)
	}
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("%s: httpGet port %d is invalid", kind, port)
	}
	scheme, err := probeScheme(get.Scheme)
	if err != nil {
		return "", fmt.Errorf("%s: %w", kind, err)
	}
	host := "127.0.0.1"
	if get.Host != "" {
		ip := net.ParseIP(get.Host)
		if ip == nil {
			return "", fmt.Errorf("%s: httpGet host %q is not an IP literal (q8s does not resolve host names in probes)", kind, get.Host)
		}
		if v4 := ip.To4(); v4 != nil {
			host = v4.String()
		} else {
			host = "[" + ip.String() + "]"
		}
	}
	path := get.Path
	if path == "" {
		path = "/"
	}
	if err := noControlChars(kind+" path", path); err != nil {
		return "", err
	}

	insecure := ""
	if scheme == "https" {
		insecure = "1"
	}
	args := []string{
		"sh", "-c", httpProbeScript, "q8s-probe",
		insecure, fmt.Sprintf("%s://%s:%d%s", scheme, host, port, path),
	}
	for _, header := range get.HTTPHeaders {
		if !httpTokenRE.MatchString(header.Name) {
			return "", fmt.Errorf("%s: httpHeader name %q is not a valid HTTP token", kind, header.Name)
		}
		if err := noControlChars(kind+" httpHeader value", header.Value); err != nil {
			return "", err
		}
		args = append(args, header.Name+": "+header.Value)
	}
	return jsonExecCommand(args)
}

// httpProbePythonProgram is the python3 fallback used by httpProbeScript. It
// receives the insecure flag, the URL and the "Name: value" headers as argv,
// adds each header, uses an unverified SSL context when the flag is non-empty,
// and lets urllib raise on an HTTP status of 400 or more so the process exits
// non-zero. The script single-quotes it, so it contains no single quote and no
// percent sign.
const httpProbePythonProgram = `import sys,ssl,urllib.request
k=sys.argv[1]; u=sys.argv[2]
r=urllib.request.Request(u)
for h in sys.argv[3:]:
 n,v=h.split(": ",1)
 r.add_header(n,v)
if k:
 c=ssl._create_unverified_context()
else:
 c=None
urllib.request.urlopen(r,context=c)`

// httpProbeScript is the in-container health check for an httpGet probe. It
// uses the first client the image provides and exits with that client's status
// through exec. podman's HealthTimeout bounds the check, so the script sets no
// timeout of its own. All variable data (the insecure flag, the URL and the
// headers) is passed in as positional arguments; only $0 is fixed.
const httpProbeScript = `k=$1; u=$2; shift 2
if command -v curl >/dev/null 2>&1; then
  n=$#; for h do set -- "$@" -H "$h"; done; shift "$n"
  exec curl -fsS -o /dev/null ${k:+-k} "$@" "$u"
elif command -v wget >/dev/null 2>&1; then
  n=$#; for h do set -- "$@" --header "$h"; done; shift "$n"
  exec wget -q -O /dev/null ${k:+--no-check-certificate} "$@" "$u"
elif command -v python3 >/dev/null 2>&1; then
  exec python3 -c '` + httpProbePythonProgram + `' "$k" "$u" "$@"
else
  echo "q8s probe: the image has no curl, wget or python3; use an exec probe" >&2
  exit 1
fi`

// probeScheme maps a Kubernetes URI scheme to the URL scheme used by the
// in-container probe script. An unset scheme is HTTP, as in Kubernetes.
func probeScheme(s corev1.URIScheme) (string, error) {
	switch strings.ToUpper(string(s)) {
	case "", "HTTP":
		return "http", nil
	case "HTTPS":
		return "https", nil
	default:
		return "", fmt.Errorf("httpGet scheme %q is not supported (only HTTP and HTTPS)", s)
	}
}

// resolveProbePort returns the numeric port an httpGet probe should hit. A
// named port is looked up in the container's own port list; an unknown name
// is an error, never a probe against port 0 and never a silently skipped
// health check.
func resolveProbePort(c corev1.Container, port intstr.IntOrString) (int32, error) {
	if port.Type == intstr.Int {
		return port.IntVal, nil
	}
	for _, p := range c.Ports {
		if p.Name != "" && p.Name == port.StrVal {
			return p.ContainerPort, nil
		}
	}
	return 0, fmt.Errorf("httpGet port %q does not match any named container port", port.StrVal)
}

// jsonExecCommand encodes args as a JSON array without HTML escaping, which
// keeps "&&" and friends readable in the generated unit. The result is still
// valid JSON that podman decodes back into the exact argv.
func jsonExecCommand(args []string) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(args); err != nil {
		return "", fmt.Errorf("cannot encode health check command: %w", err)
	}
	return strings.TrimSuffix(buf.String(), "\n"), nil
}

// writeHealthCheck emits the single Podman health check a unit can have.
//
// A livenessProbe owns it when present (it is what drives restarts, through
// HealthOnFailure=kill plus the unit's Restart=), otherwise a readinessProbe
// supplies a status-only check. When both are set only liveness is
// representable: podman has one health check per container, so Ready follows
// it. A startupProbe becomes podman's separate startup health check.
func writeHealthCheck(b *strings.Builder, c corev1.Container) error {
	if err := ValidateProbes(c); err != nil {
		return err
	}

	probe := c.LivenessProbe
	liveness := probe != nil
	if probe == nil {
		probe = c.ReadinessProbe
	}
	if probe != nil {
		kind := "readinessProbe"
		if liveness {
			kind = "livenessProbe"
		}
		cmd, err := probeHealthCmd(c, probe, kind)
		if err != nil {
			return err
		}
		line, err := healthCmdLine("HealthCmd", cmd)
		if err != nil {
			return err
		}
		b.WriteString(line)
		writeProbeTiming(b, "Health", probe)
		if liveness {
			// podman's documented restart pattern: kill the container on a
			// failed liveness check, systemd's Restart= brings it back.
			// A readiness probe must not restart anything.
			b.WriteString("HealthOnFailure=kill\n")
		}
	}

	if startup := c.StartupProbe; startup != nil {
		cmd, err := probeHealthCmd(c, startup, "startupProbe")
		if err != nil {
			return err
		}
		line, err := healthCmdLine("HealthStartupCmd", cmd)
		if err != nil {
			return err
		}
		b.WriteString(line)
		b.WriteString(fmt.Sprintf("HealthStartupInterval=%ds\n", probeSeconds(startup.PeriodSeconds, defaultProbePeriodSeconds)))
		b.WriteString(fmt.Sprintf("HealthStartupTimeout=%ds\n", probeSeconds(startup.TimeoutSeconds, defaultProbeTimeoutSeconds)))
		b.WriteString(fmt.Sprintf("HealthStartupRetries=%d\n", probeSeconds(startup.FailureThreshold, defaultProbeFailureThreshold)))
		b.WriteString(fmt.Sprintf("HealthStartupSuccess=%d\n", probeSeconds(startup.SuccessThreshold, defaultProbeSuccessThreshold)))
	}
	return nil
}

// healthCmdLine renders a HealthCmd= or HealthStartupCmd= directive. Quadlet
// reads these values itself: it strips one surrounding pair of double quotes
// but does not decode C escapes, so a JSON exec array must be written
// verbatim. Quoting it systemd-style would leave the backslashes in podman's
// --health-cmd argument and corrupt the command. The transformations that
// still happen in the generated ExecStart are systemd's variable expansion
// (${NAME}, $NAME) and specifier expansion (%s and friends), so every literal
// dollar becomes $$ and every literal percent becomes %%. This also protects
// an exec probe whose command text contains a shell variable: systemd must not
// expand it on the host, the container's shell has to see it unchanged.
func healthCmdLine(directive, cmd string) (string, error) {
	if err := noControlChars(directive+" value", cmd); err != nil {
		return "", err
	}
	escaped := strings.ReplaceAll(cmd, "$", "$$")
	escaped = strings.ReplaceAll(escaped, "%", "%%")
	return fmt.Sprintf("%s=%s\n", directive, escaped), nil
}

// writeProbeTiming emits the interval/timeout/retries/start-period keys shared
// by the main health check. Prefix is "Health" for the main check (there is
// no other caller today).
func writeProbeTiming(b *strings.Builder, prefix string, p *corev1.Probe) {
	b.WriteString(fmt.Sprintf("%sInterval=%ds\n", prefix, probeSeconds(p.PeriodSeconds, defaultProbePeriodSeconds)))
	b.WriteString(fmt.Sprintf("%sTimeout=%ds\n", prefix, probeSeconds(p.TimeoutSeconds, defaultProbeTimeoutSeconds)))
	b.WriteString(fmt.Sprintf("%sRetries=%d\n", prefix, probeSeconds(p.FailureThreshold, defaultProbeFailureThreshold)))
	if p.InitialDelaySeconds > 0 {
		b.WriteString(fmt.Sprintf("%sStartPeriod=%ds\n", prefix, p.InitialDelaySeconds))
	}
}

// probeSeconds applies the Kubernetes default to an unset (0) or negative
// timing field. Kubernetes itself rejects negative values, but the generator
// is also reached from stored specs and the podman-label import path, so a
// negative value must not turn into an invalid unit.
func probeSeconds(v, def int32) int32 {
	if v <= 0 {
		return def
	}
	return v
}
