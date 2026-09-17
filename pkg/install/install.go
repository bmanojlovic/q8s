package install

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// InstallConfig holds the configuration for installation.
type InstallConfig struct {
	Rootful bool
	Home    string
	// Name is the kubeconfig cluster/context name. Defaults to the hostname.
	Name string
	// Port is the TCP port the API server listens on. Defaults to 6443.
	Port int
	// ServerURL is the server address for kubeconfig. Defaults to https://localhost:{port}.
	ServerURL string
	// ExtraSANIPs are additional IP addresses to include in the server certificate SAN.
	ExtraSANIPs []net.IP
	// ExtraSANDNS are additional DNS names to include in the server certificate SAN.
	ExtraSANDNS []string
	// RegenerateCerts forces certificate regeneration even if they already exist.
	RegenerateCerts bool
}

// PersistentConfig is the install-time configuration persisted to config.json.
// It survives cert regeneration and is the source of truth for port, server URL,
// and certificate SANs.
type PersistentConfig struct {
	Name        string   `json:"name"`
	Port        int      `json:"port"`
	ServerURL   string   `json:"serverURL"`
	ExtraSANIPs []string `json:"extraSANIPs,omitempty"`
	ExtraSANDNS []string `json:"extraSANDNS,omitempty"`
}

// ConfigPath returns the path to config.json for the given dataDir.
func ConfigPath(dataDir string) string {
	return filepath.Join(dataDir, "config.json")
}

// LoadConfig reads the persistent config from dataDir/config.json.
// Returns a zero-value config (not an error) if the file doesn't exist.
func LoadConfig(dataDir string) (PersistentConfig, error) {
	data, err := os.ReadFile(ConfigPath(dataDir))
	if err != nil {
		if os.IsNotExist(err) {
			return PersistentConfig{}, nil
		}
		return PersistentConfig{}, err
	}
	var cfg PersistentConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return PersistentConfig{}, fmt.Errorf("parse config.json: %w", err)
	}
	return cfg, nil
}

// SaveConfig writes the persistent config to dataDir/config.json.
func SaveConfig(dataDir string, cfg PersistentConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(ConfigPath(dataDir), data, 0600)
}

// DefaultPort is the standard q8s API port (matches kube-apiserver's default).
const DefaultPort = 6443

// binInstallPath returns the canonical install location for the binary.
func binInstallPath(rootful bool, home string) string {
	if rootful {
		return "/usr/local/bin/q8s"
	}
	if home == "" {
		home = os.Getenv("HOME")
	}
	return filepath.Join(home, ".local", "bin", "q8s")
}

// installBinary copies the running executable to dst.
func installBinary(dst string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot locate current binary: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	src, err := os.Open(self)
	if err != nil {
		return err
	}
	defer src.Close()

	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// certsNeedRegen decides whether TLS certs must be regenerated: always when
// they don't exist or --regenerate-certs was passed; when the merged SAN
// list differs from what was previously persisted; when any existing cert
// fails to parse (truncated or corrupt file); or when the server cert
// expires within certRenewWindow. Unchanged SANs on re-run must not mint a
// fresh identity — but neither may `q8s install` keep saying "skipping
// generation" while every client gets opaque TLS errors on an expired cert.
func certsNeedRegen(dataDir string, certsExist, force bool, merged, existing PersistentConfig) bool {
	if !certsExist || force {
		return true
	}
	if !sameStrings(merged.ExtraSANIPs, existing.ExtraSANIPs) ||
		!sameStrings(merged.ExtraSANDNS, existing.ExtraSANDNS) {
		return true
	}
	notAfter, err := certExpiry(filepath.Join(dataDir, "certs", "server.crt"))
	if err != nil {
		return true // unreadable/unparseable cert: safest is to regenerate
	}
	if remaining := time.Until(notAfter); remaining < certRenewWindow {
		fmt.Printf("Server certificate expires in %s — regenerating.\n", remaining.Round(time.Hour))
		return true
	}
	return false
}

// certRenewWindow is how close to expiry a certificate must be before
// `q8s install` regenerates it.
const certRenewWindow = 30 * 24 * time.Hour

// certExpiry parses a PEM certificate file and returns its NotAfter.
func certExpiry(path string) (time.Time, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return time.Time{}, fmt.Errorf("%s: no PEM block", path)
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s: %w", path, err)
	}
	return c.NotAfter, nil
}

