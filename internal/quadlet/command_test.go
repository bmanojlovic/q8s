package quadlet_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"q8s/internal/quadlet"
)

// commandCase is one Kubernetes command/args combination and the quadlet
// lines it must (or must not) produce. The four rows follow the Kubernetes
// table: command replaces the image ENTRYPOINT, args replace the image CMD.
type commandCase struct {
	name    string
	command []string
	args    []string
	want    []string
	notWant []string
}

func commandCases() []commandCase {
	return []commandCase{
		{
			name:    "neither",
			notWant: []string{"Entrypoint=", "Exec="},
		},
		{
			name:    "command only",
			command: []string{"sleep", "600"},
			want:    []string{`Entrypoint=["sleep","600"]`},
			notWant: []string{"Exec="},
		},
		{
			name:    "command and args",
			command: []string{"sleep"},
			args:    []string{"600"},
			want:    []string{`Entrypoint=["sleep"]`, "Exec=600"},
		},
		{
			name:    "args only",
			args:    []string{"sleep", "600"},
			want:    []string{"Exec=sleep 600"},
			notWant: []string{"Entrypoint="},
		},
	}
}

func assertCommandLines(t *testing.T, out string, want, notWant []string) {
	t.Helper()
	for _, w := range want {
		assertContains(t, out, w)
	}
	for _, n := range notWant {
		assertNotContains(t, out, n)
	}
}

func TestCommandStandalonePod(t *testing.T) {
	for _, tc := range commandCases() {
		t.Run(tc.name, func(t *testing.T) {
			pod := simplePod("default", "app", "img")
			pod.Spec.Containers[0].Command = tc.command
			pod.Spec.Containers[0].Args = tc.args
			out := mustContainer(t, pod, "")
			assertCommandLines(t, out, tc.want, tc.notWant)
		})
	}
}

func TestCommandMultiContainerPod(t *testing.T) {
	for _, tc := range commandCases() {
		t.Run(tc.name, func(t *testing.T) {
			pod := simplePod("default", "multi", "img")
			pod.Spec.Containers = []corev1.Container{
				{Name: "target", Image: "img", Command: tc.command, Args: tc.args},
				{Name: "other", Image: "img", Command: []string{"other-command"}},
			}
			files, err := quadlet.PodUnits(pod.Name, pod, "", nil, nil, "", nil)
			if err != nil {
				t.Fatalf("PodUnits: %v", err)
			}
			out := string(files["default_multi_target.container"])
			assertCommandLines(t, out, tc.want, tc.notWant)
			// The other container keeps its own command: keys are per container.
			assertContains(t, string(files["default_multi_other.container"]), `Entrypoint=["other-command"]`)
		})
	}
}

func jobWithContainer(c corev1.Container) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "myjob", Namespace: "default"},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{c}},
			},
		},
	}
}

func cronJobWithContainer(c corev1.Container) *batchv1.CronJob {
	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "mycron", Namespace: "default"},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 3 * * *",
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{Containers: []corev1.Container{c}},
					},
				},
			},
		},
	}
}

func TestCommandJobContainer(t *testing.T) {
	for _, tc := range commandCases() {
		t.Run(tc.name, func(t *testing.T) {
			job := jobWithContainer(corev1.Container{
				Name: "worker", Image: "img", Command: tc.command, Args: tc.args,
			})
			b, err := quadlet.JobContainer(job.Name, job, "", nil, "")
			if err != nil {
				t.Fatalf("JobContainer: %v", err)
			}
			assertCommandLines(t, string(b), tc.want, tc.notWant)
		})
	}
}

func TestCommandCronContainer(t *testing.T) {
	for _, tc := range commandCases() {
		t.Run(tc.name, func(t *testing.T) {
			cj := cronJobWithContainer(corev1.Container{
				Name: "task", Image: "img", Command: tc.command, Args: tc.args,
			})
			b, err := quadlet.CronContainer(cj.Name, cj, "", nil, "")
			if err != nil {
				t.Fatalf("CronContainer: %v", err)
			}
			assertCommandLines(t, string(b), tc.want, tc.notWant)
		})
	}
}

// entrypointValue returns the raw Entrypoint= value (still systemd escaped)
// from a rendered unit, failing when the line is absent.
func entrypointValue(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "Entrypoint=") {
			return strings.TrimPrefix(line, "Entrypoint=")
		}
	}
	t.Fatalf("no Entrypoint= line in output:\n%s", out)
	return ""
}

func TestCommandEntrypointJSONKeepsSpacesAndQuotes(t *testing.T) {
	command := []string{"/bin/sh", "-c", `echo "hello world"`}
	pod := simplePod("default", "app", "img")
	pod.Spec.Containers[0].Command = command
	out := mustContainer(t, pod, "")

	value := entrypointValue(t, out)
	var got []string
	if err := json.Unmarshal([]byte(value), &got); err != nil {
		t.Fatalf("Entrypoint value is not valid JSON: %v\nvalue: %s", err, value)
	}
	if !reflect.DeepEqual(got, command) {
		t.Errorf("Entrypoint decoded to %q, want %q", got, command)
	}
}

func TestCommandEntrypointEscapesPercentForSystemd(t *testing.T) {
	command := []string{"date", "+%Y-%m-%d"}
	pod := simplePod("default", "app", "img")
	pod.Spec.Containers[0].Command = command
	out := mustContainer(t, pod, "")

	value := entrypointValue(t, out)
	if want := `["date","+%%Y-%%m-%%d"]`; value != want {
		t.Errorf("Entrypoint = %s, want %s (percent doubled for systemd)", value, want)
	}
	// systemd collapses %% back to % before podman sees the value, so the
	// JSON must decode to the original command at that point.
	var got []string
	if err := json.Unmarshal([]byte(strings.ReplaceAll(value, "%%", "%")), &got); err != nil {
		t.Fatalf("Entrypoint value is not valid JSON after systemd escaping: %v\nvalue: %s", err, value)
	}
	if !reflect.DeepEqual(got, command) {
		t.Errorf("Entrypoint decoded to %q, want %q", got, command)
	}
}

// TestCommandArgsOnlyKeepsExactExecLines pins the pre-change rendering for the
// two cases that must stay byte-identical: args alone keeps the historic
// Exec= line, and neither key emits neither line. Only containers that set
// command render differently after tic-c67f.
func TestCommandArgsOnlyKeepsExactExecLines(t *testing.T) {
	t.Run("args only", func(t *testing.T) {
		pod := simplePod("default", "app", "img")
		pod.Spec.Containers[0].Args = []string{"/bin/sh", "-c", "echo hello"}
		out := mustContainer(t, pod, "")
		assertContains(t, out, `Exec=/bin/sh -c "echo hello"`+"\n")
		assertNotContains(t, out, "Entrypoint=")
	})

	t.Run("neither", func(t *testing.T) {
		pod := simplePod("default", "app", "img")
		out := mustContainer(t, pod, "")
		assertNotContains(t, out, "Entrypoint=")
		assertNotContains(t, out, "Exec=")
	})
}
