// Package hostcheck inspects what the local host gives q8s workloads and
// reports it in plain words. The checks are read-only: every host access goes
// through Env, and the only external commands they may run are podman version,
// podman info, systemctl (rootful, for firewalld state) and (rootless) loginctl
// via install.LingerEnabled.
//
// The one writer is EnsureContainersRange, which appends the subordinate id
// range podman's --userns=auto needs. Rootful `q8s serve` calls it before
// rendering units, the way kubelet prepares the host it owns.
//
// The package is deliberately independent of cmd/q8s so the same checks can be
// reused at `q8s serve` startup, for example to decide whether pods default to
// UserNS=auto.
package hostcheck

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Status string

const (
	StatusOK   Status = "ok"
	StatusInfo Status = "info"
	StatusWarn Status = "warn"
	StatusFail Status = "fail"
)

type Check struct {
	ID     string `json:"id"` // stable, kebab-case, e.g. "userns"
	Title  string `json:"title"`
	Status Status `json:"status"`
	Detail string `json:"detail"`         // what was found, facts and values
	Gain   string `json:"gain,omitempty"` // what the workloads get
	Loss   string `json:"loss,omitempty"` // what they lose / the risk
	Fix    string `json:"fix,omitempty"`  // exact command(s) to fix it
}

type Report struct {
	Mode    string        `json:"mode"`   // "rootful" | "rootless"
	Podman  string        `json:"podman"` // version string, "" if not found
	Kernel  string        `json:"kernel"` // uname release
	Checks  []Check       `json:"checks"`
	Summary []SummaryLine `json:"summary"`
}

type SummaryLine struct {
	Topic string `json:"topic"` // "isolation" | "limits" | "networking" | "persistence" | "certificates"
	Text  string `json:"text"`
}

// Env abstracts every host access so tests run with fakes.
type Env struct {
	Rootful  bool
	UID      int
	User     string // login name, for /etc/subuid lookup in rootless
	Kernel   string // uname -r
	ReadFile func(path string) ([]byte, error)
	Command  func(name string, args ...string) ([]byte, error) // stdout only
	FSType   func(path string) (string, error)                 // "ext4","xfs","btrfs","tmpfs","overlay","zfs","nfs","other:0x..."
	Writable func(path string) bool
	Linger   func() (bool, error) // rootless only
	Now      time.Time
	Dirs     Dirs
}

type Dirs struct{ Data, Quadlet, Config, Secret, Systemd, Traefik string }

const (
	subUIDPath = "/etc/subuid"
	subGIDPath = "/etc/subgid"

	// minSubIDCount is the smallest range worth handing to a user namespace.
	// 65536 ids is the conventional minimum for a single container image.
	minSubIDCount = 65536

	// rootlessSubIDRange is a placeholder range for rootless usermod fixes.
	rootlessSubIDRange = "100000-165535"
)

// largeSubIDRange is the range suggested for the rootful 'containers' entry.
// It is deliberately high to avoid colliding with login users. Derived from
// the constants so the doctor fix and subid.go cannot drift apart.
var largeSubIDRange = fmt.Sprintf("%d:%d", ContainersRangeStart, ContainersRangeCount)

func (e Env) readFile(path string) ([]byte, error) {
	if e.ReadFile == nil {
		return nil, fmt.Errorf("no file reader")
	}
	return e.ReadFile(path)
}

func (e Env) command(name string, args ...string) ([]byte, error) {
	if e.Command == nil {
		return nil, fmt.Errorf("no command runner")
	}
	return e.Command(name, args...)
}

func (e Env) fsType(path string) (string, error) {
	if e.FSType == nil {
		return "", fmt.Errorf("no statfs")
	}
	return e.FSType(path)
}

func (e Env) writable(path string) bool {
	if e.Writable == nil {
		return false
	}
	return e.Writable(path)
}

func (e Env) lingerEnabled() (bool, error) {
	if e.Linger == nil {
		return false, fmt.Errorf("no loginctl")
	}
	return e.Linger()
}

