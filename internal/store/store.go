package store

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"

	"q8s/internal/quadlet"
	"q8s/internal/traefik"
)

// snapshot is the on-disk JSON format for the store.
type snapshot struct {
	ResourceVer         int                             `json:"resourceVersion"`
	Pods                []*corev1.Pod                   `json:"pods,omitempty"`
	Services            []*corev1.Service               `json:"services,omitempty"`
	PVCs                []*corev1.PersistentVolumeClaim `json:"pvcs,omitempty"`
	ConfigMaps          []*corev1.ConfigMap             `json:"configMaps,omitempty"`
	Secrets             []*corev1.Secret                `json:"secrets,omitempty"`
	ServiceAccounts     []*corev1.ServiceAccount        `json:"serviceAccounts,omitempty"`
	Namespaces          []*corev1.Namespace             `json:"namespaces,omitempty"`
	Jobs                []*batchv1.Job                  `json:"jobs,omitempty"`
	CronJobs            []*batchv1.CronJob              `json:"cronJobs,omitempty"`
	Deployments         []*appsv1.Deployment            `json:"deployments,omitempty"`
	Ingresses           []*networkingv1.Ingress         `json:"ingresses,omitempty"`
	Middlewares         []*traefik.Middleware           `json:"middlewares,omitempty"`
	IngressRouteTCPs    []*traefik.IngressRouteTCP      `json:"ingressRouteTCPs,omitempty"`
	Roles               []*rbacv1.Role                  `json:"roles,omitempty"`
	RoleBindings        []*rbacv1.RoleBinding           `json:"roleBindings,omitempty"`
	ClusterRoles        []*rbacv1.ClusterRole           `json:"clusterRoles,omitempty"`
	ClusterRoleBindings []*rbacv1.ClusterRoleBinding    `json:"clusterRoleBindings,omitempty"`
}

// Store is an in-memory store for k8s resources.
type Store struct {
	mu       sync.RWMutex
	saveMu   sync.Mutex // serialises file writes
	dataFile string
	// lastGood is the JSON of the state that was last successfully written
	// to dataFile (or loaded from it). A failed write restores the in-memory
	// maps from it so memory never gets ahead of disk.
	lastGood []byte
	// writeHook, when set, runs before every store file write. It is a test
	// seam for injecting a failed write; it runs under saveMu.
	writeHook func([]byte) error
	// lastSnapshot is when the newest gzip snapshot was written. Only
	// touched under saveMu.
	lastSnapshot        time.Time
	pods                map[types.NamespacedName]*corev1.Pod
	services            map[types.NamespacedName]*corev1.Service
	pvcs                map[types.NamespacedName]*corev1.PersistentVolumeClaim
	configmaps          map[types.NamespacedName]*corev1.ConfigMap
	secrets             map[types.NamespacedName]*corev1.Secret
	serviceAccounts     map[types.NamespacedName]*corev1.ServiceAccount
	namespaces          map[types.NamespacedName]*corev1.Namespace
	jobs                map[types.NamespacedName]*batchv1.Job
	cronjobs            map[types.NamespacedName]*batchv1.CronJob
	deployments         map[types.NamespacedName]*appsv1.Deployment
	ingresses           map[types.NamespacedName]*networkingv1.Ingress
	middlewares         map[types.NamespacedName]*traefik.Middleware
	ingressRouteTCPs    map[types.NamespacedName]*traefik.IngressRouteTCP
	roles               map[types.NamespacedName]*rbacv1.Role
	roleBindings        map[types.NamespacedName]*rbacv1.RoleBinding
	clusterRoles        map[string]*rbacv1.ClusterRole
	clusterRoleBindings map[string]*rbacv1.ClusterRoleBinding
	resourceVer         int

	// runStarts remembers, in memory only, when each container's current
	// run started (ns/pod/container). By the time a restart is recorded the
	// container state has already moved from running to waiting, so this is
	// where lastState.terminated.startedAt comes from.
	runStarts map[string]runStart

	notifyMu sync.Mutex
	notifyCh chan struct{}

	// events are in-memory only — never written to the JSON snapshot, never
	// restored on startup. They're a best-effort activity feed for dashboards
	// like Freelens, not an audit log; losing them on restart is fine.
	eventsMu sync.Mutex
	events   []*corev1.Event
	eventSeq int

	// deleting marks resources whose store entry is gone but whose units and
	// containers are still being torn down. Reconcile skips them so a
	// container that is still visible to podman is not imported back.
	deletingMu sync.Mutex
	deleting   map[string]struct{}
}

// maxEvents caps the in-memory event feed; oldest events are dropped once
// exceeded, same as real clusters expiring old Events after their TTL.
const maxEvents = 500

// recordEvent appends a best-effort lifecycle event for a resource. Safe to
// call regardless of whether the caller already holds s.mu, since events
// have their own lock and never touch the resource maps.
func (s *Store) recordEvent(kind, ns, name string, uid types.UID, eventType, reason, message string) {
	s.eventsMu.Lock()
	s.eventSeq++
	now := metav1.Now()
	ev := &corev1.Event{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Event"},
		ObjectMeta: metav1.ObjectMeta{
			Name:              fmt.Sprintf("%s.%x", name, s.eventSeq),
			Namespace:         ns,
			UID:               uuid.NewUUID(),
			ResourceVersion:   fmt.Sprintf("%d", s.eventSeq),
			CreationTimestamp: now,
		},
		InvolvedObject: corev1.ObjectReference{
			APIVersion: eventAPIVersion(kind),
			Kind:       kind,
			Namespace:  ns,
			Name:       name,
			UID:        uid,
		},
		Reason:         reason,
		Message:        message,
		Type:           eventType,
		Source:         corev1.EventSource{Component: "q8s"},
		FirstTimestamp: now,
		LastTimestamp:  now,
		Count:          1,
	}
	s.events = append(s.events, ev)
	if len(s.events) > maxEvents {
		s.events = s.events[len(s.events)-maxEvents:]
	}
	s.eventsMu.Unlock()
	s.notifyChanged()
}

// RecordEvent is the exported wrapper for recordEvent, for callers outside
// the store's own lifecycle methods (e.g. the API server surfacing a failed
// quadlet write as a PVC event).
func (s *Store) RecordEvent(kind, ns, name string, uid types.UID, eventType, reason, message string) {
	s.recordEvent(kind, ns, name, uid, eventType, reason, message)
}

// assignUID gives a newly created object a fresh random UID. As in
// Kubernetes, a UID in the create request is ignored: every incarnation of an
// object is distinct, which is what binds ServiceAccount certificates to one
// workload instance.
func assignUID(obj metav1.Object) {
	obj.SetUID(uuid.NewUUID())
}

func eventAPIVersion(kind string) string {
	switch kind {
	case "Deployment":
		return "apps/v1"
	case "Job", "CronJob":
		return "batch/v1"
	default:
		return "v1"
	}
}

// AllEvents returns all events across all namespaces, oldest first.
func (s *Store) AllEvents() []*corev1.Event {
	s.eventsMu.Lock()
	defer s.eventsMu.Unlock()
	result := make([]*corev1.Event, len(s.events))
	for i, e := range s.events {
		result[i] = e.DeepCopy()
	}
	return result
}

// Events returns all events for a namespace, oldest first.
func (s *Store) Events(ns string) []*corev1.Event {
	s.eventsMu.Lock()
	defer s.eventsMu.Unlock()
	var result []*corev1.Event
	for _, e := range s.events {
		if e.Namespace == ns {
			result = append(result, e.DeepCopy())
		}
	}
	return result
}

func newEmpty() *Store {
	return &Store{
		runStarts:           make(map[string]runStart),
		pods:                make(map[types.NamespacedName]*corev1.Pod),
		services:            make(map[types.NamespacedName]*corev1.Service),
		pvcs:                make(map[types.NamespacedName]*corev1.PersistentVolumeClaim),
		configmaps:          make(map[types.NamespacedName]*corev1.ConfigMap),
		secrets:             make(map[types.NamespacedName]*corev1.Secret),
		serviceAccounts:     make(map[types.NamespacedName]*corev1.ServiceAccount),
		namespaces:          make(map[types.NamespacedName]*corev1.Namespace),
		jobs:                make(map[types.NamespacedName]*batchv1.Job),
		cronjobs:            make(map[types.NamespacedName]*batchv1.CronJob),
		deployments:         make(map[types.NamespacedName]*appsv1.Deployment),
		ingresses:           make(map[types.NamespacedName]*networkingv1.Ingress),
		middlewares:         make(map[types.NamespacedName]*traefik.Middleware),
		ingressRouteTCPs:    make(map[types.NamespacedName]*traefik.IngressRouteTCP),
		roles:               make(map[types.NamespacedName]*rbacv1.Role),
		roleBindings:        make(map[types.NamespacedName]*rbacv1.RoleBinding),
		clusterRoles:        make(map[string]*rbacv1.ClusterRole),
		clusterRoleBindings: make(map[string]*rbacv1.ClusterRoleBinding),
		notifyCh:            make(chan struct{}),
	}
}

// Changed returns a channel that is closed the next time any resource in the
// store is mutated. Callers should re-fetch the resources they care about,
// then call Changed() again to wait for the next change. This is the basis
// for watch endpoints: they diff successive snapshots instead of the store
// tracking per-subscriber event queues.
func (s *Store) Changed() <-chan struct{} {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	return s.notifyCh
}

// notifyChanged wakes all current Changed() waiters.
func (s *Store) notifyChanged() {
	s.notifyMu.Lock()
	old := s.notifyCh
	s.notifyCh = make(chan struct{})
	s.notifyMu.Unlock()
	close(old)
}

// New creates a new empty Store with a "default" namespace.
func New() *Store {
	s := newEmpty()
	s.ensureDefaultNamespace()
	return s
}

// DataDir returns the directory holding the store file, or "" when the store
// is memory only.
func (s *Store) DataDir() string {
	if s.dataFile == "" {
		return ""
	}
	return filepath.Dir(s.dataFile)
}

// ErrPersist marks a store mutation that could not be written to disk. The
// in-memory state has already been rolled back to the last persisted state
// when it is returned.
var ErrPersist = errors.New("store: could not persist change")

// snapshotRetain is how many gzip snapshots are kept next to store.json.
const snapshotRetain = 20

// snapshotInterval is how old the newest snapshot must be before persist
// writes another one. store.json is still written on every persist; the
// snapshots are recovery points, so a burst of status updates must not fill
// the retain window with a few seconds of history. Tests set it to 0 to write
// a snapshot on every persist.
var snapshotInterval = 60 * time.Second

// Load reads the store from dataFile if it exists; otherwise returns a fresh Store.
// The returned store will persist changes back to dataFile on every mutation.
//
// If store.json is missing or unparseable but a gzip snapshot can be read,
// Load recovers the newest readable snapshot, keeps the unreadable file as
// store.json.corrupt-<unix time> and rewrites store.json immediately.
func Load(dataFile string) (*Store, error) {
	s := newEmpty()
	s.dataFile = dataFile
	s.lastSnapshot = s.newestSnapshotModTime()

	data, err := os.ReadFile(dataFile)
	if os.IsNotExist(err) {
		if file, snap, raw, ok := s.newestValidSnapshot(); ok {
			s.adoptSnapshot(file, snap, raw, err)
			return s, nil
		}
		s.mu.Lock()
		s.ensureDefaultNamespace()
		s.mu.Unlock()
		s.setLastGoodFromState()
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading store file: %w", err)
	}

	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		parseErr := fmt.Errorf("parsing store file: %w", err)
		file, good, raw, ok := s.newestValidSnapshot()
		if !ok {
			return nil, parseErr
		}
		s.keepCorruptStoreFile()
		s.adoptSnapshot(file, good, raw, parseErr)
		return s, nil
	}

	backfillCreationTimestamps(&snap, dataFile)
	s.mu.Lock()
	s.installSnapshotLocked(&snap)
	s.lastGood = append([]byte(nil), data...)
	s.mu.Unlock()
	return s, nil
}

// adoptSnapshot installs a recovered snapshot, records it as the last good
// state, logs the recovery and rewrites store.json so the next start is clean.
func (s *Store) adoptSnapshot(file string, snap *snapshot, raw []byte, readErr error) {
	s.mu.Lock()
	s.installSnapshotLocked(snap)
	s.lastGood = append([]byte(nil), raw...)
	s.mu.Unlock()

	fmt.Printf("store: store.json unreadable (%v), recovered from snapshot %s (rv %d)\n", readErr, file, snap.ResourceVer)
	if err := s.persist(); err != nil {
		fmt.Printf("store: rewriting store.json after snapshot recovery failed: %v\n", err)
	}
}

// keepCorruptStoreFile renames an unreadable store.json instead of deleting
// it, so its contents can still be inspected after the recovery.
func (s *Store) keepCorruptStoreFile() {
	base := s.dataFile + ".corrupt-" + strconv.FormatInt(time.Now().Unix(), 10)
	path := base
	for i := 1; ; i++ {
		if _, err := os.Stat(path); err != nil {
			break
		}
		path = fmt.Sprintf("%s.%d", base, i)
	}
	if err := os.Rename(s.dataFile, path); err != nil {
		fmt.Printf("store: keeping unreadable store file: %v\n", err)
		return
	}
	fmt.Printf("store: kept unreadable store file as %s\n", path)
}

