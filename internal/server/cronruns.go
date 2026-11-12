package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"q8s/internal/cronruns"
)

// Labels and annotations carried by objects derived from a CronJob run.
const (
	cronRunJobNameLabel         = "job-name"
	cronRunCronJobLabel         = "q8s.io/cronjob"
	cronRunInvocationAnnotation = "q8s.io/invocation-id"

	defaultSuccessfulJobsHistoryLimit = 3
	defaultFailedJobsHistoryLimit     = 1
)

// cronRunObject is one run together with the Job and Pod derived from it.
type cronRunObject struct {
	run     cronruns.Run
	jobName string
	job     *batchv1.Job
	pod     *corev1.Pod
}

// cronRunObjects derives the Jobs and Pods for every stored CronJob in ns
// (all namespaces when ns is empty). It reads the journal on every call and
// keeps no state: a request that lists pods or jobs pays one journalctl call
// per CronJob, and no request pays anything when Config.CronRuns is nil.
func (s *Server) cronRunObjects(ctx context.Context, ns string) ([]*batchv1.Job, []*corev1.Pod) {
	src := s.config.CronRuns
	if src == nil {
		return nil, nil
	}
	var cjs []*batchv1.CronJob
	if ns == "" {
		cjs = s.config.Store.AllCronJobs()
	} else {
		cjs = s.config.Store.CronJobs(ns)
	}
	var jobs []*batchv1.Job
	var pods []*corev1.Pod
	for _, cj := range cjs {
		runs, err := src.Runs(ctx, cj.Namespace, cj.Name)
		runs = runsSince(runs, cj)
		if err != nil || len(runs) == 0 {
			continue
		}
		for _, o := range s.cronRunObjectsFor(cj, runs) {
			jobs = append(jobs, o.job)
			pods = append(pods, o.pod)
		}
	}
	sortCronRunJobs(jobs)
	sortCronRunPods(pods)
	return jobs, pods
}

func sortCronRunJobs(jobs []*batchv1.Job) {
	sort.SliceStable(jobs, func(i, j int) bool {
		if jobs[i].Namespace != jobs[j].Namespace {
			return jobs[i].Namespace < jobs[j].Namespace
		}
		return jobs[i].Name < jobs[j].Name
	})
}

func sortCronRunPods(pods []*corev1.Pod) {
	sort.SliceStable(pods, func(i, j int) bool {
		if pods[i].Namespace != pods[j].Namespace {
			return pods[i].Namespace < pods[j].Namespace
		}
		return pods[i].Name < pods[j].Name
	})
}

// cronRunPodsExtra returns the synthetic-pod source for respondListMerged,
// or nil when runs are disabled. The caller passes it only to the non-watch
// path.
func (s *Server) cronRunPodsExtra(ctx context.Context, ns string) func() []*corev1.Pod {
	if s.config.CronRuns == nil {
		return nil
	}
	return func() []*corev1.Pod {
		_, pods := s.cronRunObjects(ctx, ns)
		return pods
	}
}

// cronRunJobsExtra returns the synthetic-job source for respondListMerged,
// or nil when runs are disabled.
func (s *Server) cronRunJobsExtra(ctx context.Context, ns string) func() []*batchv1.Job {
	if s.config.CronRuns == nil {
		return nil
	}
	return func() []*batchv1.Job {
		jobs, _ := s.cronRunObjects(ctx, ns)
		return jobs
	}
}

func (s *Server) cronRunFindPod(ctx context.Context, ns, name string) (*corev1.Pod, bool) {
	if s.config.CronRuns == nil {
		return nil, false
	}
	_, pods := s.cronRunObjects(ctx, ns)
	for _, p := range pods {
		if p.Name == name {
			return p, true
		}
	}
	return nil, false
}

func (s *Server) cronRunFindJob(ctx context.Context, ns, name string) (*batchv1.Job, bool) {
	if s.config.CronRuns == nil {
		return nil, false
	}
	jobs, _ := s.cronRunObjects(ctx, ns)
	for _, j := range jobs {
		if j.Name == name {
			return j, true
		}
	}
	return nil, false
}