// Run performs every check in a fixed order and derives the five-topic summary.
// It never mutates host state.
func Run(env Env) Report {
	// Traefik always lives under the data directory. Deriving it here keeps
	// callers honest even when they only fill in Dirs.Data.
	if env.Dirs.Traefik == "" && env.Dirs.Data != "" {
		env.Dirs.Traefik = filepath.Join(env.Dirs.Data, "traefik")
	}

	r := Report{Mode: "rootless", Kernel: env.Kernel}
	if env.Rootful {
		r.Mode = "rootful"
	}

	podmanCheck, podmanVersion := checkPodman(env)
	r.Podman = podmanVersion
	r.Checks = append(r.Checks, podmanCheck)
	r.Checks = append(r.Checks, checkKernel(env))
	r.Checks = append(r.Checks, checkUserNS(env))
	r.Checks = append(r.Checks, checkIDMap(env))

	// Linger only exists as a concept for systemd user units. In rootful mode
	// the units live in the system manager and survive logout on their own, so
	// emitting a linger check would only confuse the operator.
	if !env.Rootful {
		r.Checks = append(r.Checks, checkLinger(env))
	}

	r.Checks = append(r.Checks, checkCgroups(env))
	r.Checks = append(r.Checks, checkNetwork(env))
	// Rootless bridge networks live inside the rootless network namespace, so
	// firewalld on the host cannot drop their rules; the check is rootful only.
	if env.Rootful {
		r.Checks = append(r.Checks, checkFirewallReload(env))
	}
	certCheck, certInfo := checkCertificates(env)
	r.Checks = append(r.Checks, certCheck)
	r.Checks = append(r.Checks, checkTraefikDir(env))

	r.Summary = buildSummary(env, r.Checks, certInfo)
	if r.Summary == nil {
		r.Summary = []SummaryLine{}
	}
	return r
}

// UserNSAvailable is exported for startup reuse: true when the host can run
// --userns=auto. Rootful needs a 'containers' entry in BOTH
// /etc/subuid and /etc/subgid with at least 65536 ids. Rootless is already
// inside a user namespace, so it always reports false here.
func UserNSAvailable(env Env) bool {
	if !env.Rootful {
		return false
	}
	uid, uidOK := findSubID(readSubIDEntries(env, subUIDPath), func(name string) bool {
		return name == "containers"
	})
	gid, gidOK := findSubID(readSubIDEntries(env, subGIDPath), func(name string) bool {
		return name == "containers"
	})
	return uidOK && gidOK && uid.Count >= minSubIDCount && gid.Count >= minSubIDCount
}

func checkPodman(env Env) (Check, string) {
	c := Check{ID: "podman", Title: "podman runtime"}
	out, err := env.command("podman", "version", "--format", "{{.Client.Version}}")
	version := strings.TrimSpace(string(out))
	if err != nil || version == "" {
		c.Status = StatusFail
		c.Detail = "podman not found"
		c.Loss = "q8s cannot run workloads"
		c.Fix = "install podman 5 or newer"
		return c, ""
	}
	c.Detail = "podman " + version
	major, ok := leadingInt(version)
	if !ok || major < 5 {
		c.Status = StatusFail
		c.Detail = fmt.Sprintf("podman %s (need 5 or newer)", version)
		c.Loss = "Quadlet [Pod] units and the keys q8s writes need podman 5"
		c.Fix = "upgrade podman to 5 or newer"
		return c, version
	}
	c.Status = StatusOK
	c.Gain = "q8s can run workloads"
	return c, version
}

func checkKernel(env Env) Check {
	c := Check{ID: "kernel", Title: "kernel", Status: StatusInfo}
	if env.Kernel == "" {
		c.Detail = "kernel release unknown"
	} else {
		c.Detail = env.Kernel
	}
	return c
}

type subIDEntry struct {
	Name  string
	Start uint64
	Count uint64
}

func parseSubIDs(data []byte) []subIDEntry {
	var entries []subIDEntry
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) != 3 {
			continue
		}
		start, err := strconv.ParseUint(strings.TrimSpace(parts[1]), 10, 64)
		if err != nil {
			continue
		}
		count, err := strconv.ParseUint(strings.TrimSpace(parts[2]), 10, 64)
		if err != nil {
			continue
		}
		entries = append(entries, subIDEntry{Name: strings.TrimSpace(parts[0]), Start: start, Count: count})
	}
	return entries
}

