package server

import (
	"fmt"
	"sort"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"q8s/internal/quadlet"
)

// pvcWaitReason is the Kubernetes scheduling reason shown while a Pod's
// PersistentVolumeClaim does not exist yet.
const pvcWaitReason = "Unschedulable"

// pvcWaitAnnotation marks a CronJob whose quadlet files were not rendered
// because one of its claims was missing. A CronJob has no Pod status to carry
// the PodScheduled condition Pods and Jobs use, so its object metadata holds
// the marker instead.
const pvcWaitAnnotation = "q8s.io/waiting-for-pvc"

// pvcWaitMessage is the message Kubernetes uses for an unschedulable pod whose
// claim does not exist.
func pvcWaitMessage(claim string) string {
	return fmt.Sprintf("persistentvolumeclaim %q not found", claim)
}

// pvcWaitDeletingMessage is the message Kubernetes uses for an unschedulable
// pod whose claim is Terminating.
func pvcWaitDeletingMessage(claim string) string {
	return fmt.Sprintf("persistentvolumeclaim %q is being deleted", claim)
}

// pvcWaitClaimFromMessage extracts the claim name from either wait message.
// The exact shape is required so an arbitrary Unschedulable condition cannot
// be mistaken for this server-owned marker.
func pvcWaitClaimFromMessage(message string) (string, bool) {
	const prefix = `persistentvolumeclaim "`
	for _, suffix := range []string{`" not found`, `" is being deleted`} {
		if !strings.HasPrefix(message, prefix) || !strings.HasSuffix(message, suffix) {
			continue
		}
		claim := strings.TrimSuffix(strings.TrimPrefix(message, prefix), suffix)
		if claim != "" {
			return claim, true
		}
	}
	return "", false
}

// pvcWaitClaimFromPod returns the claim whose wait marker pod carries.
func pvcWaitClaimFromPod(pod *corev1.Pod) (string, bool) {
	for _, c := range pod.Status.Conditions {
		if c.Type != corev1.PodScheduled || c.Status != corev1.ConditionFalse || c.Reason != pvcWaitReason {
			continue
		}
		if claim, ok := pvcWaitClaimFromMessage(c.Message); ok {
			return claim, true
		}
	}
	return "", false
}

// podWaitingForClaim reports whether pod carries the in-memory marker set when
// its unit was not rendered because a claim was missing or Terminating.
func podWaitingForClaim(pod *corev1.Pod) bool {
	_, ok := pvcWaitClaimFromPod(pod)
	return ok
}

// firstMissingClaim returns the first claim in spec that cannot be mounted:
// either it is not in the store or it is Terminating. The message matches the
// reason, so the wait marker shows the same text a kubelet would. The
// in-memory store is the only source: no quadlet file is read or stat'ed.
func (s *Server) firstMissingClaim(ns string, spec corev1.PodSpec) (claim, message string, ok bool) {
	seen := make(map[string]bool)
	for _, vol := range spec.Volumes {
		if vol.PersistentVolumeClaim == nil {
			continue
		}
		claim := vol.PersistentVolumeClaim.ClaimName
		if claim == "" || seen[claim] {
			continue
		}
		seen[claim] = true
		pvc, err := s.config.Store.GetPVC(ns, claim)
		if err != nil {
			return claim, pvcWaitMessage(claim), true
		}
		if pvc.DeletionTimestamp != nil {
			return claim, pvcWaitDeletingMessage(claim), true
		}
	}
	return "", "", false
}

