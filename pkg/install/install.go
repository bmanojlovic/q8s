package install

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// InstallConfig holds the configuration for installation.
type InstallConfig struct {
	Rootful bool
	Home    string
}

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

// Install sets up q8s: generates TLS certs, creates directories, installs systemd units.
func Install(cfg InstallConfig) error {
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

	// Install binary to a well-known PATH location
	binPath := binInstallPath(cfg.Rootful, cfg.Home)
	if err := installBinary(binPath); err != nil {
		return fmt.Errorf("failed to install binary to %s: %w", binPath, err)
	}
	fmt.Printf("Binary installed to %s\n", binPath)

	// Generate TLS certs only if they don't already exist
	if _, err := os.Stat(filepath.Join(dataDir, "certs", "ca.crt")); os.IsNotExist(err) {
		certs := generateCerts()
		if err := writeCerts(dataDir, certs); err != nil {
			return fmt.Errorf("failed to write certs: %w", err)
		}
		fmt.Println("Generated TLS certificates.")
	} else {
		fmt.Println("TLS certificates already exist, skipping generation.")
	}

	// Install systemd units
	if err := installSystemdUnits(cfg); err != nil {
		return fmt.Errorf("failed to install systemd units: %w", err)
	}

	// Print instructions
	fmt.Println("=== q8s installed successfully ===")
	fmt.Println()
	fmt.Printf("Data directory: %s\n", dataDir)
	fmt.Printf("Quadlet directory: %s/quadlets\n", dataDir)
	fmt.Println()
	fmt.Println("To configure kubectl, run:")
	fmt.Printf("  kubectl config set-cluster q8s --server=https://localhost:6443 --certificate-authority=%s/certs/ca.crt --client-certificate=%s/certs/client.crt --client-key=%s/certs/client.key --embed-certs=true\n", dataDir, dataDir, dataDir)
	fmt.Println("  kubectl config set-credentials q8s-user --embed-certs=true")
	fmt.Println("  kubectl config set-context q8s --cluster=q8s --user=q8s-user")
	fmt.Println("  kubectl config use-context q8s")
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
	caCert    []byte
	caKey     []byte
	serverCert []byte
	serverKey  []byte
	clientCert []byte
	clientKey  []byte
}

func generateCerts() certs {
	// Generate CA
	caPriv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTemplate := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"q8s"},
			CommonName:   "q8s CA",
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caCertDER, _ := x509.CreateCertificate(rand.Reader, &caTemplate, &caTemplate, &caPriv.PublicKey, caPriv)

	// Generate server cert
	serverPriv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	serverTemplate := x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject: pkix.Name{
			Organization: []string{"q8s"},
			CommonName:   "q8s-server",
		},
		NotBefore:   time.Now(),
		NotAfter:    time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		DNSNames:    []string{"localhost"},
	}
	serverCertDER, _ := x509.CreateCertificate(rand.Reader, &serverTemplate, &caTemplate, &serverPriv.PublicKey, caPriv)

	// Generate client cert
	clientPriv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	clientTemplate := x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject: pkix.Name{
			Organization: []string{"q8s-user"},
			CommonName:   "q8s-user",
		},
		NotBefore:   time.Now(),
		NotAfter:    time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	clientCertDER, _ := x509.CreateCertificate(rand.Reader, &clientTemplate, &caTemplate, &clientPriv.PublicKey, caPriv)

	return certs{
		caCert:     toPEMBlock("CERTIFICATE", caCertDER),
		caKey:      marshalECDSA(caPriv),
		serverCert: toPEMBlock("CERTIFICATE", serverCertDER),
		serverKey:  marshalECDSA(serverPriv),
		clientCert: toPEMBlock("CERTIFICATE", clientCertDER),
		clientKey:  marshalECDSA(clientPriv),
	}
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
	socketUnit := fmt.Sprintf(`[Unit]
Description=q8s API Server Socket

[Socket]
ListenStream=6443
ListenStream=%%t/q8s/api.sock
Service=q8s-api.service
RemoveOnStop=yes

[Install]
WantedBy=sockets.target
`)

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