func readSubIDEntries(env Env, path string) []subIDEntry {
	data, err := env.readFile(path)
	if err != nil {
		return nil
	}
	return parseSubIDs(data)
}

// findSubID returns the matching entry with the largest range. Multiple
// entries for one name are legal, and the largest is the one that can
// actually hold a container image.
func findSubID(entries []subIDEntry, match func(string) bool) (subIDEntry, bool) {
	var best subIDEntry
	found := false
	for _, e := range entries {
		if !match(e.Name) {
			continue
		}
		if !found || e.Count > best.Count {
			best = e
			found = true
		}
	}
	return best, found
}

func checkUserNS(env Env) Check {
	c := Check{ID: "userns", Title: "user namespaces"}
	if env.Rootful {
		uid, uidOK := findSubID(readSubIDEntries(env, subUIDPath), func(name string) bool {
			return name == "containers"
		})
		gid, gidOK := findSubID(readSubIDEntries(env, subGIDPath), func(name string) bool {
			return name == "containers"
		})

		if uidOK && gidOK && uid.Count >= minSubIDCount && gid.Count >= minSubIDCount {
			c.Status = StatusOK
			c.Detail = fmt.Sprintf("'containers' has %d ids in %s and %d ids in %s", uid.Count, subUIDPath, gid.Count, subGIDPath)
			c.Gain = "each pod gets its own uid range; root in a container is an unprivileged uid on the host"
			return c
		}

		missing := missingSubIDFiles(uidOK, gidOK)
		c.Status = StatusWarn
		c.Loss = "container uids are host uids: root in a container is real host root, and two containers using the same uid share an identity"
		if len(missing) > 0 {
			c.Detail = fmt.Sprintf("no 'containers' entry in %s", strings.Join(missing, ", "))
			// q8s serve now adds the range itself before rendering units, so
			// the manual commands are only the fallback for a stopped service.
			c.Fix = fmt.Sprintf("q8s serve adds it on its next start (or by hand: echo containers:%s >> %s && echo containers:%s >> %s)",
				largeSubIDRange, subUIDPath, largeSubIDRange, subGIDPath)
			return c
		}
		c.Detail = fmt.Sprintf("'containers' range is too small: %d ids in %s and %d ids in %s (need at least %d)",
			uid.Count, subUIDPath, gid.Count, subGIDPath, minSubIDCount)
		// 'containers' is not a login user, so usermod cannot edit it: the
		// lines in the files are the whole configuration.
		c.Fix = fmt.Sprintf("replace the 'containers' lines in %s and %s with containers:%s",
			subUIDPath, subGIDPath, largeSubIDRange)
		return c
	}

	match := func(name string) bool {
		if name == strconv.Itoa(env.UID) {
			return true
		}
		return env.User != "" && name == env.User
	}
	uid, uidOK := findSubID(readSubIDEntries(env, subUIDPath), match)
	gid, gidOK := findSubID(readSubIDEntries(env, subGIDPath), match)

	if uidOK && gidOK && uid.Count >= minSubIDCount && gid.Count >= minSubIDCount {
		c.Status = StatusOK
		c.Detail = fmt.Sprintf("subordinate range for %s: %d ids in %s and %d ids in %s",
			userLabel(env), uid.Count, subUIDPath, gid.Count, subGIDPath)
		c.Gain = "containers run inside your subordinate uid range; root in a container is not host root"
		return c
	}

	missing := missingSubIDFiles(uidOK, gidOK)
	c.Status = StatusFail
	c.Loss = "rootless podman cannot run images that need more than one uid"
	if len(missing) > 0 {
		c.Detail = fmt.Sprintf("no subordinate uid range for %s in %s", userLabel(env), strings.Join(missing, ", "))
	} else {
		c.Detail = fmt.Sprintf("subordinate range for %s is too small: %d ids in %s and %d ids in %s (need at least %d)",
			userLabel(env), uid.Count, subUIDPath, gid.Count, subGIDPath, minSubIDCount)
	}
	c.Fix = fmt.Sprintf("usermod --add-subuids %s --add-subgids %s %s",
		rootlessSubIDRange, rootlessSubIDRange, userLabel(env))
	return c
}

