package server

import (
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
)

// pvcProtectionFinalizer is the finalizer Kubernetes puts on a claim that is
// being deleted while a workload still uses it. It is not stored on a claim
// that is removed immediately.
const pvcProtectionFinalizer = "kubernetes.io/pvc-protection"

// preparePVCUpdate applies the rules for updating deletion state: an update
// can neither clear nor invent deletionTimestamp, and removing the protection
// finalizer from a Terminating object forces the delete. It returns true when
// the caller must complete the delete.
func preparePVCUpdate(existing, updated *corev1.PersistentVolumeClaim) bool {
	updated.DeletionTimestamp = existing.DeletionTimestamp
	if existing.DeletionTimestamp == nil {
		return false
	}
	return pvcHasFinalizer(existing, pvcProtectionFinalizer) &&
		!pvcHasFinalizer(updated, pvcProtectionFinalizer)
}

func pvcHasFinalizer(pvc *corev1.PersistentVolumeClaim, finalizer string) bool {
	for _, f := range pvc.Finalizers {
		if f == finalizer {
			return true
		}
	}
	return false
}

// addPVCFinalizer appends finalizer once. The caller owns the object copy, so
// the slice is changed in place.
func addPVCFinalizer(pvc *corev1.PersistentVolumeClaim, finalizer string) {
	if pvcHasFinalizer(pvc, finalizer) {
		return
	}
	pvc.Finalizers = append(pvc.Finalizers, finalizer)
}

// deletePVCAndVolume runs the real delete of a claim: the store delete first,
// then the volume unit and quadlet cleanup. The caller holds the claim lock.
func (s *Server) deletePVCAndVolume(pvc *corev1.PersistentVolumeClaim) error {
	if err := s.config.Store.DeletePVC(pvc.Namespace, pvc.Name); err != nil {
		return err
	}
	s.removePVCVolume(pvc)
	return nil
}

// FinishTerminatingPVCs completes claims whose delete was deferred because a
// workload still used them. It runs inside the sync pass, synchronously, and
// reads only the in-memory store to decide. When no claim is Terminating the
// pass is one walk over the stored claims and no locks are taken.
func (s *Server) FinishTerminatingPVCs() {
	pvcs := s.config.Store.AllPVCs()
	sort.Slice(pvcs, func(i, j int) bool {
		if pvcs[i].Namespace != pvcs[j].Namespace {
			return pvcs[i].Namespace < pvcs[j].Namespace
		}
		return pvcs[i].Name < pvcs[j].Name
	})
	for _, pvc := range pvcs {
		if pvc.DeletionTimestamp == nil {
			continue
		}
		if _, _, used := s.pvcClaimUser(pvc.Namespace, pvc.Name); used {
			continue
		}
		s.finishTerminatingPVC(pvc.Namespace, pvc.Name)
	}
}

// finishTerminatingPVC takes the same locks a PVC DELETE takes (shared
// Namespace, then the claim) and re-reads and re-checks under them, so an
// update or delete of the claim is ordered and a user that appeared before the
// re-check is seen.
func (s *Server) finishTerminatingPVC(ns, name string) {
	unlockNS := s.lockObjectShared("Namespace", "", ns)
	defer unlockNS()
	unlock := s.lockObject("PersistentVolumeClaim", ns, name)
	defer unlock()

	pvc, err := s.config.Store.GetPVC(ns, name)
	if err != nil || pvc.DeletionTimestamp == nil {
		return
	}
	if _, _, used := s.pvcClaimUser(ns, name); used {
		return
	}
	if err := s.deletePVCAndVolume(pvc); err != nil {
		fmt.Printf("pvc finish %s/%s: %v\n", ns, name, err)
	}
}
