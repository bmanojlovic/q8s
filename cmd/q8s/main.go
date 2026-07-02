package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/coreos/go-systemd/v22/daemon"
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

// --- Management commands ---

func cmdInstall() {
	rootful := os.Getuid() == 0
	if err := install.Install(install.InstallConfig{Rootful: rootful, Home: os.Getenv("HOME")}); err != nil {
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
	flags := systemctlFlags(rootful)

	socketOut, _ := exec.Command("systemctl", append(flags, "is-active", "q8s.socket")...).Output()
	socketActive := string(socketOut) == "active\n"

	serviceOut, _ := exec.Command("systemctl", append(flags, "is-active", "q8s-api.service")...).Output()
	serviceActive := string(serviceOut) == "active\n"

	conn, dialErr := net.DialTimeout("tcp", "localhost:6443", time.Second)
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
		fmt.Println("q8s port 6443: reachable")
	} else {
		fmt.Println("q8s port 6443: not reachable")
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
    server: https://localhost:6443
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
`, readB64(certDir+"/ca.crt"), readB64(certDir+"/client.crt"), readB64(certDir+"/client.key"))
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
		Mode:       mode,
		Manager:    mgr,
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
		listeners, err = systemd.DefaultListeners()
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
	importExisting(st)

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

func importExisting(st *store.Store) {
	containers, err := podman.ListOwned()
	if err != nil {
		fmt.Printf("import: %v\n", err)
		return
	}
	imported := 0
	for _, c := range containers {
		ns, name := c.PodNamespace(), c.PodName()
		// Only import into namespaces that exist — prevents re-importing
		// containers whose pods were explicitly deleted.
		if _, err := st.GetNamespace(ns); err != nil {
			continue
		}
		if _, err := st.GetPod(ns, name); err == nil {
			continue
		}
		pod := &corev1.Pod{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, CreationTimestamp: metav1.Now()},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: name, Image: c.Image}}},
		}
		if created, err := st.CreatePod(pod); err == nil {
			st.UpdatePodPhase(created.Namespace, created.Name, podmanStateToPhase(c.State, c.ExitCode))
			imported++
			fmt.Printf("imported container: %s/%s (%s)\n", ns, name, c.State)
		}
	}
	if imported > 0 {
		fmt.Printf("imported %d existing container(s)\n", imported)
	}
}

func syncLoop(st *store.Store, mgr *systemd.Manager) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
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
}

func unitStateToPhase(s *systemd.UnitState) corev1.PodPhase {
	switch s.Active {
	case "active":
		return corev1.PodRunning
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
