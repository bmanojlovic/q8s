package server_test

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"q8s/internal/cronruns"
)

// A Job unit name is <ns>-<name>-job.service, so the namespace team-prod with
// Job db and the namespace team with Job prod-db share one unit. After the
// first Job is deleted the journal still holds its run; the new Job must not
// see that run or serve its output as its own logs.
func TestJobRunsDoNotLeakAcrossNamespacesAfterRecreate(t *testing.T) {
	fake := newFakeJournal()
	base := time.Unix(1700000000, 0).UTC()
	installJobRun(fake, "team-prod", "db", "11111111111111111111111111111111", base, cronruns.StateSucceeded)
	fake.logs["team-prod_db__job"] = []byte(strings.Join([]string{
		`{"MESSAGE":"old tenant output","__REALTIME_TIMESTAMP":"1700000000000000"}`,
	}, "\n"))
	ts, _ := newCronRunServer(t, fake)

	oldJob := jobBody("team-prod", "db")
	oldJob["metadata"].(map[string]interface{})["creationTimestamp"] = oldCreationTimestamp
	resp := post(t, ts.URL+"/apis/batch/v1/namespaces/team-prod/jobs", oldJob)
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	// The Job that owns the run still shows it and serves its logs.
	resp = get(t, ts.URL+"/api/v1/namespaces/team-prod/pods/db-run/log")
	assertStatus(t, resp, http.StatusOK)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if got := string(body); !strings.Contains(got, "old tenant output") {
		t.Fatalf("own Job logs = %q, want old tenant output", got)
	}

	resp = do(t, http.MethodDelete, ts.URL+"/apis/batch/v1/namespaces/team-prod/jobs/db", nil)
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	// A different namespace and Job name that map to the same unit. Its
	// creation timestamp is after the old run, so it has no runs of its own.
	newJob := jobBody("team", "prod-db")
	newJob["metadata"].(map[string]interface{})["creationTimestamp"] = "2024-01-01T00:00:00Z"
	resp = post(t, ts.URL+"/apis/batch/v1/namespaces/team/jobs", newJob)
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	resp = get(t, ts.URL+"/api/v1/namespaces/team/pods")
	assertStatus(t, resp, http.StatusOK)
	items := listItems(t, decodeBody(t, resp))
	pod := findItem(items, "prod-db-run")
	if pod == nil {
		t.Fatalf("new Job has no derived pod: %v", items)
	}
	annotations, _ := nested(pod, "metadata")["annotations"].(map[string]interface{})
	if id, ok := annotations["q8s.io/invocation-id"]; ok {
		t.Fatalf("new Job inherited run %v from another namespace", id)
	}
	if got := nested(pod, "status")["phase"]; got != "Pending" {
		t.Fatalf("new Job pod phase = %v, want Pending (no run since creation)", got)
	}
	if _, ok := nested(pod, "status")["containerStatuses"]; ok {
		t.Fatalf("new Job pod shows the old run's container status: %v", nested(pod, "status"))
	}

	resp = get(t, ts.URL+"/api/v1/namespaces/team/pods/prod-db-run/log")
	if resp.StatusCode == http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.Contains(string(body), "old tenant output") {
			t.Fatalf("new Job served the old tenant's logs: %q", body)
		}
		t.Fatalf("new Job logs request status 200, want a refusal")
	}
	assertStatus(t, resp, http.StatusNotFound)
	resp.Body.Close()
}

