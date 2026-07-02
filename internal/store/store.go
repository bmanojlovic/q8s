package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// snapshot is the on-disk JSON format for the store.
type snapshot struct {
	ResourceVer int                             `json:"resourceVersion"`
	Pods        []*corev1.Pod                  `json:"pods,omitempty"`
	Services    []*corev1.Service               `json:"services,omitempty"`
	PVCs        []*corev1.PersistentVolumeClaim `json:"pvcs,omitempty"`
	ConfigMaps  []*corev1.ConfigMap             `json:"configMaps,omitempty"`
	Secrets     []*corev1.Secret                `json:"secrets,omitempty"`
	Namespaces  []*corev1.Namespace             `json:"namespaces,omitempty"`
	Jobs        []*batchv1.Job                  `json:"jobs,omitempty"`
	CronJobs    []*batchv1.CronJob              `json:"cronJobs,omitempty"`
	Deployments []*appsv1.Deployment            `json:"deployments,omitempty"`
}

// Store is an in-memory store for k8s resources.
type Store struct {
	mu          sync.RWMutex
	saveMu      sync.Mutex // serialises file writes
	dataFile    string
	pods        map[types.NamespacedName]*corev1.Pod
	services    map[types.NamespacedName]*corev1.Service
	pvcs        map[types.NamespacedName]*corev1.PersistentVolumeClaim
	configmaps  map[types.NamespacedName]*corev1.ConfigMap
	secrets     map[types.NamespacedName]*corev1.Secret
	namespaces  map[types.NamespacedName]*corev1.Namespace
	jobs        map[types.NamespacedName]*batchv1.Job
	cronjobs    map[types.NamespacedName]*batchv1.CronJob
	deployments map[types.NamespacedName]*appsv1.Deployment
	resourceVer int
}

func newEmpty() *Store {
	return &Store{
		pods:       make(map[types.NamespacedName]*corev1.Pod),
		services:   make(map[types.NamespacedName]*corev1.Service),
		pvcs:       make(map[types.NamespacedName]*corev1.PersistentVolumeClaim),
		configmaps: make(map[types.NamespacedName]*corev1.ConfigMap),
		secrets:    make(map[types.NamespacedName]*corev1.Secret),
		namespaces: make(map[types.NamespacedName]*corev1.Namespace),
		jobs:       make(map[types.NamespacedName]*batchv1.Job),
		cronjobs:    make(map[types.NamespacedName]*batchv1.CronJob),
		deployments: make(map[types.NamespacedName]*appsv1.Deployment),
	}
}

// New creates a new empty Store with a "default" namespace.
func New() *Store {
	s := newEmpty()
	s.ensureDefaultNamespace()
	return s
}

// Load reads the store from dataFile if it exists; otherwise returns a fresh Store.
// The returned store will persist changes back to dataFile on every mutation.
func Load(dataFile string) (*Store, error) {
	s := newEmpty()
	s.dataFile = dataFile

	data, err := os.ReadFile(dataFile)
	if os.IsNotExist(err) {
		s.ensureDefaultNamespace()
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading store file: %w", err)
	}

	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("parsing store file: %w", err)
	}

	s.resourceVer = snap.ResourceVer
	for _, p := range snap.Pods {
		s.pods[types.NamespacedName{Namespace: p.Namespace, Name: p.Name}] = p
	}
	for _, svc := range snap.Services {
		s.services[types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}] = svc
	}
	for _, pvc := range snap.PVCs {
		s.pvcs[types.NamespacedName{Namespace: pvc.Namespace, Name: pvc.Name}] = pvc
	}
	for _, cm := range snap.ConfigMaps {
		s.configmaps[types.NamespacedName{Namespace: cm.Namespace, Name: cm.Name}] = cm
	}
	for _, sec := range snap.Secrets {
		s.secrets[types.NamespacedName{Namespace: sec.Namespace, Name: sec.Name}] = sec
	}
	for _, ns := range snap.Namespaces {
		s.namespaces[types.NamespacedName{Name: ns.Name}] = ns
	}
	for _, j := range snap.Jobs {
		s.jobs[types.NamespacedName{Namespace: j.Namespace, Name: j.Name}] = j
	}
	for _, cj := range snap.CronJobs {
		s.cronjobs[types.NamespacedName{Namespace: cj.Namespace, Name: cj.Name}] = cj
	}
	for _, dep := range snap.Deployments {
		s.deployments[types.NamespacedName{Namespace: dep.Namespace, Name: dep.Name}] = dep
	}

	s.ensureDefaultNamespace()
	return s, nil
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

