package server

import (
	"net/http"
	"sync"
)

// objectLocks is a keyed mutex: writes to the same Kubernetes object take the
// same entry, writes to different objects never wait for each other. Entries
// are reference counted so the map only holds keys with a lock currently held
// or waited for; the last unlock removes the entry.
type objectLocks struct {
	mu      sync.Mutex
	entries map[string]*objectLockEntry
}

type objectLockEntry struct {
	mu   sync.RWMutex
	refs int
}

// objectLockKey is the map key for one object. The NUL separator keeps a kind,
// namespace or name that contains a slash or dash from colliding with another
// tuple.
func objectLockKey(kind, namespace, name string) string {
	return kind + "\x00" + namespace + "\x00" + name
}

// lock takes the exclusive lock for key and returns its unlock function. The
// entry is reference counted before waiting, so an entry cannot be removed
// from the map while a waiter still points at it.
func (l *objectLocks) lock(key string) func() {
	return l.acquire(key, true)
}

// rlock takes the shared lock for key and returns its unlock function.
func (l *objectLocks) rlock(key string) func() {
	return l.acquire(key, false)
}

func (l *objectLocks) acquire(key string, exclusive bool) func() {
	l.mu.Lock()
	if l.entries == nil {
		l.entries = make(map[string]*objectLockEntry)
	}
	entry := l.entries[key]
	if entry == nil {
		entry = &objectLockEntry{}
		l.entries[key] = entry
	}
	entry.refs++
	l.mu.Unlock()

	if exclusive {
		entry.mu.Lock()
	} else {
		entry.mu.RLock()
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			if exclusive {
				entry.mu.Unlock()
			} else {
				entry.mu.RUnlock()
			}
			l.mu.Lock()
			entry.refs--
			if entry.refs == 0 {
				delete(l.entries, key)
			}
			l.mu.Unlock()
		})
	}
}

// refs reports how many holders and waiters the key currently has. Tests use
// it to observe a blocked goroutine; production code never reads it.
func (l *objectLocks) refs(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := l.entries[key]
	if entry == nil {
		return 0
	}
	return entry.refs
}

// lockObject serializes a write to one object. Callers must call the returned
// function; use `defer unlock()`.
//
// Lock order: every write path takes at most one lock per object and always in
// this order, so two requests cannot wait on each other in opposite orders:
//
//  1. Namespace (shared for a namespaced write, exclusive for namespace
//     create, patch and delete)
//  2. Deployment
//  3. Pod (a Deployment's replica Pods come after their Deployment)
//  4. CronJob
//  5. Job
//  6. Service, PersistentVolumeClaim, ConfigMap, Secret
//
// A handler that already holds a lock must not take it again: Go mutexes are
// not reentrant. The internal helpers called from a handler (delete*ByName,
// deployDeploymentInstance, recreateDeploymentReplica, the collection delete
// callbacks) therefore take no locks themselves; the handler or the
// deleteCollection wrapper takes them exactly once. Server-side apply takes
// its lock only in the create or patch handler it dispatches to.
func (s *Server) lockObject(kind, namespace, name string) func() {
	return s.objLocks.lock(objectLockKey(kind, namespace, name))
}

// lockObjectShared takes the shared side of an object's keyed lock. It is used
// for the Namespace lock in namespaced writes, which may run concurrently with
// each other but must not run while the namespace is being deleted.
func (s *Server) lockObjectShared(kind, namespace, name string) func() {
	return s.objLocks.rlock(objectLockKey(kind, namespace, name))
}

// beginNamespacedWrite takes the shared Namespace lock for a namespaced write
// and returns its unlock function. Reads do not call it.
//
// For a create it also closes the race with a namespace delete: the namespace
// is checked before waiting for the lock and again after acquiring it. If it
// existed when the request started and is gone now, the create is refused
// instead of writing a quadlet file for an object whose namespace no longer
// exists. A namespace that was never explicit is still allowed, preserving
// q8s's implicit-namespace behaviour.
func (s *Server) beginNamespacedWrite(w http.ResponseWriter, namespace string, create bool) (func(), bool) {
	existed := false
	if create {
		if _, err := s.config.Store.GetNamespace(namespace); err == nil {
			existed = true
		}
	}
	unlock := s.lockObjectShared("Namespace", "", namespace)
	if create && existed {
		if _, err := s.config.Store.GetNamespace(namespace); err != nil {
			unlock()
			s.respondStatus(w, http.StatusNotFound, "NotFound", "namespace %q not found", namespace)
			return nil, false
		}
	}
	return unlock, true
}
