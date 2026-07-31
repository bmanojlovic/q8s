package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/coreos/go-systemd/v22/daemon"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"q8s/internal/podman"
	"q8s/internal/quadlet"
	"q8s/internal/server"
	"q8s/internal/store"
	"q8s/internal/systemd"
	"q8s/pkg/install"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "serve":
		cmdServe()
	case "install":
		cmdInstall()
	case "uninstall":
		cmdUninstall()
	case "status":
		cmdStatus()
	case "start":
		cmdStart()
	case "stop":
		cmdStop()
	case "enable":
		cmdEnable()
	case "disable":
		cmdDisable()
	case "kubeconfig":
		cmdKubeconfig()
	case "-h", "--help", "help":
		printUsage()
	case "-v", "--version", "version":
		fmt.Println(version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println("Usage: q8s <command>")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  serve      Run the q8s API server (used by systemd)")
	fmt.Println("  install    Generate certs, create dirs, install systemd units")
	fmt.Println("  uninstall  Remove q8s installation")
	fmt.Println("  status     Show q8s server status and connectivity")
	fmt.Println("  start      Start the q8s socket (begin accepting connections)")
	fmt.Println("  stop       Stop the q8s socket and API server")
	fmt.Println("  enable     Enable and start q8s socket on boot")
	fmt.Println("  disable    Disable q8s socket (stop and remove from boot)")
	fmt.Println("  kubeconfig Print a kubeconfig file for this q8s instance")
}

// --- Path resolution ---

type dirs struct {
	dataDir    string
	quadletDir string
	configDir  string
	secretDir  string
	systemdDir string
}

func resolveDirs(rootful bool) dirs {
	if rootful {
		return dirs{
			dataDir:    "/etc/q8s",
			quadletDir: "/etc/containers/systemd",
			configDir:  "/run/q8s/configmaps",
			secretDir:  "/run/q8s/secrets",
			systemdDir: "/etc/systemd/system",
		}
	}
	home := os.Getenv("HOME")
	xdgConfig := os.Getenv("XDG_CONFIG_HOME")
	if xdgConfig == "" {
		xdgConfig = home + "/.config"
	}
	xdgRuntime := os.Getenv("XDG_RUNTIME_DIR")
	if xdgRuntime == "" {
		xdgRuntime = fmt.Sprintf("/run/user/%d", os.Getuid())
	}
	return dirs{
		dataDir:    home + "/.local/share/q8s",
		quadletDir: xdgConfig + "/containers/systemd",
		configDir:  xdgRuntime + "/q8s/configmaps",
		secretDir:  xdgRuntime + "/q8s/secrets",
		systemdDir: xdgConfig + "/systemd/user",
	}
}

func systemctlFlags(rootful bool) []string {
	if rootful {
		return nil
	}
	return []string{"--user"}
}

// resolvePort determines the q8s API port with the following precedence:
// 1. Q8S_PORT env var, if set — an explicit override for this invocation.
// 2. The port already baked into the installed q8s.socket unit, if any —
//    the systemd unit is the durable source of truth once installed, so
//    separate invocations (install, serve, status, kubeconfig) agree on the
//    port without needing Q8S_PORT set consistently in every shell.
// 3. install.DefaultPort (6443).
func resolvePort(d dirs) int {
	if v := os.Getenv("Q8S_PORT"); v != "" {
		if port, err := strconv.Atoi(v); err == nil && port > 0 {
			return port
		}
		fmt.Fprintf(os.Stderr, "warning: ignoring invalid Q8S_PORT %q\n", v)
	}
	if data, err := os.ReadFile(d.systemdDir + "/q8s.socket"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			v, ok := strings.CutPrefix(strings.TrimSpace(line), "ListenStream=")
			if !ok {
				continue
			}
			if port, err := strconv.Atoi(v); err == nil && port > 0 {
				return port
			}
		}
	}
	return install.DefaultPort
}

// --- Management commands ---

