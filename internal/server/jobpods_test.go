package server_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"q8s/internal/cronruns"
)

const oldCreationTimestamp = "2023-01-01T00:00:00Z"

// createPlainJob posts a Job whose creation timestamp predates the fixed fake
// journal timestamps used by these tests.
func createPlainJob(t *testing.T, ts *httptest.Server, name string, templateLabels map[string]interface{}) map[string]interface{} {
	t.Helper()
	body := jobBody("default", name)
	body["metadata"].(map[string]interface{})["creationTimestamp"] = oldCreationTimestamp
	if templateLabels != nil {
		tmpl := body["spec"].(map[string]interface{})["template"].(map[string]interface{})
		tmpl["metadata"] = map[string]interface{}{"labels": templateLabels}
	}
	resp := post(t, ts.URL+"/apis/batch/v1/namespaces/default/jobs", body)
	assertStatus(t, resp, http.StatusCreated)
	return decodeBody(t, resp)
}

func installJobRun(fake *fakeJournal, namespace, job, id string, start time.Time, state string) {
	lines := []string{journalJSON(id, cronruns.MessageIDStarted, start, "")}
	switch state {
	case cronruns.StateSucceeded:
		lines = append(lines,
			journalJSON(id, cronruns.MessageIDExited, start.Add(time.Second), `,"EXIT_STATUS":"0"`),
			journalJSON(id, cronruns.MessageIDSucceeded, start.Add(2*time.Second), ""))
	case cronruns.StateFailed:
		lines = append(lines,
			journalJSON(id, cronruns.MessageIDExited, start.Add(time.Second), `,"EXIT_STATUS":"7"`),
			journalJSON(id, cronruns.MessageIDFailed, start.Add(2*time.Second), ""))
	}
	fake.runs[fmt.Sprintf("%s-%s-job.service", namespace, job)] = []byte(strings.Join(lines, "\n"))
}

func lastJournalArgs(f *fakeJournal) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.args) == 0 {
		return nil
	}
	return f.args[len(f.args)-1]
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func TestJobPodListAndSelector(t *testing.T) {
	fake := newFakeJournal()
	base := time.Unix(1700000000, 0).UTC()
	installJobRun(fake, "default", "plain", "dddddddddddddddddddddddddddddddd", base, cronruns.StateSucceeded)
	ts, _ := newCronRunServer(t, fake)

	created := createPlainJob(t, ts, "plain", map[string]interface{}{"app": "demo-app"})
	selector := nested(created, "spec", "selector", "matchLabels")
	if selector["job-name"] != "plain" {
		t.Fatalf("job selector = %v, want job-name=plain", selector)
	}
	templateLabels := nested(created, "spec", "template", "metadata", "labels")
	if templateLabels["job-name"] != "plain" || templateLabels["batch.kubernetes.io/job-name"] != "plain" {
		t.Fatalf("job template labels = %v", templateLabels)
	}
	if templateLabels["app"] != "demo-app" {
		t.Fatalf("job template lost user label: %v", templateLabels)
	}

	resp := get(t, ts.URL+"/api/v1/namespaces/default/pods?labelSelector=job-name%3Dplain")
	assertStatus(t, resp, http.StatusOK)
	items := listItems(t, decodeBody(t, resp))
	if len(items) != 1 {
		t.Fatalf("labelSelector job-name=plain returned %d pods: %v", len(items), items)
	}
	pod := items[0]
	if got := nested(pod, "metadata")["name"]; got != "plain-run" {
		t.Fatalf("pod name = %v, want plain-run", got)
	}
	labels := nested(pod, "metadata")["labels"].(map[string]interface{})
	if labels["job-name"] != "plain" || labels["app"] != "demo-app" {
		t.Fatalf("pod labels = %v", labels)
	}
	owners, _ := nested(pod, "metadata")["ownerReferences"].([]interface{})
	if len(owners) != 1 {
		t.Fatalf("pod ownerReferences = %v", owners)
	}
	owner := owners[0].(map[string]interface{})
	if owner["kind"] != "Job" || owner["name"] != "plain" || owner["controller"] != true {
		t.Fatalf("pod owner = %v", owner)
	}
	if got := nested(pod, "status")["phase"]; got != "Succeeded" {
		t.Fatalf("pod phase = %v, want Succeeded", got)
	}
	statuses, _ := nested(pod, "status")["containerStatuses"].([]interface{})
	if len(statuses) != 1 {
		t.Fatalf("containerStatuses = %v", statuses)
	}
	terminated := nested(statuses[0].(map[string]interface{}), "state", "terminated")
	if terminated["exitCode"] != float64(0) || terminated["reason"] != "Completed" {
		t.Fatalf("terminated state = %v", terminated)
	}

	resp = get(t, ts.URL+"/api/v1/namespaces/default/pods/plain-run")
	assertStatus(t, resp, http.StatusOK)
	if got := nested(decodeBody(t, resp), "status")["phase"]; got != "Succeeded" {
		t.Fatalf("single pod phase = %v", got)
	}

	resp = get(t, ts.URL+"/api/v1/pods")
	assertStatus(t, resp, http.StatusOK)
	if got := len(listItems(t, decodeBody(t, resp))); got != 1 {
		t.Fatalf("all-namespaces pods = %d, want 1", got)
	}
}

