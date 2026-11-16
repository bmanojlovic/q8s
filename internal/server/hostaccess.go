package server

import (
	"fmt"
	"net/http"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"q8s/internal/quadlet"
)

// hostAccessReasons lists every field in spec that gives the resulting pod
// direct access to the host, in the order they appear in the specification.
// The fields are rendered as podman flags that either disable the rootful
// user namespace or join a host namespace, so configuring them is equivalent
// to host root and is reserved for administrators.
func hostAccessReasons(spec corev1.PodSpec) []string {
	var reasons []string
	for i, container := range spec.Containers {
		if containerSecurityContextPrivileged(container) {
			reasons = append(reasons,
				fmt.Sprintf("spec.containers[%d].securityContext.privileged", i))
		}
	}
	for i, container := range spec.InitContainers {
		if containerSecurityContextPrivileged(container) {
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
	return reasons
}

// containerSecurityContextPrivileged reports whether c asks for a privileged
// container. A nil securityContext or a nil privileged field means false.
func containerSecurityContextPrivileged(c corev1.Container) bool {
	return c.SecurityContext != nil && c.SecurityContext.Privileged != nil && *c.SecurityContext.Privileged
}

// pvcHostAccessReasons lists the fields that make a PVC a host path mount.
// The hostpath storage class and the q8s.io/host-path annotation both make the
// generator bind-mount a host directory chosen by the requester.
func pvcHostAccessReasons(pvc *corev1.PersistentVolumeClaim) []string {
	if pvc == nil {
		return nil
	}
	var reasons []string
	if pvc.Spec.StorageClassName != nil && *pvc.Spec.StorageClassName == quadlet.StorageClassHostPath {
		reasons = append(reasons, fmt.Sprintf("spec.storageClassName %q", quadlet.StorageClassHostPath))
	}
	if _, ok := pvc.Annotations[quadlet.AnnotationHostPath]; ok {
		reasons = append(reasons, fmt.Sprintf("metadata.annotations[%q]", quadlet.AnnotationHostPath))
	}
	return reasons
}

// rejectHostAccess answers 403 Forbidden and reports true when a non-admin
// caller submits an object that uses host access. Authentication being
// disabled (no CA) means every caller is treated as an administrator, as the
// authorizer does. callerInfo already reflects impersonation.
func (s *Server) rejectHostAccess(w http.ResponseWriter, r *http.Request, kind, name string, reasons []string) bool {
	if len(reasons) == 0 {
		return false
	}
	caller, ok := callerInfo(r)
	if !ok || isAdmin(caller) {
		return false
	}
	s.respondStatus(w, http.StatusForbidden, "Forbidden",
		"%s %q uses host access (%s), which only an administrator may configure",
		kind, name, strings.Join(reasons, ", "))
	return true
}

// rejectAppliedHostAccess evaluates the object in a server-side apply body
// before the apply middleware turns it into a PATCH or a create. The handlers
// check the resulting object again; this early check keeps a refused apply
// from reaching either write path. An unparseable or unsupported body is left
// for the normal apply validation to reject.
func (s *Server) rejectAppliedHostAccess(w http.ResponseWriter, r *http.Request, body []byte) bool {
	obj, _, err := codecs.UniversalDeserializer().Decode(body, nil, nil)
	if err != nil || obj == nil {
		return false
	}
	switch applied := obj.(type) {
	case *corev1.Pod:
		return s.rejectHostAccess(w, r, "Pod", applied.Name, hostAccessReasons(applied.Spec))
	case *appsv1.Deployment:
		return s.rejectHostAccess(w, r, "Deployment", applied.Name,
			hostAccessReasons(applied.Spec.Template.Spec))
	case *batchv1.Job:
		return s.rejectHostAccess(w, r, "Job", applied.Name,
			hostAccessReasons(applied.Spec.Template.Spec))
	case *batchv1.CronJob:
		return s.rejectHostAccess(w, r, "CronJob", applied.Name,
			hostAccessReasons(applied.Spec.JobTemplate.Spec.Template.Spec))
	case *corev1.PersistentVolumeClaim:
		return s.rejectHostAccess(w, r, "PersistentVolumeClaim", applied.Name,
			pvcHostAccessReasons(applied))
	default:
		return false
	}
}