// save writes the store to disk atomically (temp file + rename).
// saveMu is acquired first so that concurrent saves always write the latest
// state: the winner of saveMu captures a fresh snapshot under mu.RLock,
// so a stale snapshot can never overwrite a newer one.
func (s *Store) save() {
	if s.dataFile == "" {
		return
	}

	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	s.mu.RLock()
	snap := s.snapshot()
	s.mu.RUnlock()

	data, err := json.Marshal(snap)
	if err != nil {
		fmt.Printf("store: marshal error: %v\n", err)
		return
	}

	dir := filepath.Dir(s.dataFile)
	if err := os.MkdirAll(dir, 0755); err != nil {
		fmt.Printf("store: mkdir %s: %v\n", dir, err)
		return
	}
	tmp := s.dataFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		fmt.Printf("store: write %s: %v\n", tmp, err)
		return
	}
	if err := os.Rename(tmp, s.dataFile); err != nil {
		fmt.Printf("store: rename %s: %v\n", tmp, err)
	}
}

// snapshot captures a consistent view of the store. Caller must hold s.mu (read or write).
func (s *Store) snapshot() snapshot {
	snap := snapshot{ResourceVer: s.resourceVer}
	for _, p := range s.pods {
		snap.Pods = append(snap.Pods, p)
	}
	for _, svc := range s.services {
		snap.Services = append(snap.Services, svc)
	}
	for _, pvc := range s.pvcs {
		snap.PVCs = append(snap.PVCs, pvc)
	}
	for _, cm := range s.configmaps {
		snap.ConfigMaps = append(snap.ConfigMaps, cm)
	}
	for _, sec := range s.secrets {
		snap.Secrets = append(snap.Secrets, sec)
	}
	for _, ns := range s.namespaces {
		snap.Namespaces = append(snap.Namespaces, ns)
	}
	for _, j := range s.jobs {
		snap.Jobs = append(snap.Jobs, j)
	}
	for _, cj := range s.cronjobs {
		snap.CronJobs = append(snap.CronJobs, cj)
	}
	for _, dep := range s.deployments {
		snap.Deployments = append(snap.Deployments, dep)
	}
	return snap
}

// ConfigMapFiles returns all stored ConfigMaps — used to restore files on startup.
func (s *Store) ConfigMapFiles() []*corev1.ConfigMap {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*corev1.ConfigMap, 0, len(s.configmaps))
	for _, cm := range s.configmaps {
		result = append(result, cm)
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
		result = append(result, ns)
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
	for k := range s.pvcs {
		phantom(k.Namespace)
	}
	return result
}

// CreateNamespace creates a namespace.
func (s *Store) CreateNamespace(ns *corev1.Namespace) (*corev1.Namespace, error) {
	s.mu.Lock()
	key := types.NamespacedName{Name: ns.Name}
	if _, ok := s.namespaces[key]; ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("namespace %q already exists", ns.Name)
	}
	ns.ResourceVersion = s.incRV()
	ns.UID = types.UID(fmt.Sprintf("q8s-ns-%s", ns.Name))
	if ns.CreationTimestamp.IsZero() {
		ns.CreationTimestamp = metav1.Now()
	}
	s.namespaces[key] = ns
	s.mu.Unlock()
	go s.save()
	return ns, nil
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
	return ns, nil
}

// DeleteNamespace deletes a namespace.
func (s *Store) DeleteNamespace(name string) error {
	s.mu.Lock()
	key := types.NamespacedName{Name: name}
	if _, ok := s.namespaces[key]; !ok {
		s.mu.Unlock()
		return fmt.Errorf("namespace %q not found", name)
	}
	delete(s.namespaces, key)
	s.mu.Unlock()
	go s.save()
	return nil
}