// The old "return all runs" fallback also covered a run that the journal
// timestamps just before the API stores the Job. Keep that case working with a
// small window, not with a predecessor's whole history.
func TestJobRunJustBeforeCreationTimestampStillShown(t *testing.T) {
	fake := newFakeJournal()
	base := time.Unix(1700000000, 0).UTC()
	id := "33333333333333333333333333333333"
	installJobRun(fake, "default", "plain", id, base, cronruns.StateSucceeded)
	fake.logs["default_plain__job"] = []byte(`{"MESSAGE":"own output","__REALTIME_TIMESTAMP":"1700000000000000"}`)
	ts, _ := newCronRunServer(t, fake)

	body := jobBody("default", "plain")
	body["metadata"].(map[string]interface{})["creationTimestamp"] = "2023-11-14T22:13:21Z"
	resp := post(t, ts.URL+"/apis/batch/v1/namespaces/default/jobs", body)
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	resp = get(t, ts.URL+"/api/v1/namespaces/default/pods/plain-run")
	assertStatus(t, resp, http.StatusOK)
	pod := decodeBody(t, resp)
	annotations, _ := nested(pod, "metadata")["annotations"].(map[string]interface{})
	if annotations["q8s.io/invocation-id"] != id {
		t.Fatalf("pod invocation = %v, want %s", annotations["q8s.io/invocation-id"], id)
	}
	if got := nested(pod, "status")["phase"]; got != "Succeeded" {
		t.Fatalf("pod phase = %v, want Succeeded", got)
	}

	resp = get(t, ts.URL+"/api/v1/namespaces/default/pods/plain-run/log")
	assertStatus(t, resp, http.StatusOK)
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if got := string(out); !strings.Contains(got, "own output") {
		t.Fatalf("logs = %q, want own output", got)
	}
}

// The same unit-name collision exists for CronJob units. The second CronJob
// is created after the first run started, even though both fall in the same
// wall-clock second; the run must be hidden and its logs refused.
func TestCronRunsDoNotLeakAcrossNamespacesAfterRecreate(t *testing.T) {
	fake := newFakeJournal()
	base := time.Unix(1700000000, 0).UTC()
	start := base.Add(500 * time.Millisecond)
	id := "22222222222222222222222222222222"
	fake.runs["team-prod-db-cron.service"] = []byte(strings.Join([]string{
		journalJSON(id, cronruns.MessageIDStarted, start, ""),
		journalJSON(id, cronruns.MessageIDExited, start.Add(time.Second), `,"EXIT_STATUS":"0"`),
		journalJSON(id, cronruns.MessageIDSucceeded, start.Add(2*time.Second), ""),
	}, "\n"))
	fake.logs["team-prod-db-cron"] = []byte(strings.Join([]string{
		`{"MESSAGE":"old cron output","__REALTIME_TIMESTAMP":"1700000000500000"}`,
	}, "\n"))
	ts, _ := newCronRunServer(t, fake)

	runPod := fmt.Sprintf("db-%d-run", start.Unix()/60)
	oldCron := cronJobBody("team-prod", "db", "*/5 * * * *")
	oldCron["metadata"].(map[string]interface{})["creationTimestamp"] = oldCreationTimestamp
	resp := post(t, ts.URL+"/apis/batch/v1/namespaces/team-prod/cronjobs", oldCron)
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	resp = get(t, ts.URL+"/api/v1/namespaces/team-prod/pods/"+runPod+"/log")
	assertStatus(t, resp, http.StatusOK)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if got := string(body); !strings.Contains(got, "old cron output") {
		t.Fatalf("own CronJob logs = %q, want old cron output", got)
	}

	resp = do(t, http.MethodDelete, ts.URL+"/apis/batch/v1/namespaces/team-prod/cronjobs/db", nil)
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	newCron := cronJobBody("team", "prod-db", "*/5 * * * *")
	newCron["metadata"].(map[string]interface{})["creationTimestamp"] = "2023-11-14T22:13:20.9Z"
	resp = post(t, ts.URL+"/apis/batch/v1/namespaces/team/cronjobs", newCron)
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	resp = get(t, ts.URL+"/api/v1/namespaces/team/pods")
	assertStatus(t, resp, http.StatusOK)
	if items := listItems(t, decodeBody(t, resp)); len(items) != 0 {
		t.Fatalf("new CronJob listed runs from another namespace: %v", items)
	}

	resp = get(t, ts.URL+"/api/v1/namespaces/team/pods/"+runPod+"/log")
	if resp.StatusCode == http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.Contains(string(body), "old cron output") {
			t.Fatalf("new CronJob served the old tenant's logs: %q", body)
		}
		t.Fatalf("new CronJob logs request status 200, want a refusal")
	}
	assertStatus(t, resp, http.StatusNotFound)
	resp.Body.Close()
}
