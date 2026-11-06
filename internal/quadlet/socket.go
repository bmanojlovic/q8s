package quadlet

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// AnnotationSocketActivation asks q8s to hand the pod's container listening
// sockets from systemd instead of letting the process bind them itself.
// Format: "name=port,name=IPv4:port,name=[IPv6]:port".
const AnnotationSocketActivation = "q8s.io/socket-activation"

// SocketEntry is one listen socket: Addr is empty for "all addresses".
type SocketEntry struct {
	Name string
	Addr string
	Port int32
}

// socketNameRE matches an entry name; it is also used as FileDescriptorName,
// so it is restricted to the characters systemd accepts there.
var socketNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// maxSocketName bounds the name; systemd's FileDescriptorName is generous,
// but a bounded value keeps the generated filename sane.
const maxSocketName = 64

// ParseSocketActivation parses the q8s.io/socket-activation annotation.
// It returns nil, nil when the annotation is absent.
func ParseSocketActivation(annotations map[string]string) ([]SocketEntry, error) {
	raw, ok := annotations[AnnotationSocketActivation]
	if !ok {
		return nil, nil
	}
	var entries []SocketEntry
	names := make(map[string]bool)
	listens := make(map[string]bool)
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			return nil, fmt.Errorf("annotation %s: empty entry in %q", AnnotationSocketActivation, raw)
		}
		name, spec, found := strings.Cut(item, "=")
		if !found {
			return nil, fmt.Errorf("annotation %s: entry %q must be name=port", AnnotationSocketActivation, item)
		}
		if len(name) == 0 || len(name) > maxSocketName || !socketNameRE.MatchString(name) {
			return nil, fmt.Errorf("annotation %s: socket name %q is invalid", AnnotationSocketActivation, name)
		}
		if names[name] {
			return nil, fmt.Errorf("annotation %s: duplicate socket name %q", AnnotationSocketActivation, name)
		}
		names[name] = true
		addr, port, err := parseSocketListen(spec)
		if err != nil {
			return nil, fmt.Errorf("annotation %s: entry %q: %w", AnnotationSocketActivation, item, err)
		}
		key := fmt.Sprintf("%s:%d", addr, port)
		if listens[key] {
			return nil, fmt.Errorf("annotation %s: duplicate listen address %q", AnnotationSocketActivation, item)
		}
		listens[key] = true
		entries = append(entries, SocketEntry{Name: name, Addr: addr, Port: port})
	}
	return entries, nil
}

// parseSocketListen splits "port", "IPv4:port" or "[IPv6]:port". An explicit
// address must be an IP literal: a hostname would make the socket unit's
// listen address depend on resolution at boot.
func parseSocketListen(spec string) (string, int32, error) {
	addr := ""
	portStr := spec
	if strings.HasPrefix(spec, "[") {
		end := strings.IndexByte(spec, ']')
		if end < 0 {
			return "", 0, fmt.Errorf("unterminated IPv6 address in %q", spec)
		}
		addr = spec[1:end]
		rest := spec[end+1:]
		if !strings.HasPrefix(rest, ":") {
			return "", 0, fmt.Errorf("expected [IPv6]:port, got %q", spec)
		}
		portStr = rest[1:]
		ip := net.ParseIP(addr)
		if ip == nil || ip.To4() != nil {
			return "", 0, fmt.Errorf("%q is not an IPv6 address", addr)
		}
		addr = ip.String()
	} else if i := strings.LastIndexByte(spec, ':'); i >= 0 {
		addr = spec[:i]
		portStr = spec[i+1:]
		ip := net.ParseIP(addr)
		if ip == nil || ip.To4() == nil {
			return "", 0, fmt.Errorf("%q is not an IPv4 address", addr)
		}
		addr = ip.To4().String()
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("port %q is invalid: must be between 1 and 65535", portStr)
	}
	return addr, int32(port), nil
}

// ValidateSocketActivation parses the annotation and rejects a listen port
// that one of the pod's containers already publishes with hostPort: the
// socket unit and podman would both try to bind it.
func ValidateSocketActivation(pod *corev1.Pod) error {
	if pod == nil {
		return nil
	}
	entries, err := ParseSocketActivation(pod.Annotations)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	hostPorts := make(map[int32]bool)
	for _, c := range append(append([]corev1.Container{}, pod.Spec.InitContainers...), pod.Spec.Containers...) {
		for _, p := range c.Ports {
			if p.HostPort != 0 {
				hostPorts[p.HostPort] = true
			}
		}
	}
	for _, e := range entries {
		if hostPorts[e.Port] {
			return fmt.Errorf("annotation %s: socket %q port %d collides with a hostPort", AnnotationSocketActivation, e.Name, e.Port)
		}
	}
	return nil
}

// SocketUnits renders one .socket unit per entry. The unit name is derived
// from namespace, pod and entry name because FileDescriptorName applies to
// the whole unit: Traefik maps each entrypoint to an fd by that name, so
// every listen socket needs its own unit.
func SocketUnits(ns, podName string, entries []SocketEntry) (map[string][]byte, error) {
	if err := validateRefName("pod namespace", ns); err != nil {
		return nil, err
	}
	if err := validateRefName("pod name", podName); err != nil {
		return nil, err
	}
	files := make(map[string][]byte, len(entries))
	for _, e := range entries {
		if len(e.Name) == 0 || len(e.Name) > maxSocketName || !socketNameRE.MatchString(e.Name) {
			return nil, fmt.Errorf("socket name %q is invalid", e.Name)
		}
		if e.Port < 1 || e.Port > 65535 {
			return nil, fmt.Errorf("socket %q: port %d is invalid", e.Name, e.Port)
		}
		if e.Addr != "" {
			ip := net.ParseIP(e.Addr)
			if ip == nil {
				return nil, fmt.Errorf("socket %q: address %q is not an IP literal", e.Name, e.Addr)
			}
			e.Addr = ip.String()
		}
		filename := fmt.Sprintf("%s-%s-%s.socket", ns, podName, e.Name)
		if _, exists := files[filename]; exists {
			return nil, fmt.Errorf("duplicate socket name %q", e.Name)
		}

		listen := strconv.Itoa(int(e.Port))
		if e.Addr != "" {
			listen = net.JoinHostPort(e.Addr, listen)
		}

		var b strings.Builder
		b.WriteString("[Unit]\n")
		b.WriteString(fmt.Sprintf("Description=q8s socket %s for pod %s/%s\n", e.Name, ns, podName))
		b.WriteString(fmt.Sprintf("X-Q8s-Namespace=%s\n", ns))
		b.WriteString(fmt.Sprintf("X-Q8s-Pod=%s\n", podName))
		b.WriteString("\n[Socket]\n")
		b.WriteString(fmt.Sprintf("ListenStream=%s\n", listen))
		b.WriteString(fmt.Sprintf("FileDescriptorName=%s\n", e.Name))
		b.WriteString(fmt.Sprintf("Service=%s-%s.service\n", ns, podName))
		b.WriteString("\n[Install]\n")
		b.WriteString("WantedBy=sockets.target\n")
		files[filename] = []byte(b.String())
	}
	return files, nil
}