// getPod returns a stored pod, falling back to a CronJob run with the same
// name. A stored object always wins.
func (s *Server) getPod(ctx context.Context, ns, name string) (*corev1.Pod, error) {
	pod, err := s.config.Store.GetPod(ns, name)
	if err == nil {
		return pod, nil
	}
	if run, ok := s.cronRunFindPod(ctx, ns, name); ok {
		return run, nil
	}
	return nil, err
}

// getJob returns a stored job, falling back to a CronJob run with the same
// name. A stored object always wins.
func (s *Server) getJob(ctx context.Context, ns, name string) (*batchv1.Job, error) {
	job, err := s.config.Store.GetJob(ns, name)
	if err == nil {
		return job, nil
	}
	if run, ok := s.cronRunFindJob(ctx, ns, name); ok {
		return run, nil
	}
	return nil, err
}

// rejectCronRunPodWrite answers 405 for writes to a pod that only exists as
// a CronJob run. A stored pod with the same name keeps normal behaviour.
func (s *Server) rejectCronRunPodWrite(w http.ResponseWriter, r *http.Request, ns, name string) bool {
	if s.config.CronRuns == nil || name == "" {
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
	if _, ok := s.cronRunFindPod(r.Context(), ns, name); !ok {
		return false
	}
	s.respondStatus(w, http.StatusMethodNotAllowed, "MethodNotAllowed",
		"Pod %s is a CronJob run derived from the systemd journal: delete/update is not implemented", name)
	return true
}

// rejectCronRunJobWrite is rejectCronRunPodWrite for jobs.
func (s *Server) rejectCronRunJobWrite(w http.ResponseWriter, r *http.Request, ns, name string) bool {
	if s.config.CronRuns == nil || name == "" {
		return false
	}
	switch r.Method {
	case http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return false
	}
	if _, err := s.config.Store.GetJob(ns, name); err == nil {
		return false
	}
	if _, ok := s.cronRunFindJob(r.Context(), ns, name); !ok {
		return false
	}
	s.respondStatus(w, http.StatusMethodNotAllowed, "MethodNotAllowed",
		"Job %s is a CronJob run derived from the systemd journal: delete/update is not implemented", name)
	return true
}

// cronRunObjectsFor applies the history limits to runs and builds the Job
// and Pod for each retained run. runs must be newest first.
func (s *Server) cronRunObjectsFor(cj *batchv1.CronJob, runs []cronruns.Run) []cronRunObject {
	selected := selectCronRuns(runs,
		historyLimit(cj.Spec.SuccessfulJobsHistoryLimit, defaultSuccessfulJobsHistoryLimit),
		historyLimit(cj.Spec.FailedJobsHistoryLimit, defaultFailedJobsHistoryLimit))
	names := cronRunNames(cj.Name, selected)
	out := make([]cronRunObject, 0, len(selected))
	for i, run := range selected {
		out = append(out, s.cronRunObjectFor(cj, run, names[i]))
	}
	return out
}

func historyLimit(p *int32, def int) int {
	if p == nil {
		return def
	}
	if *p < 0 {
		return 0
	}
	return int(*p)
}

// selectCronRuns keeps every running run, the newest successfulJobsHistoryLimit
// succeeded runs and the newest failedJobsHistoryLimit failed runs. runs is
// newest first.
func selectCronRuns(runs []cronruns.Run, successLimit, failedLimit int) []cronruns.Run {
	out := make([]cronruns.Run, 0, len(runs))
	succeeded, failed := 0, 0
	for _, r := range runs {
		switch r.State {
		case cronruns.StateSucceeded:
			if succeeded >= successLimit {
				continue
			}
			succeeded++
		case cronruns.StateFailed:
			if failed >= failedLimit {
				continue
			}
			failed++
		}
		out = append(out, r)
	}
	return out
}

// cronRunNames assigns <cronjob>-<unix minutes> to each run. When several
// retained runs fall in the same minute, the older ones get -2, -3, ...
func cronRunNames(cronJob string, runs []cronruns.Run) []string {
	names := make([]string, len(runs))
	seen := make(map[int64]int)
	for i, r := range runs {
		minute := r.Start.Unix() / 60
		seen[minute]++
		name := fmt.Sprintf("%s-%d", cronJob, minute)
		if n := seen[minute]; n > 1 {
			name = fmt.Sprintf("%s-%d", name, n)
		}
		names[i] = name
	}
	return names
}

func (s *Server) cronRunObjectFor(cj *batchv1.CronJob, run cronruns.Run, jobName string) cronRunObject {
	jobUID := cronRunJobUID(run.InvocationID)
	podUID := cronRunPodUID(run.InvocationID)
	jobRV := cronRunResourceVersion("job", run)
	podRV := cronRunResourceVersion("pod", run)
	start := metav1.NewTime(run.Start)
	end := metav1.NewTime(run.End)
	if run.State != cronruns.StateRunning && run.End.IsZero() {
		end = start
	}

	labels := make(map[string]string, len(cj.Spec.JobTemplate.Labels)+2)
	for k, v := range cj.Spec.JobTemplate.Labels {
		labels[k] = v
	}
	labels[cronRunJobNameLabel] = jobName
	labels[cronRunCronJobLabel] = cj.Name

	jobSpec := cj.Spec.JobTemplate.Spec.DeepCopy()
	jobSpec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{cronRunJobNameLabel: jobName}}

	job := &batchv1.Job{
		TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{
			Name:              jobName,
			Namespace:         cj.Namespace,
			UID:               jobUID,
			ResourceVersion:   jobRV,
			CreationTimestamp: start,
			Labels:            labels,
			Annotations: map[string]string{
				cronRunInvocationAnnotation: run.InvocationID,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1",
				Kind:       "CronJob",
				Name:       cj.Name,
				UID:        cj.UID,
				Controller: boolPtr(true),
			}},
		},
		Spec: *jobSpec,
	}
	job.Status.StartTime = &start
	switch run.State {
	case cronruns.StateSucceeded:
		job.Status.Succeeded = 1
		job.Status.CompletionTime = &end
		job.Status.Conditions = []batchv1.JobCondition{{
			Type:               batchv1.JobComplete,
			Status:             corev1.ConditionTrue,
			Reason:             "Completed",
			LastProbeTime:      end,
			LastTransitionTime: end,
		}}
	case cronruns.StateFailed:
		job.Status.Failed = 1
		job.Status.CompletionTime = &end
		job.Status.Conditions = []batchv1.JobCondition{{
			Type:               batchv1.JobFailed,
			Status:             corev1.ConditionTrue,
			Reason:             "Failed",
			LastProbeTime:      end,
			LastTransitionTime: end,
		}}
	default:
		job.Status.Active = 1
	}

	podSpec := cj.Spec.JobTemplate.Spec.Template.Spec.DeepCopy()
	containerName := ""
	containerImage := ""
	if len(podSpec.Containers) > 0 {
		containerName = podSpec.Containers[0].Name
		containerImage = podSpec.Containers[0].Image
	}
	pod := &corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{
			Name:              jobName + "-run",
			Namespace:         cj.Namespace,
			UID:               podUID,
			ResourceVersion:   podRV,
			CreationTimestamp: start,
			Labels: map[string]string{
				cronRunJobNameLabel: jobName,
				cronRunCronJobLabel: cj.Name,
			},
			Annotations: map[string]string{
				cronRunInvocationAnnotation: run.InvocationID,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1",
				Kind:       "Job",
				Name:       jobName,
				UID:        jobUID,
				Controller: boolPtr(true),
			}},
		},
		Spec: *podSpec,
	}
	pod.Status.StartTime = &start
	containerStatus := corev1.ContainerStatus{
		Name:  containerName,
		Image: containerImage,
	}
	switch run.State {
	case cronruns.StateSucceeded:
		pod.Status.Phase = corev1.PodSucceeded
		containerStatus.State.Terminated = &corev1.ContainerStateTerminated{
			ExitCode:   finishedExitCode(run, 0),
			Reason:     "Completed",
			StartedAt:  start,
			FinishedAt: end,
		}
	case cronruns.StateFailed:
		pod.Status.Phase = corev1.PodFailed
		containerStatus.State.Terminated = &corev1.ContainerStateTerminated{
			ExitCode:   finishedExitCode(run, 1),
			Reason:     "Error",
			StartedAt:  start,
			FinishedAt: end,
		}
	default:
		pod.Status.Phase = corev1.PodRunning
		containerStatus.State.Running = &corev1.ContainerStateRunning{StartedAt: start}
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{containerStatus}

	return cronRunObject{run: run, jobName: jobName, job: job, pod: pod}
}

