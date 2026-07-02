package podman

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"time"
)

// Event mirrors the JSON emitted by `podman events --format json`. Only the
// fields q8s actually needs are modeled — podman's schema varies by Type
// (container/image/network/volume/pod), and Attributes in particular is
// shaped differently (or nil) depending on Type.
type Event struct {
	ID         string            `json:"ID"`
	Image      string            `json:"Image"`
	Name       string            `json:"Name"`
	Status     string            `json:"Status"`
	Type       string            `json:"Type"`
	Time       int64             `json:"time"`
	Attributes map[string]string `json:"Attributes"`
}

func (e Event) PodName() string       { return e.Attributes["io.kubernetes.pod.name"] }
func (e Event) PodNamespace() string  { return e.Attributes["io.kubernetes.pod.namespace"] }
func (e Event) PodDeployment() string { return e.Attributes["io.kubernetes.pod.deployment"] }

// scanEvents reads newline-delimited JSON events from r and invokes fn for
// each one that parses successfully. Lines that don't parse as an Event are
// skipped rather than aborting the whole stream — a single unexpected event
// shape shouldn't take down reconciliation.
func scanEvents(r io.Reader, fn func(Event)) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		var ev Event
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue
		}
		fn(ev)
	}
}

// watchOnce runs `podman events --format json` once and streams parsed
// events to fn until the process exits or ctx is cancelled.
func watchOnce(ctx context.Context, fn func(Event)) error {
	cmd := exec.CommandContext(ctx, "podman", "events", "--format", "json")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("podman events: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("podman events: %w", err)
	}
	scanEvents(stdout, fn)
	return cmd.Wait()
}

// WatchEvents subscribes to the live podman event stream and calls fn for
// every event, reconnecting with exponential backoff (capped at 30s) if the
// podman events process exits — e.g. the podman socket restarting. Blocks
// until ctx is cancelled, so callers should run it in its own goroutine.
func WatchEvents(ctx context.Context, fn func(Event)) {
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		if err := watchOnce(ctx, fn); err != nil && ctx.Err() == nil {
			fmt.Printf("podman events: %v\n", err)
		}
		if time.Since(started) > 30*time.Second {
			backoff = time.Second
		} else if backoff < 30*time.Second {
			backoff *= 2
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
	}
}
