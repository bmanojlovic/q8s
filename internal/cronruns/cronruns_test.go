package cronruns

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func journalEntry(idField, id, messageID string, ts time.Time, extra string) string {
	return fmt.Sprintf(`{"%s":"%s","MESSAGE_ID":"%s","__REALTIME_TIMESTAMP":"%d"%s}`,
		idField, id, messageID, ts.UnixMicro(), extra)
}

func fakeSource(rootful bool, lines ...string) *Source {
	return &Source{
		Rootful: rootful,
		Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return []byte(strings.Join(lines, "\n")), nil
		},
	}
}

func TestRunsRootless(t *testing.T) {
	base := time.Unix(1700000000, 0).UTC()
	idField := "USER_INVOCATION_ID"
	src := fakeSource(false,
		journalEntry(idField, "aaa", MessageIDStarted, base, ""),
		journalEntry(idField, "aaa", MessageIDExited, base.Add(time.Second), `,"EXIT_STATUS":"0"`),
		journalEntry(idField, "aaa", MessageIDSucceeded, base.Add(2*time.Second), ""),
		// malformed lines are skipped
		"not json",
		`{"MESSAGE":"no id"}`,
		`{"USER_INVOCATION_ID":"nosuch","MESSAGE_ID":"not-a-run","__REALTIME_TIMESTAMP":"1"}`,
		journalEntry(idField, "bbb", MessageIDStarted, base.Add(5*time.Minute), ""),
		journalEntry(idField, "bbb", MessageIDExited, base.Add(5*time.Minute+time.Second), `,"EXIT_STATUS":"7"`),
		journalEntry(idField, "bbb", MessageIDFailed, base.Add(5*time.Minute+2*time.Second), ""),
		journalEntry(idField, "ccc", MessageIDStarted, base.Add(10*time.Minute), ""),
	)
	runs, err := src.Runs(context.Background(), "default", "cron")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 3 {
		t.Fatalf("expected 3 runs, got %d: %+v", len(runs), runs)
	}
	if runs[0].InvocationID != "ccc" || runs[0].State != StateRunning || runs[0].ExitStatus != -1 || !runs[0].End.IsZero() {
		t.Fatalf("unexpected newest run: %+v", runs[0])
	}
	if runs[0].Start != base.Add(10*time.Minute) {
		t.Fatalf("unexpected run start: %v", runs[0].Start)
	}
	if runs[1].InvocationID != "bbb" || runs[1].State != StateFailed || runs[1].ExitStatus != 7 {
		t.Fatalf("unexpected failed run: %+v", runs[1])
	}
	if runs[2].InvocationID != "aaa" || runs[2].State != StateSucceeded || runs[2].ExitStatus != 0 {
		t.Fatalf("unexpected succeeded run: %+v", runs[2])
	}
	if !runs[2].End.Equal(base.Add(2 * time.Second)) {
		t.Fatalf("unexpected end: %v", runs[2].End)
	}
}

func TestRunsRootfulUsesInvocationID(t *testing.T) {
	base := time.Unix(1700000000, 0).UTC()
	var calls [][]string
	src := &Source{
		Rootful: true,
		Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			calls = append(calls, append([]string{name}, args...))
			// USER_INVOCATION_ID is the rootless field and must be ignored.
			return []byte(strings.Join([]string{
				journalEntry("USER_INVOCATION_ID", "ignored", MessageIDStarted, base, ""),
				journalEntry("INVOCATION_ID", "rootful", MessageIDStarted, base.Add(time.Minute), ""),
				journalEntry("INVOCATION_ID", "rootful", MessageIDSucceeded, base.Add(time.Minute+time.Second), ""),
			}, "\n")), nil
		},
	}
	runs, err := src.Runs(context.Background(), "default", "cron")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].InvocationID != "rootful" || runs[0].State != StateSucceeded {
		t.Fatalf("unexpected runs: %+v", runs)
	}
	if len(calls) != 2 {
		t.Fatalf("journalctl calls = %d, want one for the new and one for the legacy unit", len(calls))
	}
	for _, call := range calls {
		for _, a := range call {
			if a == "--user" {
				t.Fatalf("rootful journalctl must not get --user: %v", call)
			}
		}
	}
	joined := strings.Join(calls[0], " ")
	for _, want := range []string{"-u default_cron__cron.service", "-o json", "-n 400",
		"MESSAGE_ID=" + MessageIDStarted, "MESSAGE_ID=" + MessageIDExited,
		"MESSAGE_ID=" + MessageIDSucceeded, "MESSAGE_ID=" + MessageIDFailed, "MESSAGE_ID=" + MessageIDResources} {
		if !strings.Contains(joined, want) {
			t.Fatalf("journalctl args %q missing %q", joined, want)
		}
	}
	if !strings.Contains(strings.Join(calls[1], " "), "-u default-cron-cron.service") {
		t.Fatalf("legacy journalctl call = %v, want the legacy unit name", calls[1])
	}
}

