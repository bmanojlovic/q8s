package server

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Disk pressure thresholds. They are variables so tests can lower them without
// having to fill a real filesystem. The same values back the q8s doctor check,
// through DiskFree, so the guard and the report cannot disagree.
var (
	minFreeBytes   uint64 = 100 << 20
	minFreePercent uint64 = 5
)

// statfsFn and diskPressureTTL are variables for tests.
var (
	statfsFn        = unix.Statfs
	diskPressureTTL = 2 * time.Second
)

// fsStat is the part of Statfs the guard needs.
type fsStat struct {
	free uint64
	size uint64
	fsid unix.Fsid
}

// statPath stats the nearest existing ancestor of path.
func statPath(path string) (fsStat, error) {
	path = filepath.Clean(path)
	for {
		var st unix.Statfs_t
		err := statfsFn(path, &st)
		if err == nil {
			bsize := uint64(st.Bsize)
			return fsStat{
				free: uint64(st.Bavail) * bsize,
				size: uint64(st.Blocks) * bsize,
				fsid: st.Fsid,
			}, nil
		}
		// Only a missing path (or a non-directory component) is worth walking
		// up: any other error means statfs itself is unusable here.
		if !errors.Is(err, unix.ENOENT) && !errors.Is(err, unix.ENOTDIR) {
			return fsStat{}, err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return fsStat{}, err
		}
		path = parent
	}
}

// freeThreshold returns min(minFreeBytes, size*minFreePercent/100).
func freeThreshold(size uint64) uint64 {
	pct := size/100*minFreePercent + (size%100)*minFreePercent/100
	if pct < minFreeBytes {
		return pct
	}
	return minFreeBytes
}

// DiskFree reports free bytes and the required free bytes for the filesystem
// holding path. It walks up to the nearest existing ancestor, so callers may
// pass directories that have not been created yet. It is exported so the q8s
// doctor check can reuse the guard's threshold logic.
func DiskFree(path string) (free, need uint64, err error) {
	st, err := statPath(path)
	if err != nil {
		return 0, 0, err
	}
	return st.free, freeThreshold(st.size), nil
}

// diskPressure reports whether any of paths is on a filesystem with too little
// free space. Filesystems are checked once even when several paths share them.
// detail names the first low path with its free space and threshold.
func diskPressure(paths []string) (low bool, detail string) {
	seen := make(map[unix.Fsid]struct{}, len(paths))
	for _, path := range paths {
		if path == "" {
			continue
		}
		st, err := statPath(path)
		if err != nil {
			// A path we cannot statfs is not evidence of a full disk.
			continue
		}
		if _, ok := seen[st.fsid]; ok {
			continue
		}
		seen[st.fsid] = struct{}{}
		need := freeThreshold(st.size)
		if st.free < need {
			return true, fmt.Sprintf("%s: %s free, need %s", path, formatBytes(st.free), formatBytes(need))
		}
	}
	return false, ""
}

// formatBytes renders a byte count for the DiskPressure message and doctor.
func formatBytes(n uint64) string {
	const (
		kib = 1 << 10
		mib = 1 << 20
		gib = 1 << 30
	)
	switch {
	case n >= gib:
		return fmt.Sprintf("%.1f GiB", float64(n)/gib)
	case n >= mib:
		return fmt.Sprintf("%.1f MiB", float64(n)/mib)
	case n >= kib:
		return fmt.Sprintf("%.1f KiB", float64(n)/kib)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// diskPressureCache remembers the last statfs result for a short while so a
// burst of writes does not statfs every request.
type diskPressureCache struct {
	mu      sync.Mutex
	checked time.Time
	low     bool
	detail  string
}

func (c *diskPressureCache) get(paths []string) (bool, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.checked.IsZero() && time.Since(c.checked) < diskPressureTTL {
		return c.low, c.detail
	}
	low, detail := diskPressure(paths)
	c.checked = time.Now()
	c.low = low
	c.detail = detail
	return low, detail
}

// diskPaths lists the directories q8s writes to. Empty entries are ignored by
// diskPressure.
func (s *Server) diskPaths() []string {
	paths := make([]string, 0, 5)
	if s.config.Store != nil {
		paths = append(paths, s.config.Store.DataDir())
	}
	return append(paths,
		s.config.QuadletDir,
		s.config.SystemdDir,
		s.config.ConfigDir,
		s.config.TraefikDir,
	)
}

// diskPressureLow is the cached disk pressure state for this server.
func (s *Server) diskPressureLow() (bool, string) {
	return s.disk.get(s.diskPaths())
}

// diskPressureCondition builds the node DiskPressure condition from the real
// statfs state instead of reporting a hard-coded False.
func (s *Server) diskPressureCondition(now, bootTime time.Time) map[string]interface{} {
	low, detail := s.diskPressureLow()
	if low {
		return map[string]interface{}{
			"type":               "DiskPressure",
			"status":             "True",
			"lastHeartbeatTime":  now.Format(time.RFC3339),
			"lastTransitionTime": bootTime.Format(time.RFC3339),
			"reason":             "KubeletHasDiskPressure",
			"message":            detail,
		}
	}
	return map[string]interface{}{
		"type":               "DiskPressure",
		"status":             "False",
		"lastHeartbeatTime":  now.Format(time.RFC3339),
		"lastTransitionTime": bootTime.Format(time.RFC3339),
		"reason":             "KubeletHasNoDiskPressure",
		"message":            "q8s has no disk pressure",
	}
}

// nodeSpec carries the disk-pressure taint while the write guard refuses
// changes, as the kubelet does, so tools that read taints see the same state
// as the DiskPressure condition.
func (s *Server) nodeSpec() map[string]interface{} {
	if low, _ := s.diskPressureLow(); low {
		return map[string]interface{}{
			"taints": []map[string]interface{}{
				{"key": "node.kubernetes.io/disk-pressure", "effect": "NoSchedule"},
			},
		}
	}
	return map[string]interface{}{}
}
