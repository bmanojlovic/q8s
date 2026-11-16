package install

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/url"
	"os"
	"testing"
	"time"
)

func testCA(t *testing.T) certs {
	t.Helper()
	c, err := generateCerts(nil, nil)
	if err != nil {
		t.Fatalf("generateCerts: %v", err)
	}
	return c
}

func TestIssueClientCertificateIdentityAndExpiry(t *testing.T) {
	ca := testCA(t)
	before := time.Now()
	issued, err := IssueClientCertificate(ca.caCert, ca.caKey, "alice", []string{"developers", "team-a"}, 30)
	if err != nil {
		t.Fatalf("IssueClientCertificate: %v", err)
	}

	block, _ := pem.Decode(issued.CertPEM)
	if block == nil {
		t.Fatal("certificate is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	if cert.Subject.CommonName != "alice" {
		t.Errorf("CN = %q, want alice", cert.Subject.CommonName)
	}
	wantGroups := []string{"developers", "team-a"}
	if len(cert.Subject.Organization) != len(wantGroups) {
		t.Fatalf("O = %v, want %v", cert.Subject.Organization, wantGroups)
	}
	for i := range wantGroups {
		if cert.Subject.Organization[i] != wantGroups[i] {
			t.Errorf("O[%d] = %q, want %q", i, cert.Subject.Organization[i], wantGroups[i])
		}
	}
	if cert.KeyUsage != x509.KeyUsageDigitalSignature {
		t.Errorf("KeyUsage = %v, want DigitalSignature", cert.KeyUsage)
	}
	if len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Errorf("ExtKeyUsage = %v, want [ClientAuth]", cert.ExtKeyUsage)
	}
	if cert.SerialNumber == nil || cert.SerialNumber.Sign() <= 0 {
		t.Errorf("SerialNumber = %v, want positive", cert.SerialNumber)
	}
	if installSerial := SerialHex(issued.Serial); installSerial != SerialHex(cert.SerialNumber) {
		t.Errorf("reported serial %q != certificate serial %q", installSerial, SerialHex(cert.SerialNumber))
	}
	if cert.NotBefore.After(before) || cert.NotBefore.Before(before.Add(-2*time.Minute)) {
		t.Errorf("NotBefore = %v, want about one minute before %v", cert.NotBefore, before)
	}
	wantNotAfter := before.Add(30 * 24 * time.Hour)
	if diff := cert.NotAfter.Sub(wantNotAfter); diff < -time.Minute || diff > time.Minute {
		t.Errorf("NotAfter = %v, want about %v", cert.NotAfter, wantNotAfter)
	}

	caBlock, _ := pem.Decode(ca.caCert)
	caCert, _ := x509.ParseCertificate(caBlock.Bytes)
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Fatalf("certificate does not verify against the CA: %v", err)
	}

	keyBlock, _ := pem.Decode(issued.KeyPEM)
	if keyBlock == nil {
		t.Fatal("key is not PEM")
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		t.Fatalf("parse key: %v", err)
	}
	certKey, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !certKey.Equal(&key.PublicKey) {
		t.Fatal("certificate public key does not match the generated key")
	}
}

