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
