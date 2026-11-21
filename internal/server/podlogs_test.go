package server_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"q8s/internal/store"
)

// logPodCreated predates the journal lines used below, like a pod that ran
// after it was created.
const logPodCreated = "2023-11-14T22:13:19Z"

// fakePodmanScript answers the two podman calls the log subresource makes.
// The test controls the answers through environment variables so one script
// covers a container that is gone and one that is still there.
const fakePodmanScript = `#!/bin/sh
if [ "$1" = "container" ] && [ "$2" = "exists" ]; then
	if [ "$FAKE_PODMAN_EXISTS" = "1" ]; then
		exit 0
	fi
	exit 1
fi
if [ "$1" = "logs" ]; then
	printf '%s' "$FAKE_PODMAN_LOGS"
	printf '%s' "$FAKE_PODMAN_STDERR" >&2
	exit "${FAKE_PODMAN_EXIT:-0}"
fi
exit 125
`

// installFakePodman puts a script named podman first in PATH and configures
// its answers. Nothing in these tests reaches a real podman.
func installFakePodman(t *testing.T, exists bool, logs, stderr string, exitCode int) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "podman")
	if err := os.WriteFile(path, []byte(fakePodmanScript), 0o755); err != nil {
		t.Fatalf("write fake podman: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if exists {
		t.Setenv("FAKE_PODMAN_EXISTS", "1")
	} else {
		t.Setenv("FAKE_PODMAN_EXISTS", "0")
	}
	t.Setenv("FAKE_PODMAN_LOGS", logs)
	t.Setenv("FAKE_PODMAN_STDERR", stderr)
	t.Setenv("FAKE_PODMAN_EXIT", strconv.Itoa(exitCode))
}

// createLogPod stores a pod with a fixed creation timestamp and then moves it
// to phase, the way the container sync loop does after the pod has run.
func createLogPod(t *testing.T, ts *httptest.Server, st *store.Store, name string, containers []interface{}, phase corev1.PodPhase) {
	t.Helper()
	body := podBody("default", name, "busybox")
	body["metadata"].(map[string]interface{})["creationTimestamp"] = logPodCreated
	body["spec"].(map[string]interface{})["containers"] = containers
	resp := post(t, ts.URL+"/api/v1/namespaces/default/pods", body)
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()
	st.UpdatePodPhase("default", name, phase, "")
}

func containsArgPair(args []string, first, second string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == first && args[i+1] == second {
			return true
		}
	}
	return false
}

func readLogs(t *testing.T, url string) string {
	t.Helper()
	resp := get(t, url)
	assertStatus(t, resp, http.StatusOK)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return string(body)
}

func TestPodLogsFinishedPodReadsJournal(t *testing.T) {
	fake := newFakeJournal()
	fake.logs["default_done"] = []byte(`{"MESSAGE":"finished output","__REALTIME_TIMESTAMP":"1700000000000000"}`)
	ts, st := newCronRunServer(t, fake)
	installFakePodman(t, false, "", "", 0)
	createLogPod(t, ts, st, "done", []interface{}{
		map[string]interface{}{"name": "app", "image": "busybox"},
	}, corev1.PodSucceeded)

	if got, want := readLogs(t, ts.URL+"/api/v1/namespaces/default/pods/done/log"), "finished output\n"; got != want {
		t.Fatalf("logs = %q, want %q", got, want)
	}

	args := lastJournalArgs(fake)
	if !containsArg(args, "default_done.service") {
		t.Fatalf("journal args selected wrong unit: %v", args)
	}
	if !containsArg(args, "CONTAINER_NAME=default_done") {
		t.Fatalf("journal args selected wrong container: %v", args)
	}
	created, err := time.Parse(time.RFC3339, logPodCreated)
	if err != nil {
		t.Fatal(err)
	}
	if !containsArgPair(args, "--since", "@"+strconv.FormatInt(created.Unix(), 10)) {
		t.Fatalf("journal args did not start at the pod creation: %v", args)
	}

	// Follow on a finished pod returns what is there and ends.
	if got, want := readLogs(t, ts.URL+"/api/v1/namespaces/default/pods/done/log?follow=true"), "finished output\n"; got != want {
		t.Fatalf("followed logs = %q, want %q", got, want)
	}
	if args := lastJournalArgs(fake); containsArg(args, "-f") {
		t.Fatalf("finished pod followed the journal: %v", args)
	}
}