// markPodWaitingForClaim records the unschedulable condition on the stored pod.
// It is a store-only mutation so a create request can leave the pod visibly
// waiting without touching systemd or the filesystem.
func (s *Server) markPodWaitingForClaim(ns, name, claim, message string) {
	pod, err := s.config.Store.GetPod(ns, name)
	if err != nil {
		fmt.Printf("pvc wait: mark pod %s/%s: %v\n", ns, name, err)
		return
	}
	unchangedStatus := pod.Status.Phase == corev1.PodPending && pod.Status.StartTime != nil
	if pod.Status.Phase != corev1.PodPending {
		pod.Status.Phase = corev1.PodPending
	}
	if pod.Status.StartTime == nil {
		now := metav1.Now()
		pod.Status.StartTime = &now
	}
	now := metav1.Now()
	cond := corev1.PodCondition{
		Type:               corev1.PodScheduled,
		Status:             corev1.ConditionFalse,
		Reason:             pvcWaitReason,
		Message:            message,
		LastTransitionTime: now,
	}
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type != corev1.PodScheduled {
			continue
		}
		if pod.Status.Conditions[i].Status == cond.Status &&
			pod.Status.Conditions[i].Reason == cond.Reason &&
			pod.Status.Conditions[i].Message == cond.Message &&
			unchangedStatus {
			return
		}
		pod.Status.Conditions[i] = cond
		if _, err := s.config.Store.UpdatePod(pod); err != nil {
			fmt.Printf("pvc wait: mark pod %s/%s: %v\n", ns, name, err)
		}
		return
	}
	pod.Status.Conditions = append(pod.Status.Conditions, cond)
	if _, err := s.config.Store.UpdatePod(pod); err != nil {
		fmt.Printf("pvc wait: mark pod %s/%s: %v\n", ns, name, err)
	}
}

// clearPodWaitingForClaim removes the waiting marker once the pod's unit has
// been rendered. PodScheduled becomes true so readers see a scheduled pod
// immediately; the next status sync fills in the remaining conditions.
func (s *Server) clearPodWaitingForClaim(ns, name string) {
	pod, err := s.config.Store.GetPod(ns, name)
	if err != nil || !podWaitingForClaim(pod) {
		return
	}
	now := metav1.Now()
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type != corev1.PodScheduled {
			continue
		}
		if pod.Status.Conditions[i].Status == corev1.ConditionFalse &&
			pod.Status.Conditions[i].Reason == pvcWaitReason {
			pod.Status.Conditions[i] = corev1.PodCondition{
				Type:               corev1.PodScheduled,
				Status:             corev1.ConditionTrue,
				LastTransitionTime: now,
			}
		}
	}
	if _, err := s.config.Store.UpdatePod(pod); err != nil {
		fmt.Printf("pvc wait: clear pod %s/%s: %v\n", ns, name, err)
	}
}

// pvcWaitClaimFromJob returns the claim whose wait marker job carries.
func pvcWaitClaimFromJob(job *batchv1.Job) (string, bool) {
	for _, c := range job.Status.Conditions {
		if string(c.Type) != string(corev1.PodScheduled) || c.Status != corev1.ConditionFalse || c.Reason != pvcWaitReason {
			continue
		}
		if claim, ok := pvcWaitClaimFromMessage(c.Message); ok {
			return claim, true
		}
	}
	return "", false
}

// jobWaitingForClaim reports whether job carries the same waiting marker in its
// status conditions. Jobs have no Pod object in the store, so the condition is
// kept on the Job until its unit is rendered.
func jobWaitingForClaim(job *batchv1.Job) bool {
	_, ok := pvcWaitClaimFromJob(job)
	return ok
}

