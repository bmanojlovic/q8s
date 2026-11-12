package server_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"q8s/internal/cronruns"
	"q8s/internal/server"
	"q8s/internal/store"
)

// fakeJournal answers journalctl calls from in-memory maps. It counts calls so
// a test can assert that a watch never touches the Source.
type fakeJournal struct {
	mu    sync.Mutex
	calls int
	runs  map[string][]byte // unit name -> run message lines
	logs  map[string][]byte // container name -> output lines
	args  [][]string
}

func newFakeJournal() *fakeJournal {
	return &fakeJournal{runs: make(map[string][]byte), logs: make(map[string][]byte)}
}

func (f *fakeJournal) source() *cronruns.Source {
	return &cronruns.Source{Rootful: false, Run: f.run, Now: time.Now}
}

func (f *fakeJournal) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	f.calls++
	f.args = append(f.args, append([]string{name}, args...))
	f.mu.Unlock()

	var unit, container string
	for i, a := range args {
		if a == "-u" && i+1 < len(args) {
			unit = args[i+1]
		}
		if strings.HasPrefix(a, "CONTAINER_NAME=") {
			container = strings.TrimPrefix(a, "CONTAINER_NAME=")
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if container != "" {
		return f.logs[container], nil
	}
	return f.runs[unit], nil
}

func (f *fakeJournal) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func newCronRunServer(t *testing.T, fake *fakeJournal) (*httptest.Server, *store.Store) {
	t.Helper()
	certPEM, keyPEM := genTestCert(t)
	st := store.New()
	srv, err := server.New(server.Config{
		Store:    st,
		CertPEM:  certPEM,
		KeyPEM:   keyPEM,
		CronRuns: fake.source(),
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, st
}

func journalJSON(id, messageID string, ts time.Time, extra string) string {
	return fmt.Sprintf(`{"USER_INVOCATION_ID":"%s","MESSAGE_ID":"%s","__REALTIME_TIMESTAMP":"%d"%s}`,
		id, messageID, ts.UnixMicro(), extra)
}

type cronFixture struct {
	base  time.Time
	idA   string
	idB   string
	idC   string
	jobA  string
	jobB  string
	jobC  string
	lines []string
}

func newCronFixture() *cronFixture {
	base := time.Unix(1700000000, 0).UTC()
	f := &cronFixture{base: base, idA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", idB: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", idC: "cccccccccccccccccccccccccccccccc"}
	f.jobA = fmt.Sprintf("cron-%d", base.Unix()/60)
	f.jobB = fmt.Sprintf("cron-%d", (base.Unix()+60)/60)
	f.jobC = fmt.Sprintf("cron-%d", (base.Unix()+120)/60)
	f.lines = []string{
		journalJSON(f.idA, cronruns.MessageIDStarted, base, ""),
		journalJSON(f.idA, cronruns.MessageIDExited, base.Add(time.Second), `,"EXIT_STATUS":"0"`),
		journalJSON(f.idA, cronruns.MessageIDSucceeded, base.Add(2*time.Second), ""),
		journalJSON(f.idB, cronruns.MessageIDStarted, base.Add(60*time.Second), ""),
		journalJSON(f.idB, cronruns.MessageIDExited, base.Add(61*time.Second), `,"EXIT_STATUS":"3"`),
		journalJSON(f.idB, cronruns.MessageIDFailed, base.Add(62*time.Second), ""),
		journalJSON(f.idC, cronruns.MessageIDStarted, base.Add(120*time.Second), ""),
	}
	return f
}

func (f *cronFixture) install(fake *fakeJournal) {
	fake.runs["default-cron-cron.service"] = []byte(strings.Join(f.lines, "\n"))
}

func cronJobWithLabels(ns, name string) map[string]interface{} {
	body := cronJobBody(ns, name, "*/5 * * * *")
	spec := body["spec"].(map[string]interface{})
	jt := spec["jobTemplate"].(map[string]interface{})
	jt["metadata"] = map[string]interface{}{
		"labels": map[string]interface{}{"app": "demo-app"},
	}
	jtSpec := jt["spec"].(map[string]interface{})
	tmpl := jtSpec["template"].(map[string]interface{})
	tmplSpec := tmpl["spec"].(map[string]interface{})
	tmplSpec["restartPolicy"] = "Never"
	return body
}

func createCronJob(t *testing.T, ts *httptest.Server, body map[string]interface{}) {
	t.Helper()
	// The fixture runs are from 2023; runs older than the CronJob are hidden,
	// so the CronJob must be older than them.
	if md, ok := body["metadata"].(map[string]interface{}); ok {
		if _, set := md["creationTimestamp"]; !set {
			md["creationTimestamp"] = "2023-01-01T00:00:00Z"
		}
	}
	resp := post(t, ts.URL+"/apis/batch/v1/namespaces/default/cronjobs", body)
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()
}

func listItems(t *testing.T, m map[string]interface{}) []map[string]interface{} {
	t.Helper()
	raw, _ := m["items"].([]interface{})
	items := make([]map[string]interface{}, 0, len(raw))
	for _, it := range raw {
		obj, ok := it.(map[string]interface{})
		if !ok {
			t.Fatalf("list item is not an object: %T", it)
		}
		items = append(items, obj)
	}
	return items
}

func nested(m map[string]interface{}, keys ...string) map[string]interface{} {
	cur := m
	for _, k := range keys {
		next, _ := cur[k].(map[string]interface{})
		if next == nil {
			return map[string]interface{}{}
		}
		cur = next
	}
	return cur
}

func findItem(items []map[string]interface{}, name string) map[string]interface{} {
	for _, it := range items {
		if nested(it, "metadata")["name"] == name {
			return it
		}
	}
	return nil
}

func TestCronRunPodsList(t *testing.T) {
	fake := newFakeJournal()
	f := newCronFixture()
	f.install(fake)
	ts, _ := newCronRunServer(t, fake)
	createCronJob(t, ts, cronJobWithLabels("default", "cron"))

	resp := get(t, ts.URL+"/api/v1/namespaces/default/pods")
	assertStatus(t, resp, 200)
	m := decodeBody(t, resp)
	assertKind(t, m, "PodList")
	items := listItems(t, m)
	if len(items) != 3 {
		t.Fatalf("expected 3 run pods, got %d", len(items))
	}

	wantPhases := map[string]corev1.PodPhase{
		f.jobA + "-run": corev1.PodSucceeded,
		f.jobB + "-run": corev1.PodFailed,
		f.jobC + "-run": corev1.PodRunning,
	}
	for name, phase := range wantPhases {
		pod := findItem(items, name)
		if pod == nil {
			t.Fatalf("missing synthetic pod %q in %v", name, items)
		}
		labels := nested(pod, "metadata")["labels"].(map[string]interface{})
		if labels["job-name"] == nil || labels["q8s.io/cronjob"] != "cron" {
			t.Fatalf("pod %s labels = %v", name, labels)
		}
		owners, _ := nested(pod, "metadata")["ownerReferences"].([]interface{})
		if len(owners) != 1 {
			t.Fatalf("pod %s ownerReferences = %v", name, owners)
		}
		owner := owners[0].(map[string]interface{})
		if owner["kind"] != "Job" || owner["name"] != strings.TrimSuffix(name, "-run") || owner["controller"] != true {
			t.Fatalf("pod %s owner = %v", name, owner)
		}
		if got := nested(pod, "status")["phase"]; got != string(phase) {
			t.Fatalf("pod %s phase = %v, want %s", name, got, phase)
		}
		statuses, _ := nested(pod, "status")["containerStatuses"].([]interface{})
		if len(statuses) != 1 {
			t.Fatalf("pod %s containerStatuses = %v", name, statuses)
		}
		cs := statuses[0].(map[string]interface{})
		if cs["name"] != "task" {
			t.Fatalf("pod %s container name = %v", name, cs["name"])
		}
	}

	// The all-namespaces list (kubectl get pods -A) contains the same runs.
	resp = get(t, ts.URL+"/api/v1/pods")
	assertStatus(t, resp, 200)
	if got := len(listItems(t, decodeBody(t, resp))); got != 3 {
		t.Fatalf("all-namespaces pods = %d, want 3", got)
	}
}

func TestCronRunJobsList(t *testing.T) {
	fake := newFakeJournal()
	f := newCronFixture()
	f.install(fake)
	ts, st := newCronRunServer(t, fake)
	createCronJob(t, ts, cronJobWithLabels("default", "cron"))
	cj, err := st.GetCronJob("default", "cron")
	if err != nil {
		t.Fatal(err)
	}

	resp := get(t, ts.URL+"/apis/batch/v1/namespaces/default/jobs")
	assertStatus(t, resp, 200)
	m := decodeBody(t, resp)
	assertKind(t, m, "JobList")
	items := listItems(t, m)
	if len(items) != 3 {
		t.Fatalf("expected 3 run jobs, got %d", len(items))
	}

	want := map[string]struct {
		active    float64
		succeeded float64
		failed    float64
		condition string
	}{
		f.jobA: {succeeded: 1, condition: "Complete"},
		f.jobB: {failed: 1, condition: "Failed"},
		f.jobC: {active: 1},
	}
	for name, w := range want {
		job := findItem(items, name)
		if job == nil {
			t.Fatalf("missing synthetic job %q", name)
		}
		labels := nested(job, "metadata")["labels"].(map[string]interface{})
		if labels["app"] != "demo-app" || labels["job-name"] != name || labels["q8s.io/cronjob"] != "cron" {
			t.Fatalf("job %s labels = %v", name, labels)
		}
		annotations := nested(job, "metadata")["annotations"].(map[string]interface{})
		if annotations["q8s.io/invocation-id"] == nil {
			t.Fatalf("job %s annotations = %v", name, annotations)
		}
		owners, _ := nested(job, "metadata")["ownerReferences"].([]interface{})
		if len(owners) != 1 {
			t.Fatalf("job %s ownerReferences = %v", name, owners)
		}
		owner := owners[0].(map[string]interface{})
		if owner["kind"] != "CronJob" || owner["name"] != "cron" || owner["uid"] != string(cj.UID) || owner["controller"] != true {
			t.Fatalf("job %s owner = %v", name, owner)
		}
		selector := nested(job, "spec", "selector", "matchLabels")
		if selector["job-name"] != name {
			t.Fatalf("job %s selector = %v", name, selector)
		}
		status := nested(job, "status")
		if status["active"] != w.active && w.active != 0 {
			t.Fatalf("job %s active = %v, want %v", name, status["active"], w.active)
		}
		if status["succeeded"] != w.succeeded && w.succeeded != 0 {
			t.Fatalf("job %s succeeded = %v, want %v", name, status["succeeded"], w.succeeded)
		}
		if status["failed"] != w.failed && w.failed != 0 {
			t.Fatalf("job %s failed = %v, want %v", name, status["failed"], w.failed)
		}
		conds, _ := status["conditions"].([]interface{})
		if w.condition != "" {
			if len(conds) != 1 || conds[0].(map[string]interface{})["type"] != w.condition {
				t.Fatalf("job %s conditions = %v, want %s", name, conds, w.condition)
			}
		}
	}

	// The all-namespaces list (kubectl get jobs -A) contains the same runs.
	resp = get(t, ts.URL+"/apis/batch/v1/jobs")
	assertStatus(t, resp, 200)
	if got := len(listItems(t, decodeBody(t, resp))); got != 3 {
		t.Fatalf("all-namespaces jobs = %d, want 3", got)
	}
}

func TestCronRunHistoryLimits(t *testing.T) {
	fake := newFakeJournal()
	base := time.Unix(1700000000, 0).UTC()
	var lines []string
	add := func(id, messageID string, ts time.Time, extra string) {
		lines = append(lines, journalJSON(id, messageID, ts, extra))
	}
	// Oldest to newest: 3 succeeded, 2 failed, 1 running.
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("%032d", i+1)
		add(id, cronruns.MessageIDStarted, base.Add(time.Duration(i)*time.Minute), "")
		add(id, cronruns.MessageIDSucceeded, base.Add(time.Duration(i)*time.Minute+time.Second), "")
	}
	for i := 3; i < 5; i++ {
		id := fmt.Sprintf("%032d", i+1)
		add(id, cronruns.MessageIDStarted, base.Add(time.Duration(i)*time.Minute), "")
		add(id, cronruns.MessageIDFailed, base.Add(time.Duration(i)*time.Minute+time.Second), "")
	}
	add(fmt.Sprintf("%032d", 6), cronruns.MessageIDStarted, base.Add(5*time.Minute), "")
	fake.runs["default-limited-cron.service"] = []byte(strings.Join(lines, "\n"))
	ts, _ := newCronRunServer(t, fake)

	body := cronJobWithLabels("default", "limited")
	spec := body["spec"].(map[string]interface{})
	spec["successfulJobsHistoryLimit"] = 1
	spec["failedJobsHistoryLimit"] = 0
	createCronJob(t, ts, body)

	resp := get(t, ts.URL+"/apis/batch/v1/namespaces/default/jobs")
	assertStatus(t, resp, 200)
	items := listItems(t, decodeBody(t, resp))
	if len(items) != 2 {
		t.Fatalf("expected running + newest succeeded job, got %d: %v", len(items), items)
	}
	names := map[string]bool{}
	for _, it := range items {
		names[nested(it, "metadata")["name"].(string)] = true
	}
	newestSuccess := fmt.Sprintf("limited-%d", (base.Unix()+2*60)/60)
	running := fmt.Sprintf("limited-%d", (base.Unix()+5*60)/60)
	if !names[newestSuccess] || !names[running] || len(names) != 2 {
		t.Fatalf("history limit names = %v, want %s and %s", names, newestSuccess, running)
	}
}

func TestCronRunStoredPodWins(t *testing.T) {
	fake := newFakeJournal()
	f := newCronFixture()
	f.install(fake)
	ts, _ := newCronRunServer(t, fake)
	createCronJob(t, ts, cronJobWithLabels("default", "cron"))

	runPod := f.jobC + "-run"
	resp := post(t, ts.URL+"/api/v1/namespaces/default/pods", podBody("default", runPod, "nginx:latest"))
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	resp = get(t, ts.URL+"/api/v1/namespaces/default/pods/"+runPod)
	assertStatus(t, resp, 200)
	pod := decodeBody(t, resp)
	if uid := nested(pod, "metadata")["uid"]; !strings.HasPrefix(fmt.Sprint(uid), "q8s-pod-") {
		t.Fatalf("GET returned the synthetic pod, uid = %v", uid)
	}

	resp = get(t, ts.URL+"/api/v1/namespaces/default/pods")
	assertStatus(t, resp, 200)
	items := listItems(t, decodeBody(t, resp))
	count := 0
	for _, it := range items {
		if nested(it, "metadata")["name"] == runPod {
			count++
			if uid := nested(it, "metadata")["uid"]; !strings.HasPrefix(fmt.Sprint(uid), "q8s-pod-") {
				t.Fatalf("list contains both pods: uid = %v", uid)
			}
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one pod named %s, got %d", runPod, count)
	}

	// The stored pod still deletes normally.
	resp = do(t, http.MethodDelete, ts.URL+"/api/v1/namespaces/default/pods/"+runPod, nil)
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()
}

func TestCronRunLabelSelector(t *testing.T) {
	fake := newFakeJournal()
	f := newCronFixture()
	f.install(fake)
	ts, _ := newCronRunServer(t, fake)
	createCronJob(t, ts, cronJobWithLabels("default", "cron"))

	resp := get(t, ts.URL+"/api/v1/namespaces/default/pods?labelSelector=job-name%3D"+f.jobA)
	assertStatus(t, resp, 200)
	items := listItems(t, decodeBody(t, resp))
	if len(items) != 1 {
		t.Fatalf("labelSelector job-name=%s returned %d pods", f.jobA, len(items))
	}
	if got := nested(items[0], "metadata")["name"]; got != f.jobA+"-run" {
		t.Fatalf("labelSelector returned %v, want %s-run", got, f.jobA)
	}
}

func TestCronRunPodLogs(t *testing.T) {
	fake := newFakeJournal()
	f := newCronFixture()
	f.install(fake)
	fake.logs["default-cron-cron"] = []byte(strings.Join([]string{
		`{"MESSAGE":"hello","__REALTIME_TIMESTAMP":"1700000000000000"}`,
		`{"MESSAGE":"world","__REALTIME_TIMESTAMP":"1700000001000000"}`,
		`{"MESSAGE":[104,105],"__REALTIME_TIMESTAMP":"1700000002000000"}`,
	}, "\n"))
	ts, _ := newCronRunServer(t, fake)
	createCronJob(t, ts, cronJobWithLabels("default", "cron"))

	pod := f.jobA + "-run"
	resp := get(t, ts.URL+"/api/v1/namespaces/default/pods/"+pod+"/log")
	assertStatus(t, resp, 200)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if got, want := string(body), "hello\nworld\nhi\n"; got != want {
		t.Fatalf("logs = %q, want %q", got, want)
	}

	resp = get(t, ts.URL+"/api/v1/namespaces/default/pods/"+pod+"/log?timestamps=true")
	assertStatus(t, resp, 200)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	want := "2023-11-14T22:13:20Z hello\n2023-11-14T22:13:21Z world\n2023-11-14T22:13:22Z hi\n"
	if string(body) != want {
		t.Fatalf("timestamped logs = %q, want %q", body, want)
	}
}

func TestCronRunWritesRejected(t *testing.T) {
	fake := newFakeJournal()
	f := newCronFixture()
	f.install(fake)
	ts, _ := newCronRunServer(t, fake)
	createCronJob(t, ts, cronJobWithLabels("default", "cron"))

	jobURL := ts.URL + "/apis/batch/v1/namespaces/default/jobs/" + f.jobC
	podURL := ts.URL + "/api/v1/namespaces/default/pods/" + f.jobC + "-run"
	for _, tc := range []struct {
		method string
		url    string
		body   interface{}
		kind   string
	}{
		{http.MethodDelete, jobURL, nil, "Job"},
		{http.MethodPut, jobURL, jobBody("default", f.jobC), "Job"},
		{http.MethodPatch, jobURL, map[string]interface{}{"metadata": map[string]interface{}{"labels": map[string]interface{}{"x": "y"}}}, "Job"},
		{http.MethodDelete, podURL, nil, "Pod"},
		{http.MethodPut, podURL, podBody("default", f.jobC+"-run", "nginx:latest"), "Pod"},
		{http.MethodPatch, podURL, map[string]interface{}{"metadata": map[string]interface{}{"labels": map[string]interface{}{"x": "y"}}}, "Pod"},
	} {
		resp := do(t, tc.method, tc.url, tc.body)
		if resp.StatusCode != http.StatusMethodNotAllowed {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("%s %s: status %d, want 405: %s", tc.method, tc.url, resp.StatusCode, body)
		}
		m := decodeBody(t, resp)
		if m["reason"] != "MethodNotAllowed" {
			t.Fatalf("%s %s: reason %v", tc.method, tc.url, m["reason"])
		}
		msg, _ := m["message"].(string)
		if !strings.Contains(msg, "is a CronJob run derived from the systemd journal: delete/update is not implemented") {
			t.Fatalf("%s %s: message %q", tc.method, tc.url, msg)
		}
	}

	// The run still exists after the rejected writes.
	resp := get(t, jobURL)
	assertStatus(t, resp, 200)
	resp.Body.Close()
}

func TestCronRunCronJobStatus(t *testing.T) {
	fake := newFakeJournal()
	f := newCronFixture()
	f.install(fake)
	ts, _ := newCronRunServer(t, fake)
	createCronJob(t, ts, cronJobWithLabels("default", "cron"))

	resp := get(t, ts.URL+"/apis/batch/v1/namespaces/default/cronjobs/cron")
	assertStatus(t, resp, 200)
	cj := decodeBody(t, resp)
	status := nested(cj, "status")

	if got := status["lastScheduleTime"]; got != "2023-11-14T22:15:20Z" {
		t.Fatalf("lastScheduleTime = %v", got)
	}
	if got := status["lastSuccessfulTime"]; got != "2023-11-14T22:13:22Z" {
		t.Fatalf("lastSuccessfulTime = %v", got)
	}
	active, _ := status["active"].([]interface{})
	if len(active) != 1 {
		t.Fatalf("active = %v", status["active"])
	}
	ref := active[0].(map[string]interface{})
	if ref["kind"] != "Job" || ref["name"] != f.jobC {
		t.Fatalf("active ref = %v, want job %s", ref, f.jobC)
	}

	// Lists carry the same derived status.
	resp = get(t, ts.URL+"/apis/batch/v1/namespaces/default/cronjobs")
	assertStatus(t, resp, 200)
	items := listItems(t, decodeBody(t, resp))
	listed := findItem(items, "cron")
	if listed == nil {
		t.Fatalf("cronjob missing from list")
	}
	if got := nested(listed, "status")["lastScheduleTime"]; got != "2023-11-14T22:15:20Z" {
		t.Fatalf("listed lastScheduleTime = %v", got)
	}
}

func TestCronRunsDisabledByDefault(t *testing.T) {
	ts, _ := newTestServer(t)
	createCronJob(t, ts, cronJobWithLabels("default", "cron"))

	resp := get(t, ts.URL+"/api/v1/namespaces/default/pods")
	assertStatus(t, resp, 200)
	if items := listItems(t, decodeBody(t, resp)); len(items) != 0 {
		t.Fatalf("expected no synthetic pods with CronRuns nil, got %v", items)
	}
	resp = get(t, ts.URL+"/apis/batch/v1/namespaces/default/jobs")
	assertStatus(t, resp, 200)
	if items := listItems(t, decodeBody(t, resp)); len(items) != 0 {
		t.Fatalf("expected no synthetic jobs with CronRuns nil, got %v", items)
	}
	resp = get(t, ts.URL+"/apis/batch/v1/namespaces/default/cronjobs/cron")
	assertStatus(t, resp, 200)
	cj := decodeBody(t, resp)
	if _, ok := nested(cj, "status")["lastScheduleTime"]; ok {
		t.Fatalf("unexpected derived status with CronRuns nil: %v", cj["status"])
	}
}

func TestCronRunWatchNeverReadsJournal(t *testing.T) {
	fake := newFakeJournal()
	f := newCronFixture()
	f.install(fake)
	ts, st := newCronRunServer(t, fake)
	createCronJob(t, ts, cronJobWithLabels("default", "cron"))

	before := fake.callCount()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/pods?watch=true", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	defer resp.Body.Close()
	assertStatus(t, resp, 200)

	// A store change makes the watcher relist; it must still not read runs.
	if _, err := st.CreatePod(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "watched", Namespace: "default"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx"}}},
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if got := fake.callCount(); got != before {
		t.Fatalf("watch called the journal %d times", got-before)
	}
}

func TestCronRunNamesForSameMinute(t *testing.T) {
	fake := newFakeJournal()
	base := time.Unix(1700000000, 0).UTC()
	var lines []string
	for i := 0; i < 2; i++ {
		id := fmt.Sprintf("%032d", i+1)
		lines = append(lines,
			journalJSON(id, cronruns.MessageIDStarted, base.Add(time.Duration(i)*time.Second), ""),
			journalJSON(id, cronruns.MessageIDFailed, base.Add(time.Duration(i)*time.Second+time.Millisecond), ""))
	}
	fake.runs["default-sameminute-cron.service"] = []byte(strings.Join(lines, "\n"))
	ts, _ := newCronRunServer(t, fake)
	body := cronJobWithLabels("default", "sameminute")
	body["spec"].(map[string]interface{})["failedJobsHistoryLimit"] = 2
	createCronJob(t, ts, body)

	resp := get(t, ts.URL+"/apis/batch/v1/namespaces/default/jobs")
	assertStatus(t, resp, 200)
	items := listItems(t, decodeBody(t, resp))
	minute := base.Unix() / 60
	names := map[string]bool{}
	for _, it := range items {
		names[nested(it, "metadata")["name"].(string)] = true
	}
	if !names[fmt.Sprintf("sameminute-%d", minute)] || !names[fmt.Sprintf("sameminute-%d-2", minute)] {
		t.Fatalf("same-minute job names = %v", names)
	}
}

func TestCronRunGetSingleAndTable(t *testing.T) {
	fake := newFakeJournal()
	f := newCronFixture()
	f.install(fake)
	ts, _ := newCronRunServer(t, fake)
	createCronJob(t, ts, cronJobWithLabels("default", "cron"))

	resp := get(t, ts.URL+"/api/v1/namespaces/default/pods/"+f.jobA+"-run")
	assertStatus(t, resp, 200)
	pod := decodeBody(t, resp)
	if got := nested(pod, "status")["phase"]; got != "Succeeded" {
		t.Fatalf("synthetic pod phase = %v", got)
	}

	resp = get(t, ts.URL+"/apis/batch/v1/namespaces/default/jobs/"+f.jobA)
	assertStatus(t, resp, 200)
	job := decodeBody(t, resp)
	if got := nested(job, "status")["succeeded"]; got != float64(1) {
		t.Fatalf("synthetic job succeeded = %v", got)
	}

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/namespaces/default/pods", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "application/json;as=Table;v=v1;g=meta.k8s.io")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, resp, 200)
	table := decodeBody(t, resp)
	if got := len(table["rows"].([]interface{})); got != 3 {
		t.Fatalf("table rows = %d, want 3", got)
	}
}

func TestCronRunDeleteCollectionSkipsRuns(t *testing.T) {
	fake := newFakeJournal()
	f := newCronFixture()
	f.install(fake)
	ts, _ := newCronRunServer(t, fake)
	createCronJob(t, ts, cronJobWithLabels("default", "cron"))

	resp := do(t, http.MethodDelete, ts.URL+"/api/v1/namespaces/default/pods", nil)
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp = get(t, ts.URL+"/api/v1/namespaces/default/pods")
	assertStatus(t, resp, 200)
	if got := len(listItems(t, decodeBody(t, resp))); got != 3 {
		t.Fatalf("deletecollection removed runs: %d pods left", got)
	}
}

func TestCronRunExecRejected(t *testing.T) {
	fake := newFakeJournal()
	f := newCronFixture()
	f.install(fake)
	ts, _ := newCronRunServer(t, fake)
	createCronJob(t, ts, cronJobWithLabels("default", "cron"))

	resp := get(t, ts.URL+"/api/v1/namespaces/default/pods/"+f.jobC+"-run/exec?command=ls")
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("exec status = %d: %s", resp.StatusCode, body)
	}
	m := decodeBody(t, resp)
	msg, _ := m["message"].(string)
	if !strings.Contains(msg, "CronJob runs cannot be exec'd") {
		t.Fatalf("exec message = %q", msg)
	}
}
