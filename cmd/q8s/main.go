package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/coreos/go-systemd/v22/daemon"
	"golang.org/x/sys/unix"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"q8s/internal/cronruns"
	"q8s/internal/hostcheck"
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
	case "doctor":
		cmdDoctor()
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
	fmt.Println("  doctor     Report host capabilities and limits for workloads (read-only)")
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
//  1. Q8S_PORT env var, if set — an explicit override for this invocation.
//  2. The port already baked into the installed q8s.socket unit, if any —
//     the systemd unit is the durable source of truth once installed, so
//     separate invocations (install, serve, status, kubeconfig) agree on the
//     port without needing Q8S_PORT set consistently in every shell.
//  3. install.DefaultPort (6443).
func resolvePort(d dirs) int {
	if v := os.Getenv("Q8S_PORT"); v != "" {
		if port, err := strconv.Atoi(v); err == nil && port > 0 {
			return port
		}
		fmt.Fprintf(os.Stderr, "warning: ignoring invalid Q8S_PORT %q\n", v)
	}
	if cfg, err := install.LoadConfig(d.dataDir); err == nil && cfg.Port > 0 {
		return cfg.Port
	}
	// Fallback: parse from socket unit (pre-config.json installs).
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
	var serverURL string
	var name string
	regenCerts := false

	args := os.Args[2:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-h", "--help":
			fmt.Println("Usage: q8s install [flags]")
			fmt.Println()
			fmt.Println("Flags:")
			fmt.Println("  --name <name>        Cluster/context name for kubeconfig (default: hostname)")
			fmt.Println("  --san-ip <ip>        Add extra IP to server certificate SAN")
			fmt.Println("  --san-dns <name>     Add extra DNS name to server certificate SAN")
			fmt.Println("  --server <url>       Server URL for kubeconfig (default: https://localhost:{port})")
			fmt.Println("  --regenerate-certs   Force certificate regeneration")
			return
		case "--name":
			i++
			if i >= len(args) {
				fmt.Fprintln(os.Stderr, "--name requires a value")
				os.Exit(1)
			}
			name = args[i]
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
		case "--san-dns":
			i++
			if i >= len(args) {
				fmt.Fprintln(os.Stderr, "--san-dns requires a value")
				os.Exit(1)
			}
			name, err := install.NormalizeSANDNS(args[i])
			if err != nil {
				fmt.Fprintf(os.Stderr, "invalid --san-dns %q: %v\n", args[i], err)
				os.Exit(1)
			}
			extraDNS = append(extraDNS, name)
		case "--server":
			i++
			if i >= len(args) {
				fmt.Fprintln(os.Stderr, "--server requires a value")
				os.Exit(1)
			}
			serverURL = args[i]
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
		Name:            name,
		Port:            resolvePort(d),
		ServerURL:       serverURL,
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
	if storeErr != nil {
		// Don't silently skip quadlet cleanup and still claim success: the
		// user must know container/volume/network files remain.
		fmt.Fprintf(os.Stderr, "warning: could not read %s (%v) — resource quadlet files were NOT cleaned up\n", storeFile, storeErr)
	}
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
		// Pod sockets (q8s.io/socket-activation) live in the systemd dir next
		// to q8s's own q8s.socket; only files carrying the pod owner key are
		// q8s-generated pod sockets.
		if socks, err := filepath.Glob(filepath.Join(d.systemdDir, "*.socket")); err == nil {
			for _, path := range socks {
				content, err := os.ReadFile(path)
				if err != nil || !strings.Contains(string(content), "\nX-Q8s-Namespace=") {
					continue
				}
				unit := filepath.Base(path)
				exec.Command("systemctl", append(flags, "stop", unit)...).Run()
				exec.Command("systemctl", append(flags, "disable", unit)...).Run()
				rm(path)
			}
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

	// Remove the rest of the installation. store.json.bak is kept: it is
	// the restore path for `q8s install && q8s serve` after uninstall.
	// The certs directory includes the CA private key — an "uninstall" that
	// leaves a CA key on disk is not an uninstall.
	for _, p := range []string{
		d.dataDir + "/certs",    // ca.key, server/client keys
		d.dataDir + "/traefik",  // generated ingress dynamic configs
		d.dataDir + "/quadlets", // legacy empty dir from old installs
		d.dataDir + "/config.json",
	} {
		if err := os.RemoveAll(p); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not remove %s: %v\n", p, err)
		} else {
			fmt.Printf("  removed %s\n", p)
		}
	}

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

	if !rootful {
		if on, err := install.LingerEnabled(); err == nil && !on {
			fmt.Println("linger: disabled (workloads stop at logout — run: q8s install, or sudo loginctl enable-linger $USER)")
		} else if err != nil {
			fmt.Println("linger: unknown (loginctl unavailable)")
		}
	}

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

func cmdDoctor() {
	rootful := os.Getuid() == 0
	d := resolveDirs(rootful)

	jsonOut := false
	for _, arg := range os.Args[2:] {
		switch arg {
		case "-h", "--help":
			printDoctorUsage(os.Stdout)
			return
		case "--json":
			jsonOut = true
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s\n", arg)
			printDoctorUsage(os.Stderr)
			os.Exit(2)
		}
	}

	uid := os.Getuid()
	username := strconv.Itoa(uid)
	if u, err := user.Current(); err == nil && u.Username != "" {
		username = u.Username
	}

	kernel := ""
	var uts unix.Utsname
	if err := unix.Uname(&uts); err == nil {
		kernel = unix.ByteSliceToString(uts.Release[:])
	}

	report := hostcheck.Run(hostcheck.Env{
		Rootful:  rootful,
		UID:      uid,
		User:     username,
		Kernel:   kernel,
		ReadFile: os.ReadFile,
		Command: func(name string, args ...string) ([]byte, error) {
			return exec.Command(name, args...).Output()
		},
		FSType:   realFSType,
		Writable: func(path string) bool { return unix.Access(path, unix.W_OK) == nil },
		Linger:   install.LingerEnabled,
		Now:      time.Now(),
		Dirs: hostcheck.Dirs{
			Data:    d.dataDir,
			Quadlet: d.quadletDir,
			Config:  d.configDir,
			Secret:  d.secretDir,
			Systemd: d.systemdDir,
			Traefik: d.dataDir + "/traefik",
		},
	})

	if jsonOut {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to encode report: %v\n", err)
			os.Exit(2)
		}
		fmt.Println(string(data))
	} else {
		hostcheck.RenderText(os.Stdout, report)
	}
	os.Exit(hostcheck.ExitCode(report))
}

func printDoctorUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: q8s doctor [--json]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Reports what this host gives q8s workloads and what it does not.")
	fmt.Fprintln(w, "Read-only: it never writes files or starts containers.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Flags:")
	fmt.Fprintln(w, "  --json   Print the report as JSON")
	fmt.Fprintln(w, "  -h, --help")
}