func (s *Server) markJobWaitingForClaim(ns, name, claim, message string) {
	job, err := s.config.Store.GetJob(ns, name)
	if err != nil {
		fmt.Printf("pvc wait: mark job %s/%s: %v\n", ns, name, err)
		return
	}
	now := metav1.Now()
	cond := batchv1.JobCondition{
		Type:               batchv1.JobConditionType(corev1.PodScheduled),
		Status:             corev1.ConditionFalse,
		Reason:             pvcWaitReason,
		Message:            message,
		LastTransitionTime: now,
	}
	for i := range job.Status.Conditions {
		if job.Status.Conditions[i].Type != cond.Type {
			continue
		}
		if job.Status.Conditions[i].Status == cond.Status &&
			job.Status.Conditions[i].Reason == cond.Reason &&
			job.Status.Conditions[i].Message == cond.Message {
			return
		}
		job.Status.Conditions[i] = cond
		if _, err := s.config.Store.UpdateJob(job); err != nil {
			fmt.Printf("pvc wait: mark job %s/%s: %v\n", ns, name, err)
		}
		return
	}
	job.Status.Conditions = append(job.Status.Conditions, cond)
	if _, err := s.config.Store.UpdateJob(job); err != nil {
		fmt.Printf("pvc wait: mark job %s/%s: %v\n", ns, name, err)
	}
}

func (s *Server) clearJobWaitingForClaim(ns, name string) {
	job, err := s.config.Store.GetJob(ns, name)
	if err != nil || !jobWaitingForClaim(job) {
		return
	}
	conds := job.Status.Conditions[:0]
	for _, c := range job.Status.Conditions {
		if string(c.Type) == string(corev1.PodScheduled) && c.Status == corev1.ConditionFalse && c.Reason == pvcWaitReason {
			continue
		}
		conds = append(conds, c)
	}
	job.Status.Conditions = conds
	if _, err := s.config.Store.UpdateJob(job); err != nil {
		fmt.Printf("pvc wait: clear job %s/%s: %v\n", ns, name, err)
	}
}

func cronJobWaitingForClaim(cj *batchv1.CronJob) bool {
	claim, ok := cj.Annotations[pvcWaitAnnotation]
	return ok && claim != ""
}

func (s *Server) markCronJobWaitingForClaim(ns, name, claim string) {
	cj, err := s.config.Store.GetCronJob(ns, name)
	if err != nil {
		fmt.Printf("pvc wait: mark cronjob %s/%s: %v\n", ns, name, err)
		return
	}
	if cj.Annotations == nil {
		cj.Annotations = make(map[string]string)
	}
	if cj.Annotations[pvcWaitAnnotation] == claim {
		return
	}
	cj.Annotations[pvcWaitAnnotation] = claim
	if _, err := s.config.Store.UpdateCronJob(cj); err != nil {
		fmt.Printf("pvc wait: mark cronjob %s/%s: %v\n", ns, name, err)
	}
}

func (s *Server) clearCronJobWaitingForClaim(ns, name string) {
	cj, err := s.config.Store.GetCronJob(ns, name)
	if err != nil {
		return
	}
	if _, ok := cj.Annotations[pvcWaitAnnotation]; !ok {
		return
	}
	delete(cj.Annotations, pvcWaitAnnotation)
	if _, err := s.config.Store.UpdateCronJob(cj); err != nil {
		fmt.Printf("pvc wait: clear cronjob %s/%s: %v\n", ns, name, err)
	}
}

// StartPodsWaitingForClaims renders and starts workloads that were held back
// because a PersistentVolumeClaim they mount did not exist. The marker on the
// stored object is the only trigger: a pod without it is never touched, so an
// already running unit is never restarted. The check reads only the in-memory
// store; when nothing waits it makes no podman, systemd or filesystem calls.
//
// This is called from the sync loop's reconcile closure and once at startup
// after ReconcileQuadlets, so no extra loop or timer is needed.
func (s *Server) StartPodsWaitingForClaims() {
	pods := s.config.Store.AllPods()
	sort.Slice(pods, func(i, j int) bool {
		if pods[i].Namespace != pods[j].Namespace {
			return pods[i].Namespace < pods[j].Namespace
		}
		return pods[i].Name < pods[j].Name
	})
	for _, pod := range pods {
		if podWaitingForClaim(pod) {
			s.startWaitingPod(pod.Namespace, pod.Name)
		}
	}

	jobs := s.config.Store.AllJobs()
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].Namespace != jobs[j].Namespace {
			return jobs[i].Namespace < jobs[j].Namespace
		}
		return jobs[i].Name < jobs[j].Name
	})
	for _, job := range jobs {
		if jobWaitingForClaim(job) {
			s.startWaitingJob(job.Namespace, job.Name)
		}
	}

	cronJobs := s.config.Store.AllCronJobs()
	sort.Slice(cronJobs, func(i, j int) bool {
		if cronJobs[i].Namespace != cronJobs[j].Namespace {
			return cronJobs[i].Namespace < cronJobs[j].Namespace
		}
		return cronJobs[i].Name < cronJobs[j].Name
	})
	for _, cj := range cronJobs {
		if cronJobWaitingForClaim(cj) {
			s.startWaitingCronJob(cj.Namespace, cj.Name)
		}
	}
}