// allCertsPresent reports whether every file q8s needs is on disk. Checking
// only ca.crt would let a partial set (ca.crt present, server.crt missing
// after a failed write) count as "certs exist", skip regeneration, and then
// die at `q8s serve` reading the missing file.
func allCertsPresent(dataDir string) bool {
	for _, name := range []string{"ca.crt", "ca.key", "server.crt", "server.key", "client.crt", "client.key"} {
		if _, err := os.Stat(filepath.Join(dataDir, "certs", name)); err != nil {
			return false
		}
	}
	return true
}

// sameStrings reports whether two string slices contain the same set.
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]bool, len(a))
	for _, s := range a {
		seen[s] = true
	}
	for _, s := range b {
		if !seen[s] {
			return false
		}
	}
	return true
}

// restartService restarts q8s-api and verifies the restart actually
// happened. A plain `systemctl restart` can report success without anything
// changing (e.g. on a socket-activated service whose process was started
// outside systemd), which would leave the server serving the old
// certificate silently. The MainPID check plus the stop/start fallback turn
// that failure mode into a loud warning instead.
func restartService(systemctlArgs []string) {
	mainPID := func() int {
		out, err := exec.Command("systemctl", append(systemctlArgs, "show", "q8s-api.service", "-p", "MainPID", "--value")...).Output()
		if err != nil {
			return -1
		}
		pid, _ := strconv.Atoi(strings.TrimSpace(string(out)))
		return pid
	}

	before := mainPID()
	if err := exec.Command("systemctl", append(systemctlArgs, "restart", "q8s-api.service")...).Run(); err != nil {
		fmt.Printf("Warning: systemctl restart q8s-api.service failed: %v — restart it manually to load the new certificates.\n", err)
		return
	}
	time.Sleep(500 * time.Millisecond)
	if after := mainPID(); after > 0 && after != before {
		fmt.Println("Restarted q8s-api.service to load new certificates.")
		return
	}
	// The restart didn't move the service (or it wasn't managed by systemd
	// at all) — try an explicit stop/start.
	exec.Command("systemctl", append(systemctlArgs, "stop", "q8s-api.service")...).Run()
	if err := exec.Command("systemctl", append(systemctlArgs, "start", "q8s-api.service")...).Run(); err != nil {
		fmt.Printf("Warning: starting q8s-api.service failed: %v — start it manually to load the new certificates.\n", err)
		return
	}
	time.Sleep(500 * time.Millisecond)
	if after := mainPID(); after > 0 && after != before {
		fmt.Println("Restarted q8s-api.service to load new certificates.")
		return
	}
	fmt.Println("Warning: could not verify q8s-api.service restarted — restart it manually to load the new certificates.")
}

