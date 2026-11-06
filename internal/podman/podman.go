package podman

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"
)

// cmdTimeout bounds one-shot podman invocations. A wedged podman (locked
// store, stalled varlink) must not freeze the reconcile loop that calls
// these — the 30s resync ticker cannot fire while a child hangs.
const cmdTimeout = 10 * time.Second

// ContainerInfo holds the fields we care about from `podman ps --format json`.
type ContainerInfo struct {
	Names     []string          `json:"Names"`
	State     string            `json:"State"`
	Status    string            `json:"Status"`
	ExitCode  int               `json:"ExitCode"`
	Image     string            `json:"Image"`
	StartedAt int64             `json:"StartedAt"`
	Labels    map[string]string `json:"Labels"`
}

func (c ContainerInfo) Name() string {
	if len(c.Names) == 0 {
		return ""
	}
	return strings.TrimPrefix(c.Names[0], "/")
}

// Health returns the container health reported by Podman, parsed from the
// parenthesised suffix of Status: "Up 2 minutes (starting)", "Up 5 minutes
// (healthy)", "Up 1 minute (unhealthy)". It returns "" when Status carries no
// health suffix, i.e. the container has no health check or the state is
// unknown.
func (c ContainerInfo) Health() string {
	s := c.Status
	if !strings.HasSuffix(s, ")") {
		return ""
	}
	open := strings.LastIndex(s, "(")
	if open < 0 {
		return ""
	}
	switch h := s[open+1 : len(s)-1]; h {
	case "healthy", "unhealthy", "starting":
		return h
	}
	return ""
}

func (c ContainerInfo) PodName() string       { return c.Labels["io.kubernetes.pod.name"] }
func (c ContainerInfo) PodNamespace() string  { return c.Labels["io.kubernetes.pod.namespace"] }
func (c ContainerInfo) PodDeployment() string { return c.Labels["io.kubernetes.pod.deployment"] }

// ContainerName returns the Kubernetes container name q8s writes on a
// multi-container pod member. It is empty for single-container pods, which
// keep the historical pod-only labels.
func (c ContainerInfo) ContainerName() string {
	return c.Labels["io.kubernetes.container.name"]
}

// List returns all containers (running and stopped).
func List() ([]ContainerInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cmdTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "podman", "ps", "--all", "--format", "json").Output()
	if err != nil {
		return nil, fmt.Errorf("podman ps: %w", err)
	}
	var containers []ContainerInfo
	if err := json.Unmarshal(out, &containers); err != nil {
		return nil, fmt.Errorf("podman ps parse: %w", err)
	}
	return containers, nil
}

// ContainerState holds the minimal state from `podman inspect`.
type ContainerState struct {
	State    string `json:"Status"`
	ExitCode int    `json:"ExitCode"`
}

// InspectState returns the current state of a single container by name.
// Returns an error if the container does not exist.
func InspectState(name string) (*ContainerState, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cmdTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "podman", "inspect", "--format", "json", name).Output()
	if err != nil {
		return nil, fmt.Errorf("podman inspect %s: %w", name, err)
	}
	// inspect returns a JSON array with one element
	var items []struct {
		State struct {
			Status   string `json:"Status"`
			ExitCode int    `json:"ExitCode"`
		} `json:"State"`
	}
	if err := json.Unmarshal(out, &items); err != nil || len(items) == 0 {
		return nil, fmt.Errorf("podman inspect %s: parse error", name)
	}
	return &ContainerState{
		State:    items[0].State.Status,
		ExitCode: items[0].State.ExitCode,
	}, nil
}

// ListOwned returns only containers that carry q8s pod labels.
func ListOwned() ([]ContainerInfo, error) {
	all, err := List()
	if err != nil {
		return nil, err
	}
	var owned []ContainerInfo
	for _, c := range all {
		if c.PodName() != "" && c.PodNamespace() != "" {
			owned = append(owned, c)
		}
	}
	return owned, nil
}

// NetworkSubnet returns the first IPv4 subnet of a podman network.
// Callers derive a fixed address from it, so a network without an IPv4
// subnet is an error rather than an empty string.
func NetworkSubnet(name string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cmdTimeout)
	defer cancel()
	// stdout only: podman logs warnings to stderr (e.g. level=error lines
	// about runtimes missing from containers.conf) even when it succeeds,
	// and those would corrupt the JSON.
	out, err := exec.CommandContext(ctx, "podman", "network", "inspect", "--format", "json", name).Output()
	if err != nil {
		var stderr []byte
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = ee.Stderr
		}
		return "", fmt.Errorf("podman network inspect %s: %w: %s", name, err, strings.TrimSpace(string(stderr)))
	}
	subnet, err := parseNetworkSubnet(out)
	if err != nil {
		return "", fmt.Errorf("podman network inspect %s: %w", name, err)
	}
	return subnet, nil
}

// networkInspect is the subset of `podman network inspect --format json`
// q8s needs.
type networkInspect struct {
	Subnets []struct {
		Subnet string `json:"subnet"`
	} `json:"subnets"`
}

// parseNetworkSubnet extracts the first IPv4 subnet from network inspect
// output. Podman may list an IPv6 subnet first, so entries are inspected
// rather than taking subnets[0].
func parseNetworkSubnet(out []byte) (string, error) {
	var networks []networkInspect
	if err := json.Unmarshal(out, &networks); err != nil {
		return "", fmt.Errorf("parse network inspect: %w", err)
	}
	for _, network := range networks {
		for _, s := range network.Subnets {
			ip, _, err := net.ParseCIDR(s.Subnet)
			if err != nil || ip.To4() == nil {
				continue
			}
			return s.Subnet, nil
		}
	}
	return "", fmt.Errorf("network has no IPv4 subnet")
}

// NetworkConnect attaches a container to a network, optionally with a fixed
// IPv4 address. Connecting twice is the desired end state, not an error, so
// the idempotent reconcile path can retry freely.
func NetworkConnect(network, container, ip string) error {
	args := []string{"network", "connect"}
	if ip != "" {
		args = append(args, "--ip", ip)
	}
	args = append(args, network, container)
	ctx, cancel := context.WithTimeout(context.Background(), cmdTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "podman", args...).CombinedOutput()
	if err != nil {
		if isAlreadyConnected(string(out)) {
			return nil
		}
		return fmt.Errorf("podman network connect %s %s: %w: %s", network, container, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// NetworkDisconnect detaches a container from a network. A container that is
// already absent from the network is the desired end state, so "not
// connected" and "no such ..." count as success.
func NetworkDisconnect(network, container string) error {
	ctx, cancel := context.WithTimeout(context.Background(), cmdTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "podman", "network", "disconnect", network, container).CombinedOutput()
	if err != nil {
		if isAlreadyDisconnected(string(out)) {
			return nil
		}
		return fmt.Errorf("podman network disconnect %s %s: %w: %s", network, container, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// isAlreadyConnected matches podman's wording for a redundant connect.
func isAlreadyConnected(output string) bool {
	return strings.Contains(strings.ToLower(output), "already connected")
}

// isAlreadyDisconnected matches podman's wording for a redundant disconnect.
func isAlreadyDisconnected(output string) bool {
	lower := strings.ToLower(output)
	return strings.Contains(lower, "not connected") || strings.Contains(lower, "no such")
}
