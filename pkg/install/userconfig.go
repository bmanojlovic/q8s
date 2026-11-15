package install

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// MaxIdentityLength is the maximum length of a user name or group, matching
// the Kubernetes limit for a user name. Longer identities are refused rather
// than written into a certificate that clients cannot use reliably.
const MaxIdentityLength = 253

// DefaultClientCertDays is the default client certificate lifetime, the same
// 365 days `q8s install` uses for the admin certificate.
const DefaultClientCertDays = 365

// MaxClientCertDays caps --days so a certificate cannot outlive every
// reasonable operational horizon.
const MaxClientCertDays = 3650

// ValidateIdentity checks a user name or group before it is placed in a
// certificate subject. kind is only used in error messages.
func ValidateIdentity(kind, value string) error {
	if value == "" {
		return fmt.Errorf("%s must not be empty", kind)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s must be valid UTF-8", kind)
	}
	if utf8.RuneCountInString(value) > MaxIdentityLength || len(value) > MaxIdentityLength {
		return fmt.Errorf("%s must be at most %d characters", kind, MaxIdentityLength)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("%s must not contain control characters", kind)
		}
	}
	return nil
}

// ValidateClientCertDays checks the requested certificate lifetime.
func ValidateClientCertDays(days int) error {
	if days <= 0 {
		return fmt.Errorf("--days must be a positive number of days")
	}
	if days > MaxClientCertDays {
		return fmt.Errorf("--days must be at most %d", MaxClientCertDays)
	}
	return nil
}

// ClientCertificate is a freshly issued user certificate and its metadata.
type ClientCertificate struct {
	CertPEM []byte
	KeyPEM  []byte
	Serial  *big.Int
}

// IssueClientCertificate creates a client certificate signed by the q8s CA.
// The certificate follows the Kubernetes identity convention: Subject
// CommonName is the user name and every Subject Organization entry is a
// group. The CA certificate and key are the PEM files written by
// `q8s install`.
func IssueClientCertificate(caCertPEM, caKeyPEM []byte, userName string, groups []string, days int) (*ClientCertificate, error) {
	if err := ValidateClientCertDays(days); err != nil {
		return nil, err
	}
	now := time.Now()
	return IssueClientCertificateAt(caCertPEM, caKeyPEM, userName, groups, now.Add(-time.Minute), now.Add(time.Duration(days)*24*time.Hour))
}

// IssueClientCertificateAt is IssueClientCertificate with an explicit
// validity window. It lets short-lived credentials (the workload socket
// issues one-hour service account certificates) use the same signing path as
// the CLI without rounding through whole days.
func IssueClientCertificateAt(caCertPEM, caKeyPEM []byte, userName string, groups []string, notBefore, notAfter time.Time) (*ClientCertificate, error) {
	if err := ValidateIdentity("user name", userName); err != nil {
		return nil, err
	}
	for _, group := range groups {
		if err := ValidateIdentity("group", group); err != nil {
			return nil, err
		}
	}
	if !notAfter.After(notBefore) {
		return nil, fmt.Errorf("certificate notAfter must be after notBefore")
	}

	caCert, caKey, err := parseCA(caCertPEM, caKeyPEM)
	if err != nil {
		return nil, err
	}

	clientKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate client key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate certificate serial: %w", err)
	}
	if serial.Sign() == 0 {
		serial = big.NewInt(1)
	}

	// Groups are RDN-encoded one per RDN instead of together in one
	// Organization SET. DER sorts the members of a SET, which would otherwise
	// lose the requested group order when the certificate is parsed again.
	subject := pkix.Name{CommonName: userName}
	for _, group := range groups {
		subject.ExtraNames = append(subject.ExtraNames, pkix.AttributeTypeAndValue{
			Type:  asn1.ObjectIdentifier{2, 5, 4, 10}, // organizationName
			Value: group,
		})
	}
	template := x509.Certificate{
		SerialNumber: serial,
		Subject:      subject,
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, caCert, &clientKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("create client certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(clientKey)
	if err != nil {
		return nil, fmt.Errorf("marshal client key: %w", err)
	}

	return &ClientCertificate{
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		Serial:  serial,
	}, nil
}

func parseCA(caCertPEM, caKeyPEM []byte) (*x509.Certificate, any, error) {
	certBlock, _ := pem.Decode(caCertPEM)
	if certBlock == nil {
		return nil, nil, fmt.Errorf("CA certificate is not parseable PEM")
	}
	caCert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA certificate: %w", err)
	}
	if !caCert.IsCA {
		return nil, nil, fmt.Errorf("CA certificate is not a CA")
	}

	keyBlock, _ := pem.Decode(caKeyPEM)
	if keyBlock == nil {
		return nil, nil, fmt.Errorf("CA key is not parseable PEM")
	}
	var caKey any
	switch keyBlock.Type {
	case "EC PRIVATE KEY":
		caKey, err = x509.ParseECPrivateKey(keyBlock.Bytes)
	case "PRIVATE KEY":
		caKey, err = x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	default:
		return nil, nil, fmt.Errorf("unsupported CA key type %q", keyBlock.Type)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA key: %w", err)
	}
	return caCert, caKey, nil
}