func missingSubIDFiles(uidOK, gidOK bool) []string {
	var missing []string
	if !uidOK {
		missing = append(missing, subUIDPath)
	}
	if !gidOK {
		missing = append(missing, subGIDPath)
	}
	return missing
}

func userLabel(env Env) string {
	if env.User != "" {
		return env.User
	}
	return strconv.Itoa(env.UID)
}

func checkIDMap(env Env) Check {
	c := Check{ID: "idmap", Title: "idmapped volumes"}
	if !env.Rootful {
		c.Status = StatusInfo
		c.Detail = "not needed in rootless mode"
		return c
	}

	storageRoot := "/var/lib/containers/storage"
	if out, err := env.command("podman", "info", "--format", "{{.Store.GraphRoot}}"); err == nil {
		if v := strings.TrimSpace(string(out)); v != "" {
			storageRoot = v
		}
	}

	type fsPath struct {
		Path string
		FS   string
	}
	var paths []fsPath
	add := func(path string) {
		if path == "" {
			return
		}
		fs, err := env.fsType(path)
		if err != nil {
			fs = "unknown"
		}
		paths = append(paths, fsPath{Path: path, FS: fs})
	}

	add(storageRoot)
	add(env.Dirs.Data)

	// ConfigDir is under /run on both modes and may not exist until the first
	// serve. The filesystem of its parent still decides idmap support.
	configPath := env.Dirs.Config
	if configPath != "" {
		if _, err := env.fsType(configPath); err != nil {
			configPath = filepath.Dir(configPath)
		}
		add(configPath)
	}

	// Traefik is created lazily on the first Ingress, so only report it when
	// it already exists.
	if env.Dirs.Traefik != "" {
		if _, err := env.fsType(env.Dirs.Traefik); err == nil {
			add(env.Dirs.Traefik)
		}
	}

	var detailParts []string
	var unsupported []string
	for _, p := range paths {
		detailParts = append(detailParts, p.Path+"="+p.FS)
		if !kernelSupportsIDMap(env.Kernel, p.FS) {
			unsupported = append(unsupported, p.Path+" ("+p.FS+")")
		}
	}
	c.Detail = strings.Join(detailParts, ", ")

	if len(unsupported) == 0 {
		c.Status = StatusOK
		c.Gain = "volumes keep their host owners inside a user namespace, no chown needed"
		return c
	}
	c.Status = StatusWarn
	c.Loss = "volumes on these paths cannot be idmapped; under a user namespace they would be unreadable"
	c.Fix = "move podman storage and q8s data to ext4, xfs or btrfs on kernel 5.12 or newer (tmpfs needs 6.3 or newer)"
	return c
}

// kernelSupportsIDMap encodes the filesystems and kernel versions where idmap
// mounts are available. overlay, zfs, nfs and anything unknown are treated as
// unsupported: silently promising idmapped volumes there would break pods.
func kernelSupportsIDMap(release, fs string) bool {
	major, minor, ok := parseKernelVersion(release)
	if !ok {
		return false
	}
	switch fs {
	case "ext4", "xfs", "btrfs":
		return versionAtLeast(major, minor, 5, 12)
	case "tmpfs":
		return versionAtLeast(major, minor, 6, 3)
	default:
		return false
	}
}

func parseKernelVersion(release string) (int, int, bool) {
	parts := strings.SplitN(release, ".", 3)
	if len(parts) < 2 {
		return 0, 0, false
	}
	major, ok := leadingInt(parts[0])
	if !ok {
		return 0, 0, false
	}
	minor, ok := leadingInt(parts[1])
	if !ok {
		return 0, 0, false
	}
	return major, minor, true
}

func leadingInt(s string) (int, bool) {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(s[:i])
	if err != nil {
		return 0, false
	}
	return n, true
}

