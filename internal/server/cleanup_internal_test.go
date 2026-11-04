package server

import (
	"os"
	"path/filepath"
	"testing"

	"q8s/internal/quadlet"
)

// TestRemoveStaleOwnedUnitsExactLabels pins the ownership rule: only files
// carrying the managed-by marker AND the exact namespace and pod-name label
// lines are candidates. Filename prefixes and label substrings are not enough.
func TestRemoveStaleOwnedUnitsExactLabels(t *testing.T) {
	dir := t.TempDir()
	srv := &Server{config: Config{QuadletDir: dir}}

	owned := func(podName string) string {
		return "[Container]\n" + quadlet.ManagedByLabel + "\n" +
			"Label=io.kubernetes.pod.namespace=default\n" +
			"Label=io.kubernetes.pod.name=" + podName + "\n"
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	exists := func(name string) bool {
		t.Helper()
		_, err := os.Stat(filepath.Join(dir, name))
		return err == nil
	}

	// Kept by the caller, even though it belongs to this pod.
	write("default-duo.pod", owned("duo"))
	// Stale member of pod duo: shares the default-duo-web filename shape.
	write("default-duo-web.container", owned("duo"))
	// Another pod whose container is also called web. Its filename has the
	// same prefix, so only the exact label can save it.
	write("default-duo-web-other.container", owned("duo-web"))
	// Same prefix and a pod name that contains "duo" as a substring.
	write("default-duo-0.container", owned("duo-0"))
	// Hand-written file with the same io.kubernetes labels but no marker.
	handWritten := "[Container]\nImage=nginx\n" +
		"Label=io.kubernetes.pod.namespace=default\n" +
		"Label=io.kubernetes.pod.name=duo\n"
	write("maya.container", handWritten)

	removed := srv.removeStaleOwnedUnits(dir, "default", "duo", map[string]bool{"default-duo.pod": true})
	if !removed {
		t.Fatal("expected the stale owned member to be removed")
	}
	if exists("default-duo-web.container") {
		t.Error("stale owned member of pod duo was not removed")
	}
	for _, name := range []string{"default-duo.pod", "default-duo-web-other.container", "default-duo-0.container", "maya.container"} {
		if !exists(name) {
			t.Errorf("%s should have survived cleanup", name)
		}
	}
	if got, err := os.ReadFile(filepath.Join(dir, "maya.container")); err != nil || string(got) != handWritten {
		t.Errorf("hand-written file was modified: %v\n%s", err, got)
	}
}