func finishedExitCode(run cronruns.Run, fallback int32) int32 {
	if run.ExitStatus < 0 {
		return fallback
	}
	return int32(run.ExitStatus)
}

// fillCronJobRunStatus fills status.lastScheduleTime, lastSuccessfulTime and
// active on a copy returned to the client. Nothing is written to the store.
func (s *Server) fillCronJobRunStatus(ctx context.Context, cj *batchv1.CronJob) {
	src := s.config.CronRuns
	if src == nil || cj == nil {
		return
	}
	runs, err := src.Runs(ctx, cj.Namespace, cj.Name)
	runs = runsSince(runs, cj)
	if err != nil || len(runs) == 0 {
		return
	}
	cj.Status.LastScheduleTime = &metav1.Time{Time: runs[0].Start}
	for _, r := range runs {
		if r.State != cronruns.StateSucceeded {
			continue
		}
		end := r.End
		if end.IsZero() {
			end = r.Start
		}
		cj.Status.LastSuccessfulTime = &metav1.Time{Time: end}
		break
	}
	var active []corev1.ObjectReference
	for _, o := range s.cronRunObjectsFor(cj, runs) {
		if o.run.State != cronruns.StateRunning {
			continue
		}
		active = append(active, corev1.ObjectReference{
			APIVersion:      "batch/v1",
			Kind:            "Job",
			Namespace:       cj.Namespace,
			Name:            o.jobName,
			UID:             o.job.UID,
			ResourceVersion: o.job.ResourceVersion,
		})
	}
	cj.Status.Active = active
}

