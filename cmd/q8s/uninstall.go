package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"q8s/internal/names"
	"q8s/internal/quadlet"
	"q8s/internal/server"
	"q8s/internal/store"
)

// runCmd is the seam tests use to record (and stub) every systemctl and
// podman command uninstall runs.
var runCmd = func(name string, args ...string) error {
	return exec.Command(name, args...).Run()
}

// runCmdOutput is runCmd for the commands whose output uninstall needs to
// read (the Podman pod list and the volume existence check).
var runCmdOutput = func(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).Output()
}

// uninstallTimerStampDir is server.TimerStampDir in production; tests point it
// at a temporary directory.
var uninstallTimerStampDir = server.TimerStampDir

const q8sManagedByFilter = "label=app.kubernetes.io/managed-by=q8s"

var (
	errUninstallNeedsYes = errors.New("refusing to uninstall without --yes (stdin is not a terminal)")
	errUninstallAborted  = errors.New("uninstall aborted")
)

type uninstallOptions struct {
	yes   bool
	purge bool
	help  bool
}

func parseUninstallArgs(args []string) (uninstallOptions, error) {
	var opts uninstallOptions
	for _, arg := range args {
		switch arg {
		case "--yes":
			opts.yes = true
		case "--purge":
			opts.purge = true
		case "-h", "--help":
			opts.help = true
			return opts, nil
		default:
			return opts, fmt.Errorf("unknown argument %q", arg)
		}
	}
	return opts, nil
}

func stdinIsTerminal() bool {
	_, err := unix.IoctlGetTermios(int(os.Stdin.Fd()), unix.TCGETS)
	return err == nil
}

// confirmUninstall asks only when stdin is a terminal. A non-terminal stdin
// without --yes is refused, so an unattended script never removes a host by
// accident.
func confirmUninstall(yes, tty bool, in io.Reader, out io.Writer, purge bool) error {
	if yes {
		return nil
	}
	if !tty {
		return errUninstallNeedsYes
	}
	fmt.Fprintln(out, "This will stop q8s and remove its systemd units, quadlet files and Podman objects.")
	if purge {
		fmt.Fprintln(out, "The q8s data directory will be removed as well (--purge).")
	} else {
		fmt.Fprintln(out, "store.json and store.json.bak will be kept so 'q8s install && q8s serve' can restore the resources.")
	}
	fmt.Fprintln(out, "Podman volumes holding PVC data are never removed.")
	fmt.Fprint(out, `type "yes" to continue: `)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		return errUninstallAborted
	}
	if strings.TrimSpace(line) != "yes" {
		return errUninstallAborted
	}
	return nil
}

// runUninstallCommand is cmdUninstall without the process exit, so tests can
// drive the flags and the confirmation with a fake stdin.
func runUninstallCommand(args []string, stdin io.Reader, stdinTTY bool, stdout, stderr io.Writer) int {
	opts, err := parseUninstallArgs(args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		fmt.Fprint(stderr, uninstallUsage)
		return 2
	}
	if opts.help {
		fmt.Fprint(stdout, uninstallUsage)
		return 0
	}
	if err := confirmUninstall(opts.yes, stdinTTY, stdin, stdout, opts.purge); err != nil {
		fmt.Fprintln(stderr, err)
		if errors.Is(err, errUninstallNeedsYes) {
			return 2
		}
		return 1
	}
	rootful := os.Getuid() == 0
	runUninstall(rootful, resolveDirs(rootful), opts.purge, stdout, stderr)
	return 0
}

func cmdUninstall() {
	os.Exit(runUninstallCommand(os.Args[2:], os.Stdin, stdinIsTerminal(), os.Stdout, os.Stderr))
}

