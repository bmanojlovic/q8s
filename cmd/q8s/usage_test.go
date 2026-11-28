package main

import (
	"strings"
	"testing"
)

func TestEveryCommandHasUsage(t *testing.T) {
	seen := make(map[string]bool, len(commands))
	for _, c := range commands {
		if c.name == "" {
			t.Fatal("command with an empty name")
		}
		if seen[c.name] {
			t.Fatalf("duplicate command %q", c.name)
		}
		seen[c.name] = true

		usage, ok := usageFor(c.name)
		if !ok {
			t.Fatalf("command %q has no usage entry", c.name)
		}
		if strings.TrimSpace(usage) == "" {
			t.Fatalf("command %q has an empty usage entry", c.name)
		}
		if usage != c.usage {
			t.Errorf("command %q: usageFor returned a different text than the table", c.name)
		}
		if !strings.HasPrefix(usage, "Usage: q8s "+c.name) {
			t.Errorf("command %q: usage does not start with its synopsis:\n%s", c.name, usage)
		}
		if c.run == nil {
			t.Errorf("command %q has no run function", c.name)
		}
	}

	// The dispatcher is the table itself; this list makes an accidental
	// removal of a command visible as well.
	for _, name := range []string{
		"serve", "install", "uninstall", "status", "doctor", "migrate-names",
		"start", "stop", "enable", "disable", "kubeconfig", "credential",
	} {
		if !seen[name] {
			t.Errorf("command %q is missing from the command table", name)
		}
	}
}

func TestUsageForUnknownCommand(t *testing.T) {
	if usage, ok := usageFor("no-such-command"); ok || usage != "" {
		t.Fatalf("usageFor(unknown) = %q, %v; want empty, false", usage, ok)
	}
}

func TestHelpRequested(t *testing.T) {
	tests := []struct {
		args []string
		want bool
	}{
		{nil, false},
		{[]string{}, false},
		{[]string{"-h"}, true},
		{[]string{"--help"}, true},
		{[]string{"--name", "demo", "--help"}, true},
		{[]string{"--purge", "-h"}, true},
		{[]string{"--help=true"}, false},
		{[]string{"-help"}, false},
		{[]string{"--json"}, false},
	}
	for _, tc := range tests {
		if got := helpRequested(tc.args); got != tc.want {
			t.Errorf("helpRequested(%v) = %v, want %v", tc.args, got, tc.want)
		}
	}
}
