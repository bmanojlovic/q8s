package store_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"q8s/internal/store"
)

// newFileStore returns a store backed by <tempdir>/store.json.
func newFileStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "store.json")
	st, err := store.Load(file)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return st, file
}

// snapshotRVs returns the resourceVersions of the gzip snapshots next to
// store.json, sorted ascending.
func snapshotRVs(t *testing.T, file string) []int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(filepath.Dir(file), "snapshots"))
	if err != nil {
		t.Fatalf("read snapshots dir: %v", err)
	}
	var rvs []int
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "store.") || !strings.HasSuffix(name, ".json.gz") {
			continue
		}
		rv, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "store."), ".json.gz"))
		if err != nil {
			t.Fatalf("unexpected snapshot name %q: %v", name, err)
		}
		rvs = append(rvs, rv)
	}
	sort.Ints(rvs)
	return rvs
}

// breakDataDir replaces the data directory with a regular file so every
// following write fails with ENOTDIR, regardless of whether the test runs as
// root (root ignores permission bits, but not a file where a directory is
// expected).
func breakDataDir(t *testing.T, file string) {
	t.Helper()
	dir := filepath.Dir(file)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("remove data dir: %v", err)
	}
	if err := os.WriteFile(dir, []byte("not a directory"), 0600); err != nil {
		t.Fatalf("replace data dir with file: %v", err)
	}
}

func TestPersistFailureRollsBackCreateUpdateDelete(t *testing.T) {
	st, file := newFileStore(t)

	if _, err := st.CreatePod(makePod("default", "keep", "nginx:1")); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	rvBefore := st.ResourceVersion()

	breakDataDir(t, file)

	// Create: the error wraps ErrPersist and the object is not stored.
	_, err := st.CreatePod(makePod("default", "new", "nginx:2"))
	if !errors.Is(err, store.ErrPersist) {
		t.Fatalf("CreatePod error = %v, want one wrapping ErrPersist", err)
	}
	if _, err := st.GetPod("default", "new"); err == nil {
		t.Fatal("pod created in memory despite failed persist")
	}
	if rv := st.ResourceVersion(); rv < rvBefore {
		t.Fatalf("resourceVersion went backwards: %d -> %d", rvBefore, rv)
	}

	// Update: the previous object is back, the caller's mutation is gone.
	pod, err := st.GetPod("default", "keep")
	if err != nil {
		t.Fatalf("GetPod: %v", err)
	}
	pod.Spec.Containers[0].Image = "nginx:changed"
	if _, err := st.UpdatePod(pod); !errors.Is(err, store.ErrPersist) {
		t.Fatalf("UpdatePod error = %v, want one wrapping ErrPersist", err)
	}
	got, err := st.GetPod("default", "keep")
	if err != nil {
		t.Fatalf("pod disappeared after failed update: %v", err)
	}
	if image := got.Spec.Containers[0].Image; image != "nginx:1" {
		t.Fatalf("image after failed update = %q, want nginx:1", image)
	}
	if rv := st.ResourceVersion(); rv < rvBefore {
		t.Fatalf("resourceVersion went backwards after update: %d -> %d", rvBefore, rv)
	}

	// Delete: the object is back.
	if err := st.DeletePod("default", "keep"); !errors.Is(err, store.ErrPersist) {
		t.Fatalf("DeletePod error = %v, want one wrapping ErrPersist", err)
	}
	if _, err := st.GetPod("default", "keep"); err != nil {
		t.Fatalf("pod missing after failed delete: %v", err)
	}
	if rv := st.ResourceVersion(); rv < rvBefore {
		t.Fatalf("resourceVersion went backwards after delete: %d -> %d", rvBefore, rv)
	}
}

func TestPersistCreatesSnapshotsAndKeepsNewest20(t *testing.T) {
	st, file := newFileStore(t)

	for i := 0; i < 25; i++ {
		name := fmt.Sprintf("snap-%02d", i)
		if _, err := st.CreateNamespace(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
			t.Fatalf("CreateNamespace %s: %v", name, err)
		}
	}

	rvs := snapshotRVs(t, file)
	if len(rvs) != 20 {
		t.Fatalf("snapshot count = %d, want 20 (%v)", len(rvs), rvs)
	}
	for i, rv := range rvs {
		want := i + 6 // saves 1..25, the newest 20 are 6..25
		if rv != want {
			t.Fatalf("snapshot %d has rv %d, want %d (all: %v)", i, rv, want, rvs)
		}
	}
}