func TestPodLogsMultiContainerReadsJournal(t *testing.T) {
	fake := newFakeJournal()
	fake.logs["default_duo_web"] = []byte(`{"MESSAGE":"web output"}`)
	fake.logs["default_duo_sidecar"] = []byte(`{"MESSAGE":"sidecar output"}`)
	ts, st := newCronRunServer(t, fake)
	installFakePodman(t, false, "", "", 0)
	createLogPod(t, ts, st, "duo", []interface{}{
		map[string]interface{}{"name": "web", "image": "nginx"},
		map[string]interface{}{"name": "sidecar", "image": "busybox"},
	}, corev1.PodFailed)

	if got, want := readLogs(t, ts.URL+"/api/v1/namespaces/default/pods/duo/log?container=sidecar"), "sidecar output\n"; got != want {
		t.Fatalf("sidecar logs = %q, want %q", got, want)
	}

	args := lastJournalArgs(fake)
	if !containsArg(args, "default_duo_sidecar.service") {
		t.Fatalf("journal args selected wrong unit: %v", args)
	}
	if !containsArg(args, "CONTAINER_NAME=default_duo_sidecar") {
		t.Fatalf("journal args selected wrong container: %v", args)
	}

	// Without ?container the first container is selected.
	if got, want := readLogs(t, ts.URL+"/api/v1/namespaces/default/pods/duo/log"), "web output\n"; got != want {
		t.Fatalf("default container logs = %q, want %q", got, want)
	}
	if args := lastJournalArgs(fake); !containsArg(args, "default_duo_web.service") {
		t.Fatalf("journal args selected wrong default unit: %v", args)
	}
}

func TestPodLogsExistingContainerUsesPodman(t *testing.T) {
	fake := newFakeJournal()
	fake.logs["default_live"] = []byte(`{"MESSAGE":"journal output"}`)
	ts, st := newCronRunServer(t, fake)
	installFakePodman(t, true, "podman output\n", "", 0)
	createLogPod(t, ts, st, "live", []interface{}{
		map[string]interface{}{"name": "app", "image": "busybox"},
	}, corev1.PodSucceeded)

	if got, want := readLogs(t, ts.URL+"/api/v1/namespaces/default/pods/live/log"), "podman output\n"; got != want {
		t.Fatalf("logs = %q, want %q", got, want)
	}
	if calls := fake.callCount(); calls != 0 {
		t.Fatalf("existing container read the journal %d times", calls)
	}
}

func TestPodLogsJournalTailAndTimestamps(t *testing.T) {
	fake := newFakeJournal()
	fake.logs["default_tail"] = []byte(strings.Join([]string{
		`{"MESSAGE":"first","__REALTIME_TIMESTAMP":"1700000000000000"}`,
		`{"MESSAGE":"second","__REALTIME_TIMESTAMP":"1700000001000000"}`,
	}, "\n"))
	ts, st := newCronRunServer(t, fake)
	installFakePodman(t, false, "", "", 0)
	createLogPod(t, ts, st, "tail", []interface{}{
		map[string]interface{}{"name": "app", "image": "busybox"},
	}, corev1.PodSucceeded)

	url := ts.URL + "/api/v1/namespaces/default/pods/tail/log?tailLines=2&timestamps=true"
	want := "2023-11-14T22:13:20Z first\n2023-11-14T22:13:21Z second\n"
	if got := readLogs(t, url); got != want {
		t.Fatalf("logs = %q, want %q", got, want)
	}
	args := lastJournalArgs(fake)
	if !containsArgPair(args, "-n", "2") {
		t.Fatalf("journal args did not honour tailLines: %v", args)
	}
	if containsArg(args, "-f") {
		t.Fatalf("journal args followed a finished pod: %v", args)
	}

	// A later sinceTime replaces the pod creation floor.
	later := "2023-11-14T22:13:30Z"
	readLogs(t, ts.URL+"/api/v1/namespaces/default/pods/tail/log?sinceTime="+later)
	laterTime, err := time.Parse(time.RFC3339, later)
	if err != nil {
		t.Fatal(err)
	}
	if args := lastJournalArgs(fake); !containsArgPair(args, "--since", "@"+strconv.FormatInt(laterTime.Unix(), 10)) {
		t.Fatalf("journal args did not honour sinceTime: %v", args)
	}
}

func TestPodLogsPendingPodKeepsPodmanError(t *testing.T) {
	fake := newFakeJournal()
	ts, st := newCronRunServer(t, fake)
	errText := fmt.Sprintf("Error: no container with name or ID %q found: no such container\n", "default_pending")
	installFakePodman(t, false, "", errText, 125)
	createLogPod(t, ts, st, "pending", []interface{}{
		map[string]interface{}{"name": "app", "image": "busybox"},
	}, corev1.PodPending)

	body := readLogs(t, ts.URL+"/api/v1/namespaces/default/pods/pending/log")
	if !strings.Contains(body, "no container") {
		t.Fatalf("pending pod logs = %q, want the podman error", body)
	}
	if calls := fake.callCount(); calls != 0 {
		t.Fatalf("pending pod read the journal %d times", calls)
	}
}
