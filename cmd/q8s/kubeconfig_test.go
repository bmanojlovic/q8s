package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	"q8s/pkg/install"
)

// writeTestCA creates a CA in dir/certs the way q8s install does.
func writeTestCA(t *testing.T, dir string) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(42),
		Subject:               pkix.Name{CommonName: "q8s CA", Organization: []string{"q8s"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certDir := filepath.Join(dir, "certs")
	if err := os.MkdirAll(certDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(certDir, "ca.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(certDir, "ca.key"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatal(err)
	}
	return cert
}

func TestRunKubeconfigAdminOutputUnchanged(t *testing.T) {
	t.Setenv("Q8S_PORT", "6443")
	dir := t.TempDir()
	certDir := filepath.Join(dir, "certs")
	if err := os.MkdirAll(certDir, 0700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"ca.crt": "ca", "client.crt": "cert", "client.key": "key"}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(certDir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var stdout, stderr bytes.Buffer
	if err := runKubeconfig(nil, dirs{dataDir: dir}, &stdout, &stderr); err != nil {
		t.Fatalf("runKubeconfig: %v", err)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	want := `apiVersion: v1
kind: Config
clusters:
- cluster:
    certificate-authority-data: ` + b64("ca") + `
    server: https://localhost:6443
  name: q8s
contexts:
- context:
    cluster: q8s
    user: q8s
  name: q8s
current-context: q8s
preferences: {}
users:
- name: q8s
  user:
    client-certificate-data: ` + b64("cert") + `
    client-key-data: ` + b64("key") + `
`
	if stdout.String() != want {
		t.Fatalf("admin kubeconfig changed:\n got: %q\nwant: %q", stdout.String(), want)
	}
}

func TestRunKubeconfigUser(t *testing.T) {
	t.Setenv("Q8S_PORT", "6443")
	dir := t.TempDir()
	caCert := writeTestCA(t, dir)

	var stdout, stderr bytes.Buffer
	err := runKubeconfig([]string{"--user", "alice", "--group", "developers", "--days", "30"},
		dirs{dataDir: dir}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runKubeconfig: %v", err)
	}
	if strings.Contains(stderr.String(), "administrator") {
		t.Errorf("unexpected admin warning: %q", stderr.String())
	}
	serialLine := ""
	for _, line := range strings.Split(stderr.String(), "\n") {
		if strings.HasPrefix(line, "certificate serial: ") {
			serialLine = strings.TrimPrefix(line, "certificate serial: ")
		}
	}
	if serialLine == "" {
		t.Fatalf("stderr has no serial: %q", stderr.String())
	}

	var cfg kubeconfig
	if err := yaml.Unmarshal(stdout.Bytes(), &cfg); err != nil {
		t.Fatalf("kubeconfig does not parse: %v\n%s", err, stdout.String())
	}
	if cfg.APIVersion != "v1" || cfg.Kind != "Config" {
		t.Errorf("apiVersion/kind = %q/%q", cfg.APIVersion, cfg.Kind)
	}
	if len(cfg.Clusters) != 1 || len(cfg.Contexts) != 1 || len(cfg.Users) != 1 {
		t.Fatalf("kubeconfig sections = %d/%d/%d", len(cfg.Clusters), len(cfg.Contexts), len(cfg.Users))
	}
	if cfg.Clusters[0].Name != "q8s" || cfg.Clusters[0].Cluster.Server != "https://localhost:6443" {
		t.Errorf("cluster = %+v", cfg.Clusters[0])
	}
	if cfg.Contexts[0].Name != "alice@q8s" || cfg.Contexts[0].Context.User != "alice@q8s" || cfg.Contexts[0].Context.Cluster != "q8s" {
		t.Errorf("context = %+v", cfg.Contexts[0])
	}
	if cfg.Users[0].Name != "alice@q8s" {
		t.Errorf("user name = %q, want alice@q8s", cfg.Users[0].Name)
	}
	if cfg.CurrentContext != "alice@q8s" {
		t.Errorf("current-context = %q", cfg.CurrentContext)
	}

	certDER, err := base64.StdEncoding.DecodeString(cfg.Users[0].User.ClientCertificateData)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certDER)
	if block == nil {
		t.Fatal("client certificate is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if cert.Subject.CommonName != "alice" {
		t.Errorf("CN = %q", cert.Subject.CommonName)
	}
	if len(cert.Subject.Organization) != 1 || cert.Subject.Organization[0] != "developers" {
		t.Errorf("O = %v", cert.Subject.Organization)
	}
	if install.SerialHex(cert.SerialNumber) != serialLine {
		t.Errorf("stderr serial %q != certificate serial %q", serialLine, install.SerialHex(cert.SerialNumber))
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Errorf("certificate does not verify: %v", err)
	}
	if remaining := time.Until(cert.NotAfter); remaining < 29*24*time.Hour || remaining > 31*24*time.Hour {
		t.Errorf("NotAfter leaves %v, want about 30 days", remaining)
	}

	keyDER, err := base64.StdEncoding.DecodeString(cfg.Users[0].User.ClientKeyData)
	if err != nil {
		t.Fatal(err)
	}
	if block, _ := pem.Decode(keyDER); block == nil {
		t.Fatal("client key is not PEM")
	}
}

func TestRunKubeconfigAdminGroupWarning(t *testing.T) {
	t.Setenv("Q8S_PORT", "6443")
	dir := t.TempDir()
	writeTestCA(t, dir)
	for _, group := range []string{"system:masters", "q8s-user"} {
		var stdout, stderr bytes.Buffer
		if err := runKubeconfig([]string{"--user", "bob", "--group", group}, dirs{dataDir: dir}, &stdout, &stderr); err != nil {
			t.Fatalf("runKubeconfig: %v", err)
		}
		if !strings.Contains(stderr.String(), "administrator") {
			t.Errorf("group %q produced no admin warning: %q", group, stderr.String())
		}
	}
}

func TestRunKubeconfigRejectsBadArguments(t *testing.T) {
	dir := t.TempDir()
	cases := [][]string{
		{"--user", ""},
		{"--user", "ali\nce"},
		{"--user", "alice", "--group", ""},
		{"--user", "alice", "--days", "0"},
		{"--user", "alice", "--days", "3651"},
		{"--user", "alice", "--days", "abc"},
		{"--group", "team"},
		{"--days", "30"},
		{"--days", "365"},
		{"--user", "alice", "--revoke", "bob"},
		{"--revoke", "alice", "--revoked"},
		{"--revoke-serial", "not-hex"},
		{"--revoke-serial", ""},
		{"--unknown"},
	}
	for _, args := range cases {
		var stdout, stderr bytes.Buffer
		if err := runKubeconfig(args, dirs{dataDir: dir}, &stdout, &stderr); err == nil {
			t.Errorf("args %v accepted", args)
		}
	}
}

func TestRunKubeconfigRevocationCommands(t *testing.T) {
	dir := t.TempDir()
	d := dirs{dataDir: dir}
	path := install.RevokedPath(dir)

	var stdout, stderr bytes.Buffer
	if err := runKubeconfig([]string{"--revoked"}, d, &stdout, &stderr); err != nil {
		t.Fatalf("--revoked: %v", err)
	}
	if !strings.Contains(stdout.String(), "no revoked") {
		t.Errorf("empty --revoked output = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if err := runKubeconfig([]string{"--revoke", "alice"}, d, &stdout, &stderr); err != nil {
		t.Fatalf("--revoke alice: %v", err)
	}
	if err := runKubeconfig([]string{"--revoke-serial", "0xAB12"}, d, &stdout, &stderr); err != nil {
		t.Fatalf("--revoke-serial: %v", err)
	}

	revoked, err := install.LoadRevokedCertificates(path)
	if err != nil {
		t.Fatal(err)
	}
	if !revoked.HasUser("alice") || !revoked.HasSerial("ab12") {
		t.Fatalf("revoked = %+v", revoked)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("revoked.json mode = %o, want 0600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string][]string
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("revoked.json is invalid JSON: %v", err)
	}
	if len(raw["users"]) != 1 || raw["users"][0] != "alice" {
		t.Errorf("users = %v", raw["users"])
	}
	if len(raw["serials"]) != 1 || raw["serials"][0] != "ab12" {
		t.Errorf("serials = %v", raw["serials"])
	}

	stdout.Reset()
	if err := runKubeconfig([]string{"--revoked"}, d, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "alice") || !strings.Contains(stdout.String(), "ab12") {
		t.Errorf("--revoked output = %q", stdout.String())
	}

	// Revoking the built-in administrator identity is refused and leaves the
	// file untouched.
	before := append([]byte(nil), data...)
	if err := runKubeconfig([]string{"--revoke", "q8s-user"}, d, &stdout, &stderr); err == nil {
		t.Fatal("--revoke q8s-user was accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("revoked.json changed after refusing --revoke q8s-user")
	}
}

func TestParseKubeconfigArgsHelp(t *testing.T) {
	opts, err := parseKubeconfigArgs([]string{"--help"})
	if err != nil {
		t.Fatalf("--help: %v", err)
	}
	if !opts.help {
		t.Error("--help did not set help")
	}
	opts, err = parseKubeconfigArgs([]string{"--user=alice", "--days=30"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.user != "alice" || opts.days != 30 {
		t.Errorf("inline flags parsed as %+v", opts)
	}
}
