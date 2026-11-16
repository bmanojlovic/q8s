package store

import "fmt"

// MarkDeleting remembers that a resource's store entry has been removed and
// its units and containers are still being cleaned up. Reconcile checks this
// so it does not re-import a container that podman still reports while it is
// being stopped. kind is a resource kind such as "Pod" or "Deployment".
func (s *Store) MarkDeleting(kind, namespace, name string) {
	key := deletingKey(kind, namespace, name)
	s.deletingMu.Lock()
	if s.deleting == nil {
		s.deleting = make(map[string]struct{})
	}
	s.deleting[key] = struct{}{}
	s.deletingMu.Unlock()
}

// ClearDeleting ends the deletion window for a resource. It is safe to call
// for a key that was never marked.
func (s *Store) ClearDeleting(kind, namespace, name string) {
	key := deletingKey(kind, namespace, name)
	s.deletingMu.Lock()
	delete(s.deleting, key)
	s.deletingMu.Unlock()
}

// IsDeleting reports whether a resource is currently being cleaned up after
// its store entry was removed.
func (s *Store) IsDeleting(kind, namespace, name string) bool {
	key := deletingKey(kind, namespace, name)
	s.deletingMu.Lock()
	_, ok := s.deleting[key]
	s.deletingMu.Unlock()
	return ok
}

func deletingKey(kind, namespace, name string) string {
	return fmt.Sprintf("%s/%s/%s", kind, namespace, name)
}