// PurgeNamespace removes all resources belonging to a namespace from the store
// in a single lock acquisition. Called by the namespace delete handler after
// stopping systemd units so the store and disk stay consistent.
func (s *Store) PurgeNamespace(name string) {
	s.mu.Lock()
	for key := range s.pods {
		if key.Namespace == name {
			delete(s.pods, key)
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
	s.resourceVer++
	s.mu.Unlock()
	go s.save()
}

// --- Pods ---

// AllPods returns all pods across all namespaces.
func (s *Store) AllPods() []*corev1.Pod {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*corev1.Pod, 0, len(s.pods))
	for _, p := range s.pods {
		result = append(result, p)
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
			result = append(result, p)
		}
	}
	return result
}

// CreatePod creates a pod.
func (s *Store) CreatePod(pod *corev1.Pod) (*corev1.Pod, error) {
	s.mu.Lock()
	key := types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}
	if _, ok := s.pods[key]; ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("pod %q already exists in namespace %q", pod.Name, pod.Namespace)
	}
	pod.ResourceVersion = s.incRV()
	pod.UID = types.UID(fmt.Sprintf("q8s-pod-%s-%s", pod.Namespace, pod.Name))
	if pod.Labels == nil {
		pod.Labels = make(map[string]string)
	}
	if pod.CreationTimestamp.IsZero() {
		pod.CreationTimestamp = metav1.Now()
	}
	s.pods[key] = pod
	s.mu.Unlock()
	go s.save()
	return pod, nil
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
	return pod, nil
}

// DeletePod deletes a pod.
func (s *Store) DeletePod(ns, name string) error {
	s.mu.Lock()
	key := types.NamespacedName{Namespace: ns, Name: name}
	if _, ok := s.pods[key]; !ok {
		s.mu.Unlock()
		return fmt.Errorf("pod %q not found in namespace %q", name, ns)
	}
	delete(s.pods, key)
	s.mu.Unlock()
	go s.save()
	return nil
}

// UpdatePod replaces a pod in-place, preserving UID.
func (s *Store) UpdatePod(pod *corev1.Pod) (*corev1.Pod, error) {
	s.mu.Lock()
	key := types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}
	existing, ok := s.pods[key]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("pod %q not found in namespace %q", pod.Name, pod.Namespace)
	}
	pod.UID = existing.UID
	pod.ResourceVersion = s.incRV()
	s.pods[key] = pod
	s.mu.Unlock()
	go s.save()
	return pod, nil
}

// UpdatePodPhase updates a pod's phase and syncs container ready state.
// Ignores missing pods silently (race between delete and sync is fine).
func (s *Store) UpdatePodPhase(ns, name string, phase corev1.PodPhase) {
	s.mu.Lock()
	key := types.NamespacedName{Namespace: ns, Name: name}
	pod, ok := s.pods[key]
	if !ok {
		s.mu.Unlock()
		return
	}
	changed := pod.Status.Phase != phase
	if changed {
		pod.Status.Phase = phase
		s.resourceVer++
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
	}
	for i := range pod.Status.ContainerStatuses {
		pod.Status.ContainerStatuses[i].Ready = running
	}
	s.mu.Unlock()
	if changed {
		go s.save()
	}
}

// --- Services ---

// AllServices returns all services across all namespaces.
func (s *Store) AllServices() []*corev1.Service {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*corev1.Service, 0, len(s.services))
	for _, svc := range s.services {
		result = append(result, svc)
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
			result = append(result, svc)
		}
	}
	return result
}

