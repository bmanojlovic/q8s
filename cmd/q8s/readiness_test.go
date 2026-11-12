package main

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"q8s/internal/podman"
)

func recheckTestPod(ns, name string, phase corev1.PodPhase, ready map[string]bool, containers ...corev1.Container) *corev1.Pod {
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

func recheckTestInfo(ns, name, container string, startedAt int64) []podman.ContainerInfo {
	return []podman.ContainerInfo{{
		State:     "running",
		StartedAt: startedAt,
		Labels: map[string]string{
			"io.kubernetes.pod.namespace":  ns,
			"io.kubernetes.pod.name":       name,
			"io.kubernetes.container.name": container,
		},
	}}
}

func TestNextRecheckTable(t *testing.T) {
	now := time.Unix(1700000000, 0)
	start := now.Unix()

	probe := func(period, threshold, initial int32) *corev1.Probe {
		return &corev1.Probe{PeriodSeconds: period, FailureThreshold: threshold, InitialDelaySeconds: initial}
	}
	running := corev1.PodRunning

	cases := []struct {
		name       string
		pods       []*corev1.Pod
		containers map[string][]podman.ContainerInfo
		want       time.Duration
		wantOK     bool
	}{
		{
			name: "no probes",
			pods: []*corev1.Pod{
				recheckTestPod("default", "app", running, map[string]bool{"app": false}, corev1.Container{Name: "app"}),
			},
			containers: map[string][]podman.ContainerInfo{
				"default/app": recheckTestInfo("default", "app", "app", start),
			},
			wantOK: false,
		},
		{
			name: "ready",
			pods: []*corev1.Pod{
				recheckTestPod("default", "app", running, map[string]bool{"app": true},
					corev1.Container{Name: "app", ReadinessProbe: probe(0, 0, 0)}),
			},
			containers: map[string][]podman.ContainerInfo{
				"default/app": recheckTestInfo("default", "app", "app", start),
			},
			wantOK: false,
		},
		{
			name: "not ready within budget",
			pods: []*corev1.Pod{
				recheckTestPod("default", "app", running, map[string]bool{"app": false},
					corev1.Container{Name: "app", ReadinessProbe: probe(5, 2, 0)}),
			},
			containers: map[string][]podman.ContainerInfo{
				"default/app": recheckTestInfo("default", "app", "app", start-1),
			},
			want:   5 * time.Second,
			wantOK: true,
		},
		{
			name: "probe defaults",
			pods: []*corev1.Pod{
				recheckTestPod("default", "app", running, map[string]bool{"app": false},
					corev1.Container{Name: "app", ReadinessProbe: &corev1.Probe{}}),
			},
			containers: map[string][]podman.ContainerInfo{
				"default/app": recheckTestInfo("default", "app", "app", start),
			},
			want:   10 * time.Second,
			wantOK: true,
		},
		{
			name: "smallest period across pods",
			pods: []*corev1.Pod{
				recheckTestPod("default", "slow", running, map[string]bool{"app": false},
					corev1.Container{Name: "app", ReadinessProbe: probe(10, 3, 0)}),
				recheckTestPod("default", "fast", running, map[string]bool{"app": false},
					corev1.Container{Name: "app", ReadinessProbe: probe(3, 3, 0)}),
			},
			containers: map[string][]podman.ContainerInfo{
				"default/slow": recheckTestInfo("default", "slow", "app", start),
				"default/fast": recheckTestInfo("default", "fast", "app", start),
			},
			want:   3 * time.Second,
			wantOK: true,
		},
		{
			name: "exactly at budget still contributes",
			pods: []*corev1.Pod{
				recheckTestPod("default", "app", running, map[string]bool{"app": false},
					corev1.Container{Name: "app", ReadinessProbe: &corev1.Probe{}}),
			},
			containers: map[string][]podman.ContainerInfo{
				"default/app": recheckTestInfo("default", "app", "app", start-30),
			},
			want:   10 * time.Second,
			wantOK: true,
		},
		{
			name: "past budget",
			pods: []*corev1.Pod{
				recheckTestPod("default", "app", running, map[string]bool{"app": false},
					corev1.Container{Name: "app", ReadinessProbe: &corev1.Probe{}}),
			},
			containers: map[string][]podman.ContainerInfo{
				"default/app": recheckTestInfo("default", "app", "app", start-31),
			},
			wantOK: false,
		},
		{
			name: "startup probe extends budget",
			pods: []*corev1.Pod{
				recheckTestPod("default", "app", running, map[string]bool{"app": false},
					corev1.Container{
						Name:           "app",
						ReadinessProbe: probe(10, 3, 0),
						StartupProbe:   probe(10, 3, 20),
					}),
			},
			containers: map[string][]podman.ContainerInfo{
				"default/app": recheckTestInfo("default", "app", "app", start-31),
			},
			want:   10 * time.Second,
			wantOK: true,
		},
		{
			name: "past extended budget",
			pods: []*corev1.Pod{
				recheckTestPod("default", "app", running, map[string]bool{"app": false},
					corev1.Container{
						Name:           "app",
						ReadinessProbe: probe(10, 3, 0),
						StartupProbe:   probe(10, 3, 20),
					}),
			},
			containers: map[string][]podman.ContainerInfo{
				"default/app": recheckTestInfo("default", "app", "app", start-81),
			},
			wantOK: false,
		},
		{
			name: "startup probe only",
			pods: []*corev1.Pod{
				recheckTestPod("default", "app", running, map[string]bool{"app": false},
					corev1.Container{Name: "app", StartupProbe: probe(4, 2, 5)}),
			},
			containers: map[string][]podman.ContainerInfo{
				"default/app": recheckTestInfo("default", "app", "app", start-12),
			},
			want:   4 * time.Second,
			wantOK: true,
		},
		{
			name: "non-running pod ignored",
			pods: []*corev1.Pod{
				recheckTestPod("default", "app", corev1.PodPending, map[string]bool{"app": false},
					corev1.Container{Name: "app", ReadinessProbe: probe(5, 3, 0)}),
			},
			containers: map[string][]podman.ContainerInfo{
				"default/app": recheckTestInfo("default", "app", "app", start),
			},
			wantOK: false,
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
				"default/app": recheckTestInfo("default", "app", "app", start),
			},
			want:   7 * time.Second,
			wantOK: true,
		},
		{
			name: "liveness-only probe owns the health check",
			pods: []*corev1.Pod{
				recheckTestPod("default", "app", running, map[string]bool{"app": false},
					corev1.Container{Name: "app", LivenessProbe: probe(30, 3, 20)}),
			},
			containers: map[string][]podman.ContainerInfo{
				"default/app": recheckTestInfo("default", "app", "app", start),
			},
			want:   30 * time.Second,
			wantOK: true,
		},
		{
			name: "liveness period wins over readiness period",
			pods: []*corev1.Pod{
				recheckTestPod("default", "app", running, map[string]bool{"app": false},
					corev1.Container{Name: "app", LivenessProbe: probe(20, 3, 0), ReadinessProbe: probe(5, 3, 0)}),
			},
			containers: map[string][]podman.ContainerInfo{
				"default/app": recheckTestInfo("default", "app", "app", start),
			},
			want:   20 * time.Second,
			wantOK: true,
		},
		{
			name: "no podman start time gets no recheck",
			pods: []*corev1.Pod{
				recheckTestPod("default", "app", running, map[string]bool{"app": false},
					corev1.Container{Name: "app", ReadinessProbe: probe(5, 3, 0)}),
			},
			containers: map[string][]podman.ContainerInfo{},
			wantOK:     false,
		},
		{
			name: "only the probing container contributes",
			pods: []*corev1.Pod{
				recheckTestPod("default", "app", running, map[string]bool{"side": false, "app": false},
					corev1.Container{Name: "side"},
					corev1.Container{Name: "app", ReadinessProbe: probe(6, 3, 0)}),
			},
			containers: map[string][]podman.ContainerInfo{
				"default/app": {
					{State: "running", StartedAt: start, Labels: map[string]string{"io.kubernetes.container.name": "side"}},
					{State: "running", StartedAt: start, Labels: map[string]string{"io.kubernetes.container.name": "app"}},
				},
			},
			want:   6 * time.Second,
			wantOK: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := nextRecheck(tc.pods, tc.containers, now)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("nextRecheck() = (%v, %v), want (%v, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