// respondListCronJobs fills run status on the non-watch list path only.
func (s *Server) respondListCronJobs(
	w http.ResponseWriter,
	r *http.Request,
	listFn func() []*batchv1.CronJob,
	toTable func([]*batchv1.CronJob, string) *table,
	makeList func([]batchv1.CronJob) *batchv1.CronJobList,
) {
	if s.config.CronRuns != nil && r.URL.Query().Get("watch") != "true" {
		inner := listFn
		ctx := r.Context()
		listFn = func() []*batchv1.CronJob {
			cjs := inner()
			for _, cj := range cjs {
				s.fillCronJobRunStatus(ctx, cj)
			}
			return cjs
		}
	}
	respondList(w, r, s, listFn, toTable, makeList)
}

// cronRunJobUID formats the invocation id as a UUID when it is one; a
// journal invocation id always is 32 hex digits, but a fake or a future
// format still gets a stable id by hashing.
func cronRunJobUID(invocationID string) types.UID {
	clean := strings.ReplaceAll(invocationID, "-", "")
	if raw, err := hex.DecodeString(clean); err == nil && len(raw) == 16 {
		return types.UID(formatUUID(raw))
	}
	sum := sha256.Sum256([]byte("job\x00" + invocationID))
	return types.UID(formatUUID(sum[:16]))
}

// cronRunPodUID derives a pod UID from the invocation id, distinct from the
// job UID derived from the same run.
func cronRunPodUID(invocationID string) types.UID {
	sum := sha256.Sum256([]byte("pod\x00" + invocationID))
	return types.UID(formatUUID(sum[:16]))
}

func formatUUID(b []byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// cronRunResourceVersion is a decimal hash over the fields that change when
// a run moves from running to finished, so watch diffs notice the change.
func cronRunResourceVersion(kind string, run cronruns.Run) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + run.InvocationID + "\x00" + run.State + "\x00" + strconv.Itoa(run.ExitStatus)))
	return strconv.FormatUint(binary.BigEndian.Uint64(sum[:8]), 10)
}