// installSnapshotLocked replaces every resource map with the contents of
// snap. It is the single per-kind loop shared by Load, snapshot recovery and
// rollback after a failed write, so all three restore state identically.
// s.mu must be held. The resourceVersion never goes backwards.
func (s *Store) installSnapshotLocked(snap *snapshot) {
	pods := make(map[types.NamespacedName]*corev1.Pod, len(snap.Pods))
	for _, p := range snap.Pods {
		pods[types.NamespacedName{Namespace: p.Namespace, Name: p.Name}] = p
	}
	services := make(map[types.NamespacedName]*corev1.Service, len(snap.Services))
	for _, svc := range snap.Services {
		services[types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}] = svc
	}
	pvcs := make(map[types.NamespacedName]*corev1.PersistentVolumeClaim, len(snap.PVCs))
	for _, pvc := range snap.PVCs {
		pvcs[types.NamespacedName{Namespace: pvc.Namespace, Name: pvc.Name}] = pvc
	}
	configmaps := make(map[types.NamespacedName]*corev1.ConfigMap, len(snap.ConfigMaps))
	for _, cm := range snap.ConfigMaps {
		configmaps[types.NamespacedName{Namespace: cm.Namespace, Name: cm.Name}] = cm
	}
	secrets := make(map[types.NamespacedName]*corev1.Secret, len(snap.Secrets))
	for _, sec := range snap.Secrets {
		// Self-heal legacy secrets stored before the tic-b31a fix: fold any
		// StringData into Data at load time so a bare restart's
		// restoreSecretFiles (which writes only from Data) rewrites real
		// content instead of empty placeholders. Idempotent: a normalized
		// secret (StringData nil) is left untouched. See tic-c627.
		normalizeSecret(sec)
		secrets[types.NamespacedName{Namespace: sec.Namespace, Name: sec.Name}] = sec
	}
	serviceAccounts := make(map[types.NamespacedName]*corev1.ServiceAccount, len(snap.ServiceAccounts))
	for _, sa := range snap.ServiceAccounts {
		serviceAccounts[types.NamespacedName{Namespace: sa.Namespace, Name: sa.Name}] = sa
	}
	namespaces := make(map[types.NamespacedName]*corev1.Namespace, len(snap.Namespaces))
	for _, ns := range snap.Namespaces {
		namespaces[types.NamespacedName{Name: ns.Name}] = ns
	}
	jobs := make(map[types.NamespacedName]*batchv1.Job, len(snap.Jobs))
	for _, j := range snap.Jobs {
		jobs[types.NamespacedName{Namespace: j.Namespace, Name: j.Name}] = j
	}
	cronjobs := make(map[types.NamespacedName]*batchv1.CronJob, len(snap.CronJobs))
	for _, cj := range snap.CronJobs {
		cronjobs[types.NamespacedName{Namespace: cj.Namespace, Name: cj.Name}] = cj
	}
	deployments := make(map[types.NamespacedName]*appsv1.Deployment, len(snap.Deployments))
	for _, dep := range snap.Deployments {
		deployments[types.NamespacedName{Namespace: dep.Namespace, Name: dep.Name}] = dep
	}
	ingresses := make(map[types.NamespacedName]*networkingv1.Ingress, len(snap.Ingresses))
	for _, ing := range snap.Ingresses {
		ingresses[types.NamespacedName{Namespace: ing.Namespace, Name: ing.Name}] = ing
	}
	middlewares := make(map[types.NamespacedName]*traefik.Middleware, len(snap.Middlewares))
	for _, mw := range snap.Middlewares {
		middlewares[types.NamespacedName{Namespace: mw.Namespace, Name: mw.Name}] = mw
	}
	ingressRouteTCPs := make(map[types.NamespacedName]*traefik.IngressRouteTCP, len(snap.IngressRouteTCPs))
	for _, irt := range snap.IngressRouteTCPs {
		ingressRouteTCPs[types.NamespacedName{Namespace: irt.Namespace, Name: irt.Name}] = irt
	}
	roles := make(map[types.NamespacedName]*rbacv1.Role, len(snap.Roles))
	for _, role := range snap.Roles {
		roles[types.NamespacedName{Namespace: role.Namespace, Name: role.Name}] = role
	}
	roleBindings := make(map[types.NamespacedName]*rbacv1.RoleBinding, len(snap.RoleBindings))
	for _, rb := range snap.RoleBindings {
		roleBindings[types.NamespacedName{Namespace: rb.Namespace, Name: rb.Name}] = rb
	}
	clusterRoles := make(map[string]*rbacv1.ClusterRole, len(snap.ClusterRoles))
	for _, cr := range snap.ClusterRoles {
		clusterRoles[cr.Name] = cr
	}
	clusterRoleBindings := make(map[string]*rbacv1.ClusterRoleBinding, len(snap.ClusterRoleBindings))
	for _, crb := range snap.ClusterRoleBindings {
		clusterRoleBindings[crb.Name] = crb
	}

	s.pods = pods
	s.services = services
	s.pvcs = pvcs
	s.configmaps = configmaps
	s.secrets = secrets
	s.serviceAccounts = serviceAccounts
	s.namespaces = namespaces
	s.jobs = jobs
	s.cronjobs = cronjobs
	s.deployments = deployments
	s.ingresses = ingresses
	s.middlewares = middlewares
	s.ingressRouteTCPs = ingressRouteTCPs
	s.roles = roles
	s.roleBindings = roleBindings
	s.clusterRoles = clusterRoles
	s.clusterRoleBindings = clusterRoleBindings
	s.ensureDefaultNamespace()
	if snap.ResourceVer > s.resourceVer {
		s.resourceVer = snap.ResourceVer
	}
}

// setLastGoodFromState records the current state as the last persisted one.
// Used by Load for a fresh store, which is never written until its first
// mutation.
func (s *Store) setLastGoodFromState() {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.Marshal(s.snapshot())
	if err != nil {
		fmt.Printf("store: marshal error: %v\n", err)
		return
	}
	s.lastGood = data
}

// snapshotsDir is the directory holding the gzip snapshots next to store.json.
func (s *Store) snapshotsDir() string {
	return filepath.Join(filepath.Dir(s.dataFile), "snapshots")
}

// snapshotFile is one store.<rv>.json.gz in snapshotsDir.
type snapshotFile struct {
	path string
	rv   int
}

func parseSnapshotName(name string) (int, bool) {
	if !strings.HasPrefix(name, "store.") || !strings.HasSuffix(name, ".json.gz") {
		return 0, false
	}
	rv, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "store."), ".json.gz"))
	if err != nil {
		return 0, false
	}
	return rv, true
}

// listSnapshotFiles returns the snapshots newest first by numeric
// resourceVersion. Unreadable directory means no snapshots.
func (s *Store) listSnapshotFiles() []snapshotFile {
	entries, err := os.ReadDir(s.snapshotsDir())
	if err != nil {
		return nil
	}
	var files []snapshotFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		rv, ok := parseSnapshotName(e.Name())
		if !ok {
			continue
		}
		files = append(files, snapshotFile{path: filepath.Join(s.snapshotsDir(), e.Name()), rv: rv})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].rv > files[j].rv })
	return files
}

func readSnapshotFile(path string) (*snapshot, []byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, nil, err
	}
	data, err := io.ReadAll(zr)
	zr.Close()
	if err != nil {
		return nil, nil, err
	}
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, nil, err
	}
	return &snap, data, nil
}

// newestValidSnapshot returns the newest snapshot that decompresses and
// parses. Corrupt snapshots are skipped and reported.
func (s *Store) newestValidSnapshot() (string, *snapshot, []byte, bool) {
	for _, f := range s.listSnapshotFiles() {
		snap, data, err := readSnapshotFile(f.path)
		if err != nil {
			fmt.Printf("store: snapshot %s unreadable: %v\n", f.path, err)
			continue
		}
		backfillCreationTimestamps(snap, s.dataFile)
		return f.path, snap, data, true
	}
	return "", nil, nil, false
}

// writeSnapshot stores the just-persisted JSON as snapshots/store.<rv>.json.gz
// and prunes older snapshots. Snapshot failures are logged but never fail the
// persist that produced them.
// writeSnapshot returns the compressed size, or 0 when no snapshot was written.
func (s *Store) writeSnapshot(rv int, data []byte) int {
	dir := s.snapshotsDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		fmt.Printf("store: snapshot mkdir %s: %v\n", dir, err)
		return 0
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		fmt.Printf("store: snapshot compress: %v\n", err)
		return 0
	}
	if err := zw.Close(); err != nil {
		fmt.Printf("store: snapshot compress: %v\n", err)
		return 0
	}
	path := filepath.Join(dir, fmt.Sprintf("store.%d.json.gz", rv))
	tmp := path + ".tmp"
	if err := writeFileSync(tmp, buf.Bytes(), 0600); err != nil {
		fmt.Printf("store: snapshot write %s: %v\n", tmp, err)
		return 0
	}
	if err := os.Rename(tmp, path); err != nil {
		fmt.Printf("store: snapshot rename %s: %v\n", tmp, err)
		return 0
	}
	s.pruneSnapshots()
	return buf.Len()
}

// pruneSnapshots keeps only the newest snapshotRetain snapshots by rv.
func (s *Store) pruneSnapshots() {
	files := s.listSnapshotFiles()
	if len(files) <= snapshotRetain {
		return
	}
	for _, f := range files[snapshotRetain:] {
		if err := os.Remove(f.path); err != nil {
			fmt.Printf("store: removing old snapshot %s: %v\n", f.path, err)
		}
	}
}

// mutate runs one mutation with saveMu held, and keeps holding it through the
// persist that covers the change. This is what stops another writer from
// changing the maps between this mutation and its save: without it a failed
// write's rollback to lastGood could discard that other writer's change even
// though its own save succeeded.
//
// fn runs with saveMu and s.mu held and reports whether it changed anything.
// When it did, persistLocked writes the store while saveMu is still held and
// rolls the maps back to lastGood on failure. Lock order is always saveMu and
// then mu. fn must not lock saveMu or call another mutating method.
func (s *Store) mutate(fn func() (bool, error)) error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	s.mu.Lock()
	changed, err := fn()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	return s.persistLocked()
}

// persist acquires saveMu and writes the store. Only startup paths that are
// not themselves mutations use this; mutating methods go through mutate.
func (s *Store) persist() error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	return s.persistLocked()
}

// persistLocked writes the store to disk atomically (temp file + fsync +
// rename) and, at most once per snapshotInterval, keeps a gzip snapshot of
// the state. The caller must hold saveMu and must not hold s.mu.
//
// Holding saveMu makes each write cover the latest state: no mutation can
// slip in between this snapshot and the write. On a write failure the
// in-memory maps are rolled back to lastGood before the error is returned.
func (s *Store) persistLocked() error {
	s.notifyChanged()

	if s.dataFile == "" {
		return nil
	}

	start := time.Now()
	s.mu.RLock()
	snap := s.snapshot()
	s.mu.RUnlock()

	data, err := json.Marshal(snap)
	if err != nil {
		fmt.Printf("store: marshal error: %v\n", err)
		return fmt.Errorf("%w: %v", ErrPersist, err)
	}

	if err := s.writeStoreFile(data); err != nil {
		s.rollback(err)
		return fmt.Errorf("%w: %v", ErrPersist, err)
	}

	s.mu.Lock()
	s.lastGood = append([]byte(nil), data...)
	s.mu.Unlock()

	written := time.Since(start)
	gz := 0
	if s.snapshotDue() {
		if gz = s.writeSnapshot(snap.ResourceVer, data); gz > 0 {
			s.lastSnapshot = time.Now()
		}
	}
	if gz > 0 {
		fmt.Printf("store: saved rv %d (%d KB, %d ms, snapshot %d KB)\n",
			snap.ResourceVer, len(data)/1024, written.Milliseconds(), gz/1024)
	} else {
		fmt.Printf("store: saved rv %d (%d KB, %d ms, no snapshot)\n",
			snap.ResourceVer, len(data)/1024, written.Milliseconds())
	}
	return nil
}

// snapshotDue reports whether the newest snapshot is old enough to be
// replaced. The caller holds saveMu.
func (s *Store) snapshotDue() bool {
	if snapshotInterval <= 0 || s.lastSnapshot.IsZero() {
		return true
	}
	return time.Since(s.lastSnapshot) >= snapshotInterval
}

// newestSnapshotModTime returns the modification time of the newest snapshot
// file, or the zero time when there is none. Load uses it so a restart does
// not immediately rewrite a snapshot that is still fresh.
func (s *Store) newestSnapshotModTime() time.Time {
	files := s.listSnapshotFiles()
	if len(files) == 0 {
		return time.Time{}
	}
	info, err := os.Stat(files[0].path)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

// writeStoreFile writes data to dataFile atomically. Every failing step is
// part of the returned error: the caller rolls memory back when it fails.
func (s *Store) writeStoreFile(data []byte) error {
	if s.writeHook != nil {
		if err := s.writeHook(data); err != nil {
			return fmt.Errorf("test write hook: %w", err)
		}
	}
	dir := filepath.Dir(s.dataFile)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp := s.dataFile + ".tmp"
	// Sync before the rename: without it a power loss right after the
	// rename can leave an empty store.json on ext4/xfs.
	if err := writeFileSync(tmp, data, 0600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, s.dataFile); err != nil {
		return fmt.Errorf("rename %s: %w", tmp, err)
	}
	return nil
}

// rollback restores the resource maps from the last successfully persisted
// JSON after a failed write. The resourceVersion only moves forward, so a
// version handed out to a client is never reused. Watchers are woken so they
// re-read the restored objects.
func (s *Store) rollback(writeErr error) {
	s.mu.Lock()
	if len(s.lastGood) == 0 {
		s.mu.Unlock()
		fmt.Printf("store: write failed, rolled back to last persisted state: %v\n", writeErr)
		s.notifyChanged()
		return
	}
	var snap snapshot
	if err := json.Unmarshal(s.lastGood, &snap); err != nil {
		s.mu.Unlock()
		fmt.Printf("store: write failed (%v), could not restore last persisted state: %v\n", writeErr, err)
		s.notifyChanged()
		return
	}
	s.installSnapshotLocked(&snap)
	s.mu.Unlock()
	fmt.Printf("store: write failed, rolled back to last persisted state: %v\n", writeErr)
	s.notifyChanged()
}

// normalizeSecret folds a Secret's StringData into Data so Data is the single
// source of truth, mirroring what the API server (and handler.normalizeSecret
// on POST/PATCH) does. Applied at load time to self-heal legacy secrets
// persisted before the tic-b31a fix, which kept content only in StringData;
// restoreSecretFiles writes files from Data alone, so an un-folded legacy
// secret would empty its on-disk files on a bare restart. StringData wins on
// key conflicts and is cleared afterward. Idempotent: a secret with no
// StringData is left untouched. See tic-c627.
func normalizeSecret(sec *corev1.Secret) {
	if sec == nil || len(sec.StringData) == 0 {
		return
	}
	if sec.Data == nil {
		sec.Data = make(map[string][]byte, len(sec.StringData))
	}
	for k, v := range sec.StringData {
		sec.Data[k] = []byte(v)
	}
	sec.StringData = nil
}

func (s *Store) ensureDefaultNamespace() {
	key := types.NamespacedName{Name: "default"}
	if _, ok := s.namespaces[key]; !ok {
		s.namespaces[key] = &corev1.Namespace{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
			ObjectMeta: metav1.ObjectMeta{Name: "default", UID: "q8s-default-ns"},
		}
	}
}

// snapshot captures a consistent view of the store. Caller must hold s.mu (read or write).
func (s *Store) snapshot() snapshot {
	snap := snapshot{ResourceVer: s.resourceVer}
	for _, p := range s.pods {
		snap.Pods = append(snap.Pods, p.DeepCopy())
	}
	for _, svc := range s.services {
		snap.Services = append(snap.Services, svc.DeepCopy())
	}
	for _, pvc := range s.pvcs {
		snap.PVCs = append(snap.PVCs, pvc.DeepCopy())
	}
	for _, cm := range s.configmaps {
		snap.ConfigMaps = append(snap.ConfigMaps, cm.DeepCopy())
	}
	for _, sec := range s.secrets {
		snap.Secrets = append(snap.Secrets, sec.DeepCopy())
	}
	for _, sa := range s.serviceAccounts {
		snap.ServiceAccounts = append(snap.ServiceAccounts, sa.DeepCopy())
	}
	for _, ns := range s.namespaces {
		snap.Namespaces = append(snap.Namespaces, ns.DeepCopy())
	}
	for _, j := range s.jobs {
		snap.Jobs = append(snap.Jobs, j.DeepCopy())
	}
	for _, cj := range s.cronjobs {
		snap.CronJobs = append(snap.CronJobs, cj.DeepCopy())
	}
	for _, dep := range s.deployments {
		snap.Deployments = append(snap.Deployments, dep.DeepCopy())
	}
	for _, ing := range s.ingresses {
		snap.Ingresses = append(snap.Ingresses, ing.DeepCopy())
	}
	for _, mw := range s.middlewares {
		snap.Middlewares = append(snap.Middlewares, mw.DeepCopy())
	}
	for _, irt := range s.ingressRouteTCPs {
		snap.IngressRouteTCPs = append(snap.IngressRouteTCPs, irt.DeepCopy())
	}
	for _, role := range s.roles {
		snap.Roles = append(snap.Roles, role.DeepCopy())
	}
	for _, rb := range s.roleBindings {
		snap.RoleBindings = append(snap.RoleBindings, rb.DeepCopy())
	}
	for _, cr := range s.clusterRoles {
		snap.ClusterRoles = append(snap.ClusterRoles, cr.DeepCopy())
	}
	for _, crb := range s.clusterRoleBindings {
		snap.ClusterRoleBindings = append(snap.ClusterRoleBindings, crb.DeepCopy())
	}
	return snap
}

// ConfigMapFiles returns all stored ConfigMaps — used to restore files on startup.
func (s *Store) ConfigMapFiles() []*corev1.ConfigMap {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*corev1.ConfigMap, 0, len(s.configmaps))
	for _, cm := range s.configmaps {
		result = append(result, cm.DeepCopy())
	}
	return result
}

