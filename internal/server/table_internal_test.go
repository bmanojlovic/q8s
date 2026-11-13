package server

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPodsTableRestarts(t *testing.T) {
	finished := metav1.NewTime(time.Now().Add(-5 * time.Minute))
	pod := func(name string, statuses ...corev1.ContainerStatus) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", CreationTimestamp: metav1.Now()},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "img"}}},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: statuses},
		}
	}

	tbl := podsToTable([]*corev1.Pod{
		pod("none"),
		pod("plain", corev1.ContainerStatus{Name: "app", RestartCount: 2}),
		pod("aged", corev1.ContainerStatus{
			Name:         "app",
			RestartCount: 2,
			LastTerminationState: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error", FinishedAt: finished},
			},
		}),
	}, "1")

	def := tbl.ColumnDefinitions[3]
	if def.Name != "Restarts" || def.Type != "string" {
		t.Fatalf("Restarts column = %+v, want name Restarts and type string", def)
	}

	want := []string{"0", "2", "2 (5m ago)"}
	if len(tbl.Rows) != len(want) {
		t.Fatalf("rows = %d, want %d", len(tbl.Rows), len(want))
	}
	for i, w := range want {
		got, ok := tbl.Rows[i].Cells[3].(string)
		if !ok {
			t.Fatalf("row %d restarts cell is %T, want string", i, tbl.Rows[i].Cells[3])
		}
		if got != w {
			t.Errorf("row %d restarts = %q, want %q", i, got, w)
		}
	}
}

func TestPodsTableRestartsUsesMostRecentFinished(t *testing.T) {
	older := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	newer := metav1.NewTime(time.Now().Add(-2 * time.Minute))
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "multi", Namespace: "default", CreationTimestamp: metav1.Now()},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "a", Image: "img-a"},
			{Name: "b", Image: "img-b"},
		}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{
			{Name: "a", RestartCount: 1, LastTerminationState: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, FinishedAt: older},
			}},
			{Name: "b", RestartCount: 1, LastTerminationState: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, FinishedAt: newer},
			}},
		}},
	}

	tbl := podsToTable([]*corev1.Pod{p}, "1")
	if got := tbl.Rows[0].Cells[3]; got != "2 (2m ago)" {
		t.Errorf("restarts = %v, want %q", got, "2 (2m ago)")
	}
}

func TestPodStatusContainerStates(t *testing.T) {
	cases := []struct {
		name string
		pod  *corev1.Pod
		want string
	}{
		{
			name: "creating",
			pod: &corev1.Pod{Status: corev1.PodStatus{
				Phase: corev1.PodPending,
				ContainerStatuses: []corev1.ContainerStatus{{
					Name:  "app",
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
				}},
			}},
			want: "ContainerCreating",
		},
		{
			name: "running",
			pod: &corev1.Pod{Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				ContainerStatuses: []corev1.ContainerStatus{{
					Name:  "app",
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}},
				}},
			}},
			want: "Running",
		},
		{
			name: "crashloop",
			pod: &corev1.Pod{Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				ContainerStatuses: []corev1.ContainerStatus{{
					Name:         "app",
					RestartCount: 2,
					State:        corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{ExitCode: 1},
					},
				}},
			}},
			want: "CrashLoopBackOff",
		},
	}
	for _, tc := range cases {
		if got := podStatus(tc.pod); got != tc.want {
			t.Errorf("%s: podStatus = %q, want %q", tc.name, got, tc.want)
		}
	}
}