// handleCronRunPodLogs serves the log subresource of a synthetic pod from the
// journal. The pod's template container name is only one container; its
// journald output is selected by CONTAINER_NAME and the run's invocation id.
func (s *Server) handleCronRunPodLogs(w http.ResponseWriter, r *http.Request, pod *corev1.Pod) {
	src := s.config.CronRuns
	if src == nil {
		s.respondStatus(w, http.StatusNotFound, "NotFound", "pod %q not found", pod.Name)
		return
	}
	q := r.URL.Query()
	invocationID := pod.Annotations[cronRunInvocationAnnotation]
	cronJob := pod.Labels[cronRunCronJobLabel]

	opt := cronruns.LogOptions{Follow: q.Get("follow") == "true" && pod.Status.Phase == corev1.PodRunning}
	if tail := q.Get("tailLines"); tail != "" {
		if n, err := strconv.ParseInt(tail, 10, 64); err == nil {
			opt.TailLines = &n
		}
	}
	now := time.Now()
	if src.Now != nil {
		now = src.Now()
	}
	if since := q.Get("sinceSeconds"); since != "" {
		if n, err := strconv.ParseInt(since, 10, 64); err == nil {
			opt.Since = now.Add(-time.Duration(n) * time.Second)
		}
	}
	if since := q.Get("sinceTime"); since != "" {
		if t, err := time.Parse(time.RFC3339, since); err == nil {
			opt.Since = t
		}
	}
	args := src.LogArgs(pod.Namespace, cronJob, invocationID, opt)
	timestamps := q.Get("timestamps") == "true"

	if opt.Follow && src.Stream != nil {
		rc, wait, err := src.Stream(r.Context(), "journalctl", args...)
		if err != nil {
			s.respondStatus(w, http.StatusInternalServerError, "InternalError", "journalctl: %v", err)
			return
		}
		defer rc.Close()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		flusher, _ := w.(http.Flusher)
		br := bufio.NewReader(rc)
		for {
			line, readErr := br.ReadBytes('\n')
			if len(line) > 0 {
				writeJournalLogLine(w, line, timestamps)
				if flusher != nil {
					flusher.Flush()
				}
			}
			if readErr != nil {
				break
			}
		}
		if wait != nil {
			_ = wait()
		}
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if src.Run == nil {
		s.respondStatus(w, http.StatusInternalServerError, "InternalError", "journalctl: no command runner configured")
		return
	}
	out, err := src.Run(ctx, "journalctl", args...)
	if err != nil && len(out) == 0 {
		s.respondStatus(w, http.StatusInternalServerError, "InternalError", "journalctl: %v", err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	for _, line := range bytes.Split(out, []byte("\n")) {
		writeJournalLogLine(w, line, timestamps)
	}
}

// writeJournalLogLine prints one journal JSON entry as a log line, optionally
// prefixed with its RFC3339Nano timestamp like kubectl --timestamps.
func writeJournalLogLine(w io.Writer, line []byte, timestamps bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return
	}
	var entry map[string]json.RawMessage
	if err := json.Unmarshal(line, &entry); err != nil {
		return
	}
	message := cronruns.JournalMessage(entry)
	if timestamps {
		if t, ok := cronruns.JournalTime(entry); ok {
			message = t.Format(time.RFC3339Nano) + " " + message
		}
	}
	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	io.WriteString(w, message)
}

// runsSince drops runs that started before the CronJob was created. The
// journal outlives objects: a CronJob deleted and created again under the
// same name (or in a namespace with the same name) has the same unit name,
// and its predecessor's runs must not show up as its own.
// creationTimestamp has second precision, so compare at that precision.
func runsSince(runs []cronruns.Run, cj *batchv1.CronJob) []cronruns.Run {
	created := cj.CreationTimestamp.Time
	if created.IsZero() {
		return runs
	}
	kept := runs[:0:0]
	for _, r := range runs {
		if !r.Start.Truncate(time.Second).Before(created.Truncate(time.Second)) {
			kept = append(kept, r)
		}
	}
	return kept
}
