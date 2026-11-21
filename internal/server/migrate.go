package server

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"q8s/internal/fsutil"
	"q8s/internal/names"
	"q8s/internal/quadlet"
)

// namesMarkerName is the file next to store.json that records a finished
// one-time name migration. Its absence is the only thing that makes startup
// run the eager migration and the ensure-started pass.
const namesMarkerName = "names-v2"

// NameMigrationPlan is the host-side work of one name migration direction.
// It is exported so q8s migrate-names can show exactly what startup would do
// without changing anything.
type NameMigrationPlan struct {
	// Objects names the stored workloads the plan acts on.
	Objects []string
	// StopUnits are stopped. DisableUnits are stopped and disabled.
	StopUnits    []string
	DisableUnits []string
	// Containers and Pods are force-removed with podman.
	Containers []string
	Pods       []string
	// Files are removed after the units are stopped and the containers gone.
	Files []string
	// StartUnits is the units the ensure-started pass would start. It is
	// informational: ApplyNameMigrationPlan never starts anything.
	StartUnits []string
}

// Empty reports whether the plan changes anything.
func (p NameMigrationPlan) Empty() bool {
	return len(p.StopUnits) == 0 && len(p.DisableUnits) == 0 &&
		len(p.Containers) == 0 && len(p.Pods) == 0 && len(p.Files) == 0
}

// Print writes the plan as the migrate-names command shows it.
func (p NameMigrationPlan) Print(w io.Writer) {
	fmt.Fprintf(w, "Objects: %d\n", len(p.Objects))
	for _, object := range p.Objects {
		fmt.Fprintf(w, "  %s\n", object)
	}
	printList := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(w, "%s:\n", title)
		for _, item := range items {
			fmt.Fprintf(w, "  %s\n", item)
		}
	}
	printList("Would stop", p.StopUnits)
	printList("Would stop and disable", p.DisableUnits)
	printList("Would remove containers", p.Containers)
	printList("Would remove Podman pods", p.Pods)
	printList("Would remove files", p.Files)
	printList("Would start", p.StartUnits)
}

// NewNamesMigrationServer builds the subset of Server q8s migrate-names
// needs: the store, the host paths and systemd access, without TLS, HTTP or
// authentication. The startup path builds a full Server with New.
func NewNamesMigrationServer(cfg Config) *Server {
	return &Server{config: cfg}
}

func (s *Server) namesMarkerPath() string {
	if s.config.DataDir == "" {
		return ""
	}
	return filepath.Join(s.config.DataDir, namesMarkerName)
}

// NamesMigrationPending reports whether the one-time migration has not run
// yet. A missing marker and an unreadable marker both count as pending: the
// migration is idempotent and running it again is safer than skipping it.
func (s *Server) NamesMigrationPending() bool {
	path := s.namesMarkerPath()
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err != nil
}

// WriteNamesMigrationMarker records that the whole startup sequence,
// including the ensure-started pass, finished. Writing it any earlier would
// make a crash leave some workloads down until the next start.
func (s *Server) WriteNamesMigrationMarker() error {
	path := s.namesMarkerPath()
	if path == "" {
		return fmt.Errorf("no data directory configured")
	}
	return fsutil.WriteFileAtomic(path, []byte(namesMarkerName+"\n"), 0644)
}