func versionAtLeast(major, minor, wantMajor, wantMinor int) bool {
	if major != wantMajor {
		return major > wantMajor
	}
	return minor >= wantMinor
}

func checkLinger(env Env) Check {
	c := Check{ID: "linger", Title: "linger"}
	on, err := env.lingerEnabled()
	if err != nil {
		c.Status = StatusInfo
		c.Detail = "unknown (loginctl unavailable)"
		return c
	}
	if on {
		c.Status = StatusOK
		c.Detail = fmt.Sprintf("linger is enabled for %s", userLabel(env))
		c.Gain = "workloads keep running after logout and start at boot"
		return c
	}
	c.Status = StatusWarn
	c.Detail = fmt.Sprintf("linger is disabled for %s", userLabel(env))
	c.Loss = "workloads stop when you log out and do not start at boot"
	c.Fix = fmt.Sprintf("sudo loginctl enable-linger %s", userLabel(env))
	return c
}

func cgroupControllersPath(env Env) string {
	if env.Rootful {
		return "/sys/fs/cgroup/cgroup.controllers"
	}
	return fmt.Sprintf("/sys/fs/cgroup/user.slice/user-%d.slice/user@%d.service/cgroup.controllers", env.UID, env.UID)
}

func checkCgroups(env Env) Check {
	c := Check{ID: "cgroups", Title: "cgroup limits"}
	path := cgroupControllersPath(env)
	data, err := env.readFile(path)
	if err != nil {
		c.Status = StatusWarn
		c.Detail = "cgroup v2 not detected"
		c.Loss = "resource limits are not enforced"
		if env.Rootful {
			c.Fix = "boot with cgroup v2 and enable the cpu, memory and pids controllers"
		} else {
			c.Fix = "boot with cgroup v2 so the user manager can delegate cpu, memory and pids"
		}
		return c
	}

	have := make(map[string]bool)
	for _, field := range strings.Fields(string(data)) {
		have[field] = true
	}
	var missing []string
	for _, want := range []string{"cpu", "memory", "pids"} {
		if !have[want] {
			missing = append(missing, want)
		}
	}
	if len(missing) == 0 {
		c.Status = StatusOK
		c.Detail = "cpu, memory and pids are delegated"
		c.Gain = "resources.limits are enforced"
		return c
	}

	c.Status = StatusWarn
	c.Detail = "missing controllers: " + strings.Join(missing, ", ")
	c.Loss = fmt.Sprintf("limits for %s are written to the unit but not enforced", strings.Join(missing, ", "))
	if env.Rootful {
		c.Fix = "enable the missing controllers in the root cgroup and boot with cgroup v2"
	} else {
		c.Fix = "create /etc/systemd/system/user@.service.d/delegate.conf with:\n" +
			"[Service]\nDelegate=cpu cpuset io memory pids\n" +
			"then run: systemctl daemon-reload"
	}
	return c
}

func checkNetwork(env Env) Check {
	c := Check{ID: "network", Title: "networking"}
	if env.Rootful {
		c.Status = StatusOK
		c.Detail = "published ports use kernel NAT; client IPs are preserved"
		c.Gain = "published ports use kernel NAT; client IPs are preserved"
		return c
	}
	out, err := env.command("podman", "info", "--format", "{{.Host.RootlessNetworkCmd}}")
	cmd := strings.TrimSpace(string(out))
	if err != nil || cmd == "" {
		cmd = "unknown"
	}
	c.Status = StatusInfo
	c.Detail = cmd
	return c
}

const firewallReloadService = "netavark-firewalld-reload.service"

// unitListed reports whether `systemctl list-unit-files --no-legend` output
// contains a line for unit.
func unitListed(output []byte, unit string) bool {
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == unit {
			return true
		}
	}
	return false
}

