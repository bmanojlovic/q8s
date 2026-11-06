package server

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"q8s/internal/quadlet"
)

// socketDir is where systemd-managed .socket units live. It is deliberately
// never QuadletDir: quadlet ignores .socket files, and startup sweeps every
// .socket from the quadlet dir as a legacy cleanup.
func (s *Server) socketDir() string {
	return s.config.SystemdDir
}

// writeSocketFilesTracked writes .socket units and reports which ones
// actually changed. Sockets carry no template hash, so a plain content
// comparison is what keeps an unchanged re-apply from rewriting the file.
func writeSocketFilesTracked(dir string, files map[string][]byte) (map[string]bool, error) {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	changed := make(map[string]bool, len(files))
	for _, name := range names {
		existing, err := os.ReadFile(filepath.Join(dir, name))
		if err == nil && bytes.Equal(existing, files[name]) {
			continue
		}
		if err := writeQuadletFile(dir, name, files[name]); err != nil {
			return changed, fmt.Errorf("write %s: %w", name, err)
		}
		changed[name] = true
	}
	return changed, nil
}

// socketUnitNames returns the systemd unit names of the rendered sockets,
// sorted so enable/start order is deterministic.
func socketUnitNames(files map[string][]byte) []string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// removeStalePodSockets deletes .socket units in dir owned by this pod that
// this render no longer produces, for example after an entry was removed
// from the annotation. Ownership is by the exact X-Q8s lines SocketUnits
// writes, never by filename prefix.
func (s *Server) removeStalePodSockets(dir, ns, podName string, keep map[string]bool) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	removed := false
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".socket") {
			continue
		}
		name := entry.Name()
		if keep[name] {
			continue
		}
		content, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if !hasExactSocketOwner(content, ns, podName) {
			continue
		}
		if mgr := s.config.Manager; mgr != nil {
			if err := mgr.StopUnit(name); err != nil {
				fmt.Printf("stop socket %s: %v\n", name, err)
			}
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			continue
		}
		removed = true
	}
	return removed
}

// deployPodSockets makes the pod's socket units match its annotation and
// starts them before the container service is written/restarted. It returns
// true when a socket file changed or disappeared, in which case the service
// must be restarted after the socket: systemd hands the listening fds over
// at service start.
func (s *Server) deployPodSockets(pod *corev1.Pod, podName string) bool {
	entries, err := quadlet.ParseSocketActivation(pod.Annotations)
	if err != nil {
		fmt.Printf("socket activation %s/%s: %v\n", pod.Namespace, podName, err)
		return false
	}
	dir := s.socketDir()
	if dir == "" {
		if len(entries) > 0 {
			fmt.Printf("socket activation %s/%s: no systemd unit dir configured\n", pod.Namespace, podName)
		}
		return false
	}
	files, err := quadlet.SocketUnits(pod.Namespace, podName, entries)
	if err != nil {
		fmt.Printf("socket activation %s/%s: %v\n", pod.Namespace, podName, err)
		return false
	}
	changed, err := writeSocketFilesTracked(dir, files)
	if err != nil {
		fmt.Printf("socket activation %s/%s: %v\n", pod.Namespace, podName, err)
		return false
	}
	removed := s.removeStalePodSockets(dir, pod.Namespace, podName, filenameSet(files))
	mgr := s.config.Manager
	if mgr == nil {
		return len(changed) > 0 || removed
	}
	if len(changed) > 0 || removed {
		if err := mgr.DaemonReload(); err != nil {
			fmt.Printf("daemon-reload after socket change %s/%s: %v\n", pod.Namespace, podName, err)
		}
	}
	for _, unit := range socketUnitNames(files) {
		if _, ok := changed[unit]; ok {
			// The ListenStream= line changed: a plain start is a no-op on an
			// already active socket, so restart it to rebind.
			if err := mgr.RestartUnit(unit); err != nil {
				fmt.Printf("restart socket %s: %v\n", unit, err)
			}
		} else if err := mgr.StartUnit(unit); err != nil {
			fmt.Printf("start socket %s: %v\n", unit, err)
		}
		if err := mgr.EnableUnit(unit); err != nil {
			fmt.Printf("enable socket %s: %v\n", unit, err)
		}
	}
	return len(changed) > 0 || removed
}

// reconcilePodSockets regenerates socket units that are missing or stale
// during startup reconciliation. It returns the units to enable/start and
// whether anything was written. The caller folds the reload into its
// existing daemon-reload and starts the sockets before the services.
func (s *Server) reconcilePodSockets(pod *corev1.Pod, podName string) ([]string, bool) {
	entries, err := quadlet.ParseSocketActivation(pod.Annotations)
	if err != nil {
		fmt.Printf("reconcile sockets %s/%s: %v\n", pod.Namespace, podName, err)
		return nil, false
	}
	dir := s.socketDir()
	if dir == "" {
		return nil, false
	}
	files, err := quadlet.SocketUnits(pod.Namespace, podName, entries)
	if err != nil {
		fmt.Printf("reconcile sockets %s/%s: %v\n", pod.Namespace, podName, err)
		return nil, false
	}
	changed, err := writeSocketFilesTracked(dir, files)
	if err != nil {
		fmt.Printf("reconcile sockets %s/%s: %v\n", pod.Namespace, podName, err)
		return nil, false
	}
	removed := s.removeStalePodSockets(dir, pod.Namespace, podName, filenameSet(files))
	return socketUnitNames(files), len(changed) > 0 || removed
}

// startSocketUnits enables and starts socket units. Reconcile calls this
// before starting any container service so LISTEN_FDS is available.
func (s *Server) startSocketUnits(units []string) {
	mgr := s.config.Manager
	if mgr == nil {
		return
	}
	for _, unit := range units {
		if err := mgr.EnableUnit(unit); err != nil {
			fmt.Printf("enable socket %s: %v\n", unit, err)
		}
		if err := mgr.StartUnit(unit); err != nil {
			fmt.Printf("start socket %s: %v\n", unit, err)
		}
	}
}

// removePodSockets stops, disables and deletes every socket unit owned by a
// pod. Used on pod/Deployment delete and scale-down; the service is stopped
// by the caller first.
func (s *Server) removePodSockets(ns, podName string) {
	dir := s.socketDir()
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	removed := false
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".socket") {
			continue
		}
		name := entry.Name()
		content, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if !hasExactSocketOwner(content, ns, podName) {
			continue
		}
		if mgr := s.config.Manager; mgr != nil {
			if err := mgr.StopUnit(name); err != nil {
				fmt.Printf("stop socket %s: %v\n", name, err)
			}
			if err := mgr.DisableUnit(name); err != nil {
				fmt.Printf("disable socket %s: %v\n", name, err)
			}
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			continue
		}
		removed = true
	}
	if removed {
		if mgr := s.config.Manager; mgr != nil {
			if err := mgr.DaemonReload(); err != nil {
				fmt.Printf("daemon-reload after removing sockets %s/%s: %v\n", ns, podName, err)
			}
		}
	}
}

// hasExactSocketOwner reports whether a socket unit belongs to the given
// pod, using the exact X-Q8s lines SocketUnits renders.
func hasExactSocketOwner(content []byte, ns, podName string) bool {
	text := string(content)
	return strings.Contains(text, "\nX-Q8s-Namespace="+ns+"\n") &&
		strings.Contains(text, "\nX-Q8s-Pod="+podName+"\n")
}
