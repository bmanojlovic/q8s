package store

import "time"

// SetWriteHookForTest installs hook before every store file write and returns
// a function that restores the previous hook.
func SetWriteHookForTest(s *Store, hook func([]byte) error) func() {
	s.saveMu.Lock()
	old := s.writeHook
	s.writeHook = hook
	s.saveMu.Unlock()
	return func() {
		s.saveMu.Lock()
		s.writeHook = old
		s.saveMu.Unlock()
	}
}

// SetSnapshotIntervalForTest changes the snapshot throttle and returns a
// function that restores the previous interval.
func SetSnapshotIntervalForTest(d time.Duration) func() {
	old := snapshotInterval
	snapshotInterval = d
	return func() {
		snapshotInterval = old
	}
}