// checkFirewallReload reports whether pod networking survives a firewalld
// reload. Rootful only: rootless bridge networks are not in the host firewall.
// systemctl is-active exits non-zero for an inactive unit, so the stdout text is
// the signal, not the error.
func checkFirewallReload(env Env) Check {
	c := Check{ID: "firewall-reload", Title: "firewall reload"}
	out, err := env.command("systemctl", "is-active", "firewalld.service")
	state := strings.TrimSpace(string(out))
	if state == "" && err != nil {
		// systemctl is unavailable (no systemd, no D-Bus) or failed to answer.
		c.Status = StatusInfo
		c.Detail = "unknown"
		return c
	}
	if state != "active" {
		c.Status = StatusOK
		c.Detail = "firewalld is not active"
		return c
	}

	reloadOut, _ := env.command("systemctl", "is-active", firewallReloadService)
	if strings.TrimSpace(string(reloadOut)) == "active" {
		c.Status = StatusOK
		c.Detail = firewallReloadService + " is active"
		c.Gain = "pod networking survives firewall-cmd --reload"
		return c
	}

	units, unitsErr := env.command("systemctl", "list-unit-files", "--no-legend", firewallReloadService)
	if unitsErr != nil && strings.TrimSpace(string(units)) == "" {
		c.Status = StatusInfo
		c.Detail = "unknown"
		return c
	}
	if !unitListed(units, firewallReloadService) {
		c.Status = StatusWarn
		c.Detail = "firewalld is active and " + firewallReloadService + " is not installed"
		c.Loss = "pods lose bridge networking and DNS until podman network reload --all is run after a firewall-cmd --reload"
		c.Fix = "run 'podman network reload --all' after every firewall-cmd --reload (" + firewallReloadService + " is not installed)"
		return c
	}

	c.Status = StatusWarn
	c.Detail = "pod networking breaks on firewall-cmd --reload"
	c.Loss = "pods lose bridge networking and DNS until podman network reload --all is run"
	c.Fix = "sudo systemctl enable --now " + firewallReloadService
	return c
}

type certInfo struct {
	Present  bool
	NotAfter time.Time
	Status   Status
}

func checkCertificates(env Env) (Check, certInfo) {
	c := Check{ID: "certificates", Title: "certificates"}
	path := filepath.Join(env.Dirs.Data, "certs", "server.crt")
	data, err := env.readFile(path)
	if err != nil {
		c.Status = StatusFail
		c.Detail = "q8s is not installed (run q8s install): no server certificate at " + path
		c.Loss = "q8s is not installed (run q8s install); clients cannot verify the q8s API"
		c.Fix = "q8s install"
		return c, certInfo{Status: StatusFail}
	}

	block, _ := pem.Decode(data)
	if block == nil {
		c.Status = StatusFail
		c.Detail = "server certificate is not valid PEM: " + path
		c.Loss = "clients cannot verify the q8s API"
		c.Fix = "q8s install --regenerate-certs"
		return c, certInfo{Status: StatusFail}
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		c.Status = StatusFail
		c.Detail = "server certificate cannot be parsed: " + err.Error()
		c.Loss = "clients cannot verify the q8s API"
		c.Fix = "q8s install --regenerate-certs"
		return c, certInfo{Status: StatusFail}
	}

	info := certInfo{Present: true, NotAfter: cert.NotAfter}
	detail := fmt.Sprintf("SANs: %s; expires %s", formatSANs(cert), cert.NotAfter.UTC().Format(time.RFC3339))
	c.Detail = detail

	now := env.Now
	if now.IsZero() {
		now = time.Now()
	}
	switch {
	case !cert.NotAfter.After(now):
		c.Status = StatusFail
		info.Status = StatusFail
		c.Loss = "the q8s API certificate has expired; kubectl will reject it"
		c.Fix = "q8s install --regenerate-certs"
	case cert.NotAfter.Before(now.Add(30 * 24 * time.Hour)):
		c.Status = StatusWarn
		info.Status = StatusWarn
		c.Loss = "the q8s API certificate expires within 30 days; kubectl will fail after " +
			cert.NotAfter.UTC().Format("2006-01-02")
		c.Fix = "q8s install --regenerate-certs"
	default:
		c.Status = StatusOK
		info.Status = StatusOK
		c.Gain = "the q8s API certificate is trusted until " + cert.NotAfter.UTC().Format("2006-01-02")
	}
	return c, info
}