func (s *Store) incRV() string {
	s.resourceVer++
	return fmt.Sprintf("%d", s.resourceVer)
}

// --- Namespaces ---

// Namespaces returns all namespaces.
func (s *Store) Namespaces() []*corev1.Namespace {
	s.mu.RLock()
	defer s.mu.RUnlock()

	seen := map[string]bool{}
	result := make([]*corev1.Namespace, 0, len(s.namespaces))
	for _, ns := range s.namespaces {
		seen[ns.Name] = true
		result = append(result, ns.DeepCopy())
	}

	// Surface namespaces that have resources but no explicit namespace entry
	// (e.g. after a crash mid-delete or Podman import on startup).
	phantom := func(name string) {
		if !seen[name] {
			seen[name] = true
			result = append(result, &corev1.Namespace{
				TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
				ObjectMeta: metav1.ObjectMeta{Name: name},
				Status:     corev1.NamespaceStatus{Phase: corev1.NamespaceActive},
			})
		}
	}
	for k := range s.pods {
		phantom(k.Namespace)
	}
	for k := range s.services {
		phantom(k.Namespace)
	}
	for k := range s.deployments {
		phantom(k.Namespace)
	}
	for k := range s.jobs {
		phantom(k.Namespace)
	}
	for k := range s.cronjobs {
		phantom(k.Namespace)
	}
	for k := range s.configmaps {
		phantom(k.Namespace)
	}
	for k := range s.secrets {
		phantom(k.Namespace)
	}
	for k := range s.serviceAccounts {
		phantom(k.Namespace)
	}
	for k := range s.pvcs {
		phantom(k.Namespace)
	}
	for k := range s.ingresses {
		phantom(k.Namespace)
	}
	for k := range s.middlewares {
		phantom(k.Namespace)
	}
	for k := range s.ingressRouteTCPs {
		phantom(k.Namespace)
	}
	for k := range s.roles {
		phantom(k.Namespace)
	}
	for k := range s.roleBindings {
		phantom(k.Namespace)
	}
	return result
}

// CreateNamespace creates a namespace.
func (s *Store) CreateNamespace(ns *corev1.Namespace) (*corev1.Namespace, error) {
	var out *corev1.Namespace
	err := s.mutate(func() (bool, error) {
		key := types.NamespacedName{Name: ns.Name}
		if _, ok := s.namespaces[key]; ok {
			return false, fmt.Errorf("namespace %q already exists", ns.Name)
		}
		ns.ResourceVersion = s.incRV()
		assignUID(ns)
		if ns.CreationTimestamp.IsZero() {
			ns.CreationTimestamp = metav1.Now()
		}
		s.namespaces[key] = ns
		out = ns.DeepCopy()
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetNamespace gets a namespace.
func (s *Store) GetNamespace(name string) (*corev1.Namespace, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := types.NamespacedName{Name: name}
	ns, ok := s.namespaces[key]
	if !ok {
		return nil, fmt.Errorf("namespace %q not found", name)
	}
	return ns.DeepCopy(), nil
}

// UpdateNamespace replaces a namespace in place, preserving UID and creation
// timestamp. Only metadata changes (labels/annotations) are meaningful; the
// namespace network is already in place.
func (s *Store) UpdateNamespace(ns *corev1.Namespace) (*corev1.Namespace, error) {
	var out *corev1.Namespace
	err := s.mutate(func() (bool, error) {
		key := types.NamespacedName{Name: ns.Name}
		existing, ok := s.namespaces[key]
		if !ok {
			return false, fmt.Errorf("namespace %q not found", ns.Name)
		}
		ns.UID = existing.UID
		ns.ResourceVersion = s.incRV()
		if ns.CreationTimestamp.IsZero() {
			ns.CreationTimestamp = existing.CreationTimestamp
		}
		s.namespaces[key] = ns
		out = ns.DeepCopy()
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteNamespace deletes a namespace.
func (s *Store) DeleteNamespace(name string) error {
	return s.mutate(func() (bool, error) {
		key := types.NamespacedName{Name: name}
		if _, ok := s.namespaces[key]; !ok {
			return false, fmt.Errorf("namespace %q not found", name)
		}
		delete(s.namespaces, key)
		// A delete is a write: it gets its own resourceVersion, as in Kubernetes.
		s.incRV()
		return true, nil
	})
}

// PurgeNamespace removes all resources belonging to a namespace from the store
// in a single lock acquisition. Called by the namespace delete handler after
// stopping systemd units so the store and disk stay consistent.
func (s *Store) PurgeNamespace(name string) {
	_ = s.mutate(func() (bool, error) {
		s.purgeNamespaceLocked(name)
		s.resourceVer++
		return true, nil
	})
}

// DeleteNamespaceWithResources removes a namespace and every resource in it
// in a single mutation, so a failed write cannot leave the namespace without
// its contents (or its contents without the namespace). The handler does the
// disk cleanup only after this succeeds. An unknown namespace is an error.
func (s *Store) DeleteNamespaceWithResources(name string) error {
	return s.mutate(func() (bool, error) {
		key := types.NamespacedName{Name: name}
		if _, ok := s.namespaces[key]; !ok {
			return false, fmt.Errorf("namespace %q not found", name)
		}
		s.purgeNamespaceLocked(name)
		delete(s.namespaces, key)
		s.incRV()
		return true, nil
	})
}

// purgeNamespaceLocked removes every resource in a namespace. The caller
// holds s.mu and is responsible for bumping the resourceVersion.
func (s *Store) purgeNamespaceLocked(name string) {
	for key := range s.pods {
		if key.Namespace == name {
			delete(s.pods, key)
			s.forgetRunStarts(key)
		}
	}
	for key := range s.services {
		if key.Namespace == name {
			delete(s.services, key)
		}
	}
	for key := range s.pvcs {
		if key.Namespace == name {
			delete(s.pvcs, key)
		}
	}
	for key := range s.configmaps {
		if key.Namespace == name {
			delete(s.configmaps, key)
		}
	}
	for key := range s.secrets {
		if key.Namespace == name {
			delete(s.secrets, key)
		}
	}
	for key := range s.serviceAccounts {
		if key.Namespace == name {
			delete(s.serviceAccounts, key)
		}
	}
	for key := range s.deployments {
		if key.Namespace == name {
			delete(s.deployments, key)
		}
	}
	for key := range s.jobs {
		if key.Namespace == name {
			delete(s.jobs, key)
		}
	}
	for key := range s.cronjobs {
		if key.Namespace == name {
			delete(s.cronjobs, key)
		}
	}
	// Ingresses were missing from the original hand-enumerated purge list:
	// after `kubectl delete namespace`, their objects survived in the store
	// (still listed by `kubectl get ingresses -A`) and startup
	// reconciliation kept regenerating Traefik routes pointing at the
	// deleted namespace's backends. The lesson: any new resource kind added
	// to the store must be added here and to Namespaces()'s phantom loops —
	// consider a generic helper if a tenth kind appears.
	for key := range s.ingresses {
		if key.Namespace == name {
			delete(s.ingresses, key)
		}
	}
	for key := range s.middlewares {
		if key.Namespace == name {
			delete(s.middlewares, key)
		}
	}
	for key := range s.ingressRouteTCPs {
		if key.Namespace == name {
			delete(s.ingressRouteTCPs, key)
		}
	}
	for key := range s.roles {
		if key.Namespace == name {
			delete(s.roles, key)
		}
	}
	for key := range s.roleBindings {
		if key.Namespace == name {
			delete(s.roleBindings, key)
		}
	}
}

// --- Pods ---

// AllPods returns all pods across all namespaces.
func (s *Store) AllPods() []*corev1.Pod {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*corev1.Pod, 0, len(s.pods))
	for _, p := range s.pods {
		result = append(result, p.DeepCopy())
	}
	return result
}

// Pods returns all pods in a namespace.
func (s *Store) Pods(ns string) []*corev1.Pod {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*corev1.Pod
	for key, p := range s.pods {
		if key.Namespace == ns {
			result = append(result, p.DeepCopy())
		}
	}
	return result
}

// CreatePod creates a pod.
func (s *Store) CreatePod(pod *corev1.Pod) (*corev1.Pod, error) {
	var out *corev1.Pod
	err := s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}
		if _, ok := s.pods[key]; ok {
			return false, fmt.Errorf("pod %q already exists in namespace %q", pod.Name, pod.Namespace)
		}
		pod.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}
		pod.ResourceVersion = s.incRV()
		assignUID(pod)
		if pod.Labels == nil {
			pod.Labels = make(map[string]string)
		}
		if pod.CreationTimestamp.IsZero() {
			pod.CreationTimestamp = metav1.Now()
		}
		s.pods[key] = pod
		out = pod.DeepCopy()
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	s.recordEvent("Pod", pod.Namespace, pod.Name, pod.UID, corev1.EventTypeNormal, "Created",
		fmt.Sprintf("Created pod %s/%s", pod.Namespace, pod.Name))
	return out, nil
}

// GetPod gets a pod by namespace and name.
func (s *Store) GetPod(ns, name string) (*corev1.Pod, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := types.NamespacedName{Namespace: ns, Name: name}
	pod, ok := s.pods[key]
	if !ok {
		return nil, fmt.Errorf("pod %q not found in namespace %q", name, ns)
	}
	return pod.DeepCopy(), nil
}

// DeletePod deletes a pod.
func (s *Store) DeletePod(ns, name string) error {
	var uid types.UID
	err := s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: ns, Name: name}
		pod, ok := s.pods[key]
		if !ok {
			return false, fmt.Errorf("pod %q not found in namespace %q", name, ns)
		}
		uid = pod.UID
		delete(s.pods, key)
		// A delete is a write: it gets its own resourceVersion, as in Kubernetes.
		s.incRV()
		s.forgetRunStarts(key)
		return true, nil
	})
	if err != nil {
		return err
	}
	s.recordEvent("Pod", ns, name, uid, corev1.EventTypeNormal, "Deleted",
		fmt.Sprintf("Deleted pod %s/%s", ns, name))
	return nil
}

// UpdatePod replaces a pod in-place, preserving UID.
func (s *Store) UpdatePod(pod *corev1.Pod) (*corev1.Pod, error) {
	var out *corev1.Pod
	err := s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}
		existing, ok := s.pods[key]
		if !ok {
			return false, fmt.Errorf("pod %q not found in namespace %q", pod.Name, pod.Namespace)
		}
		pod.UID = existing.UID
		pod.ResourceVersion = s.incRV()
		s.pods[key] = pod
		out = pod.DeepCopy()
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SyncPodTemplate replaces the spec and the template-derived metadata
// (labels, annotations, owner references) of a stored pod, keeping its UID.
// A Deployment instance is materialized from the template on every deploy;
// without this the stored object kept the spec it was first created with, and
// every reader of the store (namespace network attach, Ingress backends,
// kubectl) saw a stale pod. Done under the lock so a concurrent status update
// is not lost.
// Status and creation time are kept: they are reset by ResetPodForRestart
// when the template change actually restarts the unit. Returns false if the
// pod is missing.
func (s *Store) SyncPodTemplate(pod *corev1.Pod) bool {
	found := false
	_ = s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}
		existing, ok := s.pods[key]
		if !ok {
			return false, nil
		}
		found = true
		if reflect.DeepEqual(existing.Spec, pod.Spec) &&
			reflect.DeepEqual(existing.Labels, pod.Labels) &&
			reflect.DeepEqual(existing.Annotations, pod.Annotations) &&
			reflect.DeepEqual(existing.OwnerReferences, pod.OwnerReferences) {
			return false, nil
		}
		updated := existing.DeepCopy()
		updated.Spec = *pod.Spec.DeepCopy()
		updated.Labels = copyStringMap(pod.Labels)
		updated.Annotations = copyStringMap(pod.Annotations)
		updated.OwnerReferences = append([]metav1.OwnerReference(nil), pod.OwnerReferences...)
		updated.ResourceVersion = s.incRV()
		s.pods[key] = updated
		return true, nil
	})
	return found
}

func copyStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// UpdatePodPhase updates a pod's phase and syncs container ready state.
// health is the Podman health of the container ("healthy", "unhealthy",
// "starting") or "" when there is no health check, in which case readiness
// is unaffected. Ignores missing pods silently (race between delete and sync
// is fine).
//
// It carries no Podman timing observation; callers that have one use
// UpdatePodPhaseState.
func (s *Store) UpdatePodPhase(ns, name string, phase corev1.PodPhase, health string) {
	s.UpdatePodPhaseState(ns, name, phase, ContainerSyncState{Phase: phase, Health: health, Seen: true})
}

// UpdatePodPhaseState is UpdatePodPhase with the Podman observation for the
// pod's single container. obs.Health is the Podman health ("healthy",
// "unhealthy", "starting") or "" when there is no health check. When the
// container has not been seen, the pod phase decides whether it is still
// being created or crash-looping.
func (s *Store) UpdatePodPhaseState(ns, name string, phase corev1.PodPhase, obs ContainerSyncState) {
	phaseChanged := false
	var uid types.UID
	_ = s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: ns, Name: name}
		pod, ok := s.pods[key]
		if !ok {
			return false, nil
		}
		if previousContainer(pod, obs) {
			phase = corev1.PodPending
			obs = ContainerSyncState{}
		}
		phaseChanged = pod.Status.Phase != phase
		changed := phaseChanged
		if phaseChanged {
			pod.Status.Phase = phase
		}
		running := phase == corev1.PodRunning
		if len(pod.Status.ContainerStatuses) == 0 && len(pod.Spec.Containers) > 0 {
			pod.Status.ContainerStatuses = make([]corev1.ContainerStatus, len(pod.Spec.Containers))
			for i, c := range pod.Spec.Containers {
				pod.Status.ContainerStatuses[i] = corev1.ContainerStatus{
					Name:  c.Name,
					Image: c.Image,
				}
			}
			changed = true
		}
		for i := range pod.Status.ContainerStatuses {
			cs := &pod.Status.ContainerStatuses[i]
			if applyContainerObservation(cs, obs, phase) {
				changed = true
			}
			s.noteRunStart(pod, cs)
			// A health check exists when health is non-empty: Ready then needs
			// Podman to report it healthy, not just running.
			ready := running && !isCrashLooping(cs) && (obs.Health == "" || obs.Health == "healthy")
			if cs.Ready != ready {
				cs.Ready = ready
				changed = true
			}
		}
		if syncPodStartTime(pod) {
			changed = true
		}
		if syncPodConditions(pod) {
			changed = true
		}
		if changed {
			pod.ResourceVersion = s.incRV()
		}
		uid = pod.UID
		return changed, nil
	})
	if phaseChanged {
		if evType, reason, msg, ok := podPhaseEvent(ns, name, phase); ok {
			s.recordEvent("Pod", ns, name, uid, evType, reason, msg)
		}
	}
}

// ContainerSyncState is the phase, Podman health and container timing
// observed for one container of a pod by the reconcile loop.
type ContainerSyncState struct {
	Phase     corev1.PodPhase
	Health    string
	StartedAt time.Time
	ExitedAt  time.Time
	ExitCode  int
	Seen      bool
}

// isCrashLooping reports whether the container has restarted after an error.
func isCrashLooping(cs *corev1.ContainerStatus) bool {
	return cs.RestartCount > 0 && cs.LastTerminationState.Terminated != nil &&
		cs.LastTerminationState.Terminated.ExitCode != 0
}

// containerStartedAt returns the start time recorded in either container
// state variant, which is also what StartTime is derived from.
func containerStartedAt(cs *corev1.ContainerStatus) time.Time {
	if cs.State.Terminated != nil {
		return cs.State.Terminated.StartedAt.Time
	}
	if cs.State.Running != nil {
		return cs.State.Running.StartedAt.Time
	}
	return time.Time{}
}

// terminatedFromObservation builds the terminated state for a container
// Podman reported as exited. Timestamps already known are kept when Podman
// does not provide them, so polling a stopped container does not churn.
func terminatedFromObservation(cs *corev1.ContainerStatus, obs ContainerSyncState) corev1.ContainerState {
	startedAt := obs.StartedAt
	finishedAt := obs.ExitedAt
	if cs.State.Terminated != nil {
		if startedAt.IsZero() {
			startedAt = cs.State.Terminated.StartedAt.Time
		}
		if finishedAt.IsZero() {
			finishedAt = cs.State.Terminated.FinishedAt.Time
		}
	}
	if startedAt.IsZero() {
		startedAt = containerStartedAt(cs)
	}
	reason := "Completed"
	if obs.ExitCode != 0 {
		reason = "Error"
	}
	return corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		ExitCode:   int32(obs.ExitCode),
		Reason:     reason,
		StartedAt:  metav1.NewTime(startedAt),
		FinishedAt: metav1.NewTime(finishedAt),
	}}
}

// startedValue reads the optional Started flag, treating unset as false.
func startedValue(p *bool) bool {
	return p != nil && *p
}

func boolPtr(b bool) *bool { return &b }

// observedContainerState derives the Kubernetes container state from the
// Podman observation and, when no container is seen, from the pod phase.
func observedContainerState(cs *corev1.ContainerStatus, obs ContainerSyncState, phase corev1.PodPhase) (corev1.ContainerState, bool) {
	if obs.Seen {
		observedPhase := obs.Phase
		if observedPhase == "" {
			// A caller may only fill the timing fields; the pod phase is
			// still a usable description of the observed container.
			observedPhase = phase
		}
		switch observedPhase {
		case corev1.PodRunning:
			startedAt := obs.StartedAt
			if startedAt.IsZero() && cs.State.Running != nil {
				startedAt = cs.State.Running.StartedAt.Time
			}
			return corev1.ContainerState{Running: &corev1.ContainerStateRunning{
				StartedAt: metav1.NewTime(startedAt),
			}}, true
		case corev1.PodSucceeded, corev1.PodFailed:
			return terminatedFromObservation(cs, obs), true
		default:
			return waitingState(cs), startedValue(cs.Started)
		}
	}
	switch phase {
	case corev1.PodSucceeded, corev1.PodFailed:
		if cs.State.Terminated != nil {
			return cs.State, startedValue(cs.Started)
		}
		reason := "Completed"
		if phase == corev1.PodFailed {
			reason = "Error"
		}
		return corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode:   int32(obs.ExitCode),
			Reason:     reason,
			StartedAt:  metav1.NewTime(containerStartedAt(cs)),
			FinishedAt: metav1.NewTime(obs.ExitedAt),
		}}, true
	default:
		return waitingState(cs), startedValue(cs.Started)
	}
}

// waitingState is the state of a container that is not running: between the
// restarts of a crash loop it is CrashLoopBackOff, as kubelet reports it,
// otherwise it is still being created.
func waitingState(cs *corev1.ContainerStatus) corev1.ContainerState {
	reason := "ContainerCreating"
	if isCrashLooping(cs) {
		reason = "CrashLoopBackOff"
	}
	return corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}}
}

// applyContainerObservation sets cs.State and cs.Started from the observation
// and the pod phase. It reports whether either field changed, so a running
// container polled every pass does not produce a MODIFIED event.
func applyContainerObservation(cs *corev1.ContainerStatus, obs ContainerSyncState, phase corev1.PodPhase) bool {
	state, started := observedContainerState(cs, obs, phase)
	if reflect.DeepEqual(cs.State, state) && startedValue(cs.Started) == started {
		return false
	}
	cs.State = state
	cs.Started = boolPtr(started)
	return true
}

// syncPodStartTime sets Status.StartTime once, from the earliest container
// start time seen, or now when no start time is known. Container restarts do
// not move it afterwards.
func syncPodStartTime(pod *corev1.Pod) bool {
	if pod.Status.StartTime != nil && !pod.Status.StartTime.IsZero() {
		return false
	}
	var earliest time.Time
	for i := range pod.Status.ContainerStatuses {
		t := containerStartedAt(&pod.Status.ContainerStatuses[i])
		if t.IsZero() {
			continue
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	if earliest.IsZero() {
		earliest = time.Now()
	}
	pod.Status.StartTime = &metav1.Time{Time: earliest}
	return true
}

// UpdatePodContainerPhases updates a pod's phase and syncs each container's
// ready state and state from per-container observations. states is keyed by
// container name (the reconcile loop matches Podman's
// io.kubernetes.container.name label to it). A missing entry means the
// container is not observable yet and counts as pending/not ready. Ready
// follows the same rules as UpdatePodPhase: running, not crash-looping, and
// healthy (or no health check).
func (s *Store) UpdatePodContainerPhases(ns, name string, phase corev1.PodPhase, states map[string]ContainerSyncState) {
	phaseChanged := false
	var uid types.UID
	_ = s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: ns, Name: name}
		pod, ok := s.pods[key]
		if !ok {
			return false, nil
		}
		phaseChanged = pod.Status.Phase != phase
		changed := phaseChanged
		if phaseChanged {
			pod.Status.Phase = phase
		}

		old := make(map[string]corev1.ContainerStatus, len(pod.Status.ContainerStatuses))
		for _, cs := range pod.Status.ContainerStatuses {
			old[cs.Name] = cs
		}
		statuses := make([]corev1.ContainerStatus, 0, len(pod.Spec.Containers))
		for _, c := range pod.Spec.Containers {
			cs, ok := old[c.Name]
			if !ok {
				cs = corev1.ContainerStatus{Name: c.Name, Image: c.Image}
				changed = true
			}
			if cs.Image == "" {
				cs.Image = c.Image
			}
			obs := states[c.Name]
			if previousContainer(pod, obs) {
				obs = ContainerSyncState{Phase: corev1.PodPending}
			}
			if applyContainerObservation(&cs, obs, phase) {
				changed = true
			}
			s.noteRunStart(pod, &cs)
			ready := obs.Phase == corev1.PodRunning && !isCrashLooping(&cs) &&
				(obs.Health == "" || obs.Health == "healthy")
			if cs.Ready != ready {
				cs.Ready = ready
				changed = true
			}
			statuses = append(statuses, cs)
		}
		if len(statuses) != len(pod.Status.ContainerStatuses) {
			changed = true
		} else {
			for i := range statuses {
				if statuses[i].Name != pod.Status.ContainerStatuses[i].Name {
					changed = true
					break
				}
			}
		}
		pod.Status.ContainerStatuses = statuses

		if syncPodStartTime(pod) {
			changed = true
		}
		if syncPodConditions(pod) {
			changed = true
		}
		if changed {
			pod.ResourceVersion = s.incRV()
		}
		uid = pod.UID
		return changed, nil
	})
	if phaseChanged {
		if evType, reason, msg, ok := podPhaseEvent(ns, name, phase); ok {
			s.recordEvent("Pod", ns, name, uid, evType, reason, msg)
		}
	}
}

// UpdatePodRestartInfo updates a pod's per-container restart count and last
// exit info, synced from systemd unit state (NRestarts, ExecMainStatus).
// Separate from UpdatePodPhase since restart bookkeeping only makes sense
// for pods backed by a systemd unit with Restart=on-failure — the sync
// loop's Podman-fallback path (no matching unit) has no equivalent signal
// and doesn't call this.
func (s *Store) UpdatePodRestartInfo(ns, name string, restartCount int32, exitCode int32, hasExited bool) {
	_ = s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: ns, Name: name}
		pod, ok := s.pods[key]
		if !ok {
			return false, nil
		}
		if len(pod.Status.ContainerStatuses) == 0 && len(pod.Spec.Containers) > 0 {
			pod.Status.ContainerStatuses = make([]corev1.ContainerStatus, len(pod.Spec.Containers))
			for i, c := range pod.Spec.Containers {
				pod.Status.ContainerStatuses[i] = corev1.ContainerStatus{Name: c.Name, Image: c.Image}
			}
		}
		reason := "Completed"
		if exitCode != 0 {
			reason = "Error"
		}
		changed := false
		for i := range pod.Status.ContainerStatuses {
			cs := &pod.Status.ContainerStatuses[i]
			if cs.RestartCount != restartCount {
				cs.RestartCount = restartCount
				changed = true
			}
			// hasExited means the unit is in systemd's auto-restart window: the
			// container exited and systemd still reports that run's exit
			// status. That is the only moment the exit code is known (the
			// restart counter goes up only when the next run starts, and by
			// then the status reads 0), so the last state is recorded here,
			// once per ended run.
			if hasExited {
				startedAt := s.endedRunStart(pod, cs)
				if cs.State.Terminated != nil && !cs.State.Terminated.StartedAt.IsZero() {
					startedAt = cs.State.Terminated.StartedAt
				}
				existing := cs.LastTerminationState.Terminated
				if existing == nil || !existing.StartedAt.Equal(&startedAt) || existing.ExitCode != exitCode {
					finishedAt := metav1.Now()
					if cs.State.Terminated != nil && !cs.State.Terminated.FinishedAt.IsZero() {
						finishedAt = cs.State.Terminated.FinishedAt
					}
					cs.LastTerminationState = corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode:   exitCode,
							Reason:     reason,
							StartedAt:  startedAt,
							FinishedAt: finishedAt,
						},
					}
					changed = true
				}
			}

			// Mark not ready when crash-looping
			if restartCount > 0 && exitCode != 0 && cs.Ready {
				cs.Ready = false
				changed = true
			}
		}
		// The phase pass ran before this one and may have written the waiting
		// reason from the previous restart count; refresh it now that the new
		// restart and exit code are known.
		for i := range pod.Status.ContainerStatuses {
			cs := &pod.Status.ContainerStatuses[i]
			if cs.State.Waiting != nil {
				if w := waitingState(cs); w.Waiting.Reason != cs.State.Waiting.Reason {
					cs.State = w
					changed = true
				}
			}
		}
		if syncPodStartTime(pod) {
			changed = true
		}
		if syncPodConditions(pod) {
			changed = true
		}
		if changed {
			pod.ResourceVersion = s.incRV()
		}
		return changed, nil
	})
}

