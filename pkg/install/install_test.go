package install

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
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

func TestNormalizeSANDNS(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{"node1.example.com", "node1.example.com", true},
		{" Node1 ", "node1", true},
		{"*.apps.example.com", "*.apps.example.com", true},
		{"root@203.0.113.10", "", false},
		{"https://example.com", "", false},
		{"", "", false},
		{"a..b", "", false},
	} {
		got, err := NormalizeSANDNS(tc.in)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("NormalizeSANDNS(%q) = %q, %v; want %q ok=%v", tc.in, got, err, tc.want, tc.ok)
		}
	}
}

// A persisted invalid name (from an install before validation existed) is
// dropped from the existing list itself, so the cleanup does not count as a
// SAN change: that would mint a new CA and client identity.
func TestInvalidPersistedSANIsNotARegenReason(t *testing.T) {
	existing := PersistentConfig{ExtraSANDNS: validSANDNS([]string{"a.example", "root@203.0.113.10"})}
	merged := PersistentConfig{ExtraSANDNS: []string{"a.example"}}
	if !sameStrings(merged.ExtraSANDNS, existing.ExtraSANDNS) {
		t.Fatalf("existing after filter = %v, want [a.example]", existing.ExtraSANDNS)
	}
}

// --- firewall reload prerequisite ---

type fakeCmd struct {
	out string
	err error
}

type fakeCall struct {
	name string
	args []string
}

func newFakeRunner(t *testing.T, calls *[]fakeCall, responses map[string]fakeCmd) commandRunner {
	t.Helper()
	return func(name string, args ...string) ([]byte, error) {
		*calls = append(*calls, fakeCall{name: name, args: args})
		key := strings.Join(append([]string{name}, args...), " ")
		r, ok := responses[key]
		if !ok {
			t.Fatalf("unexpected command: %s", key)
		}
		return []byte(r.out), r.err
	}
}

func TestConfigureFirewallReload(t *testing.T) {
	const (
		firewalld = "systemctl is-active --quiet firewalld.service"
		listUnits = "systemctl list-unit-files --no-legend netavark-firewalld-reload.service"
		enable    = "systemctl enable --now netavark-firewalld-reload.service"
	)
	notActive := fakeCmd{out: "inactive\n", err: fmt.Errorf("exit status 3")}

	t.Run("firewalld not active", func(t *testing.T) {
		var calls []fakeCall
		var out bytes.Buffer
		run := newFakeRunner(t, &calls, map[string]fakeCmd{firewalld: notActive})
		configureFirewallReload(run, &out)
		if out.Len() != 0 {
			t.Errorf("output = %q, want nothing", out.String())
		}
		if len(calls) != 1 {
			t.Errorf("calls = %v, want only the firewalld probe", calls)
		}
	})

	t.Run("firewalld unavailable", func(t *testing.T) {
		var calls []fakeCall
		var out bytes.Buffer
		run := newFakeRunner(t, &calls, map[string]fakeCmd{
			firewalld: {err: fmt.Errorf("exec: systemctl: not found")},
		})
		configureFirewallReload(run, &out)
		if out.Len() != 0 {
			t.Errorf("output = %q, want nothing", out.String())
		}
	})

	t.Run("unit not installed", func(t *testing.T) {
		var calls []fakeCall
		var out bytes.Buffer
		run := newFakeRunner(t, &calls, map[string]fakeCmd{
			firewalld: {out: "active\n"},
			listUnits: {},
		})
		configureFirewallReload(run, &out)
		got := out.String()
		for _, want := range []string{
			"Warning: firewalld is active but netavark-firewalld-reload.service is not installed.",
			"firewall-cmd --reload",
			"podman network reload --all",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("output %q should contain %q", got, want)
			}
		}
		for _, c := range calls {
			if strings.Join(append([]string{c.name}, c.args...), " ") == enable {
				t.Error("must not try to enable a unit that is not installed")
			}
		}
	})

	t.Run("enables the service", func(t *testing.T) {
		var calls []fakeCall
		var out bytes.Buffer
		run := newFakeRunner(t, &calls, map[string]fakeCmd{
			firewalld: {out: "active\n"},
			listUnits: {out: "netavark-firewalld-reload.service enabled\n"},
			enable:    {},
		})
		configureFirewallReload(run, &out)
		want := "Enabled netavark-firewalld-reload.service (pod networking survives firewall-cmd --reload).\n"
		if out.String() != want {
			t.Errorf("output = %q, want %q", out.String(), want)
		}
		if len(calls) != 3 {
			t.Errorf("calls = %v, want probe, list and enable", calls)
		}
	})

	t.Run("enable failure is a warning", func(t *testing.T) {
		var calls []fakeCall
		var out bytes.Buffer
		run := newFakeRunner(t, &calls, map[string]fakeCmd{
			firewalld: {out: "active\n"},
			listUnits: {out: "netavark-firewalld-reload.service disabled\n"},
			enable:    {err: fmt.Errorf("unit is masked")},
		})
		configureFirewallReload(run, &out)
		got := out.String()
		for _, want := range []string{
			"Warning: could not enable netavark-firewalld-reload.service",
			"unit is masked",
			"Run it by hand: sudo systemctl enable --now netavark-firewalld-reload.service",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("output %q should contain %q", got, want)
			}
		}
	})

	t.Run("repeated install is harmless", func(t *testing.T) {
		var calls []fakeCall
		var out bytes.Buffer
		run := newFakeRunner(t, &calls, map[string]fakeCmd{
			firewalld: {out: "active\n"},
			listUnits: {out: "netavark-firewalld-reload.service enabled\n"},
			enable:    {},
		})
		configureFirewallReload(run, &out)
		configureFirewallReload(run, &out)
		if strings.Count(out.String(), "Enabled netavark-firewalld-reload.service") != 2 {
			t.Errorf("output = %q, want two success lines", out.String())
		}
		if len(calls) != 6 {
			t.Errorf("calls = %v, want two full runs", calls)
		}
	})

	t.Run("list output for another unit means not installed", func(t *testing.T) {
		var calls []fakeCall
		var out bytes.Buffer
		run := newFakeRunner(t, &calls, map[string]fakeCmd{
			firewalld: {out: "active\n"},
			listUnits: {out: "other.service enabled\n"},
		})
		configureFirewallReload(run, &out)
		if !strings.Contains(out.String(), "is not installed") {
			t.Errorf("output = %q, want a missing-unit warning", out.String())
		}
	})
}

// --- generated systemd service unit ---

func TestServiceUnitPreservesRuntimeDirectory(t *testing.T) {
	cases := []struct {
		name    string
		rootful bool
	}{
		{"rootful", true},
		{"rootless", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/run/user/1000/bus")
			t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
			unit, err := renderServiceUnit(InstallConfig{Rootful: tc.rootful, Home: "/home/tester"}, "/usr/local/bin/q8s")
			if err != nil {
				t.Fatalf("renderServiceUnit: %v", err)
			}
			if !strings.Contains(unit, "RuntimeDirectory=q8s\n") {
				t.Fatalf("unit has no RuntimeDirectory=q8s:\n%s", unit)
			}
			if !strings.Contains(unit, "RuntimeDirectoryPreserve=yes\n") {
				t.Fatalf("unit has no RuntimeDirectoryPreserve=yes:\n%s", unit)
			}
		})
	}
}
