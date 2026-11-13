package server

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"q8s/internal/cronruns"
)

// Labels and the suffix of the Pod derived from a stored Job.
const (
	jobNameLabel      = "job-name"
	batchJobNameLabel = "batch.kubernetes.io/job-name"
	jobPodSuffix      = "-run"
)

// ensureJobPodIdentity adds the selector and template labels kubectl relies
// on when they are missing. Kubernetes sets the same job-name labels on a Job
// and its Pods; without them a Job has no selector, and kubectl logs job/X
// falls back to selecting every pod in the namespace.
func ensureJobPodIdentity(job *batchv1.Job) {
	if job == nil {
		return
	}
	if job.Spec.Template.Labels == nil {
		job.Spec.Template.Labels = make(map[string]string)
	}
	if _, ok := job.Spec.Template.Labels[jobNameLabel]; !ok {
		job.Spec.Template.Labels[jobNameLabel] = job.Name
	}
	if _, ok := job.Spec.Template.Labels[batchJobNameLabel]; !ok {
		job.Spec.Template.Labels[batchJobNameLabel] = job.Name
	}
	if job.Spec.Selector == nil ||
		(len(job.Spec.Selector.MatchLabels) == 0 && len(job.Spec.Selector.MatchExpressions) == 0) {
		job.Spec.Selector = &metav1.LabelSelector{
			MatchLabels: map[string]string{jobNameLabel: job.Name},
		}
	}
}

// normalizeJobs adds the derived Pod identity to Job copies on read, so Jobs
// stored before this defaulting still return a usable selector and template.
func normalizeJobs(jobs []*batchv1.Job) []*batchv1.Job {
	for _, job := range jobs {
		ensureJobPodIdentity(job)
	}
	return jobs
}

// jobPodObjects derives one Pod per stored Job in ns (all namespaces when ns
// is empty). Like CronJob runs it reads the journal on every call and keeps
// no state. A Job pod exists even without a journal source: its phase then
// comes from the Job status the sync loop stores.
func (s *Server) jobPodObjects(ctx context.Context, ns string) []*corev1.Pod {
	var jobs []*batchv1.Job
	if ns == "" {
		jobs = s.config.Store.AllJobs()
	} else {
		jobs = s.config.Store.Jobs(ns)
	}
	pods := make([]*corev1.Pod, 0, len(jobs))
	for _, job := range jobs {
		var run *cronruns.Run
		if runs := s.jobRuns(ctx, job); len(runs) > 0 {
			run = &runs[0]
		}
		pods = append(pods, jobPodObjectFor(job, run))
	}
	sortCronRunPods(pods)
	return pods
}

// jobRuns reads the runs of a Job's unit, newest first. Runs from before the
// Job object was created belong to a previous Job with the same name. When
// there is no run since creation, the unit's last known run is still the best
// available state, so it is returned instead of pretending the Job never ran.
func (s *Server) jobRuns(ctx context.Context, job *batchv1.Job) []cronruns.Run {
	if s.config.CronRuns == nil {
		return nil
	}
	runs, err := s.config.CronRuns.RunsForUnit(ctx, cronruns.JobUnitName(job.Namespace, job.Name))
	if err != nil || len(runs) == 0 {
		return nil
	}
	if kept := runsSinceTime(runs, job.CreationTimestamp.Time); len(kept) > 0 {
		return kept
	}
	return runs
}

func (s *Server) jobPodFindPod(ctx context.Context, ns, name string) (*corev1.Pod, bool) {
	for _, pod := range s.jobPodObjects(ctx, ns) {
		if pod.Name == name {
			return pod, true
		}
	}
	return nil, false
}

// derivedPodsExtra combines CronJob run pods and plain Job pods for
// respondListMerged. CronJob run pods need the journal source; Job pods do
// not, their phase then comes from the stored Job status.
func (s *Server) derivedPodsExtra(ctx context.Context, ns string) func() []*corev1.Pod {
	cronExtra := s.cronRunPodsExtra(ctx, ns)
	return func() []*corev1.Pod {
		pods := s.jobPodObjects(ctx, ns)
		if cronExtra != nil {
			// A stored Job wins a name clash with a CronJob run, matching
			// getJob.
			pods = append(pods, cronExtra()...)
		}
		return pods
	}
}

// rejectJobPodWrite answers 405 for writes to a pod that only exists as a
// derived Job pod. A stored pod with the same name keeps normal behaviour.
func (s *Server) rejectJobPodWrite(w http.ResponseWriter, r *http.Request, ns, name string) bool {
	if name == "" {
		return false
	}
	switch r.Method {
	case http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return false
	}
	if _, err := s.config.Store.GetPod(ns, name); err == nil {
		return false
	}
	if _, ok := s.jobPodFindPod(r.Context(), ns, name); !ok {
		return false
	}
	s.respondStatus(w, http.StatusMethodNotAllowed, "MethodNotAllowed",
		"Pod %s is a Job run derived from the systemd journal: delete/update is not implemented", name)
	return true
}