func cmdInstall() {
	rootful := os.Getuid() == 0
	d := resolveDirs(rootful)

	var extraIPs []net.IP
	var extraDNS []string
	regenCerts := false

	args := os.Args[2:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-h", "--help":
			fmt.Println("Usage: q8s install [flags]")
			fmt.Println()
			fmt.Println("Flags:")
			fmt.Println("  --san-ip <ip>        Add extra IP to server certificate SAN")
			fmt.Println("  --san-dns <name>     Add extra DNS name to server certificate SAN")
			fmt.Println("  --regenerate-certs   Force certificate regeneration")
			return
		case "--san-ip":
			i++
			if i >= len(args) {
				fmt.Fprintln(os.Stderr, "--san-ip requires a value")
				os.Exit(1)
			}
			ip := net.ParseIP(args[i])
			if ip == nil {
				fmt.Fprintf(os.Stderr, "invalid IP: %s\n", args[i])
				os.Exit(1)
			}
			extraIPs = append(extraIPs, ip)
			regenCerts = true
		case "--san-dns":
			i++
			if i >= len(args) {
				fmt.Fprintln(os.Stderr, "--san-dns requires a value")
				os.Exit(1)
			}
			extraDNS = append(extraDNS, args[i])
			regenCerts = true
		case "--regenerate-certs":
			regenCerts = true
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s\n", args[i])
			os.Exit(1)
		}
	}

	if err := install.Install(install.InstallConfig{
		Rootful:         rootful,
		Home:            os.Getenv("HOME"),
		Port:            resolvePort(d),
		ExtraSANIPs:     extraIPs,
		ExtraSANDNS:     extraDNS,
		RegenerateCerts: regenCerts,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "install failed: %v\n", err)
		os.Exit(1)
	}
}

func cmdUninstall() {
	rootful := os.Getuid() == 0
	d := resolveDirs(rootful)
	flags := systemctlFlags(rootful)

	for _, unit := range []string{"q8s.socket", "q8s-api.service"} {
		exec.Command("systemctl", append(flags, "stop", unit)...).Run()
		exec.Command("systemctl", append(flags, "disable", unit)...).Run()
	}

	storeFile := d.dataDir + "/store.json"
	st, storeErr := store.Load(storeFile)
	if storeErr == nil {
		removed := 0
		rm := func(path string) {
			if os.Remove(path) == nil {
				fmt.Printf("  removed %s\n", path)
				removed++
			}
		}
		for _, dep := range st.AllDeployments() {
			n := int32(1)
			if dep.Spec.Replicas != nil && *dep.Spec.Replicas > 1 {
				n = *dep.Spec.Replicas
			}
			for i := int32(0); i < n; i++ {
				rm(fmt.Sprintf("%s/%s-%s-%d.container", d.quadletDir, dep.Namespace, dep.Name, i))
			}
		}
		for _, pod := range st.AllPods() {
			rm(fmt.Sprintf("%s/%s-%s.container", d.quadletDir, pod.Namespace, pod.Name))
		}
		for _, pvc := range st.AllPVCs() {
			rm(fmt.Sprintf("%s/%s-%s.volume", d.quadletDir, pvc.Namespace, pvc.Name))
		}
		for _, job := range st.AllJobs() {
			rm(fmt.Sprintf("%s/%s-%s-job.container", d.quadletDir, job.Namespace, job.Name))
		}
		for _, cj := range st.AllCronJobs() {
			rm(fmt.Sprintf("%s/%s-%s-cron.container", d.quadletDir, cj.Namespace, cj.Name))
			rm(fmt.Sprintf("%s/%s-%s-cron.timer", d.systemdDir, cj.Namespace, cj.Name))
		}
		for _, ns := range st.Namespaces() {
			rm(fmt.Sprintf("%s/q8s-%s.network", d.quadletDir, ns.Name))
		}
		if removed > 0 {
			fmt.Printf("Removed %d quadlet/timer file(s).\n", removed)
		}
		bakFile := storeFile + ".bak"
		if data, err := os.ReadFile(storeFile); err == nil {
			if err := os.WriteFile(bakFile, data, 0600); err == nil {
				fmt.Printf("Store backed up to %s\n", bakFile)
			}
		}
	}

	for _, f := range []string{"q8s.socket", "q8s-api.service"} {
		os.Remove(d.systemdDir + "/" + f)
	}
	exec.Command("systemctl", append(flags, "daemon-reload")...).Run()

	fmt.Println("q8s uninstalled.")
	if storeErr == nil {
		fmt.Println("Run 'q8s install && q8s serve' to reinstall — existing resources will be restored from backup.")
	}

	// List any Podman volumes that belonged to q8s PVCs — data is never deleted automatically.
	if storeErr == nil {
		pvcs := st.AllPVCs()
		if len(pvcs) > 0 {
			fmt.Println()
			fmt.Println("The following Podman volumes still contain your data and were NOT deleted:")
			for _, pvc := range pvcs {
				// Check if the volume actually exists in Podman before listing it.
				out, err := exec.Command("podman", "volume", "inspect", "--format", "{{.Name}}", pvc.Name).Output()
				if err == nil && len(out) > 0 {
					fmt.Printf("  podman volume rm %s   # PVC %s/%s\n", pvc.Name, pvc.Namespace, pvc.Name)
				}
			}
			fmt.Println()
			fmt.Println("Delete them manually with the commands above if you no longer need the data.")
		}
	}
}