// syncPodConditions derives the pod conditions from the container statuses:
// PodScheduled and Initialized are always true on the single node, Ready and
// ContainersReady follow the containers' ready flags. kubectl run --attach
// (and kubectl wait --for=condition=Ready) wait for Ready=True, so a pod
// without conditions was never attachable. A condition keeps its
// lastTransitionTime while its status does not change. Reports whether
// anything changed.
func syncPodConditions(pod *corev1.Pod) bool {
	ready := len(pod.Status.ContainerStatuses) > 0
	for _, cs := range pod.Status.ContainerStatuses {
		if !cs.Ready {
			ready = false
		}
	}
	want := map[corev1.PodConditionType]bool{
		corev1.PodScheduled:    true,
		corev1.PodInitialized:  true,
		corev1.ContainersReady: ready,
		corev1.PodReady:        ready,
	}
	order := []corev1.PodConditionType{corev1.PodScheduled, corev1.PodInitialized, corev1.ContainersReady, corev1.PodReady}
	existing := make(map[corev1.PodConditionType]corev1.PodCondition, len(pod.Status.Conditions))
	for _, c := range pod.Status.Conditions {
		existing[c.Type] = c
	}
	changed := len(pod.Status.Conditions) != len(order)
	now := metav1.Now()
	conds := make([]corev1.PodCondition, 0, len(order))
	for _, t := range order {
		status := corev1.ConditionFalse
		if want[t] {
			status = corev1.ConditionTrue
		}
		c, ok := existing[t]
		if !ok || c.Status != status {
			c = corev1.PodCondition{Type: t, Status: status, LastTransitionTime: now}
			changed = true
		}
		conds = append(conds, c)
	}
	if changed {
		pod.Status.Conditions = conds
	}
	return changed
}

// podPhaseEvent maps a pod phase transition to an event; Pending isn't
// reported since every pod passes through it and it carries no information.
func podPhaseEvent(ns, name string, phase corev1.PodPhase) (eventType, reason, message string, ok bool) {
	switch phase {
	case corev1.PodRunning:
		return corev1.EventTypeNormal, "Started", fmt.Sprintf("Started pod %s/%s", ns, name), true
	case corev1.PodSucceeded:
		return corev1.EventTypeNormal, "Completed", fmt.Sprintf("Pod %s/%s completed successfully", ns, name), true
	case corev1.PodFailed:
		return corev1.EventTypeWarning, "Failed", fmt.Sprintf("Pod %s/%s failed", ns, name), true
	default:
		return "", "", "", false
	}
}

// PodReady reports the pod's Ready condition. Deployment readiness counts
// Ready pods: a Running pod whose container is still starting (or about to be
// replaced) is not available yet.
func PodReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func runKey(pod *corev1.Pod, container string) string {
	return pod.Namespace + "/" + pod.Name + "/" + container
}

// forgetRunStarts drops the remembered run starts of a deleted pod. Caller
// holds s.mu.
func (s *Store) forgetRunStarts(key types.NamespacedName) {
	prefix := key.Namespace + "/" + key.Name + "/"
	for k := range s.runStarts {
		if strings.HasPrefix(k, prefix) {
			delete(s.runStarts, k)
		}
	}
}

// runStart is the start of a container's current run and of the run before
// it: the sync pass may already see the new run before the restart that
// ended the previous one is recorded.
type runStart struct {
	cur, prev metav1.Time
}

// noteRunStart remembers the start of a running container's current run.
// Caller holds s.mu.
func (s *Store) noteRunStart(pod *corev1.Pod, cs *corev1.ContainerStatus) {
	if cs.State.Running == nil || cs.State.Running.StartedAt.IsZero() {
		return
	}
	k := runKey(pod, cs.Name)
	r := s.runStarts[k]
	if !r.cur.Equal(&cs.State.Running.StartedAt) {
		r.prev, r.cur = r.cur, cs.State.Running.StartedAt
		s.runStarts[k] = r
	}
}

// endedRunStart is the start of the run a newly recorded restart ended: the
// previous run when the container already runs again, else the current one.
func (s *Store) endedRunStart(pod *corev1.Pod, cs *corev1.ContainerStatus) metav1.Time {
	r := s.runStarts[runKey(pod, cs.Name)]
	if cs.State.Running != nil && r.cur.Equal(&cs.State.Running.StartedAt) {
		return r.prev
	}
	return r.cur
}

// ResetPodForRestart marks a pod whose unit is being restarted for a new
// template as a new pod, the way Kubernetes replaces the pod on a rollout:
// creation time now, status back to Pending with no container statuses. Until
// podman reports a container that started after this moment, the old one is
// ignored (previousContainer), so the pod is not Ready and kubectl rollout
// status keeps waiting.
func (s *Store) ResetPodForRestart(ns, name string) {
	_ = s.mutate(func() (bool, error) {
		pod, ok := s.pods[types.NamespacedName{Namespace: ns, Name: name}]
		if !ok {
			return false, nil
		}
		pod.CreationTimestamp = metav1.Now()
		pod.Status = corev1.PodStatus{Phase: corev1.PodPending}
		syncPodConditions(pod)
		pod.ResourceVersion = s.incRV()
		return true, nil
	})
}

// previousContainer reports whether an observed container started before the
// pod was created: then it is the container of the pod this one replaced
// (still running until its unit restarts) and says nothing about this pod.
// Podman reports start times in whole seconds, hence the truncation.
func previousContainer(pod *corev1.Pod, obs ContainerSyncState) bool {
	return obs.Seen && !obs.StartedAt.IsZero() &&
		obs.StartedAt.Before(pod.CreationTimestamp.Time.Truncate(time.Second))
}

// --- Services ---

// AllServices returns all services across all namespaces.
func (s *Store) AllServices() []*corev1.Service {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*corev1.Service, 0, len(s.services))
	for _, svc := range s.services {
		result = append(result, svc.DeepCopy())
	}
	return result
}

// Services returns all services in a namespace.
func (s *Store) Services(ns string) []*corev1.Service {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*corev1.Service
	for key, svc := range s.services {
		if key.Namespace == ns {
			result = append(result, svc.DeepCopy())
		}
	}
	return result
}

// CreateService creates a service.
func (s *Store) CreateService(svc *corev1.Service) (*corev1.Service, error) {
	var out *corev1.Service
	err := s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}
		if _, ok := s.services[key]; ok {
			return false, fmt.Errorf("service %q already exists in namespace %q", svc.Name, svc.Namespace)
		}
		svc.ResourceVersion = s.incRV()
		assignUID(svc)
		if svc.Labels == nil {
			svc.Labels = make(map[string]string)
		}
		if svc.CreationTimestamp.IsZero() {
			svc.CreationTimestamp = metav1.Now()
		}
		defaultServiceFields(svc)
		s.services[key] = svc
		out = svc.DeepCopy()
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// defaultServiceFields fills in the standard fields a real Kubernetes API
// server would populate on a Service so k8s-aware clients (kubectl's table
// printer, age logic) render correctly. Without these, `kubectl get svc`
// shows a blank CLUSTER-IP, AGE <unknown>, and a truncated PORT(S) column.
// See tic-e014. All three are only defaulted when unset, so an explicit
// value (e.g. clusterIP: None, or a chosen protocol) is preserved.
func defaultServiceFields(svc *corev1.Service) {
	if svc.CreationTimestamp.IsZero() {
		svc.CreationTimestamp = metav1.Now()
	}
	// q8s has no ClusterIP virtual-IP routing: a ClusterIP Service is a DNS
	// alias + port map on the namespace network (see docs/configuration.md),
	// which is exactly headless-Service semantics. Report clusterIP: None —
	// truthful, a legal value, and enough for clients to render the column —
	// rather than fabricating an unroutable fake IP. An explicit value from
	// the client is left untouched.
	if svc.Spec.ClusterIP == "" {
		svc.Spec.ClusterIP = corev1.ClusterIPNone
	}
	if len(svc.Spec.ClusterIPs) == 0 {
		svc.Spec.ClusterIPs = []string{svc.Spec.ClusterIP}
	}
	// Real k8s admission defaults an unset port protocol to TCP; without it
	// kubectl can't build the "port:nodePort/proto" PORT(S) string.
	for i := range svc.Spec.Ports {
		if svc.Spec.Ports[i].Protocol == "" {
			svc.Spec.Ports[i].Protocol = corev1.ProtocolTCP
		}
	}
}

// GetService gets a service by namespace and name.
func (s *Store) GetService(ns, name string) (*corev1.Service, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := types.NamespacedName{Namespace: ns, Name: name}
	svc, ok := s.services[key]
	if !ok {
		return nil, fmt.Errorf("service %q not found in namespace %q", name, ns)
	}
	return svc.DeepCopy(), nil
}

// DeleteService deletes a service.
func (s *Store) DeleteService(ns, name string) error {
	return s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: ns, Name: name}
		if _, ok := s.services[key]; !ok {
			return false, fmt.Errorf("service %q not found in namespace %q", name, ns)
		}
		delete(s.services, key)
		// A delete is a write: it gets its own resourceVersion, as in Kubernetes.
		s.incRV()
		return true, nil
	})
}

// UpdateService replaces a service in-place, preserving UID.
func (s *Store) UpdateService(svc *corev1.Service) (*corev1.Service, error) {
	var out *corev1.Service
	err := s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}
		existing, ok := s.services[key]
		if !ok {
			return false, fmt.Errorf("service %q not found in namespace %q", svc.Name, svc.Namespace)
		}
		svc.UID = existing.UID
		svc.ResourceVersion = s.incRV()
		// Preserve the original creation time across updates (a full-object PUT
		// or the post-create NodePort re-store would otherwise blank it), then
		// re-default clusterIP/protocol so an update can't strip them either.
		if svc.CreationTimestamp.IsZero() {
			svc.CreationTimestamp = existing.CreationTimestamp
		}
		defaultServiceFields(svc)
		s.services[key] = svc
		out = svc.DeepCopy()
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// --- PVCs ---

// AllPVCs returns all PVCs across all namespaces.
func (s *Store) AllPVCs() []*corev1.PersistentVolumeClaim {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*corev1.PersistentVolumeClaim, 0, len(s.pvcs))
	for _, p := range s.pvcs {
		result = append(result, p.DeepCopy())
	}
	return result
}

// PVCs returns all PVCs in a namespace.
func (s *Store) PVCs(ns string) []*corev1.PersistentVolumeClaim {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*corev1.PersistentVolumeClaim
	for key, pvc := range s.pvcs {
		if key.Namespace == ns {
			result = append(result, pvc.DeepCopy())
		}
	}
	return result
}

// CreatePVC creates a PVC.
func (s *Store) CreatePVC(pvc *corev1.PersistentVolumeClaim) (*corev1.PersistentVolumeClaim, error) {
	var out *corev1.PersistentVolumeClaim
	err := s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: pvc.Namespace, Name: pvc.Name}
		if _, ok := s.pvcs[key]; ok {
			return false, fmt.Errorf("PVC %q already exists in namespace %q", pvc.Name, pvc.Namespace)
		}
		pvc.ResourceVersion = s.incRV()
		assignUID(pvc)
		if pvc.CreationTimestamp.IsZero() {
			pvc.CreationTimestamp = metav1.Now()
		}
		if pvc.Labels == nil {
			pvc.Labels = make(map[string]string)
		}
		// Default the storage class the way k8s's DefaultStorageClass admission
		// plugin would: "standard" carries the is-default-class annotation, so a
		// claim without an explicit class lands on it.
		if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName == "" {
			class := quadlet.StorageClassStandard
			pvc.Spec.StorageClassName = &class
		}
		// Immediate binding: there is no asynchronous provisioner — the quadlet
		// volume file is the provisioned volume — so the claim is bound as soon
		// as it exists. hostpath claims have no named volume to point at.
		pvc.Status.Phase = corev1.ClaimBound
		if *pvc.Spec.StorageClassName != quadlet.StorageClassHostPath {
			pvc.Spec.VolumeName = quadlet.PVCVolumeName(pvc.Namespace, pvc.Name)
		}
		s.pvcs[key] = pvc
		out = pvc.DeepCopy()
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	msg := "Bound (hostpath backing)"
	if pvc.Spec.VolumeName != "" {
		msg = fmt.Sprintf("Bound to podman volume %q", pvc.Spec.VolumeName)
	}
	s.recordEvent("PersistentVolumeClaim", pvc.Namespace, pvc.Name, pvc.UID,
		corev1.EventTypeNormal, "ProvisioningSucceeded", msg)
	return out, nil
}

// GetPVC gets a PVC by namespace and name.
func (s *Store) GetPVC(ns, name string) (*corev1.PersistentVolumeClaim, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := types.NamespacedName{Namespace: ns, Name: name}
	pvc, ok := s.pvcs[key]
	if !ok {
		return nil, fmt.Errorf("PVC %q not found in namespace %q", name, ns)
	}
	return pvc.DeepCopy(), nil
}

// DeletePVC deletes a PVC.
func (s *Store) DeletePVC(ns, name string) error {
	return s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: ns, Name: name}
		if _, ok := s.pvcs[key]; !ok {
			return false, fmt.Errorf("PVC %q not found in namespace %q", name, ns)
		}
		delete(s.pvcs, key)
		// A delete is a write: it gets its own resourceVersion, as in Kubernetes.
		s.incRV()
		return true, nil
	})
}

// UpdatePVC replaces a PVC in-place, preserving UID.
func (s *Store) UpdatePVC(pvc *corev1.PersistentVolumeClaim) (*corev1.PersistentVolumeClaim, error) {
	var out *corev1.PersistentVolumeClaim
	err := s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: pvc.Namespace, Name: pvc.Name}
		existing, ok := s.pvcs[key]
		if !ok {
			return false, fmt.Errorf("PVC %q not found in namespace %q", pvc.Name, pvc.Namespace)
		}
		pvc.UID = existing.UID
		if pvc.CreationTimestamp.IsZero() {
			pvc.CreationTimestamp = existing.CreationTimestamp
		}
		pvc.ResourceVersion = s.incRV()
		s.pvcs[key] = pvc
		out = pvc.DeepCopy()
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// --- ConfigMaps ---

// AllConfigMaps returns all configmaps across all namespaces.
func (s *Store) AllConfigMaps() []*corev1.ConfigMap {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*corev1.ConfigMap, 0, len(s.configmaps))
	for _, cm := range s.configmaps {
		result = append(result, cm.DeepCopy())
	}
	return result
}

// ConfigMaps returns all configmaps in a namespace.
func (s *Store) ConfigMaps(ns string) []*corev1.ConfigMap {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*corev1.ConfigMap
	for key, cm := range s.configmaps {
		if key.Namespace == ns {
			result = append(result, cm.DeepCopy())
		}
	}
	return result
}