// CreateService creates a service.
func (s *Store) CreateService(svc *corev1.Service) (*corev1.Service, error) {
	s.mu.Lock()
	key := types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}
	if _, ok := s.services[key]; ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("service %q already exists in namespace %q", svc.Name, svc.Namespace)
	}
	svc.ResourceVersion = s.incRV()
	svc.UID = types.UID(fmt.Sprintf("q8s-svc-%s-%s", svc.Namespace, svc.Name))
	if svc.Labels == nil {
		svc.Labels = make(map[string]string)
	}
	s.services[key] = svc
	s.mu.Unlock()
	go s.save()
	return svc, nil
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
	return svc, nil
}

// DeleteService deletes a service.
func (s *Store) DeleteService(ns, name string) error {
	s.mu.Lock()
	key := types.NamespacedName{Namespace: ns, Name: name}
	if _, ok := s.services[key]; !ok {
		s.mu.Unlock()
		return fmt.Errorf("service %q not found in namespace %q", name, ns)
	}
	delete(s.services, key)
	s.mu.Unlock()
	go s.save()
	return nil
}

// UpdateService replaces a service in-place, preserving UID.
func (s *Store) UpdateService(svc *corev1.Service) (*corev1.Service, error) {
	s.mu.Lock()
	key := types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}
	existing, ok := s.services[key]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("service %q not found in namespace %q", svc.Name, svc.Namespace)
	}
	svc.UID = existing.UID
	svc.ResourceVersion = s.incRV()
	s.services[key] = svc
	s.mu.Unlock()
	go s.save()
	return svc, nil
}

// --- PVCs ---

// AllPVCs returns all PVCs across all namespaces.
func (s *Store) AllPVCs() []*corev1.PersistentVolumeClaim {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*corev1.PersistentVolumeClaim, 0, len(s.pvcs))
	for _, p := range s.pvcs {
		result = append(result, p)
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
			result = append(result, pvc)
		}
	}
	return result
}

// CreatePVC creates a PVC.
func (s *Store) CreatePVC(pvc *corev1.PersistentVolumeClaim) (*corev1.PersistentVolumeClaim, error) {
	s.mu.Lock()
	key := types.NamespacedName{Namespace: pvc.Namespace, Name: pvc.Name}
	if _, ok := s.pvcs[key]; ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("PVC %q already exists in namespace %q", pvc.Name, pvc.Namespace)
	}
	pvc.ResourceVersion = s.incRV()
	pvc.UID = types.UID(fmt.Sprintf("q8s-pvc-%s-%s", pvc.Namespace, pvc.Name))
	if pvc.Labels == nil {
		pvc.Labels = make(map[string]string)
	}
	s.pvcs[key] = pvc
	s.mu.Unlock()
	go s.save()
	return pvc, nil
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
	return pvc, nil
}

// DeletePVC deletes a PVC.
func (s *Store) DeletePVC(ns, name string) error {
	s.mu.Lock()
	key := types.NamespacedName{Namespace: ns, Name: name}
	if _, ok := s.pvcs[key]; !ok {
		s.mu.Unlock()
		return fmt.Errorf("PVC %q not found in namespace %q", name, ns)
	}
	delete(s.pvcs, key)
	s.mu.Unlock()
	go s.save()
	return nil
}

// UpdatePVC replaces a PVC in-place, preserving UID.
func (s *Store) UpdatePVC(pvc *corev1.PersistentVolumeClaim) (*corev1.PersistentVolumeClaim, error) {
	s.mu.Lock()
	key := types.NamespacedName{Namespace: pvc.Namespace, Name: pvc.Name}
	existing, ok := s.pvcs[key]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("PVC %q not found in namespace %q", pvc.Name, pvc.Namespace)
	}
	pvc.UID = existing.UID
	pvc.ResourceVersion = s.incRV()
	s.pvcs[key] = pvc
	s.mu.Unlock()
	go s.save()
	return pvc, nil
}

// --- ConfigMaps ---

// AllConfigMaps returns all configmaps across all namespaces.
func (s *Store) AllConfigMaps() []*corev1.ConfigMap {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*corev1.ConfigMap, 0, len(s.configmaps))
	for _, cm := range s.configmaps {
		result = append(result, cm)
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
			result = append(result, cm)
		}
	}
	return result
}

