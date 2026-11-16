package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"q8s/internal/podman"
	"q8s/internal/store"
	"q8s/pkg/install"
)

// workloadSocketName is the socket a workload container connects to for a
// service account credential. It lives next to the copied q8s binary and the
// generated kubeconfig in the host workload directory, which pods mount at
// /var/run/q8s/workload.
const workloadSocketName = "sa.sock"

// ContainerLabelResolver resolves the labels of the container that owns a
// process. It is injectable so the credential tests do not need /proc or a
// real podman. The real implementation reads /proc/<pid>/cgroup and asks
// podman for the container labels.
type ContainerLabelResolver interface {
	LabelsForPID(pid int) (map[string]string, error)
}

// procContainerResolver is the production resolver.
type procContainerResolver struct{}

func (procContainerResolver) LabelsForPID(pid int) (map[string]string, error) {
	content, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return nil, fmt.Errorf("read cgroup of pid %d: %w", pid, err)
	}
	id, ok := containerIDFromCgroup(string(content))
	if !ok {
		return nil, fmt.Errorf("process %d is not a container workload", pid)
	}
	return podman.InspectLabels(id)
}

// workloadResolver returns the configured resolver, or the real one.
func (s *Server) workloadResolver() ContainerLabelResolver {
	if s.config.WorkloadPeers != nil {
		return s.config.WorkloadPeers
	}
	return procContainerResolver{}
}

// ServeWorkloadSocket creates dir, removes a stale socket, listens on
// <dir>/sa.sock with mode 0666 and serves credential requests until ctx is
// done. A cancelled context is a clean shutdown and returns nil.
func (s *Server) ServeWorkloadSocket(ctx context.Context, dir string) error {
	if dir == "" {
		return fmt.Errorf("workload socket directory is required")
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create workload directory %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0755); err != nil {
		return fmt.Errorf("chmod workload directory %s: %w", dir, err)
	}
	socketPath := filepath.Join(dir, workloadSocketName)
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale workload socket %s: %w", socketPath, err)
	}
	ln, label, err := listenWorkloadSocket(socketPath)
	if err != nil {
		return fmt.Errorf("listen on workload socket %s: %w", socketPath, err)
	}
	if label == "" {
		label = "none"
	}
	fmt.Printf("workload socket: listening on %s (SELinux socket label %s)\n", socketPath, label)
	if err := os.Chmod(socketPath, 0666); err != nil {
		ln.Close()
		os.Remove(socketPath)
		return fmt.Errorf("chmod workload socket %s: %w", socketPath, err)
	}

	// Closing the listener is what unblocks Accept when ctx is cancelled.
	// done stops the watcher when Serve returns for another reason.
	done := make(chan struct{})
	defer close(done)
	defer func() {
		ln.Close()
		os.Remove(socketPath)
	}()
	go func() {
		select {
		case <-ctx.Done():
			ln.Close()
		case <-done:
		}
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept on workload socket: %w", err)
		}
		go s.handleWorkloadConn(conn)
	}
}

// handleWorkloadConn serves one line request and closes the connection.
// The protocol is deliberately tiny: the client sends "GET /credential\n"
// and the server replies with the ExecCredential JSON, or a line starting
// with "error: " when it refuses.
func (s *Server) handleWorkloadConn(conn net.Conn) {
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		return
	}
	line, err := bufio.NewReader(io.LimitReader(conn, 4096)).ReadString('\n')
	if err != nil && line == "" {
		return
	}
	if !isCredentialRequest(line) {
		fmt.Fprint(conn, "error: invalid credential request\n")
		return
	}
	data, who, err := s.credentialForConnection(conn)
	if err != nil {
		fmt.Printf("workload credential: refused %s: %v\n", who, err)
		fmt.Fprintf(conn, "error: %s\n", err)
		return
	}
	fmt.Printf("workload credential: issued %s, valid %s\n", who, workloadCredentialTTL)
	conn.Write(data)
}

// isCredentialRequest accepts the documented one-line request and also a
// minimal HTTP request line with the same method and path.
func isCredentialRequest(line string) bool {
	fields := strings.Fields(line)
	return len(fields) >= 2 && fields[0] == "GET" && fields[1] == "/credential"
}