func cmdStatus() {
	rootful := os.Getuid() == 0
	d := resolveDirs(rootful)
	flags := systemctlFlags(rootful)
	port := resolvePort(d)

	socketOut, _ := exec.Command("systemctl", append(flags, "is-active", "q8s.socket")...).Output()
	socketActive := string(socketOut) == "active\n"

	serviceOut, _ := exec.Command("systemctl", append(flags, "is-active", "q8s-api.service")...).Output()
	serviceActive := string(serviceOut) == "active\n"

	conn, dialErr := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", port), time.Second)
	if conn != nil {
		conn.Close()
	}
	listening := dialErr == nil

	if !socketActive {
		fmt.Println("q8s socket: inactive (run: q8s start)")
		os.Exit(1)
	}
	fmt.Println("q8s socket: active")
	if serviceActive {
		fmt.Println("q8s server: running")
	} else {
		fmt.Println("q8s server: idle (socket-activated, will start on first connection)")
	}
	if listening {
		fmt.Printf("q8s port %d: reachable\n", port)
	} else {
		fmt.Printf("q8s port %d: not reachable\n", port)
	}
}

func cmdStart() {
	rootful := os.Getuid() == 0
	args := append(systemctlFlags(rootful), "start", "q8s.socket")
	if out, err := exec.Command("systemctl", args...).CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to start q8s.socket: %v\n%s", err, out)
		os.Exit(1)
	}
	fmt.Println("q8s.socket started")
}

func cmdStop() {
	rootful := os.Getuid() == 0
	flags := systemctlFlags(rootful)
	for _, unit := range []string{"q8s-api.service", "q8s.socket"} {
		args := append(flags, "stop", unit)
		if out, err := exec.Command("systemctl", args...).CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to stop %s: %v\n%s", unit, err, out)
		}
	}
	fmt.Println("q8s stopped")
}

func cmdEnable() {
	rootful := os.Getuid() == 0
	args := append(systemctlFlags(rootful), "enable", "--now", "q8s.socket")
	if out, err := exec.Command("systemctl", args...).CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to enable q8s.socket: %v\n%s", err, out)
		os.Exit(1)
	}
	fmt.Println("q8s.socket enabled and started")
}

func cmdDisable() {
	rootful := os.Getuid() == 0
	flags := systemctlFlags(rootful)
	for _, unit := range []string{"q8s-api.service", "q8s.socket"} {
		exec.Command("systemctl", append(flags, "stop", unit)...).Run()
	}
	args := append(flags, "disable", "q8s.socket")
	if out, err := exec.Command("systemctl", args...).CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to disable q8s.socket: %v\n%s", err, out)
		os.Exit(1)
	}
	fmt.Println("q8s.socket disabled and stopped")
}