// Install sets up q8s: generates TLS certs, creates directories, installs systemd units.
func Install(cfg InstallConfig) error {
	if cfg.Port == 0 {
		cfg.Port = DefaultPort
	}

	var dataDir string
	if cfg.Rootful {
		dataDir = "/etc/q8s"
	} else {
		home := cfg.Home
		if home == "" {
			home = os.Getenv("HOME")
		}
		if home == "" {
			return fmt.Errorf("HOME not set")
		}
		dataDir = filepath.Join(home, ".local", "share", "q8s")
	}

	// Create directories
	dirs := []string{
		filepath.Join(dataDir, "certs"),
		filepath.Join(dataDir, "quadlets"),
	}
	if !cfg.Rootful {
		home := cfg.Home
		if home == "" {
			home = os.Getenv("HOME")
		}
		dirs = append(dirs, filepath.Join(home, ".config", "systemd", "user"))
	} else {
		dirs = append(dirs, "/etc/systemd/system")
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}

	// Load existing persistent config (preserves SANs across regen).
	existing, _ := LoadConfig(dataDir)

	// Merge: new flags override, existing values fill gaps.
	pcfg := PersistentConfig{Port: cfg.Port}

	if cfg.Name != "" {
		pcfg.Name = cfg.Name
	} else if existing.Name != "" {
		pcfg.Name = existing.Name
	} else {
		hostname, _ := os.Hostname()
		pcfg.Name = hostname
		switch hostname {
		case "localhost", "ubuntu", "fedora", "debian", "archlinux", "opensuse", "rhel", "centos", "rocky", "alma":
			fmt.Printf("Warning: hostname %q is a distro default — consider q8s install --name <unique-name> to avoid kubeconfig collisions.\n", hostname)
		}
	}

	if cfg.ServerURL != "" {
		pcfg.ServerURL = normalizeServerURL(cfg.ServerURL, cfg.Port)
	} else if existing.ServerURL != "" {
		pcfg.ServerURL = existing.ServerURL
	} else {
		pcfg.ServerURL = fmt.Sprintf("https://localhost:%d", cfg.Port)
	}

	// SANs: merge new flags with previously persisted ones.
	sanIPSet := make(map[string]bool)
	var sanIPs []net.IP
	addIP := func(ip net.IP) {
		s := ip.String()
		if !sanIPSet[s] {
			sanIPSet[s] = true
			sanIPs = append(sanIPs, ip)
			pcfg.ExtraSANIPs = append(pcfg.ExtraSANIPs, s)
		}
	}
	for _, ip := range cfg.ExtraSANIPs {
		addIP(ip)
	}
	for _, s := range existing.ExtraSANIPs {
		if ip := net.ParseIP(s); ip != nil {
			addIP(ip)
		}
	}

	sanDNSSet := make(map[string]bool)
	addDNS := func(name string) {
		if !sanDNSSet[name] {
			sanDNSSet[name] = true
			pcfg.ExtraSANDNS = append(pcfg.ExtraSANDNS, name)
		}
	}
	for _, name := range cfg.ExtraSANDNS {
		addDNS(name)
	}
	for _, name := range existing.ExtraSANDNS {
		addDNS(name)
	}

	// Generate TLS certs if they don't exist or regeneration is requested.
	// Re-running install with an unchanged SAN list must not mint a fresh
	// CA/client identity — callers that merge repeated kubeconfig fetches
	// would otherwise accumulate stale, unusable entries under one context.
	//
	// Certs are written BEFORE config.json is persisted: config.json records
	// the SAN list that certsNeedRegen compares against, so saving it first
	// would make a failed cert write (disk full, permissions) permanently
	// suppress regeneration — every later run would see "SANs unchanged"
	// and skip, leaving the new SAN missing from the server cert forever.
	certsExist := allCertsPresent(dataDir)
	certsRegenerated := false
	if certsNeedRegen(dataDir, certsExist, cfg.RegenerateCerts, pcfg, existing) {
		c, err := generateCerts(sanIPs, pcfg.ExtraSANDNS)
		if err != nil {
			return fmt.Errorf("failed to generate certs: %w", err)
		}
		if err := writeCerts(dataDir, c); err != nil {
			return fmt.Errorf("failed to write certs: %w", err)
		}
		if certsExist {
			fmt.Println("Regenerated TLS certificates with updated SANs.")
			certsRegenerated = true
		} else {
			fmt.Println("Generated TLS certificates.")
		}
	} else {
		fmt.Println("TLS certificates already exist, skipping generation.")
	}

	// Persist config after certs succeeded (see above). A partial failure
	// past this point loses nothing that the next `q8s install` won't redo.
	if err := SaveConfig(dataDir, pcfg); err != nil {
		return fmt.Errorf("failed to write config.json: %w", err)
	}

	// Install binary to a well-known PATH location
	binPath := binInstallPath(cfg.Rootful, cfg.Home)
	if err := installBinary(binPath); err != nil {
		return fmt.Errorf("failed to install binary to %s: %w", binPath, err)
	}
	fmt.Printf("Binary installed to %s\n", binPath)

	// Install systemd units
	if err := installSystemdUnits(cfg); err != nil {
		return fmt.Errorf("failed to install systemd units: %w", err)
	}

	// Rootless units live in the user manager, which stops at last logout
	// unless lingering is enabled — that would take every workload down.
	if !cfg.Rootful {
		enableLinger()
	}

	// Restart service if certs were regenerated (so it picks up the new cert)
	if certsRegenerated {
		systemctlArgs := []string{"--user"}
		if cfg.Rootful {
			systemctlArgs = nil
		}
		restartService(systemctlArgs)
	}

	// Print instructions
	fmt.Println("=== q8s installed successfully ===")
	fmt.Println()
	fmt.Printf("Data directory: %s\n", dataDir)
	fmt.Println()
	fmt.Println("To configure kubectl, run:")
	fmt.Printf("  kubectl config set-cluster %s --server=%s --certificate-authority=%s/certs/ca.crt --client-certificate=%s/certs/client.crt --client-key=%s/certs/client.key --embed-certs=true\n", pcfg.Name, pcfg.ServerURL, dataDir, dataDir, dataDir)
	fmt.Printf("  kubectl config set-credentials %s --embed-certs=true\n", pcfg.Name)
	fmt.Printf("  kubectl config set-context %s --cluster=%s --user=%s\n", pcfg.Name, pcfg.Name, pcfg.Name)
	fmt.Printf("  kubectl config use-context %s\n", pcfg.Name)
	fmt.Println()
	fmt.Println("Or import directly:")
	fmt.Printf("  %s kubeconfig | kubectl config merge --flatten -\n", binInstallPath(cfg.Rootful, cfg.Home))
	fmt.Println()
	fmt.Println("To start the API server via systemd:")
	if cfg.Rootful {
		fmt.Println("  sudo systemctl enable --now q8s.socket")
	} else {
		fmt.Println("  systemctl --user enable --now q8s.socket")
	}
	fmt.Println()
	fmt.Println("Or run directly:")
	fmt.Printf("  %s start\n", binInstallPath(cfg.Rootful, cfg.Home))

	return nil
}

