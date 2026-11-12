package systemd

import (
	"strings"
	"testing"
)

func TestUnitNameFromPath(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{
			name: "plain unit",
			path: "/org/freedesktop/systemd1/unit/demo_2eservice",
			want: "demo.service",
		},
		{
			name: "dashes are escaped",
			path: "/org/freedesktop/systemd1/unit/demo_2dapp_2eservice",
			want: "demo-app.service",
		},
		{
			name: "cronjob unit",
			path: "/org/freedesktop/systemd1/unit/default_2ddemo_2dcron_2eservice",
			want: "default-demo-cron.service",
		},
		{
			name: "podman pod unit",
			path: "/org/freedesktop/systemd1/unit/demo_2dmulti_2dpod_2eservice",
			want: "demo-multi-pod.service",
		},
		{
			name: "underscore is escaped too",
			path: "/org/freedesktop/systemd1/unit/a_5fb_2eservice",
			want: "a_b.service",
		},
		{
			name: "timer units unescape the same way",
			path: "/org/freedesktop/systemd1/unit/demo_2dapp_2etimer",
			want: "demo-app.timer",
		},
		{
			name: "empty unit name",
			path: "/org/freedesktop/systemd1/unit/",
			want: "",
		},
		{
			name: "not a unit path",
			path: "/org/freedesktop/systemd1",
			want: "",
		},
		{
			name: "nested path is not a unit object",
			path: "/org/freedesktop/systemd1/unit/demo_2eservice/extra",
			want: "",
		},
		{
			name: "truncated escape",
			path: "/org/freedesktop/systemd1/unit/demo_2",
			want: "",
		},
		{
			name: "escape is not hex",
			path: "/org/freedesktop/systemd1/unit/demo_zzservice",
			want: "",
		},
		{
			name: "empty path",
			path: "",
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := unitNameFromPath(tc.path); got != tc.want {
				t.Errorf("unitNameFromPath(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestUnitMatchRuleIsNarrow(t *testing.T) {
	// One rule, five keys: any extra key would make the bus deliver more
	// than unit property changes to q8s.
	want := "type='signal'" +
		",interface='org.freedesktop.DBus.Properties'" +
		",member='PropertiesChanged'" +
		",path_namespace='/org/freedesktop/systemd1/unit'" +
		",arg0='org.freedesktop.systemd1.Unit'"
	if got := unitMatchRule(); got != want {
		t.Errorf("unitMatchRule() = %q, want %q", got, want)
	}
	if got := strings.Count(unitMatchRule(), "="); got != 5 {
		t.Errorf("unitMatchRule() has %d keys, want 5", got)
	}
}

func TestUnitMatchOptionsMatchTheRule(t *testing.T) {
	// The option constructors are what the bus call uses; the rendered rule
	// is what tests pin down. Both must describe the same subscription.
	opts := unitMatchOptions()
	if len(opts) != 4 {
		t.Fatalf("unitMatchOptions() returned %d options, want 4 (type is added by AddMatchSignal)", len(opts))
	}
	rule := unitMatchRule()
	for _, want := range []string{
		"interface='" + propertiesInterface + "'",
		"member='" + propertiesChanged + "'",
		"path_namespace='" + unitPathNamespace + "'",
		"arg0='" + unitInterface + "'",
	} {
		if !strings.Contains(rule, want) {
			t.Errorf("unitMatchRule() %q does not contain %q", rule, want)
		}
	}
}

func TestUserBusPath(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	got, err := userBusPath()
	if err != nil {
		t.Fatalf("userBusPath: %v", err)
	}
	if want := "/run/user/1000/bus"; got != want {
		t.Errorf("userBusPath() = %q, want %q", got, want)
	}

	t.Setenv("XDG_RUNTIME_DIR", "")
	if _, err := userBusPath(); err == nil {
		t.Error("userBusPath() with no XDG_RUNTIME_DIR: expected an error")
	}
}
