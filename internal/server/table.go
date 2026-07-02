package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// isTableRequest returns true when kubectl (or any client) wants a Table response.
func isTableRequest(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "as=Table")
}

// table is a minimal meta.k8s.io/v1 Table that kubectl understands.
type table struct {
	Kind              string            `json:"kind"`
	APIVersion        string            `json:"apiVersion"`
	Metadata          map[string]string `json:"metadata"`
	ColumnDefinitions []columnDef       `json:"columnDefinitions"`
	Rows              []tableRow        `json:"rows"`
}

type columnDef struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Format      string `json:"format,omitempty"`
	Description string `json:"description"`
	Priority    int    `json:"priority"`
}

type tableRow struct {
	Cells  []interface{} `json:"cells"`
	Object partialMeta   `json:"object"`
}

type partialMeta struct {
	Kind       string            `json:"kind"`
	APIVersion string            `json:"apiVersion"`
	Metadata   metav1.ObjectMeta `json:"metadata"`
}

func encodeTable(w http.ResponseWriter, t *table) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(t)
}

func newTable(rv string, cols []columnDef) *table {
	return &table{
		Kind:              "Table",
		APIVersion:        "meta.k8s.io/v1",
		Metadata:          map[string]string{"resourceVersion": rv},
		ColumnDefinitions: cols,
	}
}

// --- Pod table ---

var podColumns = []columnDef{
	{Name: "Name", Type: "string", Format: "name", Description: "Pod name", Priority: 0},
	{Name: "Ready", Type: "string", Description: "Containers ready", Priority: 0},
	{Name: "Status", Type: "string", Description: "Pod phase", Priority: 0},
	{Name: "Restarts", Type: "integer", Description: "Restart count", Priority: 0},
	{Name: "Age", Type: "string", Description: "Time since creation", Priority: 0},
}

func podsToTable(pods []*corev1.Pod, rv string) *table {
	t := newTable(rv, podColumns)
	for _, p := range pods {
		ready, total := 0, len(p.Status.ContainerStatuses)
		restarts := 0
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Ready {
				ready++
			}
			restarts += int(cs.RestartCount)
		}
		if total == 0 {
			total = len(p.Spec.Containers)
		}
		t.Rows = append(t.Rows, tableRow{
			Cells: []interface{}{
				p.Name,
				fmt.Sprintf("%d/%d", ready, total),
				podStatus(p),
				restarts,
				age(p.CreationTimestamp.Time),
			},
			Object: partialMeta{Kind: "Pod", APIVersion: "v1", Metadata: p.ObjectMeta},
		})
	}
	return t
}

func podStatus(p *corev1.Pod) string {
	if p.DeletionTimestamp != nil {
		return "Terminating"
	}
	if p.Status.Phase != "" {
		return string(p.Status.Phase)
	}
	return "Pending"
}

// --- Service table ---

var svcColumns = []columnDef{
	{Name: "Name", Type: "string", Format: "name", Priority: 0},
	{Name: "Type", Type: "string", Priority: 0},
	{Name: "Cluster-IP", Type: "string", Priority: 0},
	{Name: "Port(s)", Type: "string", Priority: 0},
	{Name: "Age", Type: "string", Priority: 0},
}

func svcsToTable(svcs []*corev1.Service, rv string) *table {
	t := newTable(rv, svcColumns)
	for _, s := range svcs {
		ports := make([]string, len(s.Spec.Ports))
		for j, p := range s.Spec.Ports {
			ports[j] = fmt.Sprintf("%d/%s", p.Port, p.Protocol)
		}
		t.Rows = append(t.Rows, tableRow{
			Cells: []interface{}{
				s.Name,
				string(s.Spec.Type),
				s.Spec.ClusterIP,
				strings.Join(ports, ","),
				age(s.CreationTimestamp.Time),
			},
			Object: partialMeta{Kind: "Service", APIVersion: "v1", Metadata: s.ObjectMeta},
		})
	}
	return t
}

// --- Namespace table ---

var nsColumns = []columnDef{
	{Name: "Name", Type: "string", Format: "name", Priority: 0},
	{Name: "Status", Type: "string", Priority: 0},
	{Name: "Age", Type: "string", Priority: 0},
}

func namespacesToTable(nss []*corev1.Namespace, rv string) *table {
	t := newTable(rv, nsColumns)
	for _, ns := range nss {
		status := "Active"
		if ns.Status.Phase != "" {
			status = string(ns.Status.Phase)
		}
		t.Rows = append(t.Rows, tableRow{
			Cells:  []interface{}{ns.Name, status, age(ns.CreationTimestamp.Time)},
			Object: partialMeta{Kind: "Namespace", APIVersion: "v1", Metadata: ns.ObjectMeta},
		})
	}
	return t
}

// --- PVC table ---

var pvcColumns = []columnDef{
	{Name: "Name", Type: "string", Format: "name", Priority: 0},
	{Name: "Status", Type: "string", Priority: 0},
	{Name: "Volume", Type: "string", Priority: 0},
	{Name: "Capacity", Type: "string", Priority: 0},
	{Name: "Access Modes", Type: "string", Priority: 0},
	{Name: "Age", Type: "string", Priority: 0},
}