// CreateConfigMap creates a configmap.
func (s *Store) CreateConfigMap(cm *corev1.ConfigMap) (*corev1.ConfigMap, error) {
	s.mu.Lock()
	key := types.NamespacedName{Namespace: cm.Namespace, Name: cm.Name}
	if _, ok := s.configmaps[key]; ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("configmap %q already exists in namespace %q", cm.Name, cm.Namespace)
	}
	cm.ResourceVersion = s.incRV()
	cm.UID = types.UID(fmt.Sprintf("q8s-cm-%s-%s", cm.Namespace, cm.Name))
	if cm.Labels == nil {
		cm.Labels = make(map[string]string)
	}
	s.configmaps[key] = cm
	s.mu.Unlock()
	go s.save()
	return cm, nil
}

// UpdateConfigMap replaces a configmap's data in-place.
func (s *Store) UpdateConfigMap(cm *corev1.ConfigMap) (*corev1.ConfigMap, error) {
	s.mu.Lock()
	key := types.NamespacedName{Namespace: cm.Namespace, Name: cm.Name}
	existing, ok := s.configmaps[key]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("configmap %q not found in namespace %q", cm.Name, cm.Namespace)
	}
	cm.UID = existing.UID
	cm.ResourceVersion = s.incRV()
	s.configmaps[key] = cm
	s.mu.Unlock()
	go s.save()
	return cm, nil
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
	return cm, nil
}

// DeleteConfigMap deletes a configmap.
func (s *Store) DeleteConfigMap(ns, name string) error {
	s.mu.Lock()
	key := types.NamespacedName{Namespace: ns, Name: name}
	if _, ok := s.configmaps[key]; !ok {
		s.mu.Unlock()
		return fmt.Errorf("configmap %q not found in namespace %q", name, ns)
	}
	delete(s.configmaps, key)
	s.mu.Unlock()
	go s.save()
	return nil
}

// --- Secrets ---

// AllSecrets returns all secrets across all namespaces.
func (s *Store) AllSecrets() []*corev1.Secret {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*corev1.Secret, 0, len(s.secrets))
	for _, sec := range s.secrets {
		result = append(result, sec)
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
			result = append(result, sec)
		}
	}
	return result
}

// CreateSecret creates a secret.
func (s *Store) CreateSecret(sec *corev1.Secret) (*corev1.Secret, error) {
	s.mu.Lock()
	key := types.NamespacedName{Namespace: sec.Namespace, Name: sec.Name}
	if _, ok := s.secrets[key]; ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("secret %q already exists in namespace %q", sec.Name, sec.Namespace)
	}
	sec.ResourceVersion = s.incRV()
	sec.UID = types.UID(fmt.Sprintf("q8s-secret-%s-%s", sec.Namespace, sec.Name))
	if sec.Labels == nil {
		sec.Labels = make(map[string]string)
	}
	s.secrets[key] = sec
	s.mu.Unlock()
	go s.save()
	return sec, nil
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
	return sec, nil
}

// DeleteSecret deletes a secret.
func (s *Store) DeleteSecret(ns, name string) error {
	s.mu.Lock()
	key := types.NamespacedName{Namespace: ns, Name: name}
	if _, ok := s.secrets[key]; !ok {
		s.mu.Unlock()
		return fmt.Errorf("secret %q not found in namespace %q", name, ns)
	}
	delete(s.secrets, key)
	s.mu.Unlock()
	go s.save()
	return nil
}

// UpdateSecret replaces a secret in-place, preserving UID.
func (s *Store) UpdateSecret(sec *corev1.Secret) (*corev1.Secret, error) {
	s.mu.Lock()
	key := types.NamespacedName{Namespace: sec.Namespace, Name: sec.Name}
	existing, ok := s.secrets[key]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("secret %q not found in namespace %q", sec.Name, sec.Namespace)
	}
	sec.UID = existing.UID
	sec.ResourceVersion = s.incRV()
	s.secrets[key] = sec
	s.mu.Unlock()
	go s.save()
	return sec, nil
}

// --- Jobs ---

