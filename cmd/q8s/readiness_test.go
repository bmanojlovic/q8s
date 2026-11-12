package main

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"q8s/internal/podman"
)

func watchTestPod(ns, name string, phase corev1.PodPhase, ready map[string]bool, containers ...corev1.Container) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       corev1.PodSpec{Containers: containers},
		Status:     corev1.PodStatus{Phase: phase},
	}
	for _, c := range containers {
		pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, corev1.ContainerStatus{
			Name:  c.Name,
			Ready: ready[c.Name],
		})
	}
	return pod
}

func watchTestInfo(ns, name, container, id string, startedAt int64) []podman.ContainerInfo {
	return []podman.ContainerInfo{{
		ID:        id,
		State:     "running",
		StartedAt: startedAt,
		Labels: map[string]string{
			"io.kubernetes.pod.namespace":  ns,
			"io.kubernetes.pod.name":       name,
			"io.kubernetes.container.name": container,
		},
	}}
}

func TestHealthCheckContainerID(t *testing.T) {
	id := strings.Repeat("a", 64)

	tests := []struct {
		name   string
		unit   string
		wantID string
		wantOK bool
	}{
		{
			name:   "full id and suffix",
			unit:   id + "-2bc0f6a9870abb7b.service",
			wantID: id,
			wantOK: true,
		},
		{
			name:   "single hex suffix",
			unit:   id + "-1.service",
			wantID: id,
			wantOK: true,
		},
		{
			name:   "64 hex suffix",
			unit:   id + "-" + strings.Repeat("b", 64) + ".service",
			wantID: id,
			wantOK: true,
		},
		{
			name: "timer",
			unit: id + "-2bc0f6a9870abb7b.timer",
		},
		{
			name: "short id",
			unit: strings.Repeat("a", 63) + "-2bc0f6a9870abb7b.service",
		},
		{
			name: "long id",
			unit: strings.Repeat("a", 65) + "-2bc0f6a9870abb7b.service",
		},
		{
			name: "uppercase id",
			unit: strings.ToUpper(id) + "-2bc0f6a9870abb7b.service",
		},
		{
			name: "uppercase suffix",
			unit: id + "-2BC0F6A9870ABB7B.service",
		},
		{
			name: "missing suffix",
			unit: id + ".service",
		},
		{
			name: "missing separator",
			unit: id + "2bc0f6a9870abb7b.service",
		},
		{
			name: "empty suffix",
			unit: id + "-.service",
		},
		{
			name: "suffix too long",
			unit: id + "-" + strings.Repeat("a", 65) + ".service",
		},
		{
			name: "non hex id",
			unit: strings.Repeat("g", 64) + "-2bc0f6a9870abb7b.service",
		},
		{
			name: "extra dash",
			unit: id + "-2bc0f6a-9870abb7b.service",
		},
		{
			name: "q8s service",
			unit: "demo-app.service",
		},
		{
			name: "q8s pod unit",
			unit: "demo-multi-pod.service",
		},
		{
			name: "empty",
			unit: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotID, gotOK := healthCheckContainerID(tt.unit)
			if gotOK != tt.wantOK || gotID != tt.wantID {
				t.Fatalf("healthCheckContainerID(%q) = (%q, %v), want (%q, %v)",
					tt.unit, gotID, gotOK, tt.wantID, tt.wantOK)
			}
		})
	}
}