func pvcsToTable(pvcs []*corev1.PersistentVolumeClaim, rv string) *table {
	t := newTable(rv, pvcColumns)
	for _, pvc := range pvcs {
		capacity := ""
		if q, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
			capacity = q.String()
		}
		modes := make([]string, len(pvc.Spec.AccessModes))
		for i, m := range pvc.Spec.AccessModes {
			modes[i] = string(m)
		}
		t.Rows = append(t.Rows, tableRow{
			Cells: []interface{}{
				pvc.Name,
				string(pvc.Status.Phase),
				pvc.Spec.VolumeName,
				capacity,
				strings.Join(modes, ","),
				age(pvc.CreationTimestamp.Time),
			},
			Object: partialMeta{Kind: "PersistentVolumeClaim", APIVersion: "v1", Metadata: pvc.ObjectMeta},
		})
	}
	return t
}

// --- ConfigMap table ---

var cmColumns = []columnDef{
	{Name: "Name", Type: "string", Format: "name", Priority: 0},
	{Name: "Data", Type: "integer", Priority: 0},
	{Name: "Age", Type: "string", Priority: 0},
}

func configMapsToTable(cms []*corev1.ConfigMap, rv string) *table {
	t := newTable(rv, cmColumns)
	for _, cm := range cms {
		t.Rows = append(t.Rows, tableRow{
			Cells:  []interface{}{cm.Name, len(cm.Data), age(cm.CreationTimestamp.Time)},
			Object: partialMeta{Kind: "ConfigMap", APIVersion: "v1", Metadata: cm.ObjectMeta},
		})
	}
	return t
}

// --- Secret table ---

var secretColumns = []columnDef{
	{Name: "Name", Type: "string", Format: "name", Priority: 0},
	{Name: "Type", Type: "string", Priority: 0},
	{Name: "Data", Type: "integer", Priority: 0},
	{Name: "Age", Type: "string", Priority: 0},
}

func secretsToTable(secrets []*corev1.Secret, rv string) *table {
	t := newTable(rv, secretColumns)
	for _, s := range secrets {
		t.Rows = append(t.Rows, tableRow{
			Cells:  []interface{}{s.Name, string(s.Type), len(s.Data), age(s.CreationTimestamp.Time)},
			Object: partialMeta{Kind: "Secret", APIVersion: "v1", Metadata: s.ObjectMeta},
		})
	}
	return t
}

// --- Deployment table ---

var deploymentColumns = []columnDef{
	{Name: "Name", Type: "string", Format: "name", Priority: 0},
	{Name: "Ready", Type: "string", Priority: 0},
	{Name: "Up-to-date", Type: "integer", Priority: 0},
	{Name: "Available", Type: "integer", Priority: 0},
	{Name: "Age", Type: "string", Priority: 0},
}

func deploymentsToTable(deps []*appsv1.Deployment, rv string) *table {
	t := newTable(rv, deploymentColumns)
	for _, dep := range deps {
		desired := int32(1)
		if dep.Spec.Replicas != nil {
			desired = *dep.Spec.Replicas
		}
		t.Rows = append(t.Rows, tableRow{
			Cells: []interface{}{
				dep.Name,
				fmt.Sprintf("%d/%d", dep.Status.ReadyReplicas, desired),
				dep.Status.UpdatedReplicas,
				dep.Status.AvailableReplicas,
				age(dep.CreationTimestamp.Time),
			},
			Object: partialMeta{Kind: "Deployment", APIVersion: "apps/v1", Metadata: dep.ObjectMeta},
		})
	}
	return t
}

// --- Job table ---

var jobColumns = []columnDef{
	{Name: "Name", Type: "string", Format: "name", Priority: 0},
	{Name: "Completions", Type: "string", Priority: 0},
	{Name: "Duration", Type: "string", Priority: 0},
	{Name: "Age", Type: "string", Priority: 0},
}

func jobsToTable(jobs []*batchv1.Job, rv string) *table {
	t := newTable(rv, jobColumns)
	for _, j := range jobs {
		completions := fmt.Sprintf("%d/1", j.Status.Succeeded)
		dur := ""
		if j.Status.StartTime != nil {
			end := time.Now()
			if j.Status.CompletionTime != nil {
				end = j.Status.CompletionTime.Time
			}
			dur = age(end.Add(-end.Sub(j.Status.StartTime.Time)))
		}
		t.Rows = append(t.Rows, tableRow{
			Cells:  []interface{}{j.Name, completions, dur, age(j.CreationTimestamp.Time)},
			Object: partialMeta{Kind: "Job", APIVersion: "batch/v1", Metadata: j.ObjectMeta},
		})
	}
	return t
}

// --- CronJob table ---

var cronJobColumns = []columnDef{
	{Name: "Name", Type: "string", Format: "name", Priority: 0},
	{Name: "Schedule", Type: "string", Priority: 0},
	{Name: "Suspend", Type: "boolean", Priority: 0},
	{Name: "Active", Type: "integer", Priority: 0},
	{Name: "Last Schedule", Type: "string", Priority: 0},
	{Name: "Age", Type: "string", Priority: 0},
}

func cronJobsToTable(cjs []*batchv1.CronJob, rv string) *table {
	t := newTable(rv, cronJobColumns)
	for _, cj := range cjs {
		suspend := cj.Spec.Suspend != nil && *cj.Spec.Suspend
		active := len(cj.Status.Active)
		lastSchedule := "<none>"
		if cj.Status.LastScheduleTime != nil {
			lastSchedule = age(cj.Status.LastScheduleTime.Time)
		}
		t.Rows = append(t.Rows, tableRow{
			Cells:  []interface{}{cj.Name, cj.Spec.Schedule, suspend, active, lastSchedule, age(cj.CreationTimestamp.Time)},
			Object: partialMeta{Kind: "CronJob", APIVersion: "batch/v1", Metadata: cj.ObjectMeta},
		})
	}
	return t
}

// --- Helpers ---

func age(t time.Time) string {
	if t.IsZero() {
		return "<unknown>"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
