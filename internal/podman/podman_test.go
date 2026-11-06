package podman

import "testing"

func TestContainerInfoHealth(t *testing.T) {
	tests := []struct {
		status string
		want   string
	}{
		{"Up 2 minutes (starting)", "starting"},
		{"Up 5 minutes (healthy)", "healthy"},
		{"Up 1 minute (unhealthy)", "unhealthy"},
		{"Exited (0) 3 minutes ago", ""},
		{"Created", ""},
	}
	for _, tt := range tests {
		c := ContainerInfo{Status: tt.status}
		if got := c.Health(); got != tt.want {
			t.Errorf("Health() for Status %q = %q, want %q", tt.status, got, tt.want)
		}
	}
}

func TestParseNetworkSubnet(t *testing.T) {
	// Shape captured from `podman network inspect --format json`. The IPv6
	// entry comes first on purpose: only the first IPv4 subnet is wanted.
	sample := []byte(`[
		{
			"name": "q8s-traefik",
			"id": "4d5e6f",
			"driver": "bridge",
			"network_interface": "podman3",
			"subnets": [
				{"subnet": "fd00:dead:beef::/64", "gateway": "fd00:dead:beef::1"},
				{"subnet": "10.89.3.0/24", "gateway": "10.89.3.1"}
			],
			"ipv6_enabled": true
		}
	]`)
	got, err := parseNetworkSubnet(sample)
	if err != nil {
		t.Fatalf("parseNetworkSubnet: %v", err)
	}
	if got != "10.89.3.0/24" {
		t.Fatalf("parseNetworkSubnet = %q, want 10.89.3.0/24", got)
	}
}

func TestParseNetworkSubnetErrors(t *testing.T) {
	if _, err := parseNetworkSubnet([]byte(`not json`)); err == nil {
		t.Fatal("expected malformed JSON to fail")
	}
	if _, err := parseNetworkSubnet([]byte(`[{"subnets":[{"subnet":"fd00::/64"}]}]`)); err == nil {
		t.Fatal("expected a network without IPv4 to fail")
	}
	if _, err := parseNetworkSubnet([]byte(`[]`)); err == nil {
		t.Fatal("expected an empty inspect result to fail")
	}
}

func TestNetworkErrorMatching(t *testing.T) {
	connected := []string{
		"Error: container abc is already connected to network q8s-default",
		"already connected",
	}
	for _, out := range connected {
		if !isAlreadyConnected(out) {
			t.Errorf("isAlreadyConnected(%q) = false", out)
		}
	}
	if isAlreadyConnected("Error: no such network") {
		t.Error("unrelated error must not count as already connected")
	}

	disconnected := []string{
		"Error: container abc is not connected to network q8s-default",
		"Error: no such network q8s-gone",
	}
	for _, out := range disconnected {
		if !isAlreadyDisconnected(out) {
			t.Errorf("isAlreadyDisconnected(%q) = false", out)
		}
	}
	if isAlreadyDisconnected("Error: permission denied") {
		t.Error("unrelated error must not count as already disconnected")
	}
}