// runUninstall is the whole uninstall body. Every step is best effort: a
// failure is reported and the next step still runs, so a half-installed host
// is never left half-uninstalled on purpose.
func runUninstall(rootful bool, d dirs, purge bool, out, errOut io.Writer) {
	flags := systemctlFlags(rootful)

	// 1. The q8s units themselves, as before.
	for _, unit := range []string{"q8s.socket", "q8s-api.service"} {
		if err := systemctlCmd(flags, "stop", unit); err != nil {
			fmt.Fprintf(errOut, "warning: could not stop %s: %v\n", unit, err)
		}
		if err := systemctlCmd(flags, "disable", unit); err != nil {
			fmt.Fprintf(errOut, "warning: could not disable %s: %v\n", unit, err)
		}
	}

	// 2. Stop every generated unit of the stored objects.
	storeFile := filepath.Join(d.dataDir, "store.json")
	st, storeErr := store.Load(storeFile)
	if storeErr != nil {
		// Don't silently skip quadlet cleanup and still claim success: the
		// user must know container/volume/network files remain.
		fmt.Fprintf(errOut, "warning: could not read %s (%v); resource quadlet files were NOT cleaned up\n", storeFile, storeErr)
	}
	var groups [][]string
	if storeErr == nil {
		groups = uninstallUnits(st)
		// A pod socket file whose Pod is no longer in the store is still a
		// q8s socket. Add it to the socket group so it is stopped with the
		// same call, before any volume or network unit.
		stoppedSockets := make(map[string]bool)
		for _, unit := range groups[2] {
			stoppedSockets[unit] = true
		}
		for _, path := range podSocketFiles(d.systemdDir) {
			if unit := filepath.Base(path); !stoppedSockets[unit] {
				stoppedSockets[unit] = true
				groups[2] = append(groups[2], unit)
			}
		}
		sort.Strings(groups[2])
		// Both name forms are listed, but a host has only one of them
		// loaded. Stopping a unit that is not loaded fails with exit 5, so
		// keep the loaded ones: the counts and warnings then mean something.
		for i := range groups {
			groups[i] = loadedUnits(flags, groups[i])
		}
		for i, group := range groups {
			if len(group) == 0 {
				continue
			}
			if err := systemctlCmd(flags, "stop", group...); err != nil {
				fmt.Fprintf(errOut, "warning: could not stop %s units: %v\n", uninstallGroupName(i), err)
			}
			if uninstallGroupDisabled(i) {
				if err := systemctlCmd(flags, "disable", group...); err != nil {
					fmt.Fprintf(errOut, "warning: could not disable %s units: %v\n", uninstallGroupName(i), err)
				}
			}
		}
	}

	// 3. Remove the generated files. The socket glob also finds pod sockets
	// whose Pod object is no longer in the store.
	removedFiles := 0
	removedBase := make(map[string]bool)
	removeFile := func(path string) {
		if path == "" {
			return
		}
		if err := os.Remove(path); err != nil {
			if !os.IsNotExist(err) {
				fmt.Fprintf(errOut, "warning: could not remove %s: %v\n", path, err)
			}
			return
		}
		removedFiles++
		removedBase[filepath.Base(path)] = true
		fmt.Fprintf(out, "  removed %s\n", path)
	}

	// A serve stopped in the middle of fsutil.WriteFileAtomic leaves its
	// temporary file (.<name>.tmp-*) next to the target; it goes with it.
	removeWithTemps := func(path string) {
		removeFile(path)
		if path == "" {
			return
		}
		temps, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*"))
		for _, tmp := range temps {
			removeFile(tmp)
		}
	}
	if storeErr == nil {
		for _, path := range uninstallQuadletFiles(st, d) {
			removeWithTemps(path)
		}
		for _, path := range podSocketFiles(d.systemdDir) {
			removeFile(path)
		}
	}
	for _, f := range []string{"q8s.socket", "q8s-api.service"} {
		if d.systemdDir != "" {
			removeFile(filepath.Join(d.systemdDir, f))
		}
	}

	// 4. Remove the Podman objects q8s created. Volumes are deliberately
	// never touched: PVC data is kept. The label removals need no store; the
	// generated pod names and namespace networks do.
	removedPods, removedNetworks := 0, 0
	podNames := []string{}
	networkNames := []string{"q8s-default"}
	if storeErr == nil {
		podNames = uninstallPodNames(st)
		networkNames = uninstallNetworkNames(st)
	}
	if err := runCmd("podman", "rm", "-f", "--filter", q8sManagedByFilter); err != nil {
		fmt.Fprintf(errOut, "warning: could not remove q8s Podman containers: %v\n", err)
	}
	if listed, err := runCmdOutput("podman", "pod", "ps", "--filter", q8sManagedByFilter, "--format", "{{.Name}}"); err == nil {
		for _, line := range strings.Split(string(listed), "\n") {
			if name := strings.TrimSpace(line); name != "" {
				podNames = append(podNames, name)
			}
		}
	}
	podNames = uniqueStrings(podNames)
	if len(podNames) > 0 {
		args := append([]string{"pod", "rm", "-f", "--ignore"}, podNames...)
		if err := runCmd("podman", args...); err != nil {
			fmt.Fprintf(errOut, "warning: could not remove q8s Podman pods: %v\n", err)
		}
		removedPods = len(podNames)
	}
	networkNames = uniqueStrings(networkNames)
	if len(networkNames) > 0 {
		args := append([]string{"network", "rm", "--force"}, networkNames...)
		if err := runCmd("podman", args...); err != nil {
			fmt.Fprintf(errOut, "warning: could not remove q8s Podman networks: %v\n", err)
		}
		removedNetworks = len(networkNames)
	}

	// 5. Remove only the dangling .wants links that belong to a file this
	// run removed. Any other link is left alone.
	removedLinks := cleanDanglingWantsLinks(filepath.Join(d.systemdDir, "timers.target.wants"), removedBase)
	removedLinks = append(removedLinks, cleanDanglingWantsLinks(filepath.Join(d.systemdDir, "sockets.target.wants"), removedBase)...)
	for _, path := range removedLinks {
		fmt.Fprintf(out, "  removed dangling link %s\n", path)
	}

	// 6. Timer stamps and the runtime dirs holding materialised ConfigMaps
	// and Secrets.
	// The stamps go only with --purge: a kept store is restored by the next
	// `q8s install && q8s serve` under the same timer names, and a
	// Persistent= timer without its stamp would fire at once on that start.
	removedStamps := 0
	if storeErr == nil && purge {
		removedStamps = uninstallTimerStamps(st, uninstallTimerStampDir(), errOut)
	}
	for _, dir := range []string{d.configDir, d.secretDir} {
		if dir == "" {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			fmt.Fprintf(errOut, "warning: could not remove %s: %v\n", dir, err)
		}
	}
	// The parent (<runtime>/q8s) holds only q8s runtime state: the two dirs
	// above and the workload credential dir. serve is stopped, so all of it
	// goes. Guarded by name so a custom configDir never takes its parent.
	if d.configDir != "" {
		if parent := filepath.Dir(d.configDir); filepath.Base(parent) == "q8s" {
			if err := os.RemoveAll(parent); err != nil {
				fmt.Fprintf(errOut, "warning: could not remove %s: %v\n", parent, err)
			}
		}
	}

	// 7. Reload systemd and clear the failed state of the units just removed.
	if err := systemctlCmd(flags, "daemon-reload"); err != nil {
		fmt.Fprintf(errOut, "warning: daemon-reload failed: %v\n", err)
	}
	// After the reload only units that failed are still loaded; reset-failed
	// on an unloaded unit is an error.
	allUnits := loadedUnits(flags, flattenGroups(groups))
	if len(allUnits) > 0 {
		if err := systemctlCmd(flags, "reset-failed", allUnits...); err != nil {
			fmt.Fprintf(errOut, "warning: reset-failed failed: %v\n", err)
		}
	}

	// 8. Data directory: keep the store (and its backup) unless --purge.
	dataKept := true
	if purge {
		if d.dataDir != "" {
			if err := os.RemoveAll(d.dataDir); err != nil {
				fmt.Fprintf(errOut, "warning: could not remove %s: %v\n", d.dataDir, err)
			} else {
				dataKept = false
			}
		}
	} else {
		bakFile := storeFile + ".bak"
		if storeErr == nil {
			if data, err := os.ReadFile(storeFile); err == nil {
				if err := os.WriteFile(bakFile, data, 0600); err == nil {
					fmt.Fprintf(out, "Store backed up to %s\n", bakFile)
				}
			}
		}
		// Remove the rest of the installation. store.json.bak is kept: it is
		// the restore path for `q8s install && q8s serve` after uninstall.
		// The certs directory includes the CA private key, so it must go.
		for _, p := range []string{
			filepath.Join(d.dataDir, "certs"),     // ca.key, server/client keys
			filepath.Join(d.dataDir, "traefik"),   // generated ingress dynamic configs
			filepath.Join(d.dataDir, "quadlets"),  // legacy empty dir from old installs
			filepath.Join(d.dataDir, "snapshots"), // gzip store snapshots, derived from store.json
			filepath.Join(d.dataDir, "config.json"),
		} {
			if err := os.RemoveAll(p); err != nil {
				fmt.Fprintf(errOut, "warning: could not remove %s: %v\n", p, err)
			} else {
				fmt.Fprintf(out, "  removed %s\n", p)
			}
		}
	}

	// 9. Summary: what happened, and what is still there.
	fmt.Fprintln(out)
	fmt.Fprintln(out, "q8s uninstalled.")
	for i := range groups {
		fmt.Fprintf(out, "  %s: %d stopped\n", uninstallGroupName(i), len(groups[i]))
	}
	fmt.Fprintf(out, "  files: %d removed\n", removedFiles)
	fmt.Fprintf(out, "  dangling links: %d removed\n", len(removedLinks))
	fmt.Fprintf(out, "  Podman pods: %d removed\n", removedPods)
	fmt.Fprintf(out, "  Podman networks: %d removed\n", removedNetworks)
	fmt.Fprintf(out, "  timer stamps: %d removed\n", removedStamps)

	fmt.Fprintln(out)
	if dataKept {
		fmt.Fprintf(out, "Kept the q8s data directory: %s\n", d.dataDir)
		fmt.Fprintf(out, "Remove it with: rm -rf %s\n", d.dataDir)
		fmt.Fprintln(out, "Run 'q8s install && q8s serve' to reinstall; existing resources will be restored from the backup.")
	} else {
		fmt.Fprintln(out, "Removed the q8s data directory (--purge).")
	}

	// List any Podman volumes that belonged to q8s PVCs: data is never
	// deleted automatically, with or without --purge.
	if storeErr == nil {
		hints := pvcVolumeRemovalHints(st.AllPVCs(), func(name string) bool {
			out, err := runCmdOutput("podman", "volume", "inspect", "--format", "{{.Name}}", name)
			return err == nil && len(out) > 0
		})
		if len(hints) > 0 {
			fmt.Fprintln(out)
			fmt.Fprintln(out, "The following Podman volumes still contain your data and were NOT deleted:")
			for _, hint := range hints {
				fmt.Fprintf(out, "  %s\n", hint)
			}
			fmt.Fprintln(out)
			fmt.Fprintln(out, "Delete them manually with the commands above if you no longer need the data.")
		}
	}
}