func TestJobSelectorAndLabelsDefaultedAndKept(t *testing.T) {
	ts, _ := newTestServer(t)
	url := ts.URL + "/apis/batch/v1/namespaces/default/jobs/plain"

	body := jobBody("default", "plain")
	resp := post(t, ts.URL+"/apis/batch/v1/namespaces/default/jobs", body)
	assertStatus(t, resp, http.StatusCreated)
	created := decodeBody(t, resp)
	if got := nested(created, "spec", "selector", "matchLabels")["job-name"]; got != "plain" {
		t.Fatalf("created selector = %v", got)
	}
	if got := nested(created, "spec", "template", "metadata", "labels")["job-name"]; got != "plain" {
		t.Fatalf("created template labels = %v", nested(created, "spec", "template", "metadata", "labels"))
	}

	// A PUT built from the user's original body has no selector or labels.
	resp = do(t, http.MethodPut, url, jobBody("default", "plain"))
	assertStatus(t, resp, http.StatusOK)
	updated := decodeBody(t, resp)
	if got := nested(updated, "spec", "selector", "matchLabels")["job-name"]; got != "plain" {
		t.Fatalf("PUT removed selector: %v", got)
	}
	if got := nested(updated, "spec", "template", "metadata", "labels")["job-name"]; got != "plain" {
		t.Fatalf("PUT removed template job-name: %v", nested(updated, "spec", "template", "metadata", "labels"))
	}

	// A merge patch that removes the selector and label must not leave a Job
	// whose selector matches every pod in the namespace.
	resp = do(t, http.MethodPatch, url, map[string]interface{}{
		"spec": map[string]interface{}{
			"selector": nil,
			"template": map[string]interface{}{
				"metadata": map[string]interface{}{
					"labels": map[string]interface{}{"job-name": nil},
				},
			},
		},
	})
	assertStatus(t, resp, http.StatusOK)
	patched := decodeBody(t, resp)
	if got := nested(patched, "spec", "selector", "matchLabels")["job-name"]; got != "plain" {
		t.Fatalf("PATCH removed selector: %v", got)
	}
	if got := nested(patched, "spec", "template", "metadata", "labels")["job-name"]; got != "plain" {
		t.Fatalf("PATCH removed template job-name: %v", nested(patched, "spec", "template", "metadata", "labels"))
	}

	// Without a journal source the derived pod still exists and is Pending.
	resp = get(t, ts.URL+"/api/v1/namespaces/default/pods?labelSelector=job-name%3Dplain")
	assertStatus(t, resp, http.StatusOK)
	items := listItems(t, decodeBody(t, resp))
	if len(items) != 1 || nested(items[0], "status")["phase"] != "Pending" {
		t.Fatalf("pending derived pod = %v", items)
	}
}

func TestJobPodLogsUseJobUnit(t *testing.T) {
	fake := newFakeJournal()
	base := time.Unix(1700000000, 0).UTC()
	installJobRun(fake, "default", "plain", "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", base, cronruns.StateSucceeded)
	fake.logs["default-plain-job"] = []byte(strings.Join([]string{
		`{"MESSAGE":"job output","__REALTIME_TIMESTAMP":"1700000000000000"}`,
		`{"MESSAGE":"second line","__REALTIME_TIMESTAMP":"1700000001000000"}`,
	}, "\n"))
	fake.logs["default-other-job"] = []byte(`{"MESSAGE":"wrong unit"}`)
	ts, _ := newCronRunServer(t, fake)

	createPlainJob(t, ts, "plain", nil)
	createPlainJob(t, ts, "other", nil)

	resp := get(t, ts.URL+"/api/v1/namespaces/default/pods/plain-run/log")
	assertStatus(t, resp, http.StatusOK)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if got, want := string(body), "job output\nsecond line\n"; got != want {
		t.Fatalf("logs = %q, want %q", got, want)
	}

	args := lastJournalArgs(fake)
	if !containsArg(args, "default-plain-job.service") {
		t.Fatalf("journal args selected wrong unit: %v", args)
	}
	if !containsArg(args, "CONTAINER_NAME=default-plain-job") {
		t.Fatalf("journal args selected wrong container: %v", args)
	}
	if !containsArg(args, "_SYSTEMD_INVOCATION_ID=eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee") {
		t.Fatalf("journal args did not select the run: %v", args)
	}
	if strings.Contains(string(body), "wrong unit") {
		t.Fatalf("logs came from another unit: %q", body)
	}
}