func cmdKubeconfig() {
	d := resolveDirs(os.Getuid() == 0)
	certDir := d.dataDir + "/certs"
	port := resolvePort(d)
	readB64 := func(path string) string {
		b, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to read %s: %v\n", path, err)
			os.Exit(1)
		}
		return base64.StdEncoding.EncodeToString(b)
	}
	fmt.Printf(`apiVersion: v1
kind: Config
clusters:
- cluster:
    certificate-authority-data: %s
    server: https://localhost:%d
  name: q8s
contexts:
- context:
    cluster: q8s
    user: q8s-user
  name: q8s
current-context: q8s
preferences: {}
users:
- name: q8s-user
  user:
    client-certificate-data: %s
    client-key-data: %s
`, readB64(certDir+"/ca.crt"), port, readB64(certDir+"/client.crt"), readB64(certDir+"/client.key"))
}

// --- Server ---

func cmdServe() {
	rootful := os.Getuid() == 0
	mode := systemd.ModeRootless
	if rootful {
		mode = systemd.ModeRootful
	}
	d := resolveDirs(rootful)

	caCert, err := os.ReadFile(d.dataDir + "/certs/ca.crt")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to read CA cert: %v\n", err)
		os.Exit(1)
	}
	certPEM, err := os.ReadFile(d.dataDir + "/certs/server.crt")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to read server cert: %v\n", err)
		os.Exit(1)
	}
	keyPEM, err := os.ReadFile(d.dataDir + "/certs/server.key")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to read server key: %v\n", err)
		os.Exit(1)
	}

	dataFile := d.dataDir + "/store.json"
	bakFile := dataFile + ".bak"
	if _, err := os.Stat(dataFile); os.IsNotExist(err) {
		if _, err := os.Stat(bakFile); err == nil {
			if err := os.Rename(bakFile, dataFile); err == nil {
				fmt.Println("Restored store from backup (store.json.bak → store.json).")
			}
		}
	}
	st, err := store.Load(dataFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load store: %v\n", err)
		os.Exit(1)
	}

	mgr, err := systemd.NewManager(mode)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: systemd manager unavailable, containers won't auto-start: %v\n", err)
	}
	if mgr != nil {
		defer mgr.Close()
	}

	srv, err := server.New(server.Config{
		Store:      st,
		CACert:     caCert,
		CertPEM:    certPEM,
		KeyPEM:     keyPEM,
		QuadletDir: d.quadletDir,
		SystemdDir: d.systemdDir,
		ConfigDir:  d.configDir,
		TraefikDir: d.dataDir + "/traefik",
		Mode:       mode,
		Manager:    mgr,
		Port:       resolvePort(d),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create server: %v\n", err)
		os.Exit(1)
	}

	var listeners []net.Listener
	if os.Getenv("LISTEN_FDS") != "" {
		listeners, err = systemd.ListenersFromSystemd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to get listeners from systemd: %v\n", err)
			os.Exit(1)
		}
	}
	if len(listeners) == 0 {
		listeners, err = systemd.DefaultListeners(resolvePort(d))
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create listeners: %v\n", err)
		os.Exit(1)
	}

	for _, l := range listeners {
		fmt.Printf("Listening on: %s\n", l.Addr())
	}
	if err := srv.StartTLS(listeners); err != nil {
		fmt.Fprintf(os.Stderr, "failed to start server: %v\n", err)
		os.Exit(1)
	}

	restoreConfigMapFiles(d.configDir, st)
	restoreSecretFiles(d.secretDir, st)
	ensureNamespaceNetworks(d.quadletDir, mgr, st)
	srv.ReconcileQuadlets()
	reconcilePodmanPods(st, mgr)

	if mgr != nil {
		go syncLoop(st, mgr)
	}

	daemon.SdNotify(false, daemon.SdNotifyReady)
	fmt.Println("q8s API server started")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "server shutdown error: %v\n", err)
	}
	daemon.SdNotify(false, daemon.SdNotifyStopping)
	fmt.Println("q8s API server stopped")
}

