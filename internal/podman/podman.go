package podman

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// ContainerInfo holds the fields we care about from `podman ps --format json`.
type ContainerInfo struct {
	Names     []string          `json:"Names"`
	State     string            `json:"State"`
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

func (c ContainerInfo) PodName() string      { return c.Labels["io.kubernetes.pod.name"] }
func (c ContainerInfo) PodNamespace() string { return c.Labels["io.kubernetes.pod.namespace"] }

// List returns all containers (running and stopped).
func List() ([]ContainerInfo, error) {
	out, err := exec.Command("podman", "ps", "--all", "--format", "json").Output()
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
	out, err := exec.Command("podman", "inspect", "--format", "json", name).Output()
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
