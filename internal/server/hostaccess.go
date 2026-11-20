package server

import (
	"fmt"
	"net/http"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"q8s/internal/quadlet"
)

// pvcStorageClass returns the effective storage class for a PVC, defaulting
// to standard when the field is nil or empty. It mirrors the generator's
// resolution so the same PVC is classified the same way on both sides.
func pvcStorageClass(pvc *corev1.PersistentVolumeClaim) string {
	if pvc.Spec.StorageClassName != nil && *pvc.Spec.StorageClassName != "" {
		return *pvc.Spec.StorageClassName
	}
	return quadlet.StorageClassStandard
}

// pvcHostAccessReasons lists the fields that make a PVC a host path mount.
// The generator only bind-mounts a host directory when the resolved storage
// class is hostpath; the q8s.io/host-path annotation alone, on a PVC of any
// other class, has no effect and is therefore not host access.
func pvcHostAccessReasons(pvc *corev1.PersistentVolumeClaim) []string {
	if pvc == nil || pvcStorageClass(pvc) != quadlet.StorageClassHostPath {
		return nil
	}
	reasons := []string{fmt.Sprintf("spec.storageClassName %q", quadlet.StorageClassHostPath)}
	if _, ok := pvc.Annotations[quadlet.AnnotationHostPath]; ok {
		reasons = append(reasons, fmt.Sprintf("metadata.annotations[%q]", quadlet.AnnotationHostPath))
	}
	return reasons
}

// serviceHostAccessReasons lists the fields that make a Service a host
// access object. An ExternalName Service makes Traefik resolve a name outside
// the pod network, and that name can resolve to an address inside another
// namespace's network, so only an administrator may create or keep one.
func serviceHostAccessReasons(svc *corev1.Service) []string {
	if svc == nil || svc.Spec.Type != corev1.ServiceTypeExternalName {
		return nil
	}
	return []string{"spec.type: ExternalName"}
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