func TestIssueClientCertificateRejectsInvalidInput(t *testing.T) {
	ca := testCA(t)
	cases := []struct {
		name   string
		user   string
		groups []string
		days   int
	}{
		{"empty user", "", nil, 365},
		{"control character in user", "ali\nce", nil, 365},
		{"control character in group", "alice", []string{"team\n"}, 365},
		{"too long user", string(make([]byte, 254)), nil, 365},
		{"zero days", "alice", nil, 0},
		{"negative days", "alice", nil, -1},
		{"too many days", "alice", nil, MaxClientCertDays + 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := IssueClientCertificate(ca.caCert, ca.caKey, tc.user, tc.groups, tc.days); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestValidateIdentity(t *testing.T) {
	if err := ValidateIdentity("user name", "alice@example.com"); err != nil {
		t.Errorf("valid name rejected: %v", err)
	}
	long := ""
	for len(long) < MaxIdentityLength {
		long += "a"
	}
	if err := ValidateIdentity("user name", long); err != nil {
		t.Errorf("%d character name rejected: %v", len(long), err)
	}
	if err := ValidateIdentity("user name", long+"a"); err == nil {
		t.Error("over-long name accepted")
	}
	for _, invalid := range []string{"", "a\x00b", "a\x1fb", "a\x7fb"} {
		if err := ValidateIdentity("group", invalid); err == nil {
			t.Errorf("invalid identity %q accepted", invalid)
		}
	}
}

func TestRevokedCertificatesRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := RevokedPath(dir)

	missing, err := LoadRevokedCertificates(path)
	if err != nil {
		t.Fatalf("missing file: %v", err)
	}
	if len(missing.Users) != 0 || len(missing.Serials) != 0 {
		t.Fatalf("missing file = %+v, want empty", missing)
	}

	if err := SaveRevokedCertificates(path, RevokedCertificates{
		Users:   []string{"alice", "alice", "bob"},
		Serials: []string{"0xAB", "ab", "cd"},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("mode = %o, want 0600", info.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("directory has %d entries, want only revoked.json: %v", len(entries), entries)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw RevokedCertificates
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("revoked.json is not valid JSON: %v", err)
	}
	if len(raw.Users) != 2 || len(raw.Serials) != 2 {
		t.Fatalf("dedupe failed: %+v", raw)
	}

	loaded, err := LoadRevokedCertificates(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !loaded.HasUser("bob") || loaded.HasUser("carol") {
		t.Errorf("HasUser wrong for %+v", loaded.Users)
	}
	if !loaded.HasSerial("0xAB") || !loaded.HasSerial("CD") || loaded.HasSerial("ef") {
		t.Errorf("HasSerial wrong for %+v", loaded.Serials)
	}

	if err := os.WriteFile(path, []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRevokedCertificates(path); err == nil {
		t.Error("corrupt file parsed without an error")
	}
}

func TestNormalizeSerial(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"0xABCDEF", "abcdef"},
		{" 0Xabcdef ", "abcdef"},
		{"ABCDEF", "abcdef"},
		{"abcdef", "abcdef"},
		{"", ""},
	} {
		if got := NormalizeSerial(tc.in); got != tc.want {
			t.Errorf("NormalizeSerial(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestIssueClientCertificateAtUsesTheGivenWindow(t *testing.T) {
	ca := testCA(t)
	notBefore := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	notAfter := notBefore.Add(time.Hour)
	issued, err := IssueClientCertificateAt(ca.caCert, ca.caKey, "system:serviceaccount:demo:worker", []string{"system:serviceaccounts"}, notBefore, notAfter)
	if err != nil {
		t.Fatalf("IssueClientCertificateAt: %v", err)
	}
	block, _ := pem.Decode(issued.CertPEM)
	if block == nil {
		t.Fatal("certificate is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !cert.NotBefore.Equal(notBefore) || !cert.NotAfter.Equal(notAfter) {
		t.Fatalf("validity = %v .. %v, want %v .. %v", cert.NotBefore, cert.NotAfter, notBefore, notAfter)
	}
	if _, err := IssueClientCertificateAt(ca.caCert, ca.caKey, "alice", nil, notAfter, notBefore); err == nil {
		t.Fatal("an inverted validity window was accepted")
	}
}

func TestIssueClientCertificateWithURIsAt(t *testing.T) {
	ca := testCA(t)
	notBefore := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	notAfter := notBefore.Add(time.Hour)
	uri, err := url.Parse("urn:q8s:workload:pod:demo/web/1234")
	if err != nil {
		t.Fatal(err)
	}
	issued, err := IssueClientCertificateWithURIsAt(
		ca.caCert,
		ca.caKey,
		"system:serviceaccount:demo:worker",
		[]string{"system:serviceaccounts"},
		[]*url.URL{uri},
		notBefore,
		notAfter,
	)
	if err != nil {
		t.Fatalf("IssueClientCertificateWithURIsAt: %v", err)
	}
	block, _ := pem.Decode(issued.CertPEM)
	if block == nil {
		t.Fatal("certificate is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(cert.URIs) != 1 || cert.URIs[0].String() != uri.String() {
		t.Fatalf("URIs = %v, want [%s]", cert.URIs, uri)
	}
}
