// Package fsutil provides filesystem helpers shared by q8s.
package fsutil

import (
	"bytes"
	"os"
	"path/filepath"
)

// WriteFileAtomic writes data to path through a temporary file in the same
// directory and renames it into place. A crash or a full disk can leave the
// temporary file behind, but never a truncated path: readers either see the
// old content or the complete new content.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	remove := true
	defer func() {
		if remove {
			os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	remove = false

	// Best effort: the rename is already atomic and durable enough for the
	// callers here, so a directory-sync failure (for example on a filesystem
	// that does not support fsync on directories) is ignored.
	syncDir(dir)
	return nil
}

// WriteFileAtomicIfChanged is WriteFileAtomic that leaves an existing file
// with byte-identical content untouched. It reports whether it wrote.
func WriteFileAtomicIfChanged(path string, data []byte, perm os.FileMode) (bool, error) {
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, data) {
		return false, nil
	}
	if err := WriteFileAtomic(path, data, perm); err != nil {
		return false, err
	}
	return true, nil
}

// syncDir fsyncs a directory so a rename survives a crash. Errors are
// deliberately ignored: the rename has already happened.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	_ = d.Sync()
}