func formatSANs(cert *x509.Certificate) string {
	var parts []string
	for _, dns := range cert.DNSNames {
		parts = append(parts, "DNS:"+dns)
	}
	for _, ip := range cert.IPAddresses {
		parts = append(parts, "IP:"+ip.String())
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

func checkTraefikDir(env Env) Check {
	c := Check{ID: "traefik-dir", Title: "traefik directory"}
	if env.Dirs.Traefik == "" {
		c.Status = StatusInfo
		c.Detail = "no Ingress written yet"
		return c
	}
	if _, err := env.fsType(env.Dirs.Traefik); err != nil {
		c.Status = StatusInfo
		c.Detail = "no Ingress written yet"
		return c
	}
	if env.writable(env.Dirs.Traefik) {
		c.Status = StatusOK
		c.Detail = env.Dirs.Traefik + " is writable"
		c.Gain = "Ingress changes can be written immediately"
		return c
	}
	c.Status = StatusFail
	c.Detail = "Ingress changes cannot be written"
	c.Loss = "Ingress changes cannot be written: " + env.Dirs.Traefik + " is not writable"
	c.Fix = "chown " + userLabel(env) + " " + env.Dirs.Traefik
	return c
}

func buildSummary(env Env, checks []Check, cert certInfo) []SummaryLine {
	byID := make(map[string]Check, len(checks))
	for _, c := range checks {
		byID[c.ID] = c
	}

	isolation := "host uids (weak)"
	userns, hasUserNS := byID["userns"]
	idmap, hasIDMap := byID["idmap"]
	switch {
	case !env.Rootful && hasUserNS && userns.Status == StatusOK:
		// Rootless podman maps every container into the user's single
		// subordinate range, so pods are not isolated from each other.
		isolation = "your subordinate uid range, shared by all pods; container root is not host root"
	case hasUserNS && userns.Status == StatusOK && hasIDMap && idmap.Status == StatusWarn:
		isolation = "own uid range per pod; volumes on some paths cannot be idmapped"
	case hasUserNS && userns.Status == StatusOK:
		isolation = "own uid range per pod"
	case !env.Rootful:
		isolation = "no subordinate uid range"
	}

	limits := "unknown"
	if c, ok := byID["cgroups"]; ok {
		switch {
		case c.Status == StatusOK:
			limits = "enforced"
		case strings.HasPrefix(c.Detail, "missing controllers: "):
			limits = "not enforced for: " + strings.TrimPrefix(c.Detail, "missing controllers: ")
		default:
			limits = "unknown (no cgroup v2)"
		}
	}

	networking := "unknown"
	if c, ok := byID["network"]; ok {
		if env.Rootful {
			networking = c.Detail
		} else {
			networking = fmt.Sprintf("published ports (hostPort) go through %s in user space: extra latency. Ingress through the socket-activated Traefik pod uses the kernel path.", c.Detail)
		}
	}

	persistence := "systemd system units, start at boot"
	if !env.Rootful {
		switch c, ok := byID["linger"]; {
		case !ok:
			persistence = "unknown (no linger check)"
		case c.Status == StatusOK:
			persistence = "linger enabled: workloads survive logout and start at boot"
		case c.Status == StatusWarn:
			persistence = "workloads stop at logout (no linger)"
		default:
			persistence = "unknown (loginctl unavailable)"
		}
	}

	certificates := "missing"
	switch {
	case cert.Present && cert.Status == StatusFail:
		certificates = "expired"
	case cert.Present && cert.Status == StatusWarn:
		certificates = "valid until " + cert.NotAfter.UTC().Format("2006-01-02") + " (expires soon)"
	case cert.Present:
		certificates = "valid until " + cert.NotAfter.UTC().Format("2006-01-02")
	}

	return []SummaryLine{
		{Topic: "isolation", Text: isolation},
		{Topic: "limits", Text: limits},
		{Topic: "networking", Text: networking},
		{Topic: "persistence", Text: persistence},
		{Topic: "certificates", Text: certificates},
	}
}
