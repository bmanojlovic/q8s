package fsutil

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestWriteFileAtomicWritesContentAndPerm(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	if err := WriteFileAtomic(path, []byte("first"), 0640); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "first" {
		t.Fatalf("content = %q, want %q", data, "first")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0640 {
		t.Fatalf("mode = %v, want 0640", info.Mode().Perm())
	}
}

func TestWriteFileAtomicOverwritesContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	if err := WriteFileAtomic(path, []byte("old content"), 0644); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := WriteFileAtomic(path, []byte("new"), 0600); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "new" {
		t.Fatalf("content = %q, want %q", data, "new")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestWriteFileAtomicLeavesNoTempFileOnSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	if err := WriteFileAtomic(path, []byte("data"), 0644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "config" {
		t.Fatalf("directory entries = %v, want only config", names(entries))
	}
}

func TestWriteFileAtomicRemovesTempFileAfterFailedRename(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	if err := WriteFileAtomic(target, []byte("data"), 0644); err == nil {
		t.Fatal("WriteFileAtomic onto a directory succeeded, want an error")
	}
	matches, err := filepath.Glob(filepath.Join(dir, ".target.tmp-*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary files left behind: %v", matches)
	}
}

func TestWriteFileAtomicIfChangedSkipsIdenticalContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	changed, err := WriteFileAtomicIfChanged(path, []byte("same"), 0644)
	if err != nil {
		t.Fatalf("first write: %v", err)
	}
	if !changed {
		t.Fatal("first write reported unchanged")
	}

	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	changed, err = WriteFileAtomicIfChanged(path, []byte("same"), 0644)
	if err != nil {
		t.Fatalf("identical write: %v", err)
	}
	if changed {
		t.Fatal("identical content reported as changed")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !info.ModTime().Equal(old) {
		t.Fatalf("mtime = %v, want %v (file was rewritten)", info.ModTime(), old)
	}

	changed, err = WriteFileAtomicIfChanged(path, []byte("different"), 0644)
	if err != nil {
		t.Fatalf("changed write: %v", err)
	}
	if !changed {
		t.Fatal("different content reported as unchanged")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "different" {
		t.Fatalf("content = %q, want %q", data, "different")
	}
}

// TestWriteFileAtomicKeepsDirectoryInode covers the bind-mount case: a
// container holds the directory, not the files. The rename must keep the
// directory inode, and a directory opened before the write must list the new
// entry, which is what a container reading through its bind mount sees.
func TestWriteFileAtomicKeepsDirectoryInode(t *testing.T) {
	dir := t.TempDir()
	before, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat before: %v", err)
	}
	beforeIno := before.Sys().(*syscall.Stat_t).Ino

	opened, err := os.Open(dir)
	if err != nil {
		t.Fatalf("open dir: %v", err)
	}
	defer opened.Close()

	path := filepath.Join(dir, "key")
	if err := WriteFileAtomic(path, []byte("value"), 0644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	after, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	afterIno := after.Sys().(*syscall.Stat_t).Ino
	if beforeIno != afterIno {
		t.Fatalf("directory inode changed from %d to %d", beforeIno, afterIno)
	}

	// Re-read from the start of the same, already-open directory descriptor.
	if _, err := opened.Seek(0, 0); err != nil {
		t.Fatalf("seek dir: %v", err)
	}
	entries, err := opened.Readdirnames(-1)
	if err != nil {
		t.Fatalf("readdir on already-open directory: %v", err)
	}
	found := false
	for _, name := range entries {
		if name == "key" {
			found = true
		}
	}
	if !found {
		t.Fatalf("already-open directory does not list the new file: %v", entries)
	}
}

func names(entries []os.DirEntry) []string {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.Name())
	}
	return out
}