func ensureNamespaceNetworks(quadletDir string, mgr *systemd.Manager, st *store.Store) {
	if quadletDir == "" {
		return
	}
	needReload := false
	for _, ns := range st.Namespaces() {
		filename := quadletDir + "/q8s-" + ns.Name + ".network"
		if _, err := os.Stat(filename); err == nil {
			continue
		}
		content, err := quadlet.Network(ns.Name)
		if err != nil {
			continue
		}
		if err := os.MkdirAll(quadletDir, 0755); err != nil {
			continue
		}
		if err := os.WriteFile(filename, content, 0644); err != nil {
			fmt.Printf("failed to write network quadlet for namespace %s: %v\n", ns.Name, err)
			continue
		}
		fmt.Printf("generated network quadlet: q8s-%s.network\n", ns.Name)
		needReload = true
	}
	if needReload && mgr != nil {
		if err := mgr.DaemonReload(); err != nil {
			fmt.Printf("daemon-reload after network setup failed: %v\n", err)
		}
	}
}

func restoreSecretFiles(secretDir string, st *store.Store) {
	if secretDir == "" {
		return
	}
	for _, sec := range st.AllSecrets() {
		dir := secretDir + "/" + sec.Namespace + "/" + sec.Name
		if err := os.MkdirAll(dir, 0700); err != nil {
			fmt.Printf("restore secret %s/%s: mkdir: %v\n", sec.Namespace, sec.Name, err)
			continue
		}
		for key, val := range sec.Data {
			if err := os.WriteFile(dir+"/"+key, val, 0600); err != nil {
				fmt.Printf("restore secret %s/%s key %s: %v\n", sec.Namespace, sec.Name, key, err)
			}
		}
		fmt.Printf("restored secret files: %s/%s\n", sec.Namespace, sec.Name)
	}
}

func restoreConfigMapFiles(configDir string, st *store.Store) {
	if configDir == "" {
		return
	}
	for _, cm := range st.ConfigMapFiles() {
		dir := configDir + "/" + cm.Namespace + "/" + cm.Name
		if err := os.MkdirAll(dir, 0755); err != nil {
			fmt.Printf("restore configmap %s/%s: mkdir: %v\n", cm.Namespace, cm.Name, err)
			continue
		}
		for key, val := range cm.Data {
			if err := os.WriteFile(dir+"/"+key, []byte(val), 0644); err != nil {
				fmt.Printf("restore configmap %s/%s key %s: %v\n", cm.Namespace, cm.Name, key, err)
			}
		}
		fmt.Printf("restored configmap files: %s/%s\n", cm.Namespace, cm.Name)
	}
}