// loadedUnits returns the units systemd has loaded (in any state, failed
// included), in the given order. If the query itself fails, it returns all of
// them so the stop is still attempted.
func loadedUnits(flags []string, units []string) []string {
	if len(units) == 0 {
		return units
	}
	args := append(append([]string{}, flags...), "list-units", "--all", "--plain", "--no-legend", "--full")
	listed, err := runCmdOutput("systemctl", append(args, units...)...)
	if err != nil {
		return units
	}
	loaded := make(map[string]bool)
	for _, line := range strings.Split(string(listed), "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			loaded[f[0]] = true
		}
	}
	var out []string
	for _, u := range units {
		if loaded[u] {
			out = append(out, u)
		}
	}
	return out
}

func systemctlCmd(flags []string, verb string, units ...string) error {
	args := append([]string{}, flags...)
	args = append(args, verb)
	args = append(args, units...)
	return runCmd("systemctl", args...)
}

func uninstallGroupName(i int) string {
	switch i {
	case 0:
		return "timers"
	case 1:
		return "services"
	case 2:
		return "sockets"
	case 3:
		return "volume units"
	case 4:
		return "network units"
	default:
		return "units"
	}
}

func uninstallGroupDisabled(i int) bool {
	return i == 0 || i == 2
}

func flattenGroups(groups [][]string) []string {
	var all []string
	for _, group := range groups {
		all = append(all, group...)
	}
	return all
}

