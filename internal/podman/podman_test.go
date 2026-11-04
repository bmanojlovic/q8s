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