// peerPID reads SO_PEERCRED from a unix socket connection.
func peerPID(conn net.Conn) (int, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("credential request did not arrive on a unix socket")
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("read peer credentials: %w", err)
	}
	var pid int
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if err != nil {
			credErr = err
			return
		}
		pid = int(cred.Pid)
	}); err != nil {
		return 0, fmt.Errorf("read peer credentials: %w", err)
	}
	if credErr != nil {
		return 0, fmt.Errorf("read peer credentials: %w", credErr)
	}
	if pid <= 0 {
		return 0, fmt.Errorf("peer pid is unknown")
	}
	return pid, nil
}

// workloadURIPrefix starts the URI Subject Alternative Name that binds a
// service account certificate to the object that requested it. The rest is
// <kind>:<namespace>/<name>/<uid>, with kind pod, job or cronjob.
const workloadURIPrefix = "urn:q8s:workload:"

const (
	workloadKindPod     = "pod"
	workloadKindJob     = "job"
	workloadKindCronJob = "cronjob"
)

// workloadRef identifies the stored object a certificate is bound to.
type workloadRef struct {
	Kind      string
	Namespace string
	Name      string
	UID       string
}

// workloadTarget is the live object plus the pod template fields that decide
// whether a credential may be issued.
type workloadTarget struct {
	Ref                          workloadRef
	ServiceAccount               string
	AutomountServiceAccountToken *bool
}

// URI renders the canonical Subject Alternative Name for r.
func (r workloadRef) URI() (string, error) {
	switch r.Kind {
	case workloadKindPod, workloadKindJob, workloadKindCronJob:
	default:
		return "", fmt.Errorf("unsupported workload kind %q", r.Kind)
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"namespace", r.Namespace},
		{"name", r.Name},
		{"uid", r.UID},
	} {
		if field.value == "" {
			return "", fmt.Errorf("workload %s is required", field.name)
		}
		if strings.ContainsAny(field.value, "/") {
			return "", fmt.Errorf("workload %s must not contain a slash", field.name)
		}
	}
	return workloadURIPrefix + r.Kind + ":" + r.Namespace + "/" + r.Name + "/" + r.UID, nil
}

// parseWorkloadURI parses a URI Subject Alternative Name back into a
// workloadRef. It accepts only the canonical form this server issues.
func parseWorkloadURI(u *url.URL) (workloadRef, bool) {
	if u == nil {
		return workloadRef{}, false
	}
	raw := u.String()
	if !strings.HasPrefix(raw, workloadURIPrefix) {
		return workloadRef{}, false
	}
	kind, rest, ok := strings.Cut(strings.TrimPrefix(raw, workloadURIPrefix), ":")
	if !ok {
		return workloadRef{}, false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		return workloadRef{}, false
	}
	ref := workloadRef{Kind: kind, Namespace: parts[0], Name: parts[1], UID: parts[2]}
	canonical, err := ref.URI()
	if err != nil || canonical != raw {
		return workloadRef{}, false
	}
	return ref, true
}

// workloadRefFromLabels resolves the object labels on a workload container.
// Pod containers win, then Job containers, then CronJob containers.
func workloadRefFromLabels(labels map[string]string) (workloadRef, bool) {
	namespace := labels["io.kubernetes.pod.namespace"]
	if namespace == "" {
		return workloadRef{}, false
	}
	if name := labels["io.kubernetes.pod.name"]; name != "" {
		return workloadRef{Kind: workloadKindPod, Namespace: namespace, Name: name}, true
	}
	if name := labels["io.kubernetes.job.name"]; name != "" {
		return workloadRef{Kind: workloadKindJob, Namespace: namespace, Name: name}, true
	}
	if name := labels["io.kubernetes.cronjob.name"]; name != "" {
		return workloadRef{Kind: workloadKindCronJob, Namespace: namespace, Name: name}, true
	}
	return workloadRef{}, false
}

