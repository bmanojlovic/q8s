package quadlet

import (
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
)

// AnnotationAttachAllNetworks asks q8s to attach the pod to every namespace
// network. Joining another namespace's network bypasses namespace isolation,
// so the annotation is host access and only an administrator may set it.
const AnnotationAttachAllNetworks = "q8s.io/attach-all-networks"

// HostAccessReasons lists every field in spec and every annotation that gives
// the resulting pod direct access to the host, in the order they appear in the
// specification. The fields are rendered as podman flags that either disable
// the rootful user namespace or join a host namespace, so configuring them is
// equivalent to host root. attach-all-networks joins every namespace network,
// which crosses the namespace isolation the per-namespace networks provide.
// The server uses this predicate for every write path and the generator uses
// it for the decisions that depend on the same fields.
func HostAccessReasons(spec corev1.PodSpec, annotations map[string]string) []string {
	var reasons []string
	for i, container := range spec.Containers {
		if containerPrivileged(container) {
			reasons = append(reasons,
				fmt.Sprintf("spec.containers[%d].securityContext.privileged", i))
		}
	}
	for i, container := range spec.InitContainers {
		if containerPrivileged(container) {
			reasons = append(reasons,
				fmt.Sprintf("spec.initContainers[%d].securityContext.privileged", i))
		}
	}
	if spec.HostPID {
		reasons = append(reasons, "spec.hostPID")
	}
	if spec.HostIPC {
		reasons = append(reasons, "spec.hostIPC")
	}
	if spec.HostNetwork {
		reasons = append(reasons, "spec.hostNetwork")
	}
	if spec.HostUsers != nil && *spec.HostUsers {
		reasons = append(reasons, "spec.hostUsers")
	}
	if raw, ok := annotations[AnnotationAttachAllNetworks]; ok {
		if attachAll, err := strconv.ParseBool(raw); err == nil && attachAll {
			reasons = append(reasons, "attach-all-networks")
		}
	}
	return reasons
}