// unitSet keeps the first occurrence order while removing duplicates.
type unitSet struct {
	seen map[string]bool
	list []string
}

func newUnitSet() *unitSet {
	return &unitSet{seen: make(map[string]bool)}
}

func (s *unitSet) add(items ...string) {
	for _, item := range items {
		if item == "" || s.seen[item] {
			continue
		}
		s.seen[item] = true
		s.list = append(s.list, item)
	}
}

func (s *unitSet) values() []string {
	return s.list
}

func uniqueStrings(items []string) []string {
	set := newUnitSet()
	set.add(items...)
	return sortedUnits(set)
}

func sortedUnits(set *unitSet) []string {
	units := set.values()
	sort.Strings(units)
	return units
}

// uninstallUnits returns the generated systemd units of every stored object,
// in the order they must be stopped: timers, services, sockets, volume units,
// then network units. Both the current and the legacy generated name are
// included. Every group is deduplicated.
func uninstallUnits(st *store.Store) [][]string {
	timers := newUnitSet()
	services := newUnitSet()
	sockets := newUnitSet()
	volumes := newUnitSet()
	networks := newUnitSet()

	for _, cj := range st.AllCronJobs() {
		addBoth(timers, names.KindCronJobTimer, cj.Namespace, cj.Name)
		addBoth(services, names.KindCronJobService, cj.Namespace, cj.Name)
	}
	for _, job := range st.AllJobs() {
		addBoth(services, names.KindJobService, job.Namespace, job.Name)
	}
	for _, dep := range st.AllDeployments() {
		for i := int32(0); i < uninstallReplicaCount(dep); i++ {
			instance := quadlet.DeploymentInstanceName(dep.Name, i)
			addPodUnits(services, dep.Namespace, instance, dep.Spec.Template.Spec.Containers)
			addSocketUnits(sockets, dep.Namespace, instance, dep.Spec.Template.Annotations)
		}
	}
	for _, pod := range st.AllPods() {
		addPodUnits(services, pod.Namespace, pod.Name, pod.Spec.Containers)
		addSocketUnits(sockets, pod.Namespace, pod.Name, pod.Annotations)
	}
	for _, pvc := range st.AllPVCs() {
		if pvcUsesHostPath(pvc) {
			continue
		}
		volumes.add(
			names.Host(names.KindPVCVolume, pvc.Namespace, pvc.Name)+"-volume.service",
			names.LegacyHost(names.KindPVCVolume, pvc.Namespace, pvc.Name)+"-volume.service",
			quadlet.PVCVolumeFileBase(pvc)+"-volume.service")
	}
	for _, ns := range st.Namespaces() {
		networks.add(names.Host(names.KindNetwork, ns.Name, "") + "-network.service")
	}
	networks.add("q8s-default-network.service")

	return [][]string{
		sortedUnits(timers),
		sortedUnits(services),
		sortedUnits(sockets),
		sortedUnits(volumes),
		sortedUnits(networks),
	}
}

