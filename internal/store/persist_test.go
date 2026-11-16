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
	"time"

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

// everyPersistSnapshots disables the snapshot throttle for tests that count
// snapshots or recover from a particular one.
func everyPersistSnapshots(t *testing.T) {
	t.Helper()
	restore := store.SetSnapshotIntervalForTest(0)
	t.Cleanup(restore)
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
	everyPersistSnapshots(t)
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
	everyPersistSnapshots(t)
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
	everyPersistSnapshots(t)
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
	everyPersistSnapshots(t)
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
	everyPersistSnapshots(t)
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

// TestSnapshotThrottleSkipsRecentSnapshot: store.json is written on every
// persist, but a second snapshot within snapshotInterval is skipped so a burst
// of status updates cannot fill the retain window.
func TestSnapshotThrottleSkipsRecentSnapshot(t *testing.T) {
	restore := store.SetSnapshotIntervalForTest(time.Hour)
	t.Cleanup(restore)

	st, file := newFileStore(t)
	if _, err := st.CreateNamespace(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "first"}}); err != nil {
		t.Fatalf("CreateNamespace first: %v", err)
	}
	firstRV := st.ResourceVersion()
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("later-%d", i)
		if _, err := st.CreateNamespace(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
			t.Fatalf("CreateNamespace %s: %v", name, err)
		}
	}

	rvs := snapshotRVs(t, file)
	if len(rvs) != 1 || rvs[0] != firstRV {
		t.Fatalf("snapshots = %v, want only the first save at rv %d", rvs, firstRV)
	}

	// store.json still has every change despite the skipped snapshots.
	loaded, err := store.Load(file)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("later-%d", i)
		if _, err := loaded.GetNamespace(name); err != nil {
			t.Fatalf("namespace %q missing from store.json: %v", name, err)
		}
	}

	// With the interval elapsed (0 means always write), the next persist
	// snapshots the current state.
	store.SetSnapshotIntervalForTest(0)
	if _, err := st.CreateNamespace(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "after"}}); err != nil {
		t.Fatalf("CreateNamespace after: %v", err)
	}
	rvs = snapshotRVs(t, file)
	if len(rvs) != 2 || rvs[1] != st.ResourceVersion() {
		t.Fatalf("snapshots = %v, want first and a new snapshot at rv %d", rvs, st.ResourceVersion())
	}
}

// TestSnapshotTailWritesLastBurstState: a save that skips its snapshot arms a
// timer, so the final state of a burst is still recoverable when no mutation
// follows.
func TestSnapshotTailWritesLastBurstState(t *testing.T) {
	restore := store.SetSnapshotIntervalForTest(100 * time.Millisecond)
	t.Cleanup(restore)

	st, file := newFileStore(t)
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("burst-%d", i)
		if _, err := st.CreateNamespace(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
			t.Fatalf("CreateNamespace %s: %v", name, err)
		}
	}
	finalRV := st.ResourceVersion()
	if rvs := snapshotRVs(t, file); len(rvs) == 1 && rvs[0] == finalRV {
		t.Fatal("burst finished with only its first snapshot; test needs the throttle to skip a later save")
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		rvs := snapshotRVs(t, file)
		if len(rvs) > 0 && rvs[len(rvs)-1] == finalRV {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no tail snapshot at rv %d, snapshots %v", finalRV, rvs)
		}
		time.Sleep(5 * time.Millisecond)
	}

	loaded, err := store.Load(file)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("burst-%d", i)
		if _, err := loaded.GetNamespace(name); err != nil {
			t.Fatalf("namespace %q missing from the tail snapshot: %v", name, err)
		}
	}
}

// TestLoadRemembersNewestSnapshotTime: on startup the throttle starts from
// the newest snapshot's mtime, so a restart does not immediately rewrite a
// snapshot that is still fresh.
func TestLoadRemembersNewestSnapshotTime(t *testing.T) {
	everyPersistSnapshots(t)

	st, file := newFileStore(t)
	if _, err := st.CreateNamespace(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "before"}}); err != nil {
		t.Fatalf("CreateNamespace before: %v", err)
	}
	before := snapshotRVs(t, file)

	restore := store.SetSnapshotIntervalForTest(time.Hour)
	t.Cleanup(restore)

	loaded, err := store.Load(file)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := loaded.CreateNamespace(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "after"}}); err != nil {
		t.Fatalf("CreateNamespace after: %v", err)
	}

	after := snapshotRVs(t, file)
	if len(after) != len(before) {
		t.Fatalf("snapshots after restart = %v, want unchanged %v", after, before)
	}
	for i := range before {
		if after[i] != before[i] {
			t.Fatalf("snapshots after restart = %v, want unchanged %v", after, before)
		}
	}

	loaded2, err := store.Load(file)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, err := loaded2.GetNamespace("after"); err != nil {
		t.Fatalf("namespace after missing from store.json: %v", err)
	}
}

// TestFailedPersistDoesNotDiscardOtherWritersChange is the regression for the
// rollback race: a failed write must roll back only its own mutation, never
// another writer's change that was already saved successfully. Before the fix
// a mutation released mu before persisting, so B could change the maps while
// A was writing; A's rollback then removed B's object and B's later persist
// wrote the rolled-back state, while B still returned nil.
func TestFailedPersistDoesNotDiscardOtherWritersChange(t *testing.T) {
	everyPersistSnapshots(t)

	st, file := newFileStore(t)
	started := make(chan struct{})
	sawOtherWriter := make(chan struct{})
	calls := 0

	restore := store.SetWriteHookForTest(st, func([]byte) error {
		calls++
		if calls > 1 {
			return nil
		}
		// A is first into persist. Let B reach the store, then give it time
		// to mutate before failing A's write. With mutation and persist
		// serialised B is still waiting for saveMu here, which is the point.
		close(started)
		<-sawOtherWriter
		time.Sleep(50 * time.Millisecond)
		return errors.New("injected write failure")
	})
	defer restore()

	failed := make(chan error, 1)
	go func() {
		_, err := st.CreatePod(makePod("default", "failing", "nginx:1"))
		failed <- err
	}()
	<-started

	succeeded := make(chan error, 1)
	go func() {
		close(sawOtherWriter)
		_, err := st.CreatePod(makePod("default", "survivor", "nginx:2"))
		succeeded <- err
	}()

	if err := <-failed; !errors.Is(err, store.ErrPersist) {
		t.Fatalf("failing create error = %v, want one wrapping ErrPersist", err)
	}
	if err := <-succeeded; err != nil {
		t.Fatalf("surviving create error = %v", err)
	}

	if _, err := st.GetPod("default", "survivor"); err != nil {
		t.Fatalf("surviving pod missing from memory after returning nil: %v", err)
	}
	if _, err := st.GetPod("default", "failing"); err == nil {
		t.Fatal("pod from the failed write is still in memory")
	}

	loaded, err := store.Load(file)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := loaded.GetPod("default", "survivor"); err != nil {
		t.Fatalf("surviving pod missing from disk after returning nil: %v", err)
	}
	if _, err := loaded.GetPod("default", "failing"); err == nil {
		t.Fatal("pod from the failed write is on disk")
	}
}