// RemoveNamesMigrationMarker makes the next q8s serve run the migration
// again. q8s migrate-names --to-legacy uses it before the old binary starts.
func (s *Server) RemoveNamesMigrationMarker() error {
	path := s.namesMarkerPath()
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// workloadArtifacts is one workload's generated host names in one format.
type workloadArtifacts struct {
	files      []string
	stops      []string
	disables   []string
	containers []string
	pods       []string
}

func (a workloadArtifacts) exists(containers, pods map[string]bool) bool {
	for _, path := range a.files {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	for _, name := range a.containers {
		if containers[name] {
			return true
		}
	}
	for _, name := range a.pods {
		if pods[name] {
			return true
		}
	}
	return false
}

// nameBuilder accumulates a plan and removes duplicates while keeping the
// result sorted, so a dry run and a real run agree on the order.
type nameBuilder struct {
	objects    map[string]bool
	stops      map[string]bool
	disables   map[string]bool
	containers map[string]bool
	pods       map[string]bool
	files      map[string]bool
	starts     map[string]bool
}

func newNameBuilder() *nameBuilder {
	return &nameBuilder{
		objects:    make(map[string]bool),
		stops:      make(map[string]bool),
		disables:   make(map[string]bool),
		containers: make(map[string]bool),
		pods:       make(map[string]bool),
		files:      make(map[string]bool),
		starts:     make(map[string]bool),
	}
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (b *nameBuilder) plan() NameMigrationPlan {
	return NameMigrationPlan{
		Objects:      sortedKeys(b.objects),
		StopUnits:    sortedKeys(b.stops),
		DisableUnits: sortedKeys(b.disables),
		Containers:   sortedKeys(b.containers),
		Pods:         sortedKeys(b.pods),
		Files:        sortedKeys(b.files),
		StartUnits:   sortedKeys(b.starts),
	}
}

func (b *nameBuilder) add(object string, a workloadArtifacts) {
	b.objects[object] = true
	for _, item := range a.stops {
		b.stops[item] = true
	}
	for _, item := range a.disables {
		b.disables[item] = true
	}
	for _, item := range a.containers {
		b.containers[item] = true
	}
	for _, item := range a.pods {
		b.pods[item] = true
	}
	for _, item := range a.files {
		b.files[item] = true
	}
}

// nameFunc selects one generated-name format.
type nameFunc func(kind names.Kind, namespace, name string, extra ...string) string

func fileIn(dir, name string) string {
	if dir == "" || name == "" {
		return ""
	}
	return filepath.Join(dir, name)
}

func podArtifacts(namespace, podName string, containers []corev1.Container, annotations map[string]string, host nameFunc, quadletDir, systemdDir, secretDir string) workloadArtifacts {
	var a workloadArtifacts
	addFile := func(path string) {
		if path != "" {
			a.files = append(a.files, path)
		}
	}
	addUnit := func(unit string, disable bool) {
		if unit == "" {
			return
		}
		a.stops = append(a.stops, unit)
		if disable {
			a.disables = append(a.disables, unit)
		}
	}
	if len(containers) > 1 {
		// Quadlet derives one service per member from the .container files,
		// and the Podman pod service from the .pod file.
		addFile(fileIn(quadletDir, host(names.KindPodFile, namespace, podName)))
		addUnit(host(names.KindPodUnit, namespace, podName), false)
		for _, c := range containers {
			addFile(fileIn(quadletDir, host(names.KindPodContainerFile, namespace, podName, c.Name)))
			addUnit(host(names.KindPodContainerService, namespace, podName, c.Name), false)
			a.containers = append(a.containers, host(names.KindPodContainer, namespace, podName, c.Name))
		}
		a.pods = append(a.pods, host(names.KindPod, namespace, podName))
	} else {
		addFile(fileIn(quadletDir, host(names.KindPodContainerFile, namespace, podName, "")))
		addUnit(host(names.KindPodService, namespace, podName), false)
		a.containers = append(a.containers, host(names.KindPodContainer, namespace, podName, ""))
	}
	if systemdDir != "" && annotations != nil {
		if entries, err := quadlet.ParseSocketActivation(annotations); err == nil {
			for _, entry := range entries {
				unit := host(names.KindSocketFile, namespace, podName, entry.Name)
				addFile(fileIn(systemdDir, unit))
				addUnit(unit, true)
			}
		}
	}
	// A Pod's env file is <pod>.env in both formats: only the Job and CronJob
	// env files carry a format-dependent suffix. Removing the current file
	// would only make the immediately following restore rewrite it.
	addFile(envArtifact(secretDir, namespace, podName, names.KindEnvPod, host))
	return a
}

// envArtifact returns the env file of one workload in the requested format.
// It returns "" when both formats use the same file name.
func envArtifact(secretDir, namespace, workloadName string, kind names.Kind, host nameFunc) string {
	if secretDir == "" {
		return ""
	}
	legacy := names.LegacyHost(kind, "", workloadName)
	current := names.Host(kind, "", workloadName)
	if legacy == current {
		return ""
	}
	if host(kind, "", workloadName) == legacy {
		return filepath.Join(secretDir, namespace, "_env", legacy)
	}
	return filepath.Join(secretDir, namespace, "_env", current)
}

func jobArtifacts(namespace, jobName string, host nameFunc, quadletDir, secretDir string) workloadArtifacts {
	var a workloadArtifacts
	addFile := func(path string) {
		if path != "" {
			a.files = append(a.files, path)
		}
	}
	addFile(fileIn(quadletDir, host(names.KindJobFile, namespace, jobName)))
	a.stops = append(a.stops, host(names.KindJobService, namespace, jobName))
	a.containers = append(a.containers, host(names.KindJob, namespace, jobName))
	addFile(envArtifact(secretDir, namespace, jobName, names.KindEnvJob, host))
	return a
}

func cronJobArtifacts(namespace, cronJobName string, host nameFunc, quadletDir, timerDir, secretDir string) workloadArtifacts {
	var a workloadArtifacts
	addFile := func(path string) {
		if path != "" {
			a.files = append(a.files, path)
		}
	}
	addFile(fileIn(quadletDir, host(names.KindCronJobFile, namespace, cronJobName)))
	a.stops = append(a.stops, host(names.KindCronJobService, namespace, cronJobName))
	a.containers = append(a.containers, host(names.KindCronJob, namespace, cronJobName))
	timer := host(names.KindCronJobTimer, namespace, cronJobName)
	addFile(fileIn(timerDir, timer))
	a.stops = append(a.stops, timer)
	a.disables = append(a.disables, timer)
	addFile(envArtifact(secretDir, namespace, cronJobName, names.KindEnvCronJob, host))
	return a
}

// hasStoredWorkloads reports whether any workload object can own legacy or
// new artifacts. A host with only PVCs, Services or Traefik objects needs no
// podman listing at all.
func (s *Server) hasStoredWorkloads() bool {
	return len(s.config.Store.AllDeployments()) > 0 ||
		len(s.config.Store.AllPods()) > 0 ||
		len(s.config.Store.AllJobs()) > 0 ||
		len(s.config.Store.AllCronJobs()) > 0
}

// podmanNameSet returns the names podman listed. A podman that cannot answer
// is treated as empty: file detection still finds the artifacts it can see,
// and the removals for those objects are harmless for a name that is gone.
func (s *Server) podmanNameSet(args ...string) map[string]bool {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := podmanCommand(ctx, args...)
	set := make(map[string]bool)
	if err != nil {
		return set
	}
	for _, line := range strings.Split(string(out), "\n") {
		name := strings.TrimSpace(line)
		if name != "" {
			set[name] = true
		}
	}
	return set
}

func (s *Server) migrationDirs() (quadletDir, systemdDir, timerDir, secretDir string) {
	quadletDir = s.config.QuadletDir
	systemdDir = s.config.SystemdDir
	timerDir = s.config.SystemdDir
	if timerDir == "" {
		timerDir = quadletDir
	}
	secretDir = s.secretBaseDir()
	return
}

// PlanLegacyMigration lists the old-format artifacts of every stored workload
// that still exist. It reads the host but changes nothing.
func (s *Server) PlanLegacyMigration() NameMigrationPlan {
	b := newNameBuilder()
	quadletDir, systemdDir, timerDir, secretDir := s.migrationDirs()
	var containers, pods map[string]bool
	if s.hasStoredWorkloads() {
		containers = s.podmanNameSet("ps", "-a", "--format", "{{.Names}}")
		pods = s.podmanNameSet("pod", "ps", "--format", "{{.Name}}")
	}
	seen := make(map[string]bool)
	addPod := func(object, namespace, name string, spec *corev1.PodSpec, annotations map[string]string) {
		key := namespace + "/" + name
		if seen[key] {
			return
		}
		seen[key] = true
		a := podArtifacts(namespace, name, spec.Containers, annotations, names.LegacyHost, quadletDir, systemdDir, secretDir)
		if a.exists(containers, pods) {
			b.add(object, a)
		}
	}
	for _, dep := range s.config.Store.AllDeployments() {
		for i := int32(0); i < deploymentReplicas(dep); i++ {
			instance := quadlet.DeploymentInstanceName(dep.Name, i)
			addPod("Deployment "+dep.Namespace+"/"+dep.Name, dep.Namespace, instance,
				&dep.Spec.Template.Spec, dep.Spec.Template.Annotations)
		}
	}
	for _, pod := range s.config.Store.AllPods() {
		addPod("Pod "+pod.Namespace+"/"+pod.Name, pod.Namespace, pod.Name, &pod.Spec, pod.Annotations)
	}
	for _, job := range s.config.Store.AllJobs() {
		a := jobArtifacts(job.Namespace, job.Name, names.LegacyHost, quadletDir, secretDir)
		if a.exists(containers, pods) {
			b.add("Job "+job.Namespace+"/"+job.Name, a)
		}
	}
	for _, cj := range s.config.Store.AllCronJobs() {
		a := cronJobArtifacts(cj.Namespace, cj.Name, names.LegacyHost, quadletDir, timerDir, secretDir)
		if a.exists(containers, pods) {
			b.add("CronJob "+cj.Namespace+"/"+cj.Name, a)
		}
	}
	b.starts = s.shouldStartUnits()
	return b.plan()
}

// PlanNewNamesRollback lists the new-format artifacts of every stored object.
// q8s migrate-names --to-legacy removes them before the previous release
// starts and writes the old files again.
func (s *Server) PlanNewNamesRollback() NameMigrationPlan {
	b := newNameBuilder()
	quadletDir, systemdDir, timerDir, secretDir := s.migrationDirs()
	var containers, pods map[string]bool
	if s.hasStoredWorkloads() {
		containers = s.podmanNameSet("ps", "-a", "--format", "{{.Names}}")
		pods = s.podmanNameSet("pod", "ps", "--format", "{{.Name}}")
	}
	seen := make(map[string]bool)
	addPod := func(object, namespace, name string, spec *corev1.PodSpec, annotations map[string]string) {
		key := namespace + "/" + name
		if seen[key] {
			return
		}
		seen[key] = true
		a := podArtifacts(namespace, name, spec.Containers, annotations, names.Host, quadletDir, systemdDir, secretDir)
		if a.exists(containers, pods) {
			b.add(object, a)
		}
	}
	for _, dep := range s.config.Store.AllDeployments() {
		for i := int32(0); i < deploymentReplicas(dep); i++ {
			instance := quadlet.DeploymentInstanceName(dep.Name, i)
			addPod("Deployment "+dep.Namespace+"/"+dep.Name, dep.Namespace, instance,
				&dep.Spec.Template.Spec, dep.Spec.Template.Annotations)
		}
	}
	for _, pod := range s.config.Store.AllPods() {
		addPod("Pod "+pod.Namespace+"/"+pod.Name, pod.Namespace, pod.Name, &pod.Spec, pod.Annotations)
	}
	for _, job := range s.config.Store.AllJobs() {
		a := jobArtifacts(job.Namespace, job.Name, names.Host, quadletDir, secretDir)
		if a.exists(containers, pods) {
			b.add("Job "+job.Namespace+"/"+job.Name, a)
		}
	}
	for _, cj := range s.config.Store.AllCronJobs() {
		a := cronJobArtifacts(cj.Namespace, cj.Name, names.Host, quadletDir, timerDir, secretDir)
		if a.exists(containers, pods) {
			b.add("CronJob "+cj.Namespace+"/"+cj.Name, a)
		}
	}
	// Every new PVC file exists only for a new-format claim, and such a claim
	// makes the rollback refuse before anything is applied. Listing it here
	// keeps a dry run honest about what a forced rollback would remove.
	if quadletDir != "" {
		for _, pvc := range s.config.Store.AllPVCs() {
			if pvcStorageClass(pvc) == quadlet.StorageClassHostPath {
				continue
			}
			current := names.Host(names.KindPVCVolume, pvc.Namespace, pvc.Name)
			if quadlet.PVCVolumeFileBase(pvc) != current {
				continue
			}
			path := filepath.Join(quadletDir, current+".volume")
			if _, err := os.Stat(path); err == nil {
				b.objects["PersistentVolumeClaim "+pvc.Namespace+"/"+pvc.Name] = true
				b.files[path] = true
			}
		}
	}
	if s.config.TraefikDir != "" {
		addTraefikFile := func(object, filename string) {
			path := filepath.Join(s.config.TraefikDir, filename)
			if _, err := os.Stat(path); err != nil {
				return
			}
			b.objects[object] = true
			b.files[path] = true
		}
		for _, ing := range s.config.Store.AllIngresses() {
			addTraefikFile("Ingress "+ing.Namespace+"/"+ing.Name,
				names.Host(names.KindIngressFile, ing.Namespace, ing.Name))
		}
		for _, mw := range s.config.Store.AllMiddlewares() {
			addTraefikFile("Middleware "+mw.Namespace+"/"+mw.Name,
				names.Host(names.KindMiddlewareFile, mw.Namespace, mw.Name))
		}
		for _, irt := range s.config.Store.AllIngressRouteTCPs() {
			addTraefikFile("IngressRouteTCP "+irt.Namespace+"/"+irt.Name,
				names.Host(names.KindIngressRouteTCPFile, irt.Namespace, irt.Name))
		}
	}
	return b.plan()
}

// NewFormatPVCClaims returns the claims bound to a new-format Podman volume.
// Rolling back would make the previous release bind them to a new, empty
// old-name volume, so the rollback command refuses while the list is not
// empty.
func (s *Server) NewFormatPVCClaims() []string {
	var claims []string
	for _, pvc := range s.config.Store.AllPVCs() {
		if pvc.Spec.VolumeName == names.Host(names.KindPVCVolume, pvc.Namespace, pvc.Name) {
			claims = append(claims, pvc.Namespace+"/"+pvc.Name)
		}
	}
	sort.Strings(claims)
	return claims
}

// MigrateLegacyNames is the one-time eager migration. It runs only while the
// marker is absent, stops and removes the old artifacts of every stored
// workload that still has one, and removes the files. It never writes new
// files or starts new units: the normal startup sequence does that right
// after, and the ensure-started pass closes a crash window.
func (s *Server) MigrateLegacyNames() int {
	if !s.NamesMigrationPending() {
		return 0
	}
	plan := s.PlanLegacyMigration()
	if len(plan.Objects) == 0 {
		fmt.Printf("names migration: migrated 0 object(s) to the new generated names\n")
		return 0
	}
	if err := s.ApplyNameMigrationPlan(plan); err != nil {
		fmt.Printf("names migration: %v\n", err)
	}
	fmt.Printf("names migration: migrated %d object(s) to the new generated names\n", len(plan.Objects))
	return len(plan.Objects)
}

// ApplyNameMigrationPlan stops the listed units, removes their containers,
// pods and files, then runs one daemon-reload. StartUnits is ignored: the
// caller starts the new units only after every file is in place.
func (s *Server) ApplyNameMigrationPlan(plan NameMigrationPlan) error {
	for _, unit := range plan.StopUnits {
		s.stopUnit(unit)
	}
	for _, unit := range plan.DisableUnits {
		if err := s.disableUnit(unit); err != nil {
			fmt.Printf("names migration: disable %s: %v\n", unit, err)
		}
	}
	for _, name := range plan.Containers {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, err := podmanCommand(ctx, "rm", "-f", name)
		cancel()
		if err != nil && !podmanObjectMissing(err) {
			fmt.Printf("names migration: remove container %s: %v\n", name, err)
		}
	}
	for _, name := range plan.Pods {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, err := podmanCommand(ctx, "pod", "rm", "-f", "--ignore", name)
		cancel()
		if err != nil && !podmanObjectMissing(err) {
			fmt.Printf("names migration: remove pod %s: %v\n", name, err)
		}
	}
	for _, path := range plan.Files {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			fmt.Printf("names migration: remove file %s: %v\n", path, err)
		}
	}
	if plan.Empty() {
		return nil
	}
	if err := s.daemonReload(); err != nil {
		return fmt.Errorf("daemon-reload after name migration: %w", err)
	}
	return nil
}

// podmanObjectMissing reports the errors podman returns for an object that
// is already gone. A missing container, pod or network is success here.
func podmanObjectMissing(err error) bool {
	text := strings.ToLower(err.Error())
	for _, marker := range []string{"no such", "not found", "does not exist", "no container with"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// shouldStartUnits lists the new units of every workload the ensure-started
// pass would start. It is the dry-run half of EnsureWorkloadsStarted.
func (s *Server) shouldStartUnits() map[string]bool {
	starts := make(map[string]bool)
	seen := make(map[string]bool)
	addPod := func(namespace, name string, spec *corev1.PodSpec, annotations map[string]string) {
		key := namespace + "/" + name
		if seen[key] {
			return
		}
		seen[key] = true
		if _, _, missing := s.firstMissingClaim(namespace, *spec); missing {
			return
		}
		if systemdDir := s.config.SystemdDir; systemdDir != "" && annotations != nil {
			if entries, err := quadlet.ParseSocketActivation(annotations); err == nil {
				for _, entry := range entries {
					starts[names.Host(names.KindSocketFile, namespace, name, entry.Name)] = true
				}
			}
		}
		for _, unit := range podUnitNames(namespace, name, spec.Containers) {
			starts[unit] = true
		}
	}
	for _, dep := range s.config.Store.AllDeployments() {
		for i := int32(0); i < deploymentReplicas(dep); i++ {
			instance := quadlet.DeploymentInstanceName(dep.Name, i)
			addPod(dep.Namespace, instance, &dep.Spec.Template.Spec, dep.Spec.Template.Annotations)
		}
	}
	for _, pod := range s.config.Store.AllPods() {
		// A bare pod that already ran to completion (restartPolicy Never or
		// OnFailure) must not run again because its unit name changed.
		if !isDeploymentOwned(pod) && pod.Spec.RestartPolicy != "" && pod.Spec.RestartPolicy != corev1.RestartPolicyAlways &&
			(pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed) {
			continue
		}
		addPod(pod.Namespace, pod.Name, &pod.Spec, pod.Annotations)
	}
	for _, job := range s.config.Store.AllJobs() {
		if jobFinished(job) {
			continue
		}
		if _, _, missing := s.firstMissingClaim(job.Namespace, job.Spec.Template.Spec); missing {
			continue
		}
		starts[names.Host(names.KindJobService, job.Namespace, job.Name)] = true
	}
	for _, cj := range s.config.Store.AllCronJobs() {
		if cronJobSuspended(cj) || cronJobWaitingForClaim(cj) {
			continue
		}
		if _, _, missing := s.firstMissingClaim(cj.Namespace, cj.Spec.JobTemplate.Spec.Template.Spec); missing {
			continue
		}
		starts[names.Host(names.KindCronJobTimer, cj.Namespace, cj.Name)] = true
	}
	return starts
}

// EnsureWorkloadsStarted is the crash recovery half of the eager migration.
// While the marker is absent, a crash can leave new files written but their
// units never started (ReconcileQuadlets starts only files that were
// missing). Starting an active unit is a no-op, so this pass simply asks
// systemd for every workload that should run. Completed Jobs and suspended
// CronJobs are deliberately skipped.
func (s *Server) EnsureWorkloadsStarted() int {
	if !s.hasSystemd() {
		return 0
	}
	quadletDir, _, _, _ := s.migrationDirs()
	if quadletDir == "" {
		return 0
	}
	sockets := make(map[string]bool)
	services := make(map[string]bool)
	seen := make(map[string]bool)
	addPod := func(namespace, name string, spec *corev1.PodSpec, annotations map[string]string) {
		key := namespace + "/" + name
		if seen[key] {
			return
		}
		seen[key] = true
		if _, _, missing := s.firstMissingClaim(namespace, *spec); missing {
			return
		}
		if s.config.SystemdDir != "" && annotations != nil {
			if entries, err := quadlet.ParseSocketActivation(annotations); err == nil && len(entries) > 0 {
				for _, entry := range entries {
					sockets[names.Host(names.KindSocketFile, namespace, name, entry.Name)] = true
				}
				// A socket-activated pod starts on its first connection, as
				// the normal deploy path leaves it; starting the service here
				// would change that.
				return
			}
		}
		for _, unit := range podUnitNames(namespace, name, spec.Containers) {
			services[unit] = true
		}
	}
	for _, dep := range s.config.Store.AllDeployments() {
		for i := int32(0); i < deploymentReplicas(dep); i++ {
			instance := quadlet.DeploymentInstanceName(dep.Name, i)
			addPod(dep.Namespace, instance, &dep.Spec.Template.Spec, dep.Spec.Template.Annotations)
		}
	}
	for _, pod := range s.config.Store.AllPods() {
		// A bare pod that already ran to completion (restartPolicy Never or
		// OnFailure) must not run again because its unit name changed.
		if !isDeploymentOwned(pod) && pod.Spec.RestartPolicy != "" && pod.Spec.RestartPolicy != corev1.RestartPolicyAlways &&
			(pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed) {
			continue
		}
		addPod(pod.Namespace, pod.Name, &pod.Spec, pod.Annotations)
	}
	for _, job := range s.config.Store.AllJobs() {
		if jobFinished(job) {
			continue
		}
		if _, _, missing := s.firstMissingClaim(job.Namespace, job.Spec.Template.Spec); missing {
			continue
		}
		services[names.Host(names.KindJobService, job.Namespace, job.Name)] = true
	}
	for _, cj := range s.config.Store.AllCronJobs() {
		if cronJobSuspended(cj) || cronJobWaitingForClaim(cj) {
			continue
		}
		if _, _, missing := s.firstMissingClaim(cj.Namespace, cj.Spec.JobTemplate.Spec.Template.Spec); missing {
			continue
		}
		if err := s.enableUnit(names.Host(names.KindCronJobTimer, cj.Namespace, cj.Name)); err != nil {
			fmt.Printf("names migration: enable %s: %v\n", names.Host(names.KindCronJobTimer, cj.Namespace, cj.Name), err)
		}
		services[names.Host(names.KindCronJobTimer, cj.Namespace, cj.Name)] = true
	}
	// Sockets first: a socket-activated service must find its fd already
	// listening when it starts.
	s.startSocketUnits(sortedKeys(sockets))
	for _, unit := range sortedKeys(services) {
		s.startUnit(unit)
	}
	started := len(sockets) + len(services)
	if started > 0 {
		fmt.Printf("names migration: ensured %d workload unit(s) are started\n", started)
	}
	return started
}