// AllJobs returns all jobs across all namespaces.
func (s *Store) AllJobs() []*batchv1.Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*batchv1.Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		result = append(result, j)
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
			result = append(result, j)
		}
	}
	return result
}

// CreateJob creates a job.
func (s *Store) CreateJob(job *batchv1.Job) (*batchv1.Job, error) {
	s.mu.Lock()
	key := types.NamespacedName{Namespace: job.Namespace, Name: job.Name}
	if _, ok := s.jobs[key]; ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("job %q already exists in namespace %q", job.Name, job.Namespace)
	}
	job.ResourceVersion = s.incRV()
	job.UID = types.UID(fmt.Sprintf("q8s-job-%s-%s", job.Namespace, job.Name))
	if job.Labels == nil {
		job.Labels = make(map[string]string)
	}
	if job.CreationTimestamp.IsZero() {
		job.CreationTimestamp = metav1.Now()
	}
	s.jobs[key] = job
	s.mu.Unlock()
	go s.save()
	return job, nil
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
	return job, nil
}

// DeleteJob deletes a job.
func (s *Store) DeleteJob(ns, name string) error {
	s.mu.Lock()
	key := types.NamespacedName{Namespace: ns, Name: name}
	if _, ok := s.jobs[key]; !ok {
		s.mu.Unlock()
		return fmt.Errorf("job %q not found in namespace %q", name, ns)
	}
	delete(s.jobs, key)
	s.mu.Unlock()
	go s.save()
	return nil
}

// UpdateJob replaces a job in-place, preserving UID.
func (s *Store) UpdateJob(job *batchv1.Job) (*batchv1.Job, error) {
	s.mu.Lock()
	key := types.NamespacedName{Namespace: job.Namespace, Name: job.Name}
	existing, ok := s.jobs[key]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("job %q not found in namespace %q", job.Name, job.Namespace)
	}
	job.UID = existing.UID
	job.ResourceVersion = s.incRV()
	s.jobs[key] = job
	s.mu.Unlock()
	go s.save()
	return job, nil
}

// UpdateJobStatus updates a job's active/succeeded/failed counts.
func (s *Store) UpdateJobStatus(ns, name string, active, succeeded, failed int32) {
	s.mu.Lock()
	key := types.NamespacedName{Namespace: ns, Name: name}
	job, ok := s.jobs[key]
	if !ok {
		s.mu.Unlock()
		return
	}
	changed := job.Status.Active != active || job.Status.Succeeded != succeeded || job.Status.Failed != failed
	if changed {
		job.Status.Active = active
		job.Status.Succeeded = succeeded
		job.Status.Failed = failed
		s.resourceVer++
	}
	s.mu.Unlock()
	if changed {
		go s.save()
	}
}

// --- CronJobs ---

// AllCronJobs returns all cronjobs across all namespaces.
func (s *Store) AllCronJobs() []*batchv1.CronJob {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*batchv1.CronJob, 0, len(s.cronjobs))
	for _, cj := range s.cronjobs {
		result = append(result, cj)
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
			result = append(result, cj)
		}
	}
	return result
}

// CreateCronJob creates a cronjob.
func (s *Store) CreateCronJob(cj *batchv1.CronJob) (*batchv1.CronJob, error) {
	s.mu.Lock()
	key := types.NamespacedName{Namespace: cj.Namespace, Name: cj.Name}
	if _, ok := s.cronjobs[key]; ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("cronjob %q already exists in namespace %q", cj.Name, cj.Namespace)
	}
	cj.ResourceVersion = s.incRV()
	cj.UID = types.UID(fmt.Sprintf("q8s-cj-%s-%s", cj.Namespace, cj.Name))
	if cj.Labels == nil {
		cj.Labels = make(map[string]string)
	}
	if cj.CreationTimestamp.IsZero() {
		cj.CreationTimestamp = metav1.Now()
	}
	s.cronjobs[key] = cj
	s.mu.Unlock()
	go s.save()
	return cj, nil
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
	return cj, nil
}