// The start helpers below take the same locks an API write of the object
// takes (shared Namespace, then the object; Deployment before its replica
// Pod) and re-read the object under them, so a concurrent delete or update
// of the same object either finishes first or waits for the render.

func (s *Server) startWaitingPod(ns, name string) {
	unlockNS := s.lockObjectShared("Namespace", "", ns)
	defer unlockNS()
	pod, err := s.config.Store.GetPod(ns, name)
	if err != nil {
		return
	}
	if depName, index, ok := quadlet.DeploymentReplica(pod); ok {
		unlockDep := s.lockObject("Deployment", ns, depName)
		defer unlockDep()
		unlockPod := s.lockObject("Pod", ns, name)
		defer unlockPod()
		pod, err = s.config.Store.GetPod(ns, name)
		if err != nil || !podWaitingForClaim(pod) {
			return
		}
		if claim, message, missing := s.firstMissingClaim(ns, pod.Spec); missing {
			s.markPodWaitingForClaim(ns, name, claim, message)
			return
		}
		dep, err := s.config.Store.GetDeployment(ns, depName)
		if err != nil {
			fmt.Printf("pvc wait: deployment %s/%s for pod %s: %v\n", ns, depName, name, err)
			return
		}
		if index >= deploymentReplicas(dep) {
			return
		}
		s.deployDeploymentInstance(dep, index, false)
		return
	}
	unlockPod := s.lockObject("Pod", ns, name)
	defer unlockPod()
	pod, err = s.config.Store.GetPod(ns, name)
	if err != nil || !podWaitingForClaim(pod) {
		return
	}
	if claim, message, missing := s.firstMissingClaim(ns, pod.Spec); missing {
		s.markPodWaitingForClaim(ns, name, claim, message)
		return
	}
	s.generatePodQuadlet(pod)
}

func (s *Server) startWaitingJob(ns, name string) {
	unlockNS := s.lockObjectShared("Namespace", "", ns)
	defer unlockNS()
	unlock := s.lockObject("Job", ns, name)
	defer unlock()
	job, err := s.config.Store.GetJob(ns, name)
	if err != nil || !jobWaitingForClaim(job) {
		return
	}
	if claim, message, missing := s.firstMissingClaim(ns, job.Spec.Template.Spec); missing {
		s.markJobWaitingForClaim(ns, name, claim, message)
		return
	}
	s.generateJobQuadlet(job)
}

func (s *Server) startWaitingCronJob(ns, name string) {
	unlockNS := s.lockObjectShared("Namespace", "", ns)
	defer unlockNS()
	unlock := s.lockObject("CronJob", ns, name)
	defer unlock()
	cj, err := s.config.Store.GetCronJob(ns, name)
	if err != nil || !cronJobWaitingForClaim(cj) {
		return
	}
	if claim, _, missing := s.firstMissingClaim(ns, cj.Spec.JobTemplate.Spec.Template.Spec); missing {
		s.markCronJobWaitingForClaim(ns, name, claim)
		return
	}
	// A CronJob is schedule-driven: the wait path renders its container and
	// timer but does not start the timer immediately.
	s.renderCronJobQuadlets(cj)
}