func TestWatchedHealthIDs(t *testing.T) {
	now := time.Unix(1700000000, 0)
	start := now.Unix()

	probe := func(period, threshold, initial int32) *corev1.Probe {
		return &corev1.Probe{PeriodSeconds: period, FailureThreshold: threshold, InitialDelaySeconds: initial}
	}
	running := corev1.PodRunning

	idApp := strings.Repeat("a", 64)
	idSlow := strings.Repeat("b", 64)
	idFast := strings.Repeat("c", 64)
	idSide := strings.Repeat("d", 64)

	cases := []struct {
		name       string
		pods       []*corev1.Pod
		containers map[string][]podman.ContainerInfo
		wantIDs    []string
	}{
		{
			name: "no probes",
			pods: []*corev1.Pod{
				watchTestPod("default", "app", running, map[string]bool{"app": false}, corev1.Container{Name: "app"}),
			},
			containers: map[string][]podman.ContainerInfo{
				"default/app": watchTestInfo("default", "app", "app", idApp, start),
			},
		},
		{
			name: "ready",
			pods: []*corev1.Pod{
				watchTestPod("default", "app", running, map[string]bool{"app": true},
					corev1.Container{Name: "app", ReadinessProbe: probe(0, 0, 0)}),
			},
			containers: map[string][]podman.ContainerInfo{
				"default/app": watchTestInfo("default", "app", "app", idApp, start),
			},
		},
		{
			name: "not ready within budget",
			pods: []*corev1.Pod{
				watchTestPod("default", "app", running, map[string]bool{"app": false},
					corev1.Container{Name: "app", ReadinessProbe: probe(5, 2, 0)}),
			},
			containers: map[string][]podman.ContainerInfo{
				"default/app": watchTestInfo("default", "app", "app", idApp, start-1),
			},
			wantIDs: []string{idApp},
		},
		{
			name: "probe defaults",
			pods: []*corev1.Pod{
				watchTestPod("default", "app", running, map[string]bool{"app": false},
					corev1.Container{Name: "app", ReadinessProbe: &corev1.Probe{}}),
			},
			containers: map[string][]podman.ContainerInfo{
				"default/app": watchTestInfo("default", "app", "app", idApp, start),
			},
			wantIDs: []string{idApp},
		},
		{
			name: "all within budget across pods",
			pods: []*corev1.Pod{
				watchTestPod("default", "slow", running, map[string]bool{"app": false},
					corev1.Container{Name: "app", ReadinessProbe: probe(10, 3, 0)}),
				watchTestPod("default", "fast", running, map[string]bool{"app": false},
					corev1.Container{Name: "app", ReadinessProbe: probe(3, 3, 0)}),
			},
			containers: map[string][]podman.ContainerInfo{
				"default/slow": watchTestInfo("default", "slow", "app", idSlow, start),
				"default/fast": watchTestInfo("default", "fast", "app", idFast, start),
			},
			wantIDs: []string{idSlow, idFast},
		},
		{
			name: "exactly at budget still contributes",
			pods: []*corev1.Pod{
				watchTestPod("default", "app", running, map[string]bool{"app": false},
					corev1.Container{Name: "app", ReadinessProbe: &corev1.Probe{}}),
			},
			containers: map[string][]podman.ContainerInfo{
				"default/app": watchTestInfo("default", "app", "app", idApp, start-30),
			},
			wantIDs: []string{idApp},
		},
		{
			name: "past budget",
			pods: []*corev1.Pod{
				watchTestPod("default", "app", running, map[string]bool{"app": false},
					corev1.Container{Name: "app", ReadinessProbe: &corev1.Probe{}}),
			},
			containers: map[string][]podman.ContainerInfo{
				"default/app": watchTestInfo("default", "app", "app", idApp, start-31),
			},
		},
		{
			name: "startup probe extends budget",
			pods: []*corev1.Pod{
				watchTestPod("default", "app", running, map[string]bool{"app": false},
					corev1.Container{
						Name:           "app",
						ReadinessProbe: probe(10, 3, 0),
						StartupProbe:   probe(10, 3, 20),
					}),
			},
			containers: map[string][]podman.ContainerInfo{
				"default/app": watchTestInfo("default", "app", "app", idApp, start-31),
			},
			wantIDs: []string{idApp},
		},
		{
			name: "past extended budget",
			pods: []*corev1.Pod{
				watchTestPod("default", "app", running, map[string]bool{"app": false},
					corev1.Container{
						Name:           "app",
						ReadinessProbe: probe(10, 3, 0),
						StartupProbe:   probe(10, 3, 20),
					}),
			},
			containers: map[string][]podman.ContainerInfo{
				"default/app": watchTestInfo("default", "app", "app", idApp, start-81),
			},
		},
		{
			name: "startup probe only",
			pods: []*corev1.Pod{
				watchTestPod("default", "app", running, map[string]bool{"app": false},
					corev1.Container{Name: "app", StartupProbe: probe(4, 2, 5)}),
			},
			containers: map[string][]podman.ContainerInfo{
				"default/app": watchTestInfo("default", "app", "app", idApp, start-12),
			},
			wantIDs: []string{idApp},
		},
		{
			name: "non-running pod ignored",
			pods: []*corev1.Pod{
				watchTestPod("default", "app", corev1.PodPending, map[string]bool{"app": false},
					corev1.Container{Name: "app", ReadinessProbe: probe(5, 3, 0)}),
			},
			containers: map[string][]podman.ContainerInfo{
				"default/app": watchTestInfo("default", "app", "app", idApp, start),
			},
		},
		{
			name: "missing container status counts as not ready",
			pods: []*corev1.Pod{{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "app"},
				Spec: corev1.PodSpec{Containers: []corev1.Container{
					{Name: "app", ReadinessProbe: probe(7, 3, 0)},
				}},
				Status: corev1.PodStatus{Phase: running},
			}},
			containers: map[string][]podman.ContainerInfo{
				"default/app": watchTestInfo("default", "app", "app", idApp, start),
			},
			wantIDs: []string{idApp},
		},
		{
			name: "liveness-only probe owns the health check",
			pods: []*corev1.Pod{
				watchTestPod("default", "app", running, map[string]bool{"app": false},
					corev1.Container{Name: "app", LivenessProbe: probe(30, 3, 20)}),
			},
			containers: map[string][]podman.ContainerInfo{
				"default/app": watchTestInfo("default", "app", "app", idApp, start),
			},
			wantIDs: []string{idApp},
		},
		{
			name: "liveness probe budget wins over readiness",
			pods: []*corev1.Pod{
				watchTestPod("default", "app", running, map[string]bool{"app": false},
					corev1.Container{Name: "app", LivenessProbe: probe(20, 3, 0), ReadinessProbe: probe(5, 3, 0)}),
			},
			containers: map[string][]podman.ContainerInfo{
				"default/app": watchTestInfo("default", "app", "app", idApp, start-30),
			},
			wantIDs: []string{idApp},
		},
		{
			name: "no podman start time gets no watch",
			pods: []*corev1.Pod{
				watchTestPod("default", "app", running, map[string]bool{"app": false},
					corev1.Container{Name: "app", ReadinessProbe: probe(5, 3, 0)}),
			},
			containers: map[string][]podman.ContainerInfo{},
		},
		{
			name: "only the probing container contributes",
			pods: []*corev1.Pod{
				watchTestPod("default", "app", running, map[string]bool{"side": false, "app": false},
					corev1.Container{Name: "side"},
					corev1.Container{Name: "app", ReadinessProbe: probe(6, 3, 0)}),
			},
			containers: map[string][]podman.ContainerInfo{
				"default/app": {
					{ID: idSide, State: "running", StartedAt: start, Labels: map[string]string{"io.kubernetes.container.name": "side"}},
					{ID: idApp, State: "running", StartedAt: start, Labels: map[string]string{"io.kubernetes.container.name": "app"}},
				},
			},
			wantIDs: []string{idApp},
		},
		{
			name: "missing container id is not watched",
			pods: []*corev1.Pod{
				watchTestPod("default", "app", running, map[string]bool{"app": false},
					corev1.Container{Name: "app", ReadinessProbe: probe(5, 3, 0)}),
			},
			containers: map[string][]podman.ContainerInfo{
				"default/app": watchTestInfo("default", "app", "app", "", start),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := watchedHealthIDs(tc.pods, tc.containers, now)
			if len(got) != len(tc.wantIDs) {
				t.Fatalf("watchedHealthIDs() = %v, want IDs %v", got, tc.wantIDs)
			}
			for _, id := range tc.wantIDs {
				if _, ok := got[id]; !ok {
					t.Errorf("watchedHealthIDs() = %v, missing %q", got, id)
				}
			}
		})
	}
}
