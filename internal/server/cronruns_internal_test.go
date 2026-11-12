package server

import (
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"q8s/internal/cronruns"
)

// A CronJob deleted and created again under the same name has the same unit,
// so the journal still holds its predecessor's runs: they must be hidden.
func TestCronRunsBeforeCreationHidden(t *testing.T) {
	base := time.Unix(1700000000, 0).UTC()
	cj := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.Time{Time: base.Add(30 * time.Second)}}}
	runs := []cronruns.Run{
		{InvocationID: "new", Start: base.Add(time.Minute)},
		{InvocationID: "same-second", Start: base.Add(30*time.Second + 400*time.Millisecond)},
		{InvocationID: "old", Start: base},
	}
	got := runsSince(runs, cj)
	if len(got) != 2 || got[0].InvocationID != "new" || got[1].InvocationID != "same-second" {
		t.Fatalf("runsSince = %+v", got)
	}
}

// The DURATION column is completion minus start, not the job's age.
func TestJobsTableDurationIsRunTime(t *testing.T) {
	start := time.Now().Add(-20 * time.Minute)
	end := start.Add(2 * time.Second)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "j", CreationTimestamp: metav1.Time{Time: start}},
		Status: batchv1.JobStatus{
			StartTime:      &metav1.Time{Time: start},
			CompletionTime: &metav1.Time{Time: end},
			Succeeded:      1,
		},
	}
	row := jobsToTable([]*batchv1.Job{job}, "1").Rows[0]
	if got := row.Cells[2]; got != "2s" {
		t.Fatalf("duration = %v, want 2s (age is %v)", got, row.Cells[3])
	}
}
