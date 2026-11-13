package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// serveLock holds the open lock file for the life of the process. Keeping
// the *os.File referenced matters: if it were garbage collected its finalizer
// would close the descriptor and drop the lock.
var serveLock *os.File

// lockDataDir takes an exclusive, non-blocking flock on <dataDir>/serve.lock
// so only one `q8s serve` runs per data directory. Every writer of
// store.json, the unit files and /etc/subuid is that one process; a second
// serve started by hand next to the systemd one would otherwise reconcile
// and write before failing on the busy port. The kernel releases the lock
// when the process exits, crash included, so a stale lock file is harmless.
func lockDataDir(dataDir string) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dataDir, "serve.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return fmt.Errorf("another q8s serve is already running for %s", dataDir)
		}
		return fmt.Errorf("lock %s: %w", path, err)
	}
	serveLock = f
	return nil
}
