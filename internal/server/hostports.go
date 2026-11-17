package server

import (
	"fmt"
	"net"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"q8s/internal/quadlet"
)

// hostPortClaim is one host listener a workload or Service wants to own.
// Addr is empty for all addresses. Proto is TCP or UDP. Kind is "socket",
// "hostPort" or "nodePort", and Owner is the object the claim belongs to.
type hostPortClaim struct {
	Port  int32
	Addr  string
	Proto string
	Owner string
	Kind  string
}

// normalizeHostPortAddr converts a HostIP or socket listen address to the
// comparison form used by hostPortClaim. An empty address, 0.0.0.0 and :: all
// mean "all addresses" and compare equal to each other; any other address is
// kept as its canonical IP literal.
func normalizeHostPortAddr(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	ip := net.ParseIP(addr)
	if ip == nil {
		return addr
	}
	if ip.IsUnspecified() {
		return ""
	}
	return ip.String()
}

// normalizeHostPortProto defaults an unset protocol to TCP, matching the
// Kubernetes default, and normalizes the case for comparisons.
func normalizeHostPortProto(proto corev1.Protocol) string {
	if proto == "" {
		return "TCP"
	}
	return strings.ToUpper(string(proto))
}

// hostPortClaimsForWorkload builds the claims of a Pod or Deployment pod
// template. Socket activation entries are always TCP (ListenStream); every
// hostPort uses the port's protocol and host IP.
func hostPortClaimsForWorkload(owner string, spec *corev1.PodSpec, annotations map[string]string) []hostPortClaim {
	var claims []hostPortClaim
	entries, err := quadlet.ParseSocketActivation(annotations)
	if err == nil {
		for _, entry := range entries {
			claims = append(claims, hostPortClaim{
				Port:  entry.Port,
				Addr:  normalizeHostPortAddr(entry.Addr),
				Proto: "TCP",
				Owner: owner,
				Kind:  "socket",
			})
		}
	}
	if spec == nil {
		return claims
	}
	containers := make([]corev1.Container, 0, len(spec.InitContainers)+len(spec.Containers))
	containers = append(containers, spec.InitContainers...)
	containers = append(containers, spec.Containers...)
	for _, container := range containers {
		for _, port := range container.Ports {
			if port.HostPort == 0 {
				continue
			}
			claims = append(claims, hostPortClaim{
				Port:  port.HostPort,
				Addr:  normalizeHostPortAddr(port.HostIP),
				Proto: normalizeHostPortProto(port.Protocol),
				Owner: owner,
				Kind:  "hostPort",
			})
		}
	}
	return claims
}

// hostPortClaimsForService builds the claims of a NodePort Service. Other
// Service types bind no host port. A nodePort listens on all addresses.
func hostPortClaimsForService(svc *corev1.Service) []hostPortClaim {
	if svc == nil || svc.Spec.Type != corev1.ServiceTypeNodePort {
		return nil
	}
	owner := fmt.Sprintf("Service %s/%s", svc.Namespace, svc.Name)
	var claims []hostPortClaim
	for _, port := range svc.Spec.Ports {
		if port.NodePort == 0 {
			continue
		}
		claims = append(claims, hostPortClaim{
			Port:  port.NodePort,
			Addr:  "",
			Proto: normalizeHostPortProto(port.Protocol),
			Owner: owner,
			Kind:  "nodePort",
		})
	}
	return claims
}

// hostPortClaimsOverlap reports whether two claims want the same host
// listener. The same port and protocol collide when either side listens on
// all addresses or both name the same address.
func hostPortClaimsOverlap(a, b hostPortClaim) bool {
	if a.Port != b.Port || a.Proto != b.Proto {
		return false
	}
	return a.Addr == "" || b.Addr == "" || a.Addr == b.Addr
}