func TestRunsSucceededWithoutExitStatus(t *testing.T) {
	base := time.Unix(1700000000, 0).UTC()
	src := fakeSource(false,
		journalEntry("USER_INVOCATION_ID", "aaa", MessageIDStarted, base, ""),
		journalEntry("USER_INVOCATION_ID", "aaa", MessageIDSucceeded, base.Add(time.Second), ""),
	)
	runs, err := src.Runs(context.Background(), "default", "cron")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ExitStatus != 0 {
		t.Fatalf("a succeeded run exited 0, got %+v", runs)
	}
}

// What systemd 257 really logs for a successful oneshot run: started, then
// only the "Consumed ... CPU time" resources message (no success message, no
// zero exit message at info level).
func TestRunsResourcesMessageEndsRun(t *testing.T) {
	base := time.Unix(1700000000, 0).UTC()
	src := fakeSource(false,
		journalEntry("USER_INVOCATION_ID", "aaa", MessageIDStarted, base, ""),
		journalEntry("USER_INVOCATION_ID", "aaa", MessageIDResources, base.Add(time.Second), ""),
	)
	runs, err := src.Runs(context.Background(), "default", "cron")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].State != StateSucceeded || runs[0].ExitStatus != 0 || !runs[0].End.Equal(base.Add(time.Second)) {
		t.Fatalf("unexpected runs: %+v", runs)
	}
}

// A failed run logs exit status, failed and then resources; the resources
// message must not turn it into a success.
func TestRunsFailedThenResources(t *testing.T) {
	base := time.Unix(1700000000, 0).UTC()
	id := "USER_INVOCATION_ID"
	src := fakeSource(false,
		journalEntry(id, "aaa", MessageIDStarted, base, ""),
		journalEntry(id, "aaa", MessageIDExited, base.Add(time.Second), `,"EXIT_STATUS":"3"`),
		journalEntry(id, "aaa", MessageIDFailed, base.Add(2*time.Second), ""),
		journalEntry(id, "aaa", MessageIDResources, base.Add(3*time.Second), ""),
	)
	runs, _ := src.Runs(context.Background(), "default", "cron")
	if len(runs) != 1 || runs[0].State != StateFailed || runs[0].ExitStatus != 3 {
		t.Fatalf("unexpected runs: %+v", runs)
	}
}

// An older run with no end message has ended once a newer one started:
// systemd never runs two invocations of one unit at once.
func TestRunsOlderRunWithoutEndIsFinished(t *testing.T) {
	base := time.Unix(1700000000, 0).UTC()
	id := "USER_INVOCATION_ID"
	src := fakeSource(false,
		journalEntry(id, "old", MessageIDStarted, base, ""),
		journalEntry(id, "new", MessageIDStarted, base.Add(time.Minute), ""),
	)
	runs, _ := src.Runs(context.Background(), "default", "cron")
	if len(runs) != 2 || runs[0].State != StateRunning || runs[1].State != StateSucceeded || !runs[1].End.Equal(base.Add(time.Minute)) {
		t.Fatalf("unexpected runs: %+v", runs)
	}
}