// lookupWorkloadTarget reads the live object a certificate is bound to and
// returns its pod template service account fields. The returned UID is the
// object's current UID, which the caller compares with the UID in the
// certificate.
func lookupWorkloadTarget(st *store.Store, kind, namespace, name string) (workloadTarget, error) {
	if st == nil {
		return workloadTarget{}, fmt.Errorf("workload store is not configured")
	}
	switch kind {
	case workloadKindPod:
		pod, err := st.GetPod(namespace, name)
		if err != nil {
			return workloadTarget{}, fmt.Errorf("pod %s/%s not found", namespace, name)
		}
		return workloadTarget{
			Ref:                          workloadRef{Kind: kind, Namespace: namespace, Name: name, UID: string(pod.UID)},
			ServiceAccount:               pod.Spec.ServiceAccountName,
			AutomountServiceAccountToken: pod.Spec.AutomountServiceAccountToken,
		}, nil
	case workloadKindJob:
		job, err := st.GetJob(namespace, name)
		if err != nil {
			return workloadTarget{}, fmt.Errorf("job %s/%s not found", namespace, name)
		}
		return workloadTarget{
			Ref:                          workloadRef{Kind: kind, Namespace: namespace, Name: name, UID: string(job.UID)},
			ServiceAccount:               job.Spec.Template.Spec.ServiceAccountName,
			AutomountServiceAccountToken: job.Spec.Template.Spec.AutomountServiceAccountToken,
		}, nil
	case workloadKindCronJob:
		cj, err := st.GetCronJob(namespace, name)
		if err != nil {
			return workloadTarget{}, fmt.Errorf("cronjob %s/%s not found", namespace, name)
		}
		template := cj.Spec.JobTemplate.Spec.Template.Spec
		return workloadTarget{
			Ref:                          workloadRef{Kind: kind, Namespace: namespace, Name: name, UID: string(cj.UID)},
			ServiceAccount:               template.ServiceAccountName,
			AutomountServiceAccountToken: template.AutomountServiceAccountToken,
		}, nil
	default:
		return workloadTarget{}, fmt.Errorf("unsupported workload kind %q", kind)
	}
}

// credentialForConnection resolves the peer to a workload container and
// issues its service account credential.
// credentialForConnection also returns a short description of the caller for
// the log line: the pid when nothing more is known, else the workload and the
// ServiceAccount it asks for.
func (s *Server) credentialForConnection(conn net.Conn) ([]byte, string, error) {
	pid, err := peerPID(conn)
	if err != nil {
		return nil, "unknown peer", err
	}
	who := fmt.Sprintf("pid %d", pid)
	labels, err := s.workloadResolver().LabelsForPID(pid)
	if err != nil {
		return nil, who, fmt.Errorf("resolve peer container: %w", err)
	}
	if ref, ok := workloadRefFromLabels(labels); ok {
		who = fmt.Sprintf("%s %s/%s", ref.Kind, ref.Namespace, ref.Name)
		if target, err := lookupWorkloadTarget(s.config.Store, ref.Kind, ref.Namespace, ref.Name); err == nil && target.ServiceAccount != "" {
			who += " serviceaccount " + target.ServiceAccount
		}
	}
	data, err := s.credentialForLabels(labels)
	return data, who, err
}

// credentialForLabels is the policy part of the socket: the labels must name
// a stored pod, Job or CronJob, that object must explicitly ask for a service
// account, automount must not be disabled and the ServiceAccount must exist.
// Only then is a certificate issued.
func (s *Server) credentialForLabels(labels map[string]string) ([]byte, error) {
	ref, ok := workloadRefFromLabels(labels)
	if !ok {
		return nil, fmt.Errorf("caller is not a q8s workload container")
	}
	target, err := lookupWorkloadTarget(s.config.Store, ref.Kind, ref.Namespace, ref.Name)
	if err != nil {
		return nil, err
	}
	if target.ServiceAccount == "" {
		return nil, fmt.Errorf("%s %s/%s does not request a service account", ref.Kind, ref.Namespace, ref.Name)
	}
	if target.AutomountServiceAccountToken != nil && !*target.AutomountServiceAccountToken {
		return nil, fmt.Errorf("%s %s/%s disables service account credentials", ref.Kind, ref.Namespace, ref.Name)
	}
	if _, err := s.config.Store.GetServiceAccount(ref.Namespace, target.ServiceAccount); err != nil {
		return nil, fmt.Errorf("serviceaccount %s/%s not found", ref.Namespace, target.ServiceAccount)
	}

	caCert, caKey, err := s.loadWorkloadCA()
	if err != nil {
		return nil, err
	}
	return issueServiceAccountCredentialForWorkload(caCert, caKey, target, time.Now())
}