// existingHostPortClaims collects every claim already in the store, except
// the object being written. Deployment replica pods are represented by their
// Deployment and are never counted as bare pods.
// ownerDeployment, when not empty, also excludes that Deployment in the same
// namespace: a replica pod carries its Deployment's template, so it must not
// collide with it.
func (s *Server) existingHostPortClaims(excludeKind, excludeNamespace, excludeName, ownerDeployment string) []hostPortClaim {
	excluded := func(kind, namespace, name string) bool {
		if kind == "Deployment" && ownerDeployment != "" && namespace == excludeNamespace && name == ownerDeployment {
			return true
		}
		return kind == excludeKind && namespace == excludeNamespace && name == excludeName
	}
	var claims []hostPortClaim
	for _, pod := range s.config.Store.AllPods() {
		if isDeploymentOwned(pod) {
			continue
		}
		if excluded("Pod", pod.Namespace, pod.Name) {
			continue
		}
		owner := fmt.Sprintf("Pod %s/%s", pod.Namespace, pod.Name)
		claims = append(claims, hostPortClaimsForWorkload(owner, &pod.Spec, pod.Annotations)...)
	}
	for _, dep := range s.config.Store.AllDeployments() {
		if excluded("Deployment", dep.Namespace, dep.Name) {
			continue
		}
		owner := fmt.Sprintf("Deployment %s/%s", dep.Namespace, dep.Name)
		claims = append(claims, hostPortClaimsForWorkload(owner, &dep.Spec.Template.Spec, dep.Spec.Template.Annotations)...)
	}
	for _, svc := range s.config.Store.AllServices() {
		if excluded("Service", svc.Namespace, svc.Name) {
			continue
		}
		claims = append(claims, hostPortClaimsForService(svc)...)
	}
	return claims
}

// hostPortClaimConflict checks the new claim kinds introduced by this
// ticket: a socket is exclusive against every other host listener, and a
// hostPort or nodePort may not take a port another object uses as a socket.
func hostPortClaimConflict(written, existing []hostPortClaim) (hostPortClaim, hostPortClaim, bool) {
	for _, candidate := range written {
		for _, other := range existing {
			collides := candidate.Kind == "socket" ||
				((candidate.Kind == "hostPort" || candidate.Kind == "nodePort") && other.Kind == "socket")
			if collides && hostPortClaimsOverlap(candidate, other) {
				return candidate, other, true
			}
		}
	}
	return hostPortClaim{}, hostPortClaim{}, false
}

// hostPortClaimLabel renders a claim kind for the conflict message. A socket
// is described as "socket port", matching the wording used elsewhere for
// hostPort and nodePort.
func hostPortClaimLabel(kind string) string {
	if kind == "socket" {
		return "socket port"
	}
	return kind
}

// hostPortConflictError returns the one-line conflict message for a new
// claim that overlaps an existing one.
func hostPortConflictError(written, existing hostPortClaim) error {
	return fmt.Errorf("%s %d/%s collides with %s of %s",
		hostPortClaimLabel(written.Kind), written.Port, written.Proto,
		hostPortClaimLabel(existing.Kind), existing.Owner)
}

// podHostPortConflict checks a Pod write against the claims already in the
// store. The Pod itself is excluded, so an unchanged update never conflicts
// with its own claims. A conflict is returned as the 409 message text.
func (s *Server) podHostPortConflict(pod *corev1.Pod) error {
	if pod == nil {
		return nil
	}
	owner := fmt.Sprintf("Pod %s/%s", pod.Namespace, pod.Name)
	written := hostPortClaimsForWorkload(owner, &pod.Spec, pod.Annotations)
	ownerDeployment := ""
	for _, or := range pod.OwnerReferences {
		if or.Kind == "Deployment" {
			ownerDeployment = or.Name
		}
	}
	existing := s.existingHostPortClaims("Pod", pod.Namespace, pod.Name, ownerDeployment)
	if claim, other, conflict := hostPortClaimConflict(written, existing); conflict {
		return hostPortConflictError(claim, other)
	}
	return nil
}

// deploymentHostPortConflict checks a Deployment write against the claims
// already in the store. The Deployment itself is excluded, and because
// replica pods are skipped as bare pods, its own replicas are not counted.
func (s *Server) deploymentHostPortConflict(dep *appsv1.Deployment) error {
	if dep == nil {
		return nil
	}
	owner := fmt.Sprintf("Deployment %s/%s", dep.Namespace, dep.Name)
	written := hostPortClaimsForWorkload(owner, &dep.Spec.Template.Spec, dep.Spec.Template.Annotations)
	existing := s.existingHostPortClaims("Deployment", dep.Namespace, dep.Name, "")
	if claim, other, conflict := hostPortClaimConflict(written, existing); conflict {
		return hostPortConflictError(claim, other)
	}
	return nil
}

// serviceHostPortConflict checks a Service write against existing socket
// claims. Other hostPort and nodePort collisions keep their existing checks
// and messages in handler.go.
func (s *Server) serviceHostPortConflict(svc *corev1.Service) error {
	if svc == nil {
		return nil
	}
	written := hostPortClaimsForService(svc)
	existing := s.existingHostPortClaims("Service", svc.Namespace, svc.Name, "")
	if claim, other, conflict := hostPortClaimConflict(written, existing); conflict && other.Kind == "socket" {
		return hostPortConflictError(claim, other)
	}
	return nil
}