func TestRunsNoOutput(t *testing.T) {
	src := fakeSource(false)
	runs, err := src.Runs(context.Background(), "default", "cron")
	if err != nil || len(runs) != 0 {
		t.Fatalf("expected no runs without error, got %v, %v", runs, err)
	}
}

func TestLogArgs(t *testing.T) {
	src := &Source{Rootful: false}
	tail := int64(25)
	since := time.Unix(1700000123, 0)
	args := src.LogArgs("default", "cron", "abc123", LogOptions{TailLines: &tail, Since: since, Follow: true})
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--user",
		"-u default_cron__cron.service",
		"CONTAINER_NAME=default_cron__cron",
		"_SYSTEMD_INVOCATION_ID=abc123",
		"-o json",
		"-n 25",
		"--since @1700000123",
		"-f",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("LogArgs %q missing %q", joined, want)
		}
	}

	rootful := (&Source{Rootful: true}).LogArgs("default", "cron", "abc123", LogOptions{})
	for _, a := range rootful {
		if a == "--user" {
			t.Fatalf("rootful LogArgs must not get --user: %v", rootful)
		}
	}
}

func TestNames(t *testing.T) {
	if got := UnitName("default", "cron"); got != "default_cron__cron.service" {
		t.Fatalf("UnitName = %q", got)
	}
	if got := ContainerName("default", "cron"); got != "default_cron__cron" {
		t.Fatalf("ContainerName = %q", got)
	}
}

// RunsFallsBackToLegacyUnit: runs recorded before the naming upgrade live
// under the legacy unit name. A CronJob with no new-format history must still
// show them.
func TestRunsFallsBackToLegacyUnit(t *testing.T) {
	base := time.Unix(1700000000, 0).UTC()
	legacyUnit := LegacyUnitName("default", "cron")
	if legacyUnit == UnitName("default", "cron") {
		t.Fatal("test needs distinct new and legacy unit names")
	}
	var seen []string
	src := &Source{
		Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			unit := ""
			for i, a := range args {
				if a == "-u" && i+1 < len(args) {
					unit = args[i+1]
				}
			}
			seen = append(seen, unit)
			if unit != legacyUnit {
				return nil, nil
			}
			return []byte(strings.Join([]string{
				journalEntry("USER_INVOCATION_ID", "legacy", MessageIDStarted, base, ""),
				journalEntry("USER_INVOCATION_ID", "legacy", MessageIDSucceeded, base.Add(time.Second), ""),
			}, "\n")), nil
		},
	}
	runs, err := src.Runs(context.Background(), "default", "cron")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].InvocationID != "legacy" || runs[0].Unit != legacyUnit {
		t.Fatalf("legacy runs = %+v, want one run from %s", runs, legacyUnit)
	}
	if len(seen) != 2 || seen[0] != UnitName("default", "cron") || seen[1] != legacyUnit {
		t.Fatalf("journal units queried = %v, want new then legacy", seen)
	}
}

// RunsForJobFallsBackToLegacyUnit is the Job half of the same fallback.
func TestRunsForJobFallsBackToLegacyUnit(t *testing.T) {
	base := time.Unix(1700000000, 0).UTC()
	legacyUnit := LegacyJobUnitName("default", "batch")
	src := &Source{
		Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			unit := ""
			for i, a := range args {
				if a == "-u" && i+1 < len(args) {
					unit = args[i+1]
				}
			}
			if unit != legacyUnit {
				return nil, nil
			}
			return []byte(strings.Join([]string{
				journalEntry("USER_INVOCATION_ID", "legacy", MessageIDStarted, base, ""),
				journalEntry("USER_INVOCATION_ID", "legacy", MessageIDFailed, base.Add(time.Second), ""),
			}, "\n")), nil
		},
	}
	runs, err := src.RunsForJob(context.Background(), "default", "batch")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].InvocationID != "legacy" || runs[0].Unit != legacyUnit {
		t.Fatalf("legacy Job runs = %+v, want one run from %s", runs, legacyUnit)
	}
}