func TestLoadRecoversNewestSnapshot(t *testing.T) {
	st, file := newFileStore(t)
	if _, err := st.CreatePod(makePod("default", "one", "nginx:1")); err != nil {
		t.Fatalf("CreatePod one: %v", err)
	}
	if _, err := st.CreatePod(makePod("default", "two", "nginx:2")); err != nil {
		t.Fatalf("CreatePod two: %v", err)
	}

	if err := os.WriteFile(file, []byte("{ definitely not json"), 0600); err != nil {
		t.Fatalf("corrupt store file: %v", err)
	}

	loaded, err := store.Load(file)
	if err != nil {
		t.Fatalf("Load after corruption: %v", err)
	}
	for _, name := range []string{"one", "two"} {
		if _, err := loaded.GetPod("default", name); err != nil {
			t.Fatalf("pod %q missing after recovery: %v", name, err)
		}
	}

	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read rewritten store file: %v", err)
	}
	if !json.Valid(raw) {
		t.Fatalf("store.json was not rewritten as valid JSON: %q", raw)
	}

	corrupt, err := filepath.Glob(file + ".corrupt-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(corrupt) != 1 {
		t.Fatalf("corrupt store copies = %v, want exactly one", corrupt)
	}
	kept, err := os.ReadFile(corrupt[0])
	if err != nil {
		t.Fatalf("read kept corrupt file: %v", err)
	}
	if string(kept) != "{ definitely not json" {
		t.Fatalf("kept corrupt file content = %q", kept)
	}
}

func TestLoadSkipsCorruptNewestSnapshot(t *testing.T) {
	st, file := newFileStore(t)
	if _, err := st.CreatePod(makePod("default", "one", "nginx:1")); err != nil {
		t.Fatalf("CreatePod one: %v", err)
	}
	if _, err := st.CreatePod(makePod("default", "two", "nginx:2")); err != nil {
		t.Fatalf("CreatePod two: %v", err)
	}
	newestRV := st.ResourceVersion()

	newest := filepath.Join(filepath.Dir(file), "snapshots", fmt.Sprintf("store.%d.json.gz", newestRV))
	if err := os.WriteFile(newest, []byte("not gzip"), 0600); err != nil {
		t.Fatalf("corrupt newest snapshot: %v", err)
	}
	if err := os.WriteFile(file, []byte("not json"), 0600); err != nil {
		t.Fatalf("corrupt store file: %v", err)
	}

	loaded, err := store.Load(file)
	if err != nil {
		t.Fatalf("Load after corruption: %v", err)
	}
	if _, err := loaded.GetPod("default", "one"); err != nil {
		t.Fatalf("older snapshot was not used: %v", err)
	}
	if _, err := loaded.GetPod("default", "two"); err == nil {
		t.Fatal("pod from the corrupt newest snapshot must not be present")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read rewritten store file: %v", err)
	}
	if !json.Valid(raw) {
		t.Fatalf("store.json was not rewritten as valid JSON: %q", raw)
	}
}

func TestLoadRecoversWhenStoreFileMissing(t *testing.T) {
	st, file := newFileStore(t)
	if _, err := st.CreatePod(makePod("default", "kept", "nginx:1")); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatalf("remove store file: %v", err)
	}

	loaded, err := store.Load(file)
	if err != nil {
		t.Fatalf("Load with only snapshots: %v", err)
	}
	if _, err := loaded.GetPod("default", "kept"); err != nil {
		t.Fatalf("pod missing after recovery: %v", err)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read recreated store file: %v", err)
	}
	if !json.Valid(raw) {
		t.Fatalf("store.json was not recreated as valid JSON: %q", raw)
	}
}

// TestDeleteAdvancesResourceVersion: a delete is a write of its own, so it
// gets a new resourceVersion and a snapshot of its own, and the snapshot of
// the state before the delete is kept.
func TestDeleteAdvancesResourceVersion(t *testing.T) {
	st, file := newFileStore(t)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "default"}}
	if _, err := st.CreateConfigMap(cm); err != nil {
		t.Fatalf("CreateConfigMap: %v", err)
	}
	before := st.ResourceVersion()
	if err := st.DeleteConfigMap("default", "demo"); err != nil {
		t.Fatalf("DeleteConfigMap: %v", err)
	}
	after := st.ResourceVersion()
	if after == before {
		t.Fatalf("delete kept resourceVersion %d", before)
	}
	rvs := snapshotRVs(t, file)
	if len(rvs) < 2 || rvs[0] == rvs[1] {
		t.Fatalf("expected distinct snapshots for create and delete, got %v", rvs)
	}
}