func TestJobPodWritesRejectedAndDeleteRemovesPod(t *testing.T) {
	fake := newFakeJournal()
	base := time.Unix(1700000000, 0).UTC()
	installJobRun(fake, "default", "plain", "ffffffffffffffffffffffffffffffff", base, cronruns.StateSucceeded)
	ts, _ := newCronRunServer(t, fake)
	createPlainJob(t, ts, "plain", nil)

	podURL := ts.URL + "/api/v1/namespaces/default/pods/plain-run"
	for _, tc := range []struct {
		method string
		body   interface{}
	}{
		{http.MethodDelete, nil},
		{http.MethodPut, podBody("default", "plain-run", "busybox")},
		{http.MethodPatch, map[string]interface{}{"metadata": map[string]interface{}{"labels": map[string]interface{}{"x": "y"}}}},
	} {
		resp := do(t, tc.method, podURL, tc.body)
		if resp.StatusCode != http.StatusMethodNotAllowed {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("%s %s: status %d, want 405: %s", tc.method, podURL, resp.StatusCode, body)
		}
		m := decodeBody(t, resp)
		msg, _ := m["message"].(string)
		if m["reason"] != "MethodNotAllowed" || !strings.Contains(msg, "is a Job run derived from the systemd journal") {
			t.Fatalf("%s %s: response = %v", tc.method, podURL, m)
		}
	}

	resp := do(t, http.MethodDelete, ts.URL+"/apis/batch/v1/namespaces/default/jobs/plain", nil)
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp = get(t, podURL)
	assertStatus(t, resp, http.StatusNotFound)
	resp.Body.Close()

	resp = get(t, ts.URL+"/api/v1/namespaces/default/pods")
	assertStatus(t, resp, http.StatusOK)
	if items := listItems(t, decodeBody(t, resp)); len(items) != 0 {
		t.Fatalf("deleted Job left pods behind: %v", items)
	}
}

func TestJobPodsAndCronRunPodsShareList(t *testing.T) {
	fake := newFakeJournal()
	f := newCronFixture()
	f.install(fake)
	installJobRun(fake, "default", "plain", "abababababababababababababababab", f.base.Add(time.Minute), cronruns.StateFailed)
	ts, _ := newCronRunServer(t, fake)
	createCronJob(t, ts, cronJobWithLabels("default", "cron"))
	createPlainJob(t, ts, "plain", nil)

	resp := get(t, ts.URL+"/api/v1/namespaces/default/pods")
	assertStatus(t, resp, http.StatusOK)
	items := listItems(t, decodeBody(t, resp))
	if len(items) != 4 {
		t.Fatalf("expected 3 cron run pods and 1 job pod, got %d: %v", len(items), items)
	}
	seen := make(map[string]int)
	for _, item := range items {
		name := nested(item, "metadata")["name"].(string)
		seen[name]++
	}
	for name, count := range seen {
		if count != 1 {
			t.Fatalf("pod %s appears %d times", name, count)
		}
	}
	if seen[f.jobA+"-run"] != 1 || seen[f.jobB+"-run"] != 1 || seen[f.jobC+"-run"] != 1 || seen["plain-run"] != 1 {
		t.Fatalf("pod names = %v", seen)
	}
	plain := findItem(items, "plain-run")
	if plain == nil {
		t.Fatal("plain job pod missing")
	}
	if got := nested(plain, "status")["phase"]; got != "Failed" {
		t.Fatalf("plain job pod phase = %v, want Failed", got)
	}
	statuses, _ := nested(plain, "status")["containerStatuses"].([]interface{})
	terminated := nested(statuses[0].(map[string]interface{}), "state", "terminated")
	if terminated["exitCode"] != float64(7) {
		t.Fatalf("plain job exit code = %v, want 7", terminated["exitCode"])
	}

	resp = get(t, ts.URL+"/api/v1/namespaces/default/pods?labelSelector=job-name%3Dplain")
	assertStatus(t, resp, http.StatusOK)
	if got := len(listItems(t, decodeBody(t, resp))); got != 1 {
		t.Fatalf("labelSelector job-name=plain pods = %d, want 1", got)
	}
}

func TestJobPodPhaseFromStoredStatus(t *testing.T) {
	ts, st := newTestServer(t)
	createPlainJob(t, ts, "plain", nil)

	for _, tc := range []struct {
		active, succeeded, failed int32
		phase                     string
	}{
		{0, 0, 0, "Pending"},
		{1, 0, 0, "Running"},
		{0, 1, 0, "Succeeded"},
		{0, 0, 1, "Failed"},
	} {
		st.UpdateJobStatus("default", "plain", tc.active, tc.succeeded, tc.failed)
		resp := get(t, ts.URL+"/api/v1/namespaces/default/pods/plain-run")
		assertStatus(t, resp, http.StatusOK)
		if got := nested(decodeBody(t, resp), "status")["phase"]; got != tc.phase {
			t.Fatalf("status %d/%d/%d phase = %v, want %s",
				tc.active, tc.succeeded, tc.failed, got, tc.phase)
		}
	}
}