// CreateConfigMap creates a configmap.
func (s *Store) CreateConfigMap(cm *corev1.ConfigMap) (*corev1.ConfigMap, error) {
	var out *corev1.ConfigMap
	err := s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: cm.Namespace, Name: cm.Name}
		if _, ok := s.configmaps[key]; ok {
			return false, fmt.Errorf("configmap %q already exists in namespace %q", cm.Name, cm.Namespace)
		}
		cm.ResourceVersion = s.incRV()
		assignUID(cm)
		if cm.Labels == nil {
			cm.Labels = make(map[string]string)
		}
		if cm.CreationTimestamp.IsZero() {
			cm.CreationTimestamp = metav1.Now()
		}
		s.configmaps[key] = cm
		out = cm.DeepCopy()
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateConfigMap replaces a configmap's data in-place.
func (s *Store) UpdateConfigMap(cm *corev1.ConfigMap) (*corev1.ConfigMap, error) {
	var out *corev1.ConfigMap
	err := s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: cm.Namespace, Name: cm.Name}
		existing, ok := s.configmaps[key]
		if !ok {
			return false, fmt.Errorf("configmap %q not found in namespace %q", cm.Name, cm.Namespace)
		}
		cm.UID = existing.UID
		if cm.CreationTimestamp.IsZero() {
			cm.CreationTimestamp = existing.CreationTimestamp
		}
		cm.ResourceVersion = s.incRV()
		s.configmaps[key] = cm
		out = cm.DeepCopy()
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetConfigMap gets a configmap.
func (s *Store) GetConfigMap(ns, name string) (*corev1.ConfigMap, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := types.NamespacedName{Namespace: ns, Name: name}
	cm, ok := s.configmaps[key]
	if !ok {
		return nil, fmt.Errorf("configmap %q not found in namespace %q", name, ns)
	}
	return cm.DeepCopy(), nil
}

// DeleteConfigMap deletes a configmap.
func (s *Store) DeleteConfigMap(ns, name string) error {
	return s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: ns, Name: name}
		if _, ok := s.configmaps[key]; !ok {
			return false, fmt.Errorf("configmap %q not found in namespace %q", name, ns)
		}
		delete(s.configmaps, key)
		// A delete is a write: it gets its own resourceVersion, as in Kubernetes.
		s.incRV()
		return true, nil
	})
}

// --- Secrets ---

// AllSecrets returns all secrets across all namespaces.
func (s *Store) AllSecrets() []*corev1.Secret {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*corev1.Secret, 0, len(s.secrets))
	for _, sec := range s.secrets {
		result = append(result, sec.DeepCopy())
	}
	return result
}

// Secrets returns all secrets in a namespace.
func (s *Store) Secrets(ns string) []*corev1.Secret {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*corev1.Secret
	for key, sec := range s.secrets {
		if key.Namespace == ns {
			result = append(result, sec.DeepCopy())
		}
	}
	return result
}

// CreateSecret creates a secret.
func (s *Store) CreateSecret(sec *corev1.Secret) (*corev1.Secret, error) {
	var out *corev1.Secret
	err := s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: sec.Namespace, Name: sec.Name}
		if _, ok := s.secrets[key]; ok {
			return false, fmt.Errorf("secret %q already exists in namespace %q", sec.Name, sec.Namespace)
		}
		sec.ResourceVersion = s.incRV()
		assignUID(sec)
		if sec.Labels == nil {
			sec.Labels = make(map[string]string)
		}
		if sec.CreationTimestamp.IsZero() {
			sec.CreationTimestamp = metav1.Now()
		}
		s.secrets[key] = sec
		out = sec.DeepCopy()
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetSecret gets a secret.
func (s *Store) GetSecret(ns, name string) (*corev1.Secret, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := types.NamespacedName{Namespace: ns, Name: name}
	sec, ok := s.secrets[key]
	if !ok {
		return nil, fmt.Errorf("secret %q not found in namespace %q", name, ns)
	}
	return sec.DeepCopy(), nil
}

// DeleteSecret deletes a secret.
func (s *Store) DeleteSecret(ns, name string) error {
	return s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: ns, Name: name}
		if _, ok := s.secrets[key]; !ok {
			return false, fmt.Errorf("secret %q not found in namespace %q", name, ns)
		}
		delete(s.secrets, key)
		// A delete is a write: it gets its own resourceVersion, as in Kubernetes.
		s.incRV()
		return true, nil
	})
}

// UpdateSecret replaces a secret in-place, preserving UID.
func (s *Store) UpdateSecret(sec *corev1.Secret) (*corev1.Secret, error) {
	var out *corev1.Secret
	err := s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: sec.Namespace, Name: sec.Name}
		existing, ok := s.secrets[key]
		if !ok {
			return false, fmt.Errorf("secret %q not found in namespace %q", sec.Name, sec.Namespace)
		}
		sec.UID = existing.UID
		if sec.CreationTimestamp.IsZero() {
			sec.CreationTimestamp = existing.CreationTimestamp
		}
		sec.ResourceVersion = s.incRV()
		s.secrets[key] = sec
		out = sec.DeepCopy()
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// --- ServiceAccounts ---

// AllServiceAccounts returns all ServiceAccounts across all namespaces.
func (s *Store) AllServiceAccounts() []*corev1.ServiceAccount {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*corev1.ServiceAccount, 0, len(s.serviceAccounts))
	for _, sa := range s.serviceAccounts {
		result = append(result, sa.DeepCopy())
	}
	return result
}

// ServiceAccounts returns all ServiceAccounts in a namespace.
func (s *Store) ServiceAccounts(ns string) []*corev1.ServiceAccount {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*corev1.ServiceAccount
	for key, sa := range s.serviceAccounts {
		if key.Namespace == ns {
			result = append(result, sa.DeepCopy())
		}
	}
	return result
}

// CreateServiceAccount creates a ServiceAccount.
func (s *Store) CreateServiceAccount(sa *corev1.ServiceAccount) (*corev1.ServiceAccount, error) {
	var out *corev1.ServiceAccount
	err := s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: sa.Namespace, Name: sa.Name}
		if _, ok := s.serviceAccounts[key]; ok {
			return false, fmt.Errorf("serviceaccount %q already exists in namespace %q", sa.Name, sa.Namespace)
		}
		sa.ResourceVersion = s.incRV()
		assignUID(sa)
		if sa.Labels == nil {
			sa.Labels = make(map[string]string)
		}
		if sa.CreationTimestamp.IsZero() {
			sa.CreationTimestamp = metav1.Now()
		}
		s.serviceAccounts[key] = sa
		out = sa.DeepCopy()
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetServiceAccount gets a ServiceAccount.
func (s *Store) GetServiceAccount(ns, name string) (*corev1.ServiceAccount, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sa, ok := s.serviceAccounts[types.NamespacedName{Namespace: ns, Name: name}]
	if !ok {
		return nil, fmt.Errorf("serviceaccount %q not found in namespace %q", name, ns)
	}
	return sa.DeepCopy(), nil
}

// DeleteServiceAccount deletes a ServiceAccount.
func (s *Store) DeleteServiceAccount(ns, name string) error {
	return s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: ns, Name: name}
		if _, ok := s.serviceAccounts[key]; !ok {
			return false, fmt.Errorf("serviceaccount %q not found in namespace %q", name, ns)
		}
		delete(s.serviceAccounts, key)
		// A delete is a write: it gets its own resourceVersion, as in Kubernetes.
		s.incRV()
		return true, nil
	})
}

// UpdateServiceAccount replaces a ServiceAccount in-place, preserving UID and
// creation timestamp.
func (s *Store) UpdateServiceAccount(sa *corev1.ServiceAccount) (*corev1.ServiceAccount, error) {
	var out *corev1.ServiceAccount
	err := s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: sa.Namespace, Name: sa.Name}
		existing, ok := s.serviceAccounts[key]
		if !ok {
			return false, fmt.Errorf("serviceaccount %q not found in namespace %q", sa.Name, sa.Namespace)
		}
		sa.UID = existing.UID
		if sa.CreationTimestamp.IsZero() {
			sa.CreationTimestamp = existing.CreationTimestamp
		}
		sa.ResourceVersion = s.incRV()
		s.serviceAccounts[key] = sa
		out = sa.DeepCopy()
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// --- Jobs ---

// AllJobs returns all jobs across all namespaces.
func (s *Store) AllJobs() []*batchv1.Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*batchv1.Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		result = append(result, j.DeepCopy())
	}
	return result
}

// Jobs returns all jobs in a namespace.
func (s *Store) Jobs(ns string) []*batchv1.Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*batchv1.Job
	for key, j := range s.jobs {
		if key.Namespace == ns {
			result = append(result, j.DeepCopy())
		}
	}
	return result
}

// CreateJob creates a job.
func (s *Store) CreateJob(job *batchv1.Job) (*batchv1.Job, error) {
	var out *batchv1.Job
	err := s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: job.Namespace, Name: job.Name}
		if _, ok := s.jobs[key]; ok {
			return false, fmt.Errorf("job %q already exists in namespace %q", job.Name, job.Namespace)
		}
		job.ResourceVersion = s.incRV()
		assignUID(job)
		if job.Labels == nil {
			job.Labels = make(map[string]string)
		}
		if job.CreationTimestamp.IsZero() {
			job.CreationTimestamp = metav1.Now()
		}
		s.jobs[key] = job
		out = job.DeepCopy()
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	s.recordEvent("Job", job.Namespace, job.Name, job.UID, corev1.EventTypeNormal, "Created",
		fmt.Sprintf("Created job %s/%s", job.Namespace, job.Name))
	return out, nil
}

// GetJob gets a job.
func (s *Store) GetJob(ns, name string) (*batchv1.Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := types.NamespacedName{Namespace: ns, Name: name}
	job, ok := s.jobs[key]
	if !ok {
		return nil, fmt.Errorf("job %q not found in namespace %q", name, ns)
	}
	return job.DeepCopy(), nil
}

// DeleteJob deletes a job.
func (s *Store) DeleteJob(ns, name string) error {
	var uid types.UID
	err := s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: ns, Name: name}
		job, ok := s.jobs[key]
		if !ok {
			return false, fmt.Errorf("job %q not found in namespace %q", name, ns)
		}
		uid = job.UID
		delete(s.jobs, key)
		// A delete is a write: it gets its own resourceVersion, as in Kubernetes.
		s.incRV()
		return true, nil
	})
	if err != nil {
		return err
	}
	s.recordEvent("Job", ns, name, uid, corev1.EventTypeNormal, "Deleted",
		fmt.Sprintf("Deleted job %s/%s", ns, name))
	return nil
}

// UpdateJob replaces a job in-place, preserving UID.
func (s *Store) UpdateJob(job *batchv1.Job) (*batchv1.Job, error) {
	var out *batchv1.Job
	err := s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: job.Namespace, Name: job.Name}
		existing, ok := s.jobs[key]
		if !ok {
			return false, fmt.Errorf("job %q not found in namespace %q", job.Name, job.Namespace)
		}
		job.UID = existing.UID
		job.ResourceVersion = s.incRV()
		s.jobs[key] = job
		out = job.DeepCopy()
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateJobStatus updates a job's active/succeeded/failed counts.
func (s *Store) UpdateJobStatus(ns, name string, active, succeeded, failed int32) {
	changed := false
	var uid types.UID
	_ = s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: ns, Name: name}
		job, ok := s.jobs[key]
		if !ok {
			return false, nil
		}
		changed = job.Status.Active != active || job.Status.Succeeded != succeeded || job.Status.Failed != failed
		if changed {
			job.Status.Active = active
			job.Status.Succeeded = succeeded
			job.Status.Failed = failed
			job.ResourceVersion = s.incRV()
		}
		uid = job.UID
		return changed, nil
	})
	if changed {
		switch {
		case failed > 0:
			s.recordEvent("Job", ns, name, uid, corev1.EventTypeWarning, "Failed",
				fmt.Sprintf("Job %s/%s failed", ns, name))
		case succeeded > 0:
			s.recordEvent("Job", ns, name, uid, corev1.EventTypeNormal, "Completed",
				fmt.Sprintf("Job %s/%s completed successfully", ns, name))
		case active > 0:
			s.recordEvent("Job", ns, name, uid, corev1.EventTypeNormal, "Started",
				fmt.Sprintf("Job %s/%s started", ns, name))
		}
	}
}

// --- CronJobs ---

// AllCronJobs returns all cronjobs across all namespaces.
func (s *Store) AllCronJobs() []*batchv1.CronJob {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*batchv1.CronJob, 0, len(s.cronjobs))
	for _, cj := range s.cronjobs {
		result = append(result, cj.DeepCopy())
	}
	return result
}

// CronJobs returns all cronjobs in a namespace.
func (s *Store) CronJobs(ns string) []*batchv1.CronJob {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*batchv1.CronJob
	for key, cj := range s.cronjobs {
		if key.Namespace == ns {
			result = append(result, cj.DeepCopy())
		}
	}
	return result
}

// CreateCronJob creates a cronjob.
func (s *Store) CreateCronJob(cj *batchv1.CronJob) (*batchv1.CronJob, error) {
	var out *batchv1.CronJob
	err := s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: cj.Namespace, Name: cj.Name}
		if _, ok := s.cronjobs[key]; ok {
			return false, fmt.Errorf("cronjob %q already exists in namespace %q", cj.Name, cj.Namespace)
		}
		cj.ResourceVersion = s.incRV()
		assignUID(cj)
		if cj.Labels == nil {
			cj.Labels = make(map[string]string)
		}
		if cj.CreationTimestamp.IsZero() {
			cj.CreationTimestamp = metav1.Now()
		}
		s.cronjobs[key] = cj
		out = cj.DeepCopy()
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetCronJob gets a cronjob.
func (s *Store) GetCronJob(ns, name string) (*batchv1.CronJob, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := types.NamespacedName{Namespace: ns, Name: name}
	cj, ok := s.cronjobs[key]
	if !ok {
		return nil, fmt.Errorf("cronjob %q not found in namespace %q", name, ns)
	}
	return cj.DeepCopy(), nil
}

// DeleteCronJob deletes a cronjob.
func (s *Store) DeleteCronJob(ns, name string) error {
	return s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: ns, Name: name}
		if _, ok := s.cronjobs[key]; !ok {
			return false, fmt.Errorf("cronjob %q not found in namespace %q", name, ns)
		}
		delete(s.cronjobs, key)
		// A delete is a write: it gets its own resourceVersion, as in Kubernetes.
		s.incRV()
		return true, nil
	})
}

// UpdateCronJob replaces a cronjob in-place, preserving UID.
func (s *Store) UpdateCronJob(cj *batchv1.CronJob) (*batchv1.CronJob, error) {
	var out *batchv1.CronJob
	err := s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: cj.Namespace, Name: cj.Name}
		existing, ok := s.cronjobs[key]
		if !ok {
			return false, fmt.Errorf("cronjob %q not found in namespace %q", cj.Name, cj.Namespace)
		}
		cj.UID = existing.UID
		cj.ResourceVersion = s.incRV()
		s.cronjobs[key] = cj
		out = cj.DeepCopy()
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// --- Ingresses ---