type certs struct {
	caCert     []byte
	caKey      []byte
	serverCert []byte
	serverKey  []byte
	clientCert []byte
	clientKey  []byte
}

func generateCerts(extraIPs []net.IP, extraDNS []string) (certs, error) {
	return generateCertsValid(extraIPs, extraDNS, 365*24*time.Hour)
}

// generateCertsValid is generateCerts with an explicit validity (tests use
// near-expiry certificates to exercise the renewal window).
func generateCertsValid(extraIPs []net.IP, extraDNS []string, validity time.Duration) (certs, error) {
	var c certs
	// Random serials and checked errors: a failed keygen must fail the
	// install (nil-deref panics otherwise), and per-RFC 5280 serials from
	// a CA should be unpredictable.
	newSerial := func() *big.Int {
		limit := new(big.Int).Lsh(big.NewInt(1), 128)
		n, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return big.NewInt(1)
		}
		return n
	}
	// NotBefore backdated 5 minutes so a slightly-ahead server clock
	// doesn't reject the cert as "not yet valid" in the very first seconds.
	notBefore := time.Now().Add(-5 * time.Minute)
	notAfter := time.Now().Add(validity)

	// Generate CA
	caPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return c, fmt.Errorf("generate CA key: %w", err)
	}
	caTemplate := x509.Certificate{
		SerialNumber: newSerial(),
		Subject: pkix.Name{
			Organization: []string{"q8s"},
			CommonName:   "q8s CA",
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caCertDER, err := x509.CreateCertificate(rand.Reader, &caTemplate, &caTemplate, &caPriv.PublicKey, caPriv)
	if err != nil {
		return c, fmt.Errorf("create CA cert: %w", err)
	}

	// Generate server cert
	serverPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return c, fmt.Errorf("generate server key: %w", err)
	}
	serverTemplate := x509.Certificate{
		SerialNumber: newSerial(),
		Subject: pkix.Name{
			Organization: []string{"q8s"},
			CommonName:   "q8s-server",
		},
		NotBefore:   notBefore,
		NotAfter:    notAfter,
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: appendIPs(localIPs(), extraIPs),
		DNSNames:    appendStrings(localDNSNames(), extraDNS),
	}
	serverCertDER, err := x509.CreateCertificate(rand.Reader, &serverTemplate, &caTemplate, &serverPriv.PublicKey, caPriv)
	if err != nil {
		return c, fmt.Errorf("create server cert: %w", err)
	}

	// Generate client cert
	clientPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return c, fmt.Errorf("generate client key: %w", err)
	}
	clientTemplate := x509.Certificate{
		SerialNumber: newSerial(),
		Subject: pkix.Name{
			Organization: []string{"q8s-user"},
			CommonName:   "q8s-user",
		},
		NotBefore:   notBefore,
		NotAfter:    notAfter,
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	clientCertDER, err := x509.CreateCertificate(rand.Reader, &clientTemplate, &caTemplate, &clientPriv.PublicKey, caPriv)
	if err != nil {
		return c, fmt.Errorf("create client cert: %w", err)
	}

	c = certs{
		caCert:     toPEMBlock("CERTIFICATE", caCertDER),
		caKey:      marshalECDSA(caPriv),
		serverCert: toPEMBlock("CERTIFICATE", serverCertDER),
		serverKey:  marshalECDSA(serverPriv),
		clientCert: toPEMBlock("CERTIFICATE", clientCertDER),
		clientKey:  marshalECDSA(clientPriv),
	}
	return c, nil
}