// handleJobPodLogs serves a Job pod's logs from the job unit. The unit is the
// same one JobContainer renders, <ns>-<name>-job.service.
func (s *Server) handleJobPodLogs(w http.ResponseWriter, r *http.Request, pod *corev1.Pod) {
	jobName := pod.Labels[jobNameLabel]
	s.handleJournalPodLogs(w, r, pod,
		cronruns.JobUnitName(pod.Namespace, jobName),
		cronruns.JobContainerName(pod.Namespace, jobName))
}

func jobPodObjectFor(job *batchv1.Job, run *cronruns.Run) *corev1.Pod {
	labels := make(map[string]string, len(job.Spec.Template.Labels)+2)
	for k, v := range job.Spec.Template.Labels {
		labels[k] = v
	}
	// The Job's own name wins: kubectl finds this pod through the Job
	// selector, which ensureJobPodIdentity points at job-name.
	labels[jobNameLabel] = job.Name
	labels[batchJobNameLabel] = job.Name

	annotations := make(map[string]string, len(job.Spec.Template.Annotations)+1)
	for k, v := range job.Spec.Template.Annotations {
		annotations[k] = v
	}

	podSpec := job.Spec.Template.Spec.DeepCopy()
	containerName := ""
	containerImage := ""
	if len(podSpec.Containers) > 0 {
		containerName = podSpec.Containers[0].Name
		containerImage = podSpec.Containers[0].Image
	}

	phase := jobPodPhaseFromStatus(job)
	// A non-Pending status is the unit state the sync loop observed, so it
	// wins over a journal run that may predate the current Job. The journal
	// fills in the phase while that status is still empty.
	if run != nil && phase == corev1.PodPending {
		phase = jobPodPhaseFromRun(*run)
	}
	// Ignore run details that contradict the unit state, for example the last
	// run of an earlier Job with the same name while the new one is active.
	if run != nil && jobPodPhaseFromRun(*run) != phase {
		run = nil
	}

	creation := job.CreationTimestamp
	var start *metav1.Time
	if run != nil {
		creation = metav1.NewTime(run.Start)
		start = &metav1.Time{Time: run.Start}
		annotations[cronRunInvocationAnnotation] = run.InvocationID
	}

	pod := &corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{
			Name:              job.Name + jobPodSuffix,
			Namespace:         job.Namespace,
			UID:               jobPodUID(job),
			ResourceVersion:   jobPodResourceVersion(job, run),
			CreationTimestamp: creation,
			Labels:            labels,
			Annotations:       annotations,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1",
				Kind:       "Job",
				Name:       job.Name,
				UID:        job.UID,
				Controller: boolPtr(true),
			}},
		},
		Spec: *podSpec,
	}
	pod.Status.Phase = phase
	pod.Status.StartTime = start

	if run == nil {
		return pod
	}

	status := corev1.ContainerStatus{Name: containerName, Image: containerImage}
	end := run.End
	if end.IsZero() {
		end = run.Start
	}
	switch run.State {
	case cronruns.StateSucceeded:
		status.State.Terminated = &corev1.ContainerStateTerminated{
			ExitCode:   finishedExitCode(*run, 0),
			Reason:     "Completed",
			StartedAt:  metav1.NewTime(run.Start),
			FinishedAt: metav1.NewTime(end),
		}
	case cronruns.StateFailed:
		status.State.Terminated = &corev1.ContainerStateTerminated{
			ExitCode:   finishedExitCode(*run, 1),
			Reason:     "Error",
			StartedAt:  metav1.NewTime(run.Start),
			FinishedAt: metav1.NewTime(end),
		}
	default:
		status.State.Running = &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(run.Start)}
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{status}
	return pod
}

func jobPodPhaseFromRun(run cronruns.Run) corev1.PodPhase {
	switch run.State {
	case cronruns.StateSucceeded:
		return corev1.PodSucceeded
	case cronruns.StateFailed:
		return corev1.PodFailed
	default:
		return corev1.PodRunning
	}
}

// jobPodPhaseFromStatus maps the counts the sync loop stores from the
// systemd unit state to a Pod phase.
func jobPodPhaseFromStatus(job *batchv1.Job) corev1.PodPhase {
	switch {
	case job.Status.Active > 0:
		return corev1.PodRunning
	case job.Status.Succeeded > 0:
		return corev1.PodSucceeded
	case job.Status.Failed > 0:
		return corev1.PodFailed
	default:
		return corev1.PodPending
	}
}

func jobPodUID(job *batchv1.Job) types.UID {
	seed := string(job.UID)
	if seed == "" {
		seed = job.Namespace + "/" + job.Name
	}
	return cronRunPodUID(seed)
}

func jobPodResourceVersion(job *batchv1.Job, run *cronruns.Run) string {
	if run != nil {
		return cronRunResourceVersion("job-pod", *run)
	}
	if job.ResourceVersion != "" {
		return job.ResourceVersion
	}
	sum := sha256.Sum256([]byte("job-pod\x00" + job.Namespace + "/" + job.Name))
	return fmt.Sprintf("%x", sum[:8])
}