// loadWorkloadCA reads the CA certificate and key from <dataDir>/certs, the
// same files `q8s install` writes and `q8s kubeconfig` signs with.
func (s *Server) loadWorkloadCA() ([]byte, []byte, error) {
	if s.config.DataDir == "" {
		return nil, nil, fmt.Errorf("q8s data directory is not configured")
	}
	certDir := filepath.Join(s.config.DataDir, "certs")
	caCert, err := os.ReadFile(filepath.Join(certDir, "ca.crt"))
	if err != nil {
		return nil, nil, fmt.Errorf("read CA certificate: %w", err)
	}
	caKey, err := os.ReadFile(filepath.Join(certDir, "ca.key"))
	if err != nil {
		return nil, nil, fmt.Errorf("read CA key: %w", err)
	}
	return caCert, caKey, nil
}

// execCredential is the client.authentication.k8s.io/v1 ExecCredential
// document the credential plugin prints.
type execCredential struct {
	APIVersion string               `json:"apiVersion"`
	Kind       string               `json:"kind"`
	Status     execCredentialStatus `json:"status"`
}

type execCredentialStatus struct {
	ClientCertificateData string `json:"clientCertificateData"`
	ClientKeyData         string `json:"clientKeyData"`
	ExpirationTimestamp   string `json:"expirationTimestamp"`
}

// issueServiceAccountCredentialForWorkload signs the certificate for a
// workload's service account and binds it to that object with a URI SAN.
func issueServiceAccountCredentialForWorkload(caCertPEM, caKeyPEM []byte, target workloadTarget, now ...time.Time) ([]byte, error) {
	raw, err := target.Ref.URI()
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("build workload URI: %w", err)
	}
	return issueServiceAccountCredentialWithURIs(caCertPEM, caKeyPEM, target.Ref.Namespace, target.ServiceAccount, []*url.URL{u}, now...)
}

// issueServiceAccountCredentialWithURIs signs a one-hour client certificate
// for system:serviceaccount:{namespace}:{name} and wraps it in an
// ExecCredential. URIs are copied into the certificate's URI SANs.
func issueServiceAccountCredentialWithURIs(caCertPEM, caKeyPEM []byte, namespace, name string, uris []*url.URL, now ...time.Time) ([]byte, error) {
	if namespace == "" || name == "" {
		return nil, fmt.Errorf("serviceaccount namespace and name are required")
	}
	at := time.Now()
	if len(now) > 0 {
		at = now[0]
	}
	at = at.Truncate(time.Second)
	notBefore := at.Add(-time.Minute)
	notAfter := at.Add(workloadCredentialTTL)
	issued, err := install.IssueClientCertificateWithURIsAt(
		caCertPEM,
		caKeyPEM,
		fmt.Sprintf("system:serviceaccount:%s:%s", namespace, name),
		[]string{"system:serviceaccounts", "system:serviceaccounts:" + namespace},
		uris,
		notBefore,
		notAfter,
	)
	if err != nil {
		return nil, fmt.Errorf("issue service account certificate: %w", err)
	}
	data, err := json.Marshal(execCredential{
		APIVersion: "client.authentication.k8s.io/v1",
		Kind:       "ExecCredential",
		Status: execCredentialStatus{
			ClientCertificateData: string(issued.CertPEM),
			ClientKeyData:         string(issued.KeyPEM),
			ExpirationTimestamp:   notAfter.UTC().Format(time.RFC3339),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("encode exec credential: %w", err)
	}
	return append(data, '\n'), nil
}

// FetchWorkloadCredential is the client side of the workload socket, shared
// by `q8s credential` and the socket round-trip test. It sends the one-line
// request and returns the ExecCredential JSON exactly as the server wrote it.
func FetchWorkloadCredential(socketPath string) ([]byte, error) {
	if socketPath == "" {
		return nil, fmt.Errorf("workload socket path is empty")
	}
	conn, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connect to workload socket %s: %w", socketPath, err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		return nil, fmt.Errorf("set workload socket deadline: %w", err)
	}
	if _, err := fmt.Fprint(conn, "GET /credential\n"); err != nil {
		return nil, fmt.Errorf("send credential request: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(conn, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read credential response: %w", err)
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("workload socket returned no data")
	}
	var cred execCredential
	if err := json.Unmarshal(trimmed, &cred); err != nil {
		return nil, fmt.Errorf("workload socket: %s", trimmed)
	}
	if cred.Kind != "ExecCredential" || cred.Status.ClientCertificateData == "" || cred.Status.ClientKeyData == "" {
		return nil, fmt.Errorf("workload socket returned an incomplete credential")
	}
	return data, nil
}

// containerIDFromCgroup extracts a full 64-hex container id from cgroup
// content. Rootless Podman uses a libpod-<id>.scope unit, Quadlet's pod
// split uses libpod-payload-<id>, and cgroup v1 has one line per hierarchy.
// A host process has neither marker and is refused.
func containerIDFromCgroup(content string) (string, bool) {
	for _, line := range strings.Split(content, "\n") {
		// Longest marker first: "libpod-payload-" also contains "libpod-".
		for _, marker := range []string{"libpod-payload-", "libpod-"} {
			search := 0
			for {
				idx := strings.Index(line[search:], marker)
				if idx < 0 {
					break
				}
				rest := line[search+idx+len(marker):]
				if id, ok := fullContainerID(rest); ok {
					return id, true
				}
				search += idx + 1
			}
		}
	}
	return "", false
}

// fullContainerID accepts s only when it starts with exactly 64 hexadecimal
// characters followed by something that is not a hex digit.
func fullContainerID(s string) (string, bool) {
	const idLen = 64
	if len(s) < idLen {
		return "", false
	}
	for i := 0; i < idLen; i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return "", false
		}
	}
	if len(s) > idLen {
		c := s[idLen]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') {
			return "", false
		}
	}
	return s[:idLen], true
}