func addBoth(set *unitSet, kind names.Kind, namespace, name string, extra ...string) {
	set.add(names.Host(kind, namespace, name, extra...), names.LegacyHost(kind, namespace, name, extra...))
}

func addPodUnits(services *unitSet, namespace, name string, containers []corev1.Container) {
	if len(containers) > 1 {
		addBoth(services, names.KindPodUnit, namespace, name)
		for _, c := range containers {
			addBoth(services, names.KindPodContainerService, namespace, name, c.Name)
		}
		return
	}
	addBoth(services, names.KindPodService, namespace, name)
}

func addSocketUnits(sockets *unitSet, namespace, name string, annotations map[string]string) {
	entries, err := quadlet.ParseSocketActivation(annotations)
	if err != nil {
		return
	}
	for _, entry := range entries {
		addBoth(sockets, names.KindSocketFile, namespace, name, entry.Name)
	}
}

func uninstallReplicaCount(dep *appsv1.Deployment) int32 {
	n := quadlet.DeploymentReplicaCount(dep)
	if n < 1 {
		return 1
	}
	return n
}

// uninstallPodNames returns the Podman pod names q8s generated for
// multi-container pod objects, in both name formats.
func uninstallPodNames(st *store.Store) []string {
	set := newUnitSet()
	for _, dep := range st.AllDeployments() {
		if len(dep.Spec.Template.Spec.Containers) <= 1 {
			continue
		}
		for i := int32(0); i < uninstallReplicaCount(dep); i++ {
			addBoth(set, names.KindPod, dep.Namespace, quadlet.DeploymentInstanceName(dep.Name, i))
		}
	}
	for _, pod := range st.AllPods() {
		if len(pod.Spec.Containers) > 1 {
			addBoth(set, names.KindPod, pod.Namespace, pod.Name)
		}
	}
	return sortedUnits(set)
}

