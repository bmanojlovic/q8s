// Package cronruns derives the runs of a q8s CronJob from systemd and the
// journal. A q8s CronJob is a systemd timer that starts a one-shot
// <ns>-<cronjob>-cron.service; the container is removed after every run and
// no Job or Pod object is stored. Everything needed to reconstruct the run
// history is already in systemd's journal, so this package reads it on demand
// and keeps no state.
package cronruns

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Journal MESSAGE_IDs of the manager messages that delimit a unit run.
const (
	MessageIDStarted   = "39f53479d3a045ac8e11786248231fbf"
	MessageIDExited    = "98e322203f7a4ed290d09fe03c09fe15"
	MessageIDSucceeded = "7ad2d189f7e94e70a38c781354912448"
	MessageIDFailed    = "d9b373ed55a64feb8242e02dbe79a49c"
	// MessageIDResources ("Consumed ... CPU time") is logged at info level
	// every time a unit stops. systemd does not log MessageIDSucceeded or a
	// zero MessageIDExited at info level, so this is the end marker of a
	// successful run.
	MessageIDResources = "ae8f7b866b0347b9af31fe1c80b127c0"
)

// Run states, as returned by Source.Runs.
const (
	StateRunning   = "running"
	StateSucceeded = "succeeded"
	StateFailed    = "failed"
)

// commandTimeout bounds one journalctl call. Runs are read while a client
// waits, so a hung command must not hold the request forever.
const commandTimeout = 5 * time.Second

// Run is one invocation of a CronJob's unit.
type Run struct {
	InvocationID string
	Start        time.Time // timestamp of the "started" message
	End          time.Time // zero while running
	ExitStatus   int       // from EXIT_STATUS, -1 if unknown
	State        string    // "running" | "succeeded" | "failed"
}

// LogOptions are the journalctl selections for a logs request.
type LogOptions struct {
	TailLines *int64
	Since     time.Time // zero means no --since
	Follow    bool
}

// Source reads CronJob runs from the journal. Run is the command runner and
// tests inject a fake; Stream is the optional long-lived variant used by
// follow requests (nil means follow falls back to a single Run call).
type Source struct {
	Rootful bool
	// Run executes a command and returns stdout (stdout is returned even on
	// a non-zero exit, like exec.Cmd.Output).
	Run func(ctx context.Context, name string, args ...string) ([]byte, error)
	// Stream starts a long-lived command and returns its stdout together
	// with a wait function.
	Stream func(ctx context.Context, name string, args ...string) (io.ReadCloser, func() error, error)
	Now    func() time.Time
}

// UnitName is the systemd service unit of a CronJob.
func UnitName(namespace, cronJob string) string {
	return fmt.Sprintf("%s-%s-cron.service", namespace, cronJob)
}

// ContainerName is the Podman container name of a CronJob run.
func ContainerName(namespace, cronJob string) string {
	return fmt.Sprintf("%s-%s-cron", namespace, cronJob)
}

// JobUnitName is the systemd service unit of a Job.
func JobUnitName(namespace, job string) string {
	return fmt.Sprintf("%s-%s-job.service", namespace, job)
}

// JobContainerName is the Podman container name of a Job.
func JobContainerName(namespace, job string) string {
	return fmt.Sprintf("%s-%s-job", namespace, job)
}

func (s *Source) command(ctx context.Context, args ...string) ([]byte, error) {
	if s.Run == nil {
		return nil, errors.New("cronruns: no command runner configured")
	}
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	return s.Run(ctx, "journalctl", args...)
}

// journalArgs returns the journalctl selection for the manager messages of
// one CronJob unit.
func (s *Source) journalArgs(namespace, cronJob string) []string {
	return s.journalArgsForUnit(UnitName(namespace, cronJob))
}

// journalArgsForUnit returns the journalctl selection for the manager
// messages of one unit.
func (s *Source) journalArgsForUnit(unit string) []string {
	args := make([]string, 0, 12)
	if !s.Rootful {
		args = append(args, "--user")
	}
	args = append(args, "-u", unit, "-o", "json", "-n", "400",
		"MESSAGE_ID="+MessageIDStarted,
		"MESSAGE_ID="+MessageIDExited,
		"MESSAGE_ID="+MessageIDSucceeded,
		"MESSAGE_ID="+MessageIDFailed,
		"MESSAGE_ID="+MessageIDResources)
	return args
}

// LogArgs returns the journalctl arguments that select the output of one run.
func (s *Source) LogArgs(namespace, cronJob, invocationID string, opt LogOptions) []string {
	return s.LogArgsForUnit(UnitName(namespace, cronJob), ContainerName(namespace, cronJob), invocationID, opt)
}

// LogArgsForUnit returns the journalctl arguments that select the output of
// one unit run. Rootful journals use INVOCATION_ID, rootless user journals the
// same value under USER_INVOCATION_ID; container output always carries
// _SYSTEMD_INVOCATION_ID, so that is the field to match here. An empty
// invocationID selects every run of the unit.
func (s *Source) LogArgsForUnit(unit, container, invocationID string, opt LogOptions) []string {
	args := make([]string, 0, 16)
	if !s.Rootful {
		args = append(args, "--user")
	}
	args = append(args, "-u", unit)
	if container != "" {
		args = append(args, "CONTAINER_NAME="+container)
	}
	if invocationID != "" {
		args = append(args, "_SYSTEMD_INVOCATION_ID="+invocationID)
	}
	args = append(args, "-o", "json")
	if opt.TailLines != nil {
		args = append(args, "-n", strconv.FormatInt(*opt.TailLines, 10))
	}
	if !opt.Since.IsZero() {
		args = append(args, "--since", "@"+strconv.FormatInt(opt.Since.Unix(), 10))
	}
	if opt.Follow {
		args = append(args, "-f")
	}
	return args
}

