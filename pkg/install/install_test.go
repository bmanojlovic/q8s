package install

import (
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCertsNeedRegen(t *testing.T) {
	// A valid, far-from-expiry cert set so the "no regen" cases pass the
	// expiry/corruption check too.
	dir := t.TempDir()
	if err := writeTestCerts(t, dir, 100*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		exist    bool
		force    bool
		merged   PersistentConfig
		existing PersistentConfig
		want     bool
	}{
		{"missing certs", false, false, PersistentConfig{}, PersistentConfig{}, true},
		{"force", true, true, PersistentConfig{}, PersistentConfig{}, true},
		{"first install with SANs", true, false,
			PersistentConfig{ExtraSANDNS: []string{"a.example"}}, PersistentConfig{}, true},
		{"unchanged SANs", true, false,
			PersistentConfig{ExtraSANDNS: []string{"a.example"}}, PersistentConfig{ExtraSANDNS: []string{"a.example"}}, false},
		{"new SAN added", true, false,
			PersistentConfig{ExtraSANDNS: []string{"a.example", "b.example"}}, PersistentConfig{ExtraSANDNS: []string{"a.example"}}, true},
		{"new IP SAN added", true, false,
			PersistentConfig{ExtraSANIPs: []string{"10.0.0.1"}}, PersistentConfig{}, true},
		{"no SANs at all", true, false, PersistentConfig{}, PersistentConfig{}, false},
	}
	for _, tc := range cases {
		if got := certsNeedRegen(dir, tc.exist, tc.force, tc.merged, tc.existing); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSameStrings(t *testing.T) {
	if !sameStrings([]string{"a", "b"}, []string{"b", "a"}) {
		t.Error("expected set-equal")
	}
	if sameStrings([]string{"a", "b"}, []string{"a", "c"}) {
		t.Error("expected not equal")
	}
	if sameStrings([]string{"a", "b"}, []string{"a", "b", "c"}) {
		t.Error("expected not equal (different length)")
	}
	if sameStrings(nil, []string{"a"}) {
		t.Error("expected not equal (nil vs one)")
	}
}

// --- installer fixes ---

func TestNormalizeServerURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"myhost", "https://myhost:6443"},
		{"https://myhost", "https://myhost:6443"},
		{"https://myhost:8443", "https://myhost:8443"},
		{"https://myhost/path", "https://myhost:6443/path"},
		{"http://myhost", "http://myhost:6443"},
	}
	for _, tc := range cases {
		if got := normalizeServerURL(tc.in, 6443); got != tc.want {
			t.Errorf("normalizeServerURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestGenerateCertsValidAndUniqueSerials(t *testing.T) {
	c1, err := generateCerts(nil, nil)
	if err != nil {
		t.Fatalf("generateCerts: %v", err)
	}
	// All six PEM artifacts must decode.
	for name, pemBytes := range map[string][]byte{
		"ca.crt": c1.caCert, "ca.key": c1.caKey,
		"server.crt": c1.serverCert, "server.key": c1.serverKey,
		"client.crt": c1.clientCert, "client.key": c1.clientKey,
	} {
		if block, _ := pem.Decode(pemBytes); block == nil {
			t.Errorf("%s: no PEM block", name)
		}
	}
	// Serials must be random per certificate, not constants 1/2/3.
	parseSerial := func(pemBytes []byte) *big.Int {
		block, _ := pem.Decode(pemBytes)
		if block == nil {
			return nil
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil
		}
		return c.SerialNumber
	}
	serials := []*big.Int{parseSerial(c1.caCert), parseSerial(c1.serverCert), parseSerial(c1.clientCert)}
	for _, s := range serials {
		if s == nil {
			t.Fatal("could not parse serial")
		}
		if s.Int64() >= 1 && s.Int64() <= 3 {
			t.Errorf("serial looks like the old constant: %d", s.Int64())
		}
	}
	if serials[0].Cmp(serials[1]) == 0 || serials[1].Cmp(serials[2]) == 0 {
		t.Error("serials must be unique")
	}
}

func TestCertsNeedRegenExpiryAndCorruption(t *testing.T) {
	dir := t.TempDir()
	cfgSame := PersistentConfig{ExtraSANDNS: []string{"a.example"}}

	// Not existing -> regen.
	if !certsNeedRegen(dir, false, false, cfgSame, cfgSame) {
		t.Error("missing certs must regenerate")
	}

	// Write a full set valid for 100 days, same SANs -> no regen.
	if err := writeTestCerts(t, dir, 100*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if certsNeedRegen(dir, true, false, cfgSame, cfgSame) {
		t.Error("valid unexpired certs with unchanged SANs must not regenerate")
	}

	// Expiring within the renewal window -> regen.
	if err := writeTestCerts(t, dir, 10*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if !certsNeedRegen(dir, true, false, cfgSame, cfgSame) {
		t.Error("expiring certs must regenerate")
	}

	// Corrupt server cert -> regen.
	if err := os.WriteFile(filepath.Join(dir, "certs", "server.crt"), []byte("garbage"), 0600); err != nil {
		t.Fatal(err)
	}
	if !certsNeedRegen(dir, true, false, cfgSame, cfgSame) {
		t.Error("corrupt cert must regenerate")
	}
}

func TestAllCertsPresentRequiresFullSet(t *testing.T) {
	dir := t.TempDir()
	if allCertsPresent(dir) {
		t.Fatal("empty dir must not count as certs present")
	}
	c, err := generateCerts(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeCerts(dir, c); err != nil {
		t.Fatal(err)
	}
	if !allCertsPresent(dir) {
		t.Error("full set must count as present")
	}
	// Remove one file -> partial set must not count (old code checked only
	// ca.crt and later died in serve reading server.crt).
	os.Remove(filepath.Join(dir, "certs", "server.crt"))
	if allCertsPresent(dir) {
		t.Error("partial set must not count as present")
	}
}

// writeTestCerts generates a fresh set with a custom validity and writes it.
func writeTestCerts(t *testing.T, dir string, validity time.Duration) error {
	t.Helper()
	c, err := generateCertsValid(nil, nil, validity)
	if err != nil {
		return err
	}
	return writeCerts(dir, c)
}
