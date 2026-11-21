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
	write("default_duo.pod", owned("duo"))
	// Stale member of pod duo. The new separator makes its filename
	// default_duo_web.container, next to default_duo-web.container below.
	write("default_duo_web.container", owned("duo"))
	// Another pod named duo-web. Its filename is close to the stale one, so
	// only the exact label can save it.
	write("default_duo-web.container", owned("duo-web"))
	// Same prefix and a pod name that contains "duo" as a substring.
	write("default_duo-0.container", owned("duo-0"))
	// Hand-written file with the same io.kubernetes labels but no marker.
	handWritten := "[Container]\nImage=nginx\n" +
		"Label=io.kubernetes.pod.namespace=default\n" +
		"Label=io.kubernetes.pod.name=duo\n"
	write("maya.container", handWritten)

	removed := srv.removeStaleOwnedUnits(dir, "default", "duo", map[string]bool{"default_duo.pod": true})
	if !removed {
		t.Fatal("expected the stale owned member to be removed")
	}
	if exists("default_duo_web.container") {
		t.Error("stale owned member of pod duo was not removed")
	}
	for _, name := range []string{"default_duo.pod", "default_duo-web.container", "default_duo-0.container", "maya.container"} {
		if !exists(name) {
			t.Errorf("%s should have survived cleanup", name)
		}
	}
	if got, err := os.ReadFile(filepath.Join(dir, "maya.container")); err != nil || string(got) != handWritten {
		t.Errorf("hand-written file was modified: %v\n%s", err, got)
	}
}