// Runs returns the run history of one CronJob, newest first. History limits
// are a server concern and are not applied here.
func (s *Source) Runs(ctx context.Context, namespace, cronJob string) ([]Run, error) {
	return s.RunsForUnit(ctx, UnitName(namespace, cronJob))
}

// RunsForUnit returns the run history of one unit, newest first. History
// limits are a server concern and are not applied here.
func (s *Source) RunsForUnit(ctx context.Context, unit string) ([]Run, error) {
	out, err := s.command(ctx, s.journalArgsForUnit(unit)...)
	if len(out) == 0 {
		if err != nil {
			return nil, fmt.Errorf("journalctl: %w", err)
		}
		return nil, nil
	}
	runs, _ := parseRuns(out, s.Rootful)
	return runs, nil
}

type journalRun struct {
	Run
	sawStart bool
	sawEnd   bool
	failed   bool
}

func parseRuns(out []byte, rootful bool) ([]Run, error) {
	idField := "USER_INVOCATION_ID"
	if rootful {
		idField = "INVOCATION_ID"
	}
	byID := make(map[string]*journalRun)
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var entry map[string]json.RawMessage
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}
		id := fieldString(entry, idField)
		if id == "" {
			continue
		}
		ts, ok := fieldTime(entry, "__REALTIME_TIMESTAMP")
		if !ok {
			continue
		}
		messageID := fieldString(entry, "MESSAGE_ID")
		if messageID == "" {
			continue
		}
		r := byID[id]
		if r == nil {
			r = &journalRun{Run: Run{InvocationID: id, ExitStatus: -1}}
			byID[id] = r
		}
		switch messageID {
		case MessageIDStarted:
			r.sawStart = true
			if r.Start.IsZero() || ts.Before(r.Start) {
				r.Start = ts
			}
		case MessageIDExited:
			if n, err := strconv.Atoi(fieldString(entry, "EXIT_STATUS")); err == nil {
				r.ExitStatus = n
			}
		case MessageIDSucceeded:
			if !r.sawEnd || !ts.Before(r.End) {
				r.End = ts
				r.State = StateSucceeded
				r.sawEnd = true
			}
		case MessageIDFailed:
			if !r.sawEnd || !ts.Before(r.End) {
				r.End = ts
				r.sawEnd = true
			}
			r.failed = true
		case MessageIDResources:
			if !r.sawEnd || !ts.Before(r.End) {
				r.End = ts
				r.sawEnd = true
			}
		}
	}

	runs := make([]Run, 0, len(byID))
	for _, r := range byID {
		if !r.sawStart && !r.sawEnd {
			// A lone "main process exited" message without either boundary
			// does not describe a run that can be shown.
			continue
		}
		if r.Start.IsZero() {
			// The started message fell outside -n 400; the end is the best
			// timestamp available.
			r.Start = r.End
		}
		runs = append(runs, r.Run)
		last := &runs[len(runs)-1]
		switch {
		case r.failed || r.ExitStatus > 0:
			last.State = StateFailed
		case r.sawEnd:
			last.State = StateSucceeded
			if last.ExitStatus < 0 {
				last.ExitStatus = 0
			}
		default:
			last.State = StateRunning
		}
	}
	sort.SliceStable(runs, func(i, j int) bool {
		if runs[i].Start.Equal(runs[j].Start) {
			return runs[i].InvocationID > runs[j].InvocationID
		}
		return runs[i].Start.After(runs[j].Start)
	})
	// systemd never runs two invocations of one unit at once, so every run
	// older than the newest has ended even when its end message is missing
	// (rotated away, or a systemd that logs differently). Without a failure
	// message it counts as succeeded; its end is the next run's start.
	for i := 1; i < len(runs); i++ {
		if runs[i].State == StateRunning {
			runs[i].State = StateSucceeded
			runs[i].End = runs[i-1].Start
			if runs[i].ExitStatus < 0 {
				runs[i].ExitStatus = 0
			}
		}
	}
	return runs, nil
}

func fieldString(entry map[string]json.RawMessage, key string) string {
	raw, ok := entry[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return n.String()
	}
	return ""
}

func fieldTime(entry map[string]json.RawMessage, key string) (time.Time, bool) {
	us, err := strconv.ParseInt(fieldString(entry, key), 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.UnixMicro(us).UTC(), true
}

// JournalMessage returns the MESSAGE of a journal JSON entry. journald emits
// an array of bytes when the output is not valid UTF-8.
func JournalMessage(entry map[string]json.RawMessage) string {
	raw, ok := entry["MESSAGE"]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var b []byte
	if err := json.Unmarshal(raw, &b); err == nil {
		return string(b)
	}
	return strings.TrimSpace(string(raw))
}

// JournalTime returns __REALTIME_TIMESTAMP of a journal JSON entry.
func JournalTime(entry map[string]json.RawMessage) (time.Time, bool) {
	return fieldTime(entry, "__REALTIME_TIMESTAMP")
}