// uninstallNetworkNames returns the Podman network of every stored namespace
// plus the default network.
func uninstallNetworkNames(st *store.Store) []string {
	set := newUnitSet()
	for _, ns := range st.Namespaces() {
		set.add(names.Host(names.KindNetwork, ns.Name, ""))
	}
	set.add("q8s-default")
	return sortedUnits(set)
}

// uninstallTimerStamps removes the persistent timer stamps of the CronJob
// timers uninstall just stopped, in both name formats.
func uninstallTimerStamps(st *store.Store, dir string, errOut io.Writer) int {
	if dir == "" {
		return 0
	}
	removed := 0
	seen := make(map[string]bool)
	for _, cj := range st.AllCronJobs() {
		for _, timer := range []string{
			names.Host(names.KindCronJobTimer, cj.Namespace, cj.Name),
			names.LegacyHost(names.KindCronJobTimer, cj.Namespace, cj.Name),
		} {
			if seen[timer] {
				continue
			}
			seen[timer] = true
			path := filepath.Join(dir, "stamp-"+timer)
			if err := os.Remove(path); err != nil {
				if !os.IsNotExist(err) {
					fmt.Fprintf(errOut, "warning: could not remove %s: %v\n", path, err)
				}
				continue
			}
			removed++
		}
	}
	return removed
}