// AllIngresses returns all ingresses across all namespaces.
func (s *Store) AllIngresses() []*networkingv1.Ingress {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*networkingv1.Ingress, 0, len(s.ingresses))
	for _, ing := range s.ingresses {
		result = append(result, ing.DeepCopy())
	}
	return result
}

// Ingresses returns all ingresses in a namespace.
func (s *Store) Ingresses(ns string) []*networkingv1.Ingress {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*networkingv1.Ingress
	for key, ing := range s.ingresses {
		if key.Namespace == ns {
			result = append(result, ing.DeepCopy())
		}
	}
	return result
}

// CreateIngress creates an ingress.
func (s *Store) CreateIngress(ing *networkingv1.Ingress) (*networkingv1.Ingress, error) {
	var out *networkingv1.Ingress
	err := s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: ing.Namespace, Name: ing.Name}
		if _, ok := s.ingresses[key]; ok {
			return false, fmt.Errorf("ingress %q already exists in namespace %q", ing.Name, ing.Namespace)
		}
		ing.ResourceVersion = s.incRV()
		assignUID(ing)
		if ing.Labels == nil {
			ing.Labels = make(map[string]string)
		}
		if ing.CreationTimestamp.IsZero() {
			ing.CreationTimestamp = metav1.Now()
		}
		s.ingresses[key] = ing
		out = ing.DeepCopy()
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetIngress gets an ingress.
func (s *Store) GetIngress(ns, name string) (*networkingv1.Ingress, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := types.NamespacedName{Namespace: ns, Name: name}
	ing, ok := s.ingresses[key]
	if !ok {
		return nil, fmt.Errorf("ingress %q not found in namespace %q", name, ns)
	}
	return ing.DeepCopy(), nil
}

// DeleteIngress deletes an ingress.
func (s *Store) DeleteIngress(ns, name string) error {
	return s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: ns, Name: name}
		if _, ok := s.ingresses[key]; !ok {
			return false, fmt.Errorf("ingress %q not found in namespace %q", name, ns)
		}
		delete(s.ingresses, key)
		// A delete is a write: it gets its own resourceVersion, as in Kubernetes.
		s.incRV()
		return true, nil
	})
}

// UpdateIngress replaces an ingress in-place, preserving UID.
func (s *Store) UpdateIngress(ing *networkingv1.Ingress) (*networkingv1.Ingress, error) {
	var out *networkingv1.Ingress
	err := s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: ing.Namespace, Name: ing.Name}
		existing, ok := s.ingresses[key]
		if !ok {
			return false, fmt.Errorf("ingress %q not found in namespace %q", ing.Name, ing.Namespace)
		}
		ing.UID = existing.UID
		ing.ResourceVersion = s.incRV()
		s.ingresses[key] = ing
		out = ing.DeepCopy()
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// --- Middlewares ---

// AllMiddlewares returns all Middlewares across all namespaces.
func (s *Store) AllMiddlewares() []*traefik.Middleware {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*traefik.Middleware, 0, len(s.middlewares))
	for _, mw := range s.middlewares {
		result = append(result, mw.DeepCopy())
	}
	return result
}

// Middlewares returns all Middlewares in a namespace.
func (s *Store) Middlewares(ns string) []*traefik.Middleware {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*traefik.Middleware
	for key, mw := range s.middlewares {
		if key.Namespace == ns {
			result = append(result, mw.DeepCopy())
		}
	}
	return result
}

// CreateMiddleware creates a Middleware.
func (s *Store) CreateMiddleware(mw *traefik.Middleware) (*traefik.Middleware, error) {
	var out *traefik.Middleware
	err := s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: mw.Namespace, Name: mw.Name}
		if _, ok := s.middlewares[key]; ok {
			return false, fmt.Errorf("middleware %q already exists in namespace %q", mw.Name, mw.Namespace)
		}
		mw.ResourceVersion = s.incRV()
		assignUID(mw)
		if mw.Labels == nil {
			mw.Labels = make(map[string]string)
		}
		if mw.CreationTimestamp.IsZero() {
			mw.CreationTimestamp = metav1.Now()
		}
		s.middlewares[key] = mw
		out = mw.DeepCopy()
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetMiddleware gets a Middleware.
func (s *Store) GetMiddleware(ns, name string) (*traefik.Middleware, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	mw, ok := s.middlewares[types.NamespacedName{Namespace: ns, Name: name}]
	if !ok {
		return nil, fmt.Errorf("middleware %q not found in namespace %q", name, ns)
	}
	return mw.DeepCopy(), nil
}

// DeleteMiddleware deletes a Middleware.
func (s *Store) DeleteMiddleware(ns, name string) error {
	return s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: ns, Name: name}
		if _, ok := s.middlewares[key]; !ok {
			return false, fmt.Errorf("middleware %q not found in namespace %q", name, ns)
		}
		delete(s.middlewares, key)
		// A delete is a write: it gets its own resourceVersion, as in Kubernetes.
		s.incRV()
		return true, nil
	})
}

// UpdateMiddleware replaces a Middleware in-place, preserving UID.
func (s *Store) UpdateMiddleware(mw *traefik.Middleware) (*traefik.Middleware, error) {
	var out *traefik.Middleware
	err := s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: mw.Namespace, Name: mw.Name}
		existing, ok := s.middlewares[key]
		if !ok {
			return false, fmt.Errorf("middleware %q not found in namespace %q", mw.Name, mw.Namespace)
		}
		mw.UID = existing.UID
		if mw.CreationTimestamp.IsZero() {
			mw.CreationTimestamp = existing.CreationTimestamp
		}
		mw.ResourceVersion = s.incRV()
		s.middlewares[key] = mw
		out = mw.DeepCopy()
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// --- IngressRouteTCPs ---

// AllIngressRouteTCPs returns all IngressRouteTCPs across all namespaces.
func (s *Store) AllIngressRouteTCPs() []*traefik.IngressRouteTCP {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*traefik.IngressRouteTCP, 0, len(s.ingressRouteTCPs))
	for _, irt := range s.ingressRouteTCPs {
		result = append(result, irt.DeepCopy())
	}
	return result
}

// IngressRouteTCPs returns all IngressRouteTCPs in a namespace.
func (s *Store) IngressRouteTCPs(ns string) []*traefik.IngressRouteTCP {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*traefik.IngressRouteTCP
	for key, irt := range s.ingressRouteTCPs {
		if key.Namespace == ns {
			result = append(result, irt.DeepCopy())
		}
	}
	return result
}

// CreateIngressRouteTCP creates an IngressRouteTCP.
func (s *Store) CreateIngressRouteTCP(irt *traefik.IngressRouteTCP) (*traefik.IngressRouteTCP, error) {
	var out *traefik.IngressRouteTCP
	err := s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: irt.Namespace, Name: irt.Name}
		if _, ok := s.ingressRouteTCPs[key]; ok {
			return false, fmt.Errorf("ingressroutetcp %q already exists in namespace %q", irt.Name, irt.Namespace)
		}
		irt.ResourceVersion = s.incRV()
		assignUID(irt)
		if irt.Labels == nil {
			irt.Labels = make(map[string]string)
		}
		if irt.CreationTimestamp.IsZero() {
			irt.CreationTimestamp = metav1.Now()
		}
		s.ingressRouteTCPs[key] = irt
		out = irt.DeepCopy()
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetIngressRouteTCP gets an IngressRouteTCP.
func (s *Store) GetIngressRouteTCP(ns, name string) (*traefik.IngressRouteTCP, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	irt, ok := s.ingressRouteTCPs[types.NamespacedName{Namespace: ns, Name: name}]
	if !ok {
		return nil, fmt.Errorf("ingressroutetcp %q not found in namespace %q", name, ns)
	}
	return irt.DeepCopy(), nil
}

// DeleteIngressRouteTCP deletes an IngressRouteTCP.
func (s *Store) DeleteIngressRouteTCP(ns, name string) error {
	return s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: ns, Name: name}
		if _, ok := s.ingressRouteTCPs[key]; !ok {
			return false, fmt.Errorf("ingressroutetcp %q not found in namespace %q", name, ns)
		}
		delete(s.ingressRouteTCPs, key)
		// A delete is a write: it gets its own resourceVersion, as in Kubernetes.
		s.incRV()
		return true, nil
	})
}

// UpdateIngressRouteTCP replaces an IngressRouteTCP in-place, preserving UID.
func (s *Store) UpdateIngressRouteTCP(irt *traefik.IngressRouteTCP) (*traefik.IngressRouteTCP, error) {
	var out *traefik.IngressRouteTCP
	err := s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: irt.Namespace, Name: irt.Name}
		existing, ok := s.ingressRouteTCPs[key]
		if !ok {
			return false, fmt.Errorf("ingressroutetcp %q not found in namespace %q", irt.Name, irt.Namespace)
		}
		irt.UID = existing.UID
		if irt.CreationTimestamp.IsZero() {
			irt.CreationTimestamp = existing.CreationTimestamp
		}
		irt.ResourceVersion = s.incRV()
		s.ingressRouteTCPs[key] = irt
		out = irt.DeepCopy()
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// --- Deployments ---

// AllDeployments returns all deployments across all namespaces.
func (s *Store) AllDeployments() []*appsv1.Deployment {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*appsv1.Deployment, 0, len(s.deployments))
	for _, dep := range s.deployments {
		result = append(result, dep.DeepCopy())
	}
	return result
}

// Deployments returns all deployments in a namespace.
func (s *Store) Deployments(ns string) []*appsv1.Deployment {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*appsv1.Deployment
	for key, dep := range s.deployments {
		if key.Namespace == ns {
			result = append(result, dep.DeepCopy())
		}
	}
	return result
}

// CreateDeployment creates a deployment.
func (s *Store) CreateDeployment(dep *appsv1.Deployment) (*appsv1.Deployment, error) {
	var out *appsv1.Deployment
	err := s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: dep.Namespace, Name: dep.Name}
		if _, ok := s.deployments[key]; ok {
			return false, fmt.Errorf("deployment %q already exists in namespace %q", dep.Name, dep.Namespace)
		}
		dep.ResourceVersion = s.incRV()
		assignUID(dep)
		if dep.Labels == nil {
			dep.Labels = make(map[string]string)
		}
		if dep.CreationTimestamp.IsZero() {
			dep.CreationTimestamp = metav1.Now()
		}
		// A real API server stamps generation 1 on create and bumps it on every
		// spec change (see UpdateDeployment). kubectl rollout status gates on
		// status.observedGeneration >= metadata.generation before it looks at
		// replicas, so this must be populated or rollout status hangs. tic-773b.
		dep.Generation = 1
		s.deployments[key] = dep
		out = dep.DeepCopy()
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	s.recordEvent("Deployment", dep.Namespace, dep.Name, dep.UID, corev1.EventTypeNormal, "Created",
		fmt.Sprintf("Created deployment %s/%s", dep.Namespace, dep.Name))
	return out, nil
}

// UpdateDeployment replaces a deployment in-place, preserving UID.
func (s *Store) UpdateDeployment(dep *appsv1.Deployment) (*appsv1.Deployment, error) {
	var out *appsv1.Deployment
	err := s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: dep.Namespace, Name: dep.Name}
		existing, ok := s.deployments[key]
		if !ok {
			return false, fmt.Errorf("deployment %q not found in namespace %q", dep.Name, dep.Namespace)
		}
		dep.UID = existing.UID
		dep.ResourceVersion = s.incRV()
		// Preserve creationTimestamp across updates (a full-object PUT may omit
		// it), and manage metadata.generation like a real API server: bump it
		// only when the spec actually changed, carry it forward otherwise. This
		// is what status.observedGeneration is compared against, so getting it
		// right is what lets `kubectl rollout status` terminate. tic-773b.
		if dep.CreationTimestamp.IsZero() {
			dep.CreationTimestamp = existing.CreationTimestamp
		}
		if reflect.DeepEqual(dep.Spec, existing.Spec) {
			dep.Generation = existing.Generation
		} else {
			dep.Generation = existing.Generation + 1
		}
		s.deployments[key] = dep
		out = dep.DeepCopy()
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetDeployment gets a deployment.
func (s *Store) GetDeployment(ns, name string) (*appsv1.Deployment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := types.NamespacedName{Namespace: ns, Name: name}
	dep, ok := s.deployments[key]
	if !ok {
		return nil, fmt.Errorf("deployment %q not found in namespace %q", name, ns)
	}
	return dep.DeepCopy(), nil
}

// DeleteDeployment deletes a deployment.
func (s *Store) DeleteDeployment(ns, name string) error {
	var uid types.UID
	err := s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: ns, Name: name}
		dep, ok := s.deployments[key]
		if !ok {
			return false, fmt.Errorf("deployment %q not found in namespace %q", name, ns)
		}
		uid = dep.UID
		delete(s.deployments, key)
		// A delete is a write: it gets its own resourceVersion, as in Kubernetes.
		s.incRV()
		return true, nil
	})
	if err != nil {
		return err
	}
	s.recordEvent("Deployment", ns, name, uid, corev1.EventTypeNormal, "Deleted",
		fmt.Sprintf("Deleted deployment %s/%s", ns, name))
	return nil
}

// UpdateDeploymentStatus updates a deployment's ready/available replica counts.
func (s *Store) UpdateDeploymentStatus(ns, name string, ready int32) {
	changed := false
	var uid types.UID
	var desired int32
	_ = s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: ns, Name: name}
		dep, ok := s.deployments[key]
		if !ok {
			return false, nil
		}
		desired = 1
		if dep.Spec.Replicas != nil {
			desired = *dep.Spec.Replicas
		}
		// Update when the ready count changed OR when observedGeneration still
		// lags the current generation (a spec change — e.g. rollout restart —
		// with an unchanged replica count must still be "observed", or
		// `kubectl rollout status` waits forever). tic-773b.
		changed = dep.Status.ReadyReplicas != ready || dep.Status.ObservedGeneration != dep.Generation
		if changed {
			dep.Status.Replicas = desired
			dep.Status.ReadyReplicas = ready
			dep.Status.AvailableReplicas = ready
			dep.Status.UpdatedReplicas = desired
			dep.Status.ObservedGeneration = dep.Generation
			dep.ResourceVersion = s.incRV()
		}
		uid = dep.UID
		return changed, nil
	})
	if changed {
		s.recordEvent("Deployment", ns, name, uid, corev1.EventTypeNormal, "ScalingReplicaSet",
			fmt.Sprintf("Deployment %s/%s: %d/%d replicas ready", ns, name, ready, desired))
	}
}