// SerialHex renders a certificate serial in the canonical form used by the
// revocation file and the CLI: lowercase hexadecimal without a 0x prefix.
func SerialHex(serial *big.Int) string {
	if serial == nil {
		return ""
	}
	return strings.ToLower(serial.Text(16))
}

// NormalizeSerial accepts the forms an operator is likely to paste and
// returns the canonical lowercase hexadecimal serial.
func NormalizeSerial(serial string) string {
	serial = strings.TrimSpace(serial)
	serial = strings.TrimPrefix(serial, "0x")
	serial = strings.TrimPrefix(serial, "0X")
	return strings.ToLower(serial)
}

// RevokedCertificates is the content of <dataDir>/revoked.json. Users are
// matched against the certificate Subject CommonName and serials against the
// certificate serial number.
type RevokedCertificates struct {
	Users   []string `json:"users"`
	Serials []string `json:"serials"`
}

// HasUser reports whether userName is revoked.
func (r RevokedCertificates) HasUser(userName string) bool {
	for _, user := range r.Users {
		if user == userName {
			return true
		}
	}
	return false
}

// HasSerial reports whether serial is revoked, accepting any accepted serial
// spelling.
func (r RevokedCertificates) HasSerial(serial string) bool {
	want := NormalizeSerial(serial)
	if want == "" {
		return false
	}
	for _, candidate := range r.Serials {
		if NormalizeSerial(candidate) == want {
			return true
		}
	}
	return false
}

// RevokedPath returns the revocation file for a data directory.
func RevokedPath(dataDir string) string {
	return filepath.Join(dataDir, "revoked.json")
}

// LoadRevokedCertificates reads path. A missing file is an empty list, not an
// error: nothing is revoked until the file exists.
func LoadRevokedCertificates(path string) (RevokedCertificates, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return RevokedCertificates{}, nil
		}
		return RevokedCertificates{}, err
	}
	var revoked RevokedCertificates
	if err := json.Unmarshal(data, &revoked); err != nil {
		return RevokedCertificates{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return revoked, nil
}

// SaveRevokedCertificates writes revoked atomically with tmp+rename and mode
// 0600, so a reader never sees a partial list and only the owner can read it.
func SaveRevokedCertificates(path string, revoked RevokedCertificates) error {
	if revoked.Users == nil {
		revoked.Users = []string{}
	}
	if revoked.Serials == nil {
		revoked.Serials = []string{}
	}
	normalized := make([]string, 0, len(revoked.Serials))
	for _, serial := range revoked.Serials {
		if value := NormalizeSerial(serial); value != "" {
			normalized = append(normalized, value)
		}
	}
	revoked.Serials = normalized
	sort.Strings(revoked.Users)
	sort.Strings(revoked.Serials)
	revoked.Users = dedupeStrings(revoked.Users)
	revoked.Serials = dedupeStrings(revoked.Serials)

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".revoked-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	encoder := json.NewEncoder(tmp)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(revoked); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func dedupeStrings(values []string) []string {
	out := values[:0]
	for i, value := range values {
		if i > 0 && value == values[i-1] {
			continue
		}
		out = append(out, value)
	}
	return out
}