// podSocketFiles returns the pod socket files q8s wrote into the systemd
// directory. Only files carrying the pod owner key are q8s-generated sockets.
func podSocketFiles(systemdDir string) []string {
	if systemdDir == "" {
		return nil
	}
	socks, err := filepath.Glob(filepath.Join(systemdDir, "*.socket"))
	if err != nil {
		return nil
	}
	var paths []string
	for _, path := range socks {
		content, err := os.ReadFile(path)
		if err != nil || !strings.Contains(string(content), "\nX-Q8s-Namespace=") {
			continue
		}
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

// cleanDanglingWantsLinks removes links in one .wants directory whose name is
// a removed unit and whose target no longer exists. Unrelated dangling links
// and links to files that still exist are kept. The directory itself is
// removed when it becomes empty.
func cleanDanglingWantsLinks(dir string, removedBase map[string]bool) []string {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var removed []string
	for _, entry := range entries {
		link := filepath.Join(dir, entry.Name())
		info, err := os.Lstat(link)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		// Only links to a unit this run removed are candidates. A dangling
		// link belonging to anything else is not q8s's to delete.
		if !removedBase[entry.Name()] {
			continue
		}
		target, err := os.Readlink(link)
		if err != nil {
			continue
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(dir, target)
		}
		if _, err := os.Stat(target); err == nil {
			continue
		}
		if err := os.Remove(link); err == nil {
			removed = append(removed, link)
		}
	}
	if left, err := os.ReadDir(dir); err == nil && len(left) == 0 {
		_ = os.Remove(dir)
	}
	return removed
}

// uninstallQuadletFiles returns every quadlet and timer file uninstall removes
// for the stored objects, in both the current and the pre-upgrade name. PVC
// volumes themselves are never removed; only their .volume file is listed.
// Pod socket files are handled separately by the owning-label glob.
func uninstallQuadletFiles(st *store.Store, d dirs) []string {
	seen := make(map[string]bool)
	var paths []string
	add := func(items ...string) {
		for _, path := range items {
			if path == "" || seen[path] {
				continue
			}
			seen[path] = true
			paths = append(paths, path)
		}
	}
	file := func(dir, name string) string {
		if name == "" {
			return ""
		}
		return filepath.Join(dir, name)
	}
	for _, dep := range st.AllDeployments() {
		for i := int32(0); i < uninstallReplicaCount(dep); i++ {
			instance := quadlet.DeploymentInstanceName(dep.Name, i)
			add(
				file(d.quadletDir, names.Host(names.KindPodContainerFile, dep.Namespace, instance, "")),
				file(d.quadletDir, names.LegacyHost(names.KindPodContainerFile, dep.Namespace, instance, "")),
				file(d.quadletDir, names.Host(names.KindPodFile, dep.Namespace, instance)),
				file(d.quadletDir, names.LegacyHost(names.KindPodFile, dep.Namespace, instance)))
			for _, c := range dep.Spec.Template.Spec.Containers {
				add(
					file(d.quadletDir, names.Host(names.KindPodContainerFile, dep.Namespace, instance, c.Name)),
					file(d.quadletDir, names.LegacyHost(names.KindPodContainerFile, dep.Namespace, instance, c.Name)))
			}
		}
	}
	for _, pod := range st.AllPods() {
		if len(pod.Spec.Containers) > 1 {
			add(
				file(d.quadletDir, names.Host(names.KindPodFile, pod.Namespace, pod.Name)),
				file(d.quadletDir, names.LegacyHost(names.KindPodFile, pod.Namespace, pod.Name)))
			for _, c := range pod.Spec.Containers {
				add(
					file(d.quadletDir, names.Host(names.KindPodContainerFile, pod.Namespace, pod.Name, c.Name)),
					file(d.quadletDir, names.LegacyHost(names.KindPodContainerFile, pod.Namespace, pod.Name, c.Name)))
			}
			continue
		}
		add(
			file(d.quadletDir, names.Host(names.KindPodContainerFile, pod.Namespace, pod.Name, "")),
			file(d.quadletDir, names.LegacyHost(names.KindPodContainerFile, pod.Namespace, pod.Name, "")))
	}
	for _, pvc := range st.AllPVCs() {
		if pvcUsesHostPath(pvc) {
			continue
		}
		add(
			file(d.quadletDir, quadlet.PVCVolumeFileBase(pvc)+".volume"),
			file(d.quadletDir, names.Host(names.KindPVCFile, pvc.Namespace, pvc.Name)),
			file(d.quadletDir, names.LegacyHost(names.KindPVCFile, pvc.Namespace, pvc.Name)))
	}
	for _, job := range st.AllJobs() {
		add(
			file(d.quadletDir, names.Host(names.KindJobFile, job.Namespace, job.Name)),
			file(d.quadletDir, names.LegacyHost(names.KindJobFile, job.Namespace, job.Name)))
	}
	for _, cj := range st.AllCronJobs() {
		add(
			file(d.quadletDir, names.Host(names.KindCronJobFile, cj.Namespace, cj.Name)),
			file(d.quadletDir, names.LegacyHost(names.KindCronJobFile, cj.Namespace, cj.Name)))
		add(
			file(d.systemdDir, names.Host(names.KindCronJobTimer, cj.Namespace, cj.Name)),
			file(d.systemdDir, names.LegacyHost(names.KindCronJobTimer, cj.Namespace, cj.Name)))
	}
	for _, ns := range st.Namespaces() {
		add(file(d.quadletDir, names.Host(names.KindNetwork, ns.Name, "")+".network"))
	}
	return paths
}

// pvcVolumeRemovalHints returns the `podman volume rm` command for every PVC
// whose Podman volume still exists. A PVC's volume is spec.volumeName, not the
// claim name; hostpath claims have no Podman volume at all. inspect reports
// whether a volume exists, so the command is only printed when it would work.
func pvcVolumeRemovalHints(pvcs []*corev1.PersistentVolumeClaim, inspect func(name string) bool) []string {
	var hints []string
	for _, pvc := range pvcs {
		if pvcUsesHostPath(pvc) {
			continue
		}
		volName := pvc.Spec.VolumeName
		if volName == "" {
			// A claim stored without a binding has no volume yet; the hint
			// names the volume a create would use.
			volName = quadlet.PVCVolumeName(pvc.Namespace, pvc.Name)
		}
		if !inspect(volName) {
			continue
		}
		hints = append(hints, fmt.Sprintf("podman volume rm %s   # PVC %s/%s", volName, pvc.Namespace, pvc.Name))
	}
	return hints
}

// pvcUsesHostPath reports whether a PVC binds a host directory instead of a
// Podman named volume.
func pvcUsesHostPath(pvc *corev1.PersistentVolumeClaim) bool {
	if pvc.Spec.StorageClassName != nil && *pvc.Spec.StorageClassName == quadlet.StorageClassHostPath {
		return true
	}
	_, ok := pvc.Annotations[quadlet.AnnotationHostPath]
	return ok
}