func toPEMBlock(typeStr string, derBytes []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: typeStr, Bytes: derBytes})
}

func marshalECDSA(priv *ecdsa.PrivateKey) []byte {
	b, _ := x509.MarshalECPrivateKey(priv)
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: b})
}

func writeCerts(dataDir string, c certs) error {
	certDir := filepath.Join(dataDir, "certs")
	// 0700: this directory holds the CA and client private keys.
	if err := os.MkdirAll(certDir, 0700); err != nil {
		return err
	}

	files := map[string][]byte{
		"ca.crt":     c.caCert,
		"ca.key":     c.caKey,
		"server.crt": c.serverCert,
		"server.key": c.serverKey,
		"client.crt": c.clientCert,
		"client.key": c.clientKey,
	}

	for name, content := range files {
		if err := os.WriteFile(filepath.Join(certDir, name), content, 0600); err != nil {
			return err
		}
	}
	return nil
}

func installSystemdUnits(cfg InstallConfig) error {
	var systemdDir string
	var systemctlArgs []string

	if cfg.Rootful {
		systemdDir = "/etc/systemd/system"
	} else {
		home := cfg.Home
		if home == "" {
			home = os.Getenv("HOME")
		}
		if home == "" {
			return fmt.Errorf("HOME not set")
		}
		systemdDir = filepath.Join(home, ".config", "systemd", "user")
		systemctlArgs = []string{"--user"}
	}

	binaryPath := binInstallPath(cfg.Rootful, cfg.Home)

	// Generate q8s.socket
	// %t = $XDG_RUNTIME_DIR for user units, /run for system units
	port := cfg.Port
	if port == 0 {
		port = DefaultPort
	}
	socketUnit := fmt.Sprintf(`[Unit]
Description=q8s API Server Socket

[Socket]
ListenStream=%d
ListenStream=%%t/q8s/api.sock
Service=q8s-api.service
RemoveOnStop=yes

[Install]
WantedBy=sockets.target
`, port)

	// Generate q8s-api.service
	serviceUnit := fmt.Sprintf(`[Unit]
Description=q8s API Server
Requires=q8s.socket
After=q8s.socket

[Service]
Type=notify
ExecStart=%s serve
NotifyAccess=main
Restart=on-failure
RuntimeDirectory=q8s
`, binaryPath)

	if !cfg.Rootful {
		// Prefer the live address (already has unix:path= scheme); fall back to constructing it.
		busAddr := os.Getenv("DBUS_SESSION_BUS_ADDRESS")
		if busAddr == "" {
			busAddr = fmt.Sprintf("unix:path=%s/bus", os.Getenv("XDG_RUNTIME_DIR"))
		}
		serviceUnit += fmt.Sprintf("Environment=DBUS_SESSION_BUS_ADDRESS=%s\n", busAddr)
	}

	if cfg.Rootful {
		serviceUnit += "User=root\nGroup=root\n"
	}

	serviceUnit += `
[Install]
WantedBy=multi-user.target
`

	// Write unit files
	if err := os.WriteFile(filepath.Join(systemdDir, "q8s.socket"), []byte(socketUnit), 0644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(systemdDir, "q8s-api.service"), []byte(serviceUnit), 0644); err != nil {
		return err
	}

	// Reload systemd
	cmd := exec.Command("systemctl", append(systemctlArgs, "daemon-reload")...)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to reload systemd: %w", err)
	}

	fmt.Printf("Installed systemd units to %s\n", systemdDir)
	return nil
}