// listenWorkloadSocket listens on path. With SELinux enabled the listening
// socket is created with type spc_t: a confined container (container_t) may
// connect to an spc_t socket ("allow domain spc_t:unix_stream_socket
// connectto"), but not to one owned by q8s's own unconfined domain, and a
// socket's label is checked on connect, not the file mode. The sockcreate
// attribute is per thread, so the thread is locked for the set, the listen
// and the reset. Without SELinux, or if the label cannot be set, the socket
// is created normally (pods then get a clear permission error on connect).
func listenWorkloadSocket(path string) (net.Listener, string, error) {
	if _, err := os.Stat("/sys/fs/selinux/enforce"); err != nil {
		ln, err := net.Listen("unix", path)
		return ln, "", err
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	label, err := workloadSocketLabel()
	if err == nil {
		err = os.WriteFile("/proc/thread-self/attr/sockcreate", []byte(label), 0)
	}
	if err != nil {
		fmt.Printf("workload socket: SELinux label not set (%v); containers may be denied\n", err)
		ln, err := net.Listen("unix", path)
		return ln, "", err
	}
	ln, listenErr := net.Listen("unix", path)
	if err := os.WriteFile("/proc/thread-self/attr/sockcreate", nil, 0); err != nil {
		fmt.Printf("workload socket: reset SELinux sockcreate: %v\n", err)
	}
	return ln, label, listenErr
}

// workloadSocketLabel keeps the SELinux user of the current process and asks
// for system_r:spc_t:s0 (verified to be creatable by unconfined_t and
// unconfined_service_t, the domains q8s serve runs in).
func workloadSocketLabel() (string, error) {
	current, err := os.ReadFile("/proc/thread-self/attr/current")
	if err != nil {
		return "", err
	}
	user, _, ok := strings.Cut(strings.TrimRight(string(current), "\x00\n"), ":")
	if !ok || user == "" {
		return "", fmt.Errorf("unexpected SELinux context %q", current)
	}
	return user + ":system_r:spc_t:s0", nil
}

// workloadCredentialTTL is how long an issued ServiceAccount certificate is
// valid; the exec credential plugin is asked again after it expires.
const workloadCredentialTTL = time.Hour
