package server_test

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"q8s/internal/store"
)

// kubectl get -w sends resourceVersion=0 and drops the first watch event as
// the copy of the object it already printed, so the server must replay the
// current state as ADDED first; otherwise the first real change is lost.
func TestWatchResourceVersionZeroReplaysCurrentState(t *testing.T) {
	ts, st := newTestServer(t)
	resp := post(t, ts.URL+"/api/v1/namespaces/default/pods", podBody("default", "lv", "nginx:latest"))
	assertStatus(t, resp, 201)
	resp.Body.Close()
	st.UpdatePodContainerPhases("default", "lv", corev1.PodRunning, map[string]store.ContainerSyncState{"lv": {Phase: corev1.PodRunning, Health: "healthy"}})

	events := startWatch(t, ts.URL+"/api/v1/namespaces/default/pods?resourceVersion=0&watch=true", "")
	time.Sleep(150 * time.Millisecond)
	st.UpdatePodContainerPhases("default", "lv", corev1.PodRunning, map[string]store.ContainerSyncState{"lv": {Phase: corev1.PodRunning, Health: "starting"}})

	var got []string
	deadline := time.After(2 * time.Second)
	for len(got) < 2 {
		select {
		case ev := <-events:
			cs := ev.Object["status"].(map[string]interface{})["containerStatuses"].([]interface{})[0].(map[string]interface{})
			got = append(got, ev.Type+map[bool]string{true: " ready", false: " not-ready"}[cs["ready"] == true])
		case <-deadline:
			t.Fatalf("events: %v", got)
		}
	}
	if got[0] != "ADDED ready" || got[1] != "MODIFIED not-ready" {
		t.Fatalf("events = %v, want [ADDED ready, MODIFIED not-ready]", got)
	}
}

// kubectl run -it waits for its pod with the watch-list protocol: a
// fieldSelector on the pod name, sendInitialEvents=true, and it blocks until
// a BOOKMARK annotated k8s.io/initial-events-end arrives. Only the named pod
// may be reported.
func TestWatchListSendsInitialEventsBookmarkForNamedPod(t *testing.T) {
	ts, _ := newTestServer(t)
	for _, n := range []string{"other", "mine"} {
		resp := post(t, ts.URL+"/api/v1/namespaces/default/pods", podBody("default", n, "nginx:latest"))
		assertStatus(t, resp, 201)
		resp.Body.Close()
	}

	events := startWatch(t, ts.URL+"/api/v1/namespaces/default/pods?watch=true&sendInitialEvents=true&resourceVersionMatch=NotOlderThan&allowWatchBookmarks=true&fieldSelector=metadata.name%3Dmine", "")
	var got []string
	deadline := time.After(2 * time.Second)
	for len(got) < 2 {
		select {
		case ev := <-events:
			meta := ev.Object["metadata"].(map[string]interface{})
			if ev.Type == "BOOKMARK" {
				ann, _ := meta["annotations"].(map[string]interface{})
				if ann["k8s.io/initial-events-end"] != "true" || ev.Object["kind"] != "Pod" || ev.Object["apiVersion"] != "v1" {
					t.Fatalf("bookmark = %v", ev.Object)
				}
				got = append(got, "BOOKMARK")
				continue
			}
			got = append(got, ev.Type+" "+meta["name"].(string))
		case <-deadline:
			t.Fatalf("events: %v", got)
		}
	}
	if got[0] != "ADDED mine" || got[1] != "BOOKMARK" {
		t.Fatalf("events = %v, want [ADDED mine, BOOKMARK]", got)
	}
}