// localIPs returns the IPs for the server certificate SAN:
// loopback (127.0.0.1, ::1) plus the default-route source IP.
func localIPs() []net.IP {
	ips := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	// Get the default route source IP (equivalent to: ip route get 1 | awk '{print $7}')
	conn, err := net.Dial("udp4", "1.1.1.1:80")
	if err != nil {
		return ips
	}
	defer conn.Close()
	if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		ips = append(ips, addr.IP)
	}
	return ips
}

// localDNSNames returns DNS names for the server certificate SAN.
// Includes "localhost" plus the machine hostname.
func localDNSNames() []string {
	names := []string{"localhost"}
	if hostname, err := os.Hostname(); err == nil && hostname != "localhost" {
		names = append(names, hostname)
	}
	return names
}

func appendIPs(base, extra []net.IP) []net.IP {
	seen := make(map[string]bool, len(base))
	for _, ip := range base {
		seen[ip.String()] = true
	}
	for _, ip := range extra {
		if !seen[ip.String()] {
			base = append(base, ip)
		}
	}
	return base
}

func appendStrings(base, extra []string) []string {
	seen := make(map[string]bool, len(base))
	for _, s := range base {
		seen[s] = true
	}
	for _, s := range extra {
		if !seen[s] {
			base = append(base, s)
		}
	}
	return base
}

// normalizeServerURL ensures the server URL has an https:// scheme and a
// port. Parses with net/url rather than string surgery: any colon (a URL
// path, a query, an IPv6 literal) used to trip the "has a port" heuristic
// and produce https://myhost/path:6443.
func normalizeServerURL(raw string, port int) string {
	if !strings.HasPrefix(raw, "https://") && !strings.HasPrefix(raw, "http://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw // unparseable input: leave it untouched, kubeconfig shows it verbatim
	}
	if u.Port() == "" {
		u.Host = net.JoinHostPort(u.Hostname(), strconv.Itoa(port))
	}
	return u.String()
}

// --- Linger (rootless unattended operation) ---
//
// In rootless mode everything q8s manages — the API socket, the service,
// every quadlet container, every cron timer — is a systemd *user* unit.
// systemd kills the whole user instance (KillUserProcesses semantics vary,
// but the manager itself stops) when its last login session ends, unless
// lingering is enabled for that user. Without linger, logging out of the
// machine takes every workload down. Enabling it is therefore part of a
// correct rootless install, not an optional nicety.

// LingerEnabled reports whether lingering is enabled for the current user.
// Returns an error when loginctl is unavailable or the state can't be read
// (e.g. no systemd); callers treat that as "unknown" and say so.
func LingerEnabled() (bool, error) {
	uid := strconv.Itoa(os.Getuid())
	out, err := exec.Command("loginctl", "show-user", uid, "-p", "Linger", "--value").Output()
	if err != nil {
		return false, fmt.Errorf("loginctl show-user: %w", err)
	}
	return strings.TrimSpace(string(out)) == "yes", nil
}

// enableLinger turns on lingering for the current user so rootless q8s keeps
// running after logout. Modern systemd allows users to enable linger for
// themselves via polkit; when that is denied, print the exact sudo command
// instead of failing the install.
func enableLinger() {
	uid := strconv.Itoa(os.Getuid())
	if on, err := LingerEnabled(); err == nil && on {
		return // already enabled; idempotent
	}
	if err := exec.Command("loginctl", "enable-linger", uid).Run(); err != nil {
		fmt.Printf("Warning: could not enable lingering (loginctl enable-linger: %v).\n", err)
		fmt.Printf("Without lingering, logging out stops the rootless q8s server and all its containers.\n")
		fmt.Printf("Enable it manually: sudo loginctl enable-linger %s\n", uid)
		return
	}
	fmt.Println("Enabled lingering (user services keep running after logout).")
}