// --- Roles ---

// AllRoles returns all Roles across all namespaces.
func (s *Store) AllRoles() []*rbacv1.Role {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*rbacv1.Role, 0, len(s.roles))
	for _, role := range s.roles {
		result = append(result, role.DeepCopy())
	}
	return result
}

// Roles returns all Roles in a namespace.
func (s *Store) Roles(ns string) []*rbacv1.Role {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*rbacv1.Role
	for key, role := range s.roles {
		if key.Namespace == ns {
			result = append(result, role.DeepCopy())
		}
	}
	return result
}

// CreateRole creates a Role.
func (s *Store) CreateRole(role *rbacv1.Role) (*rbacv1.Role, error) {
	var out *rbacv1.Role
	err := s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: role.Namespace, Name: role.Name}
		if _, ok := s.roles[key]; ok {
			return false, fmt.Errorf("role %q already exists in namespace %q", role.Name, role.Namespace)
		}
		role.ResourceVersion = s.incRV()
		assignUID(role)
		if role.Labels == nil {
			role.Labels = make(map[string]string)
		}
		if role.CreationTimestamp.IsZero() {
			role.CreationTimestamp = metav1.Now()
		}
		s.roles[key] = role
		out = role.DeepCopy()
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetRole gets a Role.
func (s *Store) GetRole(ns, name string) (*rbacv1.Role, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	role, ok := s.roles[types.NamespacedName{Namespace: ns, Name: name}]
	if !ok {
		return nil, fmt.Errorf("role %q not found in namespace %q", name, ns)
	}
	return role.DeepCopy(), nil
}

// DeleteRole deletes a Role.
func (s *Store) DeleteRole(ns, name string) error {
	return s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: ns, Name: name}
		if _, ok := s.roles[key]; !ok {
			return false, fmt.Errorf("role %q not found in namespace %q", name, ns)
		}
		delete(s.roles, key)
		// A delete is a write: it gets its own resourceVersion, as in Kubernetes.
		s.incRV()
		return true, nil
	})
}

// UpdateRole replaces a Role in-place, preserving UID and creation timestamp.
func (s *Store) UpdateRole(role *rbacv1.Role) (*rbacv1.Role, error) {
	var out *rbacv1.Role
	err := s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: role.Namespace, Name: role.Name}
		existing, ok := s.roles[key]
		if !ok {
			return false, fmt.Errorf("role %q not found in namespace %q", role.Name, role.Namespace)
		}
		role.UID = existing.UID
		if role.CreationTimestamp.IsZero() {
			role.CreationTimestamp = existing.CreationTimestamp
		}
		role.ResourceVersion = s.incRV()
		s.roles[key] = role
		out = role.DeepCopy()
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// --- RoleBindings ---

// AllRoleBindings returns all RoleBindings across all namespaces.
func (s *Store) AllRoleBindings() []*rbacv1.RoleBinding {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*rbacv1.RoleBinding, 0, len(s.roleBindings))
	for _, rb := range s.roleBindings {
		result = append(result, rb.DeepCopy())
	}
	return result
}

// RoleBindings returns all RoleBindings in a namespace.
func (s *Store) RoleBindings(ns string) []*rbacv1.RoleBinding {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*rbacv1.RoleBinding
	for key, rb := range s.roleBindings {
		if key.Namespace == ns {
			result = append(result, rb.DeepCopy())
		}
	}
	return result
}

// CreateRoleBinding creates a RoleBinding.
func (s *Store) CreateRoleBinding(rb *rbacv1.RoleBinding) (*rbacv1.RoleBinding, error) {
	var out *rbacv1.RoleBinding
	err := s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: rb.Namespace, Name: rb.Name}
		if _, ok := s.roleBindings[key]; ok {
			return false, fmt.Errorf("rolebinding %q already exists in namespace %q", rb.Name, rb.Namespace)
		}
		rb.ResourceVersion = s.incRV()
		assignUID(rb)
		if rb.Labels == nil {
			rb.Labels = make(map[string]string)
		}
		if rb.CreationTimestamp.IsZero() {
			rb.CreationTimestamp = metav1.Now()
		}
		s.roleBindings[key] = rb
		out = rb.DeepCopy()
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetRoleBinding gets a RoleBinding.
func (s *Store) GetRoleBinding(ns, name string) (*rbacv1.RoleBinding, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rb, ok := s.roleBindings[types.NamespacedName{Namespace: ns, Name: name}]
	if !ok {
		return nil, fmt.Errorf("rolebinding %q not found in namespace %q", name, ns)
	}
	return rb.DeepCopy(), nil
}

// DeleteRoleBinding deletes a RoleBinding.
func (s *Store) DeleteRoleBinding(ns, name string) error {
	return s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: ns, Name: name}
		if _, ok := s.roleBindings[key]; !ok {
			return false, fmt.Errorf("rolebinding %q not found in namespace %q", name, ns)
		}
		delete(s.roleBindings, key)
		// A delete is a write: it gets its own resourceVersion, as in Kubernetes.
		s.incRV()
		return true, nil
	})
}

// UpdateRoleBinding replaces a RoleBinding in-place, preserving UID and creation timestamp.
func (s *Store) UpdateRoleBinding(rb *rbacv1.RoleBinding) (*rbacv1.RoleBinding, error) {
	var out *rbacv1.RoleBinding
	err := s.mutate(func() (bool, error) {
		key := types.NamespacedName{Namespace: rb.Namespace, Name: rb.Name}
		existing, ok := s.roleBindings[key]
		if !ok {
			return false, fmt.Errorf("rolebinding %q not found in namespace %q", rb.Name, rb.Namespace)
		}
		rb.UID = existing.UID
		if rb.CreationTimestamp.IsZero() {
			rb.CreationTimestamp = existing.CreationTimestamp
		}
		rb.ResourceVersion = s.incRV()
		s.roleBindings[key] = rb
		out = rb.DeepCopy()
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// --- ClusterRoles ---

// AllClusterRoles returns all ClusterRoles.
func (s *Store) AllClusterRoles() []*rbacv1.ClusterRole {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*rbacv1.ClusterRole, 0, len(s.clusterRoles))
	for _, cr := range s.clusterRoles {
		result = append(result, cr.DeepCopy())
	}
	return result
}

// CreateClusterRole creates a ClusterRole.
func (s *Store) CreateClusterRole(cr *rbacv1.ClusterRole) (*rbacv1.ClusterRole, error) {
	var out *rbacv1.ClusterRole
	err := s.mutate(func() (bool, error) {
		if _, ok := s.clusterRoles[cr.Name]; ok {
			return false, fmt.Errorf("clusterrole %q already exists", cr.Name)
		}
		cr.ResourceVersion = s.incRV()
		assignUID(cr)
		if cr.Labels == nil {
			cr.Labels = make(map[string]string)
		}
		if cr.CreationTimestamp.IsZero() {
			cr.CreationTimestamp = metav1.Now()
		}
		s.clusterRoles[cr.Name] = cr
		out = cr.DeepCopy()
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetClusterRole gets a ClusterRole.
func (s *Store) GetClusterRole(name string) (*rbacv1.ClusterRole, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cr, ok := s.clusterRoles[name]
	if !ok {
		return nil, fmt.Errorf("clusterrole %q not found", name)
	}
	return cr.DeepCopy(), nil
}

// DeleteClusterRole deletes a ClusterRole.
func (s *Store) DeleteClusterRole(name string) error {
	return s.mutate(func() (bool, error) {
		if _, ok := s.clusterRoles[name]; !ok {
			return false, fmt.Errorf("clusterrole %q not found", name)
		}
		delete(s.clusterRoles, name)
		// A delete is a write: it gets its own resourceVersion, as in Kubernetes.
		s.incRV()
		return true, nil
	})
}

// UpdateClusterRole replaces a ClusterRole in-place, preserving UID and creation timestamp.
func (s *Store) UpdateClusterRole(cr *rbacv1.ClusterRole) (*rbacv1.ClusterRole, error) {
	var out *rbacv1.ClusterRole
	err := s.mutate(func() (bool, error) {
		existing, ok := s.clusterRoles[cr.Name]
		if !ok {
			return false, fmt.Errorf("clusterrole %q not found", cr.Name)
		}
		cr.UID = existing.UID
		if cr.CreationTimestamp.IsZero() {
			cr.CreationTimestamp = existing.CreationTimestamp
		}
		cr.ResourceVersion = s.incRV()
		s.clusterRoles[cr.Name] = cr
		out = cr.DeepCopy()
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// --- ClusterRoleBindings ---

// AllClusterRoleBindings returns all ClusterRoleBindings.
func (s *Store) AllClusterRoleBindings() []*rbacv1.ClusterRoleBinding {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*rbacv1.ClusterRoleBinding, 0, len(s.clusterRoleBindings))
	for _, crb := range s.clusterRoleBindings {
		result = append(result, crb.DeepCopy())
	}
	return result
}

// CreateClusterRoleBinding creates a ClusterRoleBinding.
func (s *Store) CreateClusterRoleBinding(crb *rbacv1.ClusterRoleBinding) (*rbacv1.ClusterRoleBinding, error) {
	var out *rbacv1.ClusterRoleBinding
	err := s.mutate(func() (bool, error) {
		if _, ok := s.clusterRoleBindings[crb.Name]; ok {
			return false, fmt.Errorf("clusterrolebinding %q already exists", crb.Name)
		}
		crb.ResourceVersion = s.incRV()
		assignUID(crb)
		if crb.Labels == nil {
			crb.Labels = make(map[string]string)
		}
		if crb.CreationTimestamp.IsZero() {
			crb.CreationTimestamp = metav1.Now()
		}
		s.clusterRoleBindings[crb.Name] = crb
		out = crb.DeepCopy()
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetClusterRoleBinding gets a ClusterRoleBinding.
func (s *Store) GetClusterRoleBinding(name string) (*rbacv1.ClusterRoleBinding, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	crb, ok := s.clusterRoleBindings[name]
	if !ok {
		return nil, fmt.Errorf("clusterrolebinding %q not found", name)
	}
	return crb.DeepCopy(), nil
}

// DeleteClusterRoleBinding deletes a ClusterRoleBinding.
func (s *Store) DeleteClusterRoleBinding(name string) error {
	return s.mutate(func() (bool, error) {
		if _, ok := s.clusterRoleBindings[name]; !ok {
			return false, fmt.Errorf("clusterrolebinding %q not found", name)
		}
		delete(s.clusterRoleBindings, name)
		// A delete is a write: it gets its own resourceVersion, as in Kubernetes.
		s.incRV()
		return true, nil
	})
}

// UpdateClusterRoleBinding replaces a ClusterRoleBinding in-place, preserving UID and creation timestamp.
func (s *Store) UpdateClusterRoleBinding(crb *rbacv1.ClusterRoleBinding) (*rbacv1.ClusterRoleBinding, error) {
	var out *rbacv1.ClusterRoleBinding
	err := s.mutate(func() (bool, error) {
		existing, ok := s.clusterRoleBindings[crb.Name]
		if !ok {
			return false, fmt.Errorf("clusterrolebinding %q not found", crb.Name)
		}
		crb.UID = existing.UID
		if crb.CreationTimestamp.IsZero() {
			crb.CreationTimestamp = existing.CreationTimestamp
		}
		crb.ResourceVersion = s.incRV()
		s.clusterRoleBindings[crb.Name] = crb
		out = crb.DeepCopy()
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ResourceVersion returns the current resource version string.
func (s *Store) ResourceVersion() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.resourceVer
}

// ContextKey is the type for context keys.
type ContextKey string

const (
	// QuadletDirKey is the context key for the quadlet directory path.
	QuadletDirKey ContextKey = "quadlet_dir"
	// ModeKey is the context key for rootful vs rootless mode.
	ModeKey ContextKey = "mode"
)

// --- NodePort allocation ---

const (
	// NodePortMin/Max bound the range for type: NodePort Services, matching
	// the Kubernetes default service-node-port range. A NodePort is
	// published on 0.0.0.0 — a real LAN listener on the backing pod.
	NodePortMin = 30000
	NodePortMax = 32767
)

// nodePortInUse reports whether a node port is already claimed by another
// Service's nodePort or by a published hostPort on any pod. The caller holds
// no lock; this takes its own read lock.
func (s *Store) nodePortInUse(port int32, exceptSvc types.NamespacedName) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for key, svc := range s.services {
		if key == exceptSvc {
			continue
		}
		for _, sp := range svc.Spec.Ports {
			if sp.NodePort == port {
				return true
			}
		}
	}
	for _, pod := range s.pods {
		for _, c := range pod.Spec.Containers {
			for _, cp := range c.Ports {
				if cp.HostPort == port {
					return true
				}
			}
		}
	}
	return false
}

// AllocateNodePort returns a free node port in [NodePortMin, NodePortMax] for
// the given Service, skipping ports already used by other Services' nodePorts
// or by any pod's published hostPort. Returns 0 if the range is exhausted.
func (s *Store) AllocateNodePort(svc types.NamespacedName) int32 {
	for p := int32(NodePortMin); p <= NodePortMax; p++ {
		if !s.nodePortInUse(p, svc) {
			return p
		}
	}
	return 0
}

// NodePortAvailable reports whether an explicitly-requested node port is in
// range and not already claimed (excluding the Service requesting it).
func (s *Store) NodePortAvailable(port int32, svc types.NamespacedName) bool {
	if port < NodePortMin || port > NodePortMax {
		return false
	}
	return !s.nodePortInUse(port, svc)
}

// writeFileSync is os.WriteFile followed by an fsync of the file.
func writeFileSync(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// backfillCreationTimestamps gives objects stored by an older q8s, which did
// not set creationTimestamp on ConfigMaps, Secrets, PVCs and Services, the
// store file's modification time: a lower bound for their real age, better
// than AGE <unknown>. It is persisted with the next save.
func backfillCreationTimestamps(snap *snapshot, dataFile string) {
	ts := metav1.Now()
	if info, err := os.Stat(dataFile); err == nil {
		ts = metav1.NewTime(info.ModTime())
	}
	fill := func(m *metav1.ObjectMeta) {
		if m.CreationTimestamp.IsZero() {
			m.CreationTimestamp = ts
		}
	}
	for _, o := range snap.ConfigMaps {
		fill(&o.ObjectMeta)
	}
	for _, o := range snap.Secrets {
		fill(&o.ObjectMeta)
	}
	for _, o := range snap.PVCs {
		fill(&o.ObjectMeta)
	}
	for _, o := range snap.Services {
		fill(&o.ObjectMeta)
	}
}