// reconcilePodmanPods keeps the store's Pod objects in sync with q8s-labeled
// Podman containers. It's the mechanism that makes Deployment instances
// visible to `kubectl get pods`: deployment quadlets carry the same
// io.kubernetes.pod.name/namespace labels as standalone pods (see
// quadlet.Container), plus io.kubernetes.pod.deployment identifying the
// owner, so they're imported here exactly like any other pre-existing
// container — no separate bookkeeping in the Deployment handlers.
//
// Standalone pods are only ever imported, never pruned here (their lifecycle
// is owned by the Pod API). Deployment-derived pods are also pruned once
// their container disappears (scale down, rollout restart, deployment
// delete), since nothing else notices that on their behalf.
func reconcilePodmanPods(st *store.Store, mgr *systemd.Manager) {
	containers, err := podman.ListOwned()
	if err != nil {
		fmt.Printf("reconcile pods: %v\n", err)
		return
	}

	live := make(map[string]bool, len(containers))
	imported := 0
	for _, c := range containers {
		ns, name := c.PodNamespace(), c.PodName()
		if ns == "" || name == "" {
			continue
		}
		live[ns+"/"+name] = true

		// Only import into namespaces that exist — prevents re-importing
		// containers whose pods were explicitly deleted.
		if _, err := st.GetNamespace(ns); err != nil {
			continue
		}

		// Ensure the owning deployment exists regardless of whether the pod
		// is already in the store — handles service restart after a deployment
		// was deleted from the store but the container kept running.
		if depName := c.PodDeployment(); depName != "" {
			if _, err := st.GetDeployment(ns, depName); err != nil {
				one := int32(1)
				newDep := &appsv1.Deployment{
					TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
					ObjectMeta: metav1.ObjectMeta{Name: depName, Namespace: ns, CreationTimestamp: metav1.Now()},
					Spec: appsv1.DeploymentSpec{
						Replicas: &one,
						Template: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: depName, Image: c.Image}},
							},
						},
					},
				}
				if dep, err := st.CreateDeployment(newDep); err == nil && dep != nil {
					fmt.Printf("restored deployment: %s/%s\n", ns, depName)
				}
			}
		}

		if _, err := st.GetPod(ns, name); err == nil {
			continue
		}

		pod := &corev1.Pod{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, CreationTimestamp: metav1.Now()},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: name, Image: c.Image}}},
		}
		if depName := c.PodDeployment(); depName != "" {
			dep, _ := st.GetDeployment(ns, depName)
			owner := metav1.OwnerReference{APIVersion: "apps/v1", Kind: "Deployment", Name: depName, Controller: boolPtr(true)}
			if dep != nil {
				owner.UID = dep.UID
			}
			pod.OwnerReferences = []metav1.OwnerReference{owner}
		}

		created, err := st.CreatePod(pod)
		if err != nil {
			continue
		}
		st.UpdatePodPhase(created.Namespace, created.Name, podmanStateToPhase(c.State, c.ExitCode))
		imported++
		fmt.Printf("imported container: %s/%s (%s)\n", ns, name, c.State)
	}
	if imported > 0 {
		fmt.Printf("imported %d existing container(s)\n", imported)
	}

	for _, pod := range st.AllPods() {
		if !isDeploymentOwned(pod) {
			continue
		}
		if live[pod.Namespace+"/"+pod.Name] {
			continue
		}
		// Don't prune if the systemd unit still exists — the container may
		// have been removed by --rm while crash-looping, but the deployment
		// still owns it and systemd will keep restarting it.
		if mgr != nil {
			unitName := fmt.Sprintf("%s-%s.service", pod.Namespace, pod.Name)
			if state, err := mgr.UnitState(unitName); err == nil && state != nil {
				continue
			}
		}
		if err := st.DeletePod(pod.Namespace, pod.Name); err == nil {
			fmt.Printf("pruned deployment pod: %s/%s (container gone)\n", pod.Namespace, pod.Name)
		}
	}
}

func isDeploymentOwned(pod *corev1.Pod) bool {
	for _, or := range pod.OwnerReferences {
		if or.Kind == "Deployment" {
			return true
		}
	}
	return false
}

func boolPtr(b bool) *bool { return &b }