// realFSType maps the Statfs magic to a filesystem name. Names, not magic
// numbers, are what the idmap check reasons about.
func realFSType(path string) (string, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return "", err
	}
	magic := uint32(st.Type)
	switch magic {
	case 0xEF53:
		return "ext4", nil
	case 0x58465342:
		return "xfs", nil
	case 0x9123683E:
		return "btrfs", nil
	case 0x01021994:
		return "tmpfs", nil
	case 0x794C7630:
		return "overlay", nil
	case 0x2FC12FC1:
		return "zfs", nil
	case 0x6969:
		return "nfs", nil
	default:
		return fmt.Sprintf("other:0x%x", magic), nil
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

	name := "q8s"
	serverURL := fmt.Sprintf("https://localhost:%d", port)
	if cfg, err := install.LoadConfig(d.dataDir); err == nil {
		if cfg.ServerURL != "" {
			serverURL = cfg.ServerURL
		}
		if cfg.Name != "" {
			name = cfg.Name
		}
	}

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
    server: %s
  name: %s
contexts:
- context:
    cluster: %s
    user: %s
  name: %s
current-context: %s
preferences: {}
users:
- name: %s
  user:
    client-certificate-data: %s
    client-key-data: %s
`, readB64(certDir+"/ca.crt"), serverURL, name, name, name, name, name, name, readB64(certDir+"/client.crt"), readB64(certDir+"/client.key"))
}

// --- Server ---

// runJournalStream starts journalctl for a follow request and returns its
// stdout plus the wait function that reaps it once the client goes away.
func runJournalStream(ctx context.Context, name string, args ...string) (io.ReadCloser, func() error, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	return out, cmd.Wait, nil
}

func cmdServe() {
	rootful := os.Getuid() == 0
	mode := systemd.ModeRootless
	if rootful {
		mode = systemd.ModeRootful
	}
	d := resolveDirs(rootful)

	// Rootful pods run in their own user namespace, the podman equivalent of
	// kubelet's pod.spec.hostUsers: false. q8s owns the host like kubelet
	// does, so it ensures the subordinate range podman's --userns=auto
	// allocates from before rendering any unit. Startup re-renders every
	// unit, which is also how existing pods move into a namespace on upgrade.
	if rootful {
		changedUID, changedGID, ensureErr := hostcheck.EnsureContainersRange("/etc/subuid", "/etc/subgid")
		if changedUID {
			fmt.Printf("added containers:%d:%d to /etc/subuid\n", hostcheck.ContainersRangeStart, hostcheck.ContainersRangeCount)
		}
		if changedGID {
			fmt.Printf("added containers:%d:%d to /etc/subgid\n", hostcheck.ContainersRangeStart, hostcheck.ContainersRangeCount)
		}
		if ensureErr != nil {
			fmt.Printf("warning: user namespaces disabled: %v\n", ensureErr)
		}
		if hostcheck.UserNSAvailable(hostcheck.Env{Rootful: true, ReadFile: os.ReadFile}) {
			quadlet.SetUserNS(true)
			fmt.Println("user namespaces: on (UserNS=auto, idmap volumes)")
		} else {
			reason := "no usable 'containers' range in /etc/subuid and /etc/subgid"
			if ensureErr != nil {
				reason = ensureErr.Error()
			}
			fmt.Printf("user namespaces: off (%s)\n", reason)
		}
	}

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
		CronRuns: &cronruns.Source{
			Rootful: rootful,
			Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
				return exec.CommandContext(ctx, name, args...).Output()
			},
			Stream: runJournalStream,
			Now:    time.Now,
		},
		Port: resolvePort(d),
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
	// Re-render per-pod Secret-derived env-files (tmpfs, wiped on restart)
	// after their source secrets/configmaps are restored, so quadlet
	// ExecStart --env-file targets exist. Before sd_notify(READY). tic-1507.
	srv.RestoreEnvFiles()
	ensureNamespaceNetworks(d.quadletDir, mgr, st)
	srv.ReconcileQuadlets()
	srv.ReconcileNetworkAttachments()
	srv.ReconcileIngressConfigs()
	reconcilePodmanPods(st, mgr)

	if mgr != nil {
		syncCtx, stopSync := context.WithCancel(context.Background())
		defer stopSync()
		go syncLoop(syncCtx, st, mgr, mode, d.quadletDir, srv.RegenerateIngressConfigs)
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
func reconcilePodmanPods(st *store.Store, mgr *systemd.Manager) map[string][]podman.ContainerInfo {
	containers, err := podman.ListOwned()
	if err != nil {
		fmt.Printf("reconcile pods: %v\n", err)
		return nil
	}

	// byPod is the live containers for each q8s pod, returned so callers
	// can read container state (e.g. health) without another podman call.
	// A multi-container pod has one entry per member, matched later by the
	// io.kubernetes.container.name label.
	byPod := make(map[string][]podman.ContainerInfo, len(containers))
	imported := 0
	for _, c := range containers {
		ns, name := c.PodNamespace(), c.PodName()
		if ns == "" || name == "" {
			continue
		}
		// Labels are attacker-controllable at `podman run --label` time.
		// A name/namespace that isn't a valid RFC-1123 label can't be
		// safely materialized (quadlet injection / filename traversal), so
		// it is never imported — with a loud log, since a silently skipped
		// container is a debugging dead end.
		if !isValidObjectName(name) || !isValidObjectName(ns) {
			fmt.Printf("reconcile pods: skipping container with unsafe label name %q ns %q\n", name, ns)
			continue
		}
		byPod[ns+"/"+name] = append(byPod[ns+"/"+name], c)
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

		containerName := c.ContainerName()
		if !isValidObjectName(containerName) {
			containerName = name
		}
		pod := &corev1.Pod{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, CreationTimestamp: metav1.Now()},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: containerName, Image: c.Image}}},
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
		st.UpdatePodPhase(created.Namespace, created.Name, podmanStateToPhase(c.State, c.ExitCode), c.Health())
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
		if _, ok := byPod[pod.Namespace+"/"+pod.Name]; ok {
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
	return byPod
}

// isValidObjectName matches the RFC-1123 label rule the API boundary
// enforces (internal/server validateName). Duplicated here so the import
// path doesn't depend on the server package.
func isValidObjectName(s string) bool {
	if len(s) == 0 || len(s) > 253 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			continue
		}
		if c == '-' && i > 0 && i < len(s)-1 {
			continue
		}
		return false
	}
	return true
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
// immediately by systemd unit property changes, with a slow ticker kept only
// as a correctness backstop (same "push + periodic full resync" shape real
// k8s controllers use around their informers).
//
// It returns the live Podman containers it found, keyed by namespace/name, so
// the sync loop can schedule a readiness recheck without another podman call.
func reconcileOnce(st *store.Store, mgr *systemd.Manager) map[string][]podman.ContainerInfo {
	live := reconcilePodmanPods(st, mgr)
	for _, pod := range st.AllPods() {
		infos := live[pod.Namespace+"/"+pod.Name]
		if len(pod.Spec.Containers) > 1 {
			// A multi-container pod runs as a Podman pod: one systemd
			// service per member container. Derive the pod phase from the
			// set of members, and fall back to the pod's own service state
			// when Podman hasn't reported any container yet.
			phase, states := podmanContainerStates(pod, infos)
			if len(infos) == 0 {
				if state, err := mgr.UnitState(fmt.Sprintf("%s-%s.service", pod.Namespace, pod.Name)); err == nil && state != nil {
					phase = unitStateToPhase(state)
				}
			}
			st.UpdatePodContainerPhases(pod.Namespace, pod.Name, phase, states)
			continue
		}
		health := ""
		if len(infos) > 0 {
			health = infos[0].Health()
		}
		state, err := mgr.UnitState(fmt.Sprintf("%s-%s.service", pod.Namespace, pod.Name))
		if err != nil || state == nil {
			// No systemd unit — fall back to Podman container state directly.
			containerName := fmt.Sprintf("%s-%s", pod.Namespace, pod.Name)
			cs, perr := podman.InspectState(containerName)
			if perr == nil {
				st.UpdatePodPhase(pod.Namespace, pod.Name, podmanStateToPhase(cs.State, cs.ExitCode), health)
			}
			continue
		}
		st.UpdatePodPhase(pod.Namespace, pod.Name, unitStateToPhase(state), health)
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
	return live
}

// isQ8sUnit reports whether unit is a systemd service generated from a
// quadlet file q8s writes in quadletDir. The check is one stat per candidate
// file and no D-Bus call, so it is cheap enough to run for every unit signal.
//
// Quadlet derives the service name from the file name: foo.container becomes
// foo.service, foo.pod becomes foo-pod.service, and q8s renders a CronJob as
// <ns>-<name>-cron.container, so its unit is <ns>-<name>-cron.service (already
// covered by the .container rule).
func isQ8sUnit(quadletDir, unit string) bool {
	base, ok := strings.CutSuffix(unit, ".service")
	if !ok || base == "" {
		return false
	}
	if fileExists(filepath.Join(quadletDir, base+".container")) {
		return true
	}
	if pod, isPod := strings.CutSuffix(base, "-pod"); isPod && pod != "" {
		return fileExists(filepath.Join(quadletDir, pod+".pod"))
	}
	return false
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// syncLoop reacts to systemd unit property changes instead of polling on a
// fixed interval, so pod status/restart-info/orphan-detection update
// immediately instead of up to 30s late. The trigger says only that a unit
// changed; the reconcile pass itself still reads authoritative unit state and
// live Podman containers. A burst of related changes for one transition
// (start, stop, restart all fire within milliseconds of each other) is
// debounced into a single reconcile pass. A slow ticker remains as a
// correctness backstop in case a signal is ever missed (e.g. during a bus
// reconnect window).
//
// Unit changes arrive over a dedicated D-Bus connection to systemd: no child
// process is started and the journal is never read, so a host with a large
// journal costs q8s nothing extra.
func syncLoop(ctx context.Context, st *store.Store, mgr *systemd.Manager, mode systemd.Mode, quadletDir string, backendsChanged func(ns string)) {
	// A pod whose probe is still starting is rechecked on the probe's own
	// period. The timer is stopped and re-armed after every pass, and stays
	// idle (nil channel) when no pod is in that window. No goroutine and no
	// cached state are involved.
	var recheckTimer *time.Timer
	var recheckC <-chan time.Time
	stopRecheck := func() {
		if recheckTimer == nil {
			return
		}
		if !recheckTimer.Stop() {
			select {
			case <-recheckTimer.C:
			default:
			}
		}
		recheckC = nil
	}
	defer stopRecheck()

	// reconcile updates pod status and then refreshes the Traefik backends of
	// every namespace whose set of servable pods changed. Backends are read
	// from pod status, so without this a file rendered while pods were down
	// (e.g. at boot, from the phases stored at shutdown) stays empty until
	// some API object changes.
	reconcile := func() {
		stopRecheck()
		before := servablePods(st.AllPods())
		live := reconcileOnce(st, mgr)
		after := st.AllPods()
		for _, ns := range changedNamespaces(before, servablePods(after)) {
			backendsChanged(ns)
		}
		if d, ok := nextRecheck(after, live, time.Now()); ok {
			if recheckTimer == nil {
				recheckTimer = time.NewTimer(d)
			} else {
				recheckTimer.Reset(d)
			}
			recheckC = recheckTimer.C
		}
	}

	trigger := make(chan struct{}, 1)
	notify := func() {
		select {
		case trigger <- struct{}{}:
		default:
		}
	}

	go systemd.WatchUnits(ctx, mode, func(unit string) bool {
		return isQ8sUnit(quadletDir, unit)
	}, func(string) {
		notify()
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
			reconcile()
		case <-ticker.C:
			reconcile()
		case <-recheckC:
			reconcile()
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

// podmanContainerStates maps the live Podman containers of a multi-container
// pod to per-container sync state, matched by io.kubernetes.container.name.
// The pod phase follows the set: Running when every container runs,
// Succeeded when every container exited 0, Failed when any container failed,
// otherwise Pending.
func podmanContainerStates(pod *corev1.Pod, infos []podman.ContainerInfo) (corev1.PodPhase, map[string]store.ContainerSyncState) {
	byName := make(map[string]podman.ContainerInfo, len(infos))
	for _, ci := range infos {
		name := ci.ContainerName()
		// Containers created before multi-container support (or imported
		// from live Podman state) carry only the pod labels; in a pod with
		// one container that is unambiguous.
		if name == "" && len(pod.Spec.Containers) == 1 {
			name = pod.Spec.Containers[0].Name
		}
		if name != "" {
			byName[name] = ci
		}
	}

	states := make(map[string]store.ContainerSyncState, len(pod.Spec.Containers))
	running, succeeded, failed := 0, 0, 0
	for _, c := range pod.Spec.Containers {
		phase := corev1.PodPending
		health := ""
		if ci, ok := byName[c.Name]; ok {
			phase = podmanStateToPhase(ci.State, ci.ExitCode)
			health = ci.Health()
		}
		states[c.Name] = store.ContainerSyncState{Phase: phase, Health: health}
		switch phase {
		case corev1.PodRunning:
			running++
		case corev1.PodSucceeded:
			succeeded++
		case corev1.PodFailed:
			failed++
		}
	}

	n := len(pod.Spec.Containers)
	switch {
	case n > 0 && running == n:
		return corev1.PodRunning, states
	case n > 0 && succeeded == n:
		return corev1.PodSucceeded, states
	case failed > 0:
		return corev1.PodFailed, states
	default:
		return corev1.PodPending, states
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

// servablePods maps namespace -> names of pods that can be a backend: the
// same rule serviceBackendTargets applies (a terminal pod is out of rotation).
func servablePods(pods []*corev1.Pod) map[string]map[string]bool {
	out := make(map[string]map[string]bool)
	for _, p := range pods {
		if p.Status.Phase == corev1.PodFailed || p.Status.Phase == corev1.PodSucceeded {
			continue
		}
		if out[p.Namespace] == nil {
			out[p.Namespace] = make(map[string]bool)
		}
		out[p.Namespace][p.Name] = true
	}
	return out
}

// changedNamespaces lists, sorted, the namespaces whose servable pod set
// differs between before and after.
func changedNamespaces(before, after map[string]map[string]bool) []string {
	seen := make(map[string]bool)
	var out []string
	add := func(ns string) {
		if !seen[ns] {
			seen[ns] = true
			out = append(out, ns)
		}
	}
	for ns, b := range before {
		a := after[ns]
		if len(a) != len(b) {
			add(ns)
			continue
		}
		for name := range b {
			if !a[name] {
				add(ns)
				break
			}
		}
	}
	for ns := range after {
		if _, ok := before[ns]; !ok {
			add(ns)
		}
	}
	sort.Strings(out)
	return out
}
