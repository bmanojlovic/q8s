package hostcheck

import (
	"fmt"
	"io"
	"strings"
)

// ExitCode maps the report to a process exit code: 2 when anything fails,
// 1 when anything needs attention, 0 when every check is ok or merely
// informational.
func ExitCode(r Report) int {
	code := 0
	for _, c := range r.Checks {
		switch c.Status {
		case StatusFail:
			return 2
		case StatusWarn:
			code = 1
		}
	}
	return code
}

// RenderText writes the human report. Gain lines are shown only for ok
// checks; loss and fix lines only when something is wrong, so a healthy host
// stays short and an unhealthy one tells the operator exactly what to run.
func RenderText(w io.Writer, r Report) {
	podman := "podman not found"
	if r.Podman != "" {
		podman = "podman " + r.Podman
	}
	kernel := r.Kernel
	if kernel == "" {
		kernel = "unknown"
	}
	fmt.Fprintf(w, "q8s doctor: %s, %s, kernel %s\n\n", r.Mode, podman, kernel)

	const indent = "       "
	for _, c := range r.Checks {
		fmt.Fprintf(w, "%-6s %-13s %s\n", "["+string(c.Status)+"]", c.ID, c.Detail)
		switch c.Status {
		case StatusOK:
			writeField(w, indent, "gain: ", c.Gain)
		case StatusWarn, StatusFail:
			writeField(w, indent, "lose: ", c.Loss)
			writeField(w, indent, "fix:  ", c.Fix)
		}
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "summary")
	for _, s := range r.Summary {
		fmt.Fprintf(w, "  %-13s %s\n", s.Topic, s.Text)
	}
}

func writeField(w io.Writer, indent, label, text string) {
	if text == "" {
		return
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if i == 0 {
			fmt.Fprintf(w, "%s%s%s\n", indent, label, line)
			continue
		}
		fmt.Fprintf(w, "%s%s%s\n", indent, strings.Repeat(" ", len(label)), line)
	}
}