// DeleteCronJob deletes a cronjob.
func (s *Store) DeleteCronJob(ns, name string) error {
	s.mu.Lock()
	key := types.NamespacedName{Namespace: ns, Name: name}
	if _, ok := s.cronjobs[key]; !ok {
		s.mu.Unlock()
		return fmt.Errorf("cronjob %q not found in namespace %q", name, ns)
	}
	delete(s.cronjobs, key)
	s.mu.Unlock()
	go s.save()
	return nil
}

// UpdateCronJob replaces a cronjob in-place, preserving UID.
func (s *Store) UpdateCronJob(cj *batchv1.CronJob) (*batchv1.CronJob, error) {
	s.mu.Lock()
	key := types.NamespacedName{Namespace: cj.Namespace, Name: cj.Name}
	existing, ok := s.cronjobs[key]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("cronjob %q not found in namespace %q", cj.Name, cj.Namespace)
	}
	cj.UID = existing.UID
	cj.ResourceVersion = s.incRV()
	s.cronjobs[key] = cj
	s.mu.Unlock()
	go s.save()
	return cj, nil
}

// --- Deployments ---

// AllDeployments returns all deployments across all namespaces.
func (s *Store) AllDeployments() []*appsv1.Deployment {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*appsv1.Deployment, 0, len(s.deployments))
	for _, dep := range s.deployments {
		result = append(result, dep)
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
			result = append(result, dep)
		}
	}
	return result
}

// CreateDeployment creates a deployment.
func (s *Store) CreateDeployment(dep *appsv1.Deployment) (*appsv1.Deployment, error) {
	s.mu.Lock()
	key := types.NamespacedName{Namespace: dep.Namespace, Name: dep.Name}
	if _, ok := s.deployments[key]; ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("deployment %q already exists in namespace %q", dep.Name, dep.Namespace)
	}
	dep.ResourceVersion = s.incRV()
	dep.UID = types.UID(fmt.Sprintf("q8s-dep-%s-%s", dep.Namespace, dep.Name))
	if dep.Labels == nil {
		dep.Labels = make(map[string]string)
	}
	if dep.CreationTimestamp.IsZero() {
		dep.CreationTimestamp = metav1.Now()
	}
	s.deployments[key] = dep
	s.mu.Unlock()
	go s.save()
	return dep, nil
}

// UpdateDeployment replaces a deployment in-place, preserving UID.
func (s *Store) UpdateDeployment(dep *appsv1.Deployment) (*appsv1.Deployment, error) {
	s.mu.Lock()
	key := types.NamespacedName{Namespace: dep.Namespace, Name: dep.Name}
	existing, ok := s.deployments[key]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("deployment %q not found in namespace %q", dep.Name, dep.Namespace)
	}
	dep.UID = existing.UID
	dep.ResourceVersion = s.incRV()
	s.deployments[key] = dep
	s.mu.Unlock()
	go s.save()
	return dep, nil
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
	return dep, nil
}

// DeleteDeployment deletes a deployment.
func (s *Store) DeleteDeployment(ns, name string) error {
	s.mu.Lock()
	key := types.NamespacedName{Namespace: ns, Name: name}
	if _, ok := s.deployments[key]; !ok {
		s.mu.Unlock()
		return fmt.Errorf("deployment %q not found in namespace %q", name, ns)
	}
	delete(s.deployments, key)
	s.mu.Unlock()
	go s.save()
	return nil
}

// UpdateDeploymentStatus updates a deployment's ready/available replica counts.
func (s *Store) UpdateDeploymentStatus(ns, name string, ready int32) {
	s.mu.Lock()
	key := types.NamespacedName{Namespace: ns, Name: name}
	dep, ok := s.deployments[key]
	if !ok {
		s.mu.Unlock()
		return
	}
	desired := int32(1)
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}
	changed := dep.Status.ReadyReplicas != ready
	if changed {
		dep.Status.Replicas = desired
		dep.Status.ReadyReplicas = ready
		dep.Status.AvailableReplicas = ready
		dep.Status.UpdatedReplicas = desired
		s.resourceVer++
	}
	s.mu.Unlock()
	if changed {
		go s.save()
	}
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