// reconcileOnce runs a full reconciliation pass: import/prune pods from live
// Podman state, then refresh pod/deployment/job status from systemd. This is
// the same work syncLoop used to run on every 5s tick; it's now triggered
// immediately by podman events, with a slow ticker kept only as a
// correctness backstop (same "push + periodic full resync" shape real k8s
// controllers use around their informers).
func reconcileOnce(st *store.Store, mgr *systemd.Manager) {
	reconcilePodmanPods(st, mgr)
	for _, pod := range st.AllPods() {
		state, err := mgr.UnitState(fmt.Sprintf("%s-%s.service", pod.Namespace, pod.Name))
		if err != nil || state == nil {
			// No systemd unit — fall back to Podman container state directly.
			containerName := fmt.Sprintf("%s-%s", pod.Namespace, pod.Name)
			cs, perr := podman.InspectState(containerName)
			if perr == nil {
				st.UpdatePodPhase(pod.Namespace, pod.Name, podmanStateToPhase(cs.State, cs.ExitCode))
			}
			continue
		}
		st.UpdatePodPhase(pod.Namespace, pod.Name, unitStateToPhase(state))
		st.UpdatePodRestartInfo(pod.Namespace, pod.Name, int32(state.NRestarts), int32(state.ExitCode), state.Result != "")
	}
	for _, dep := range st.AllDeployments() {
		n := int32(1)
		if dep.Spec.Replicas != nil && *dep.Spec.Replicas > 1 {
			n = *dep.Spec.Replicas
		}
		ready := int32(0)
		for i := int32(0); i < n; i++ {
			state, err := mgr.UnitState(fmt.Sprintf("%s-%s-%d.service", dep.Namespace, dep.Name, i))
			if err != nil || state == nil {
				continue
			}
			if state.Active == "active" {
				ready++
			}
		}
		st.UpdateDeploymentStatus(dep.Namespace, dep.Name, ready)
	}
	for _, job := range st.AllJobs() {
		state, err := mgr.UnitState(fmt.Sprintf("%s-%s-job.service", job.Namespace, job.Name))
		if err != nil || state == nil {
			continue
		}
		var active, succeeded, failed int32
		switch state.Active {
		case "active", "activating":
			active = 1
		case "inactive":
			switch state.Result {
			case "success":
				succeeded = 1
			case "exit-code", "signal", "core-dump", "watchdog", "timeout":
				failed = 1
			}
		case "failed":
			failed = 1
		}
		st.UpdateJobStatus(job.Namespace, job.Name, active, succeeded, failed)
	}
}

// syncLoop reacts to live podman container events (create/start/die/remove
// for q8s-owned containers) instead of polling on a fixed interval, so pod
// status/restart-info/orphan-detection update immediately instead of up to
// 5s late. A burst of related events for one container transition (create,
// init, start, attach all fire within milliseconds of each other) is
// debounced into a single reconcile pass. A slow ticker remains as a
// correctness backstop in case an event is ever missed (e.g. during a
// `podman events` reconnect window).
func syncLoop(st *store.Store, mgr *systemd.Manager) {
	trigger := make(chan struct{}, 1)
	notify := func() {
		select {
		case trigger <- struct{}{}:
		default:
		}
	}

	go podman.WatchEvents(context.Background(), func(e podman.Event) {
		if e.Type == "container" && e.PodNamespace() != "" && e.PodName() != "" {
			notify()
		}
	})

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-trigger:
			debounce := time.NewTimer(200 * time.Millisecond)
		drain:
			for {
				select {
				case <-trigger:
					if !debounce.Stop() {
						<-debounce.C
					}
					debounce.Reset(200 * time.Millisecond)
				case <-debounce.C:
					break drain
				}
			}
			reconcileOnce(st, mgr)
		case <-ticker.C:
			reconcileOnce(st, mgr)
		}
	}
}

func unitStateToPhase(s *systemd.UnitState) corev1.PodPhase {
	switch s.Active {
	case "active":
		return corev1.PodRunning
	case "activating":
		// auto-restart / starting — pod exists but container isn't stable yet
		if s.NRestarts > 0 {
			return corev1.PodRunning // CrashLoopBackOff is shown via restart count
		}
		return corev1.PodPending
	case "failed":
		return corev1.PodFailed
	case "inactive":
		switch s.Result {
		case "success":
			return corev1.PodSucceeded
		case "exit-code", "signal", "core-dump", "watchdog", "timeout":
			return corev1.PodFailed
		}
		return corev1.PodPending
	default:
		return corev1.PodPending
	}
}

func podmanStateToPhase(state string, exitCode int) corev1.PodPhase {
	switch state {
	case "running":
		return corev1.PodRunning
	case "exited", "stopped":
		if exitCode == 0 {
			return corev1.PodSucceeded
		}
		return corev1.PodFailed
	default:
		return corev1.PodPending
	}
}
