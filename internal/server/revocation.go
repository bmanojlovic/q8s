package server

import (
	"crypto/x509"
	"log"
	"os"
	"sync"
	"time"

	"q8s/pkg/install"
)

// revokedCache caches the content of revoked.json. The file is re-read only
// when its mtime or size changes, which keeps the per-request cost to one
// stat call. A missing file means nothing is revoked; a corrupt file keeps
// the last good list and logs once so a single broken edit cannot silently
// end every revocation.
type revokedCache struct {
	mu          sync.Mutex
	path        string
	initialized bool
	modTime     time.Time
	size        int64
	revoked     install.RevokedCertificates
	logged      bool
}

func newRevokedCache(path string) *revokedCache {
	if path == "" {
		return nil
	}
	return &revokedCache{path: path}
}

// isRevoked reports whether cert belongs to a revoked user or carries a
// revoked serial. It is safe for concurrent use.
func (c *revokedCache) isRevoked(cert *x509.Certificate) bool {
	if c == nil || cert == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	info, err := os.Stat(c.path)
	if err != nil {
		if os.IsNotExist(err) {
			// The file is gone: nothing is revoked. Forget the cached stat
			// so a recreated file is read again even with the same mtime and
			// size as the old one.
			c.initialized = false
			c.revoked = install.RevokedCertificates{}
			c.logged = false
			return false
		}
		// A transient stat error is not a reason to drop the last good list.
		return c.matches(cert)
	}

	if c.initialized && info.ModTime().Equal(c.modTime) && info.Size() == c.size {
		return c.matches(cert)
	}

	c.initialized = true
	c.modTime = info.ModTime()
	c.size = info.Size()

	revoked, err := install.LoadRevokedCertificates(c.path)
	if err != nil {
		if !c.logged {
			log.Printf("warning: ignoring unparseable revocation file: %v", err)
			c.logged = true
		}
		return c.matches(cert)
	}
	c.revoked = revoked
	c.logged = false
	return c.matches(cert)
}

func (c *revokedCache) matches(cert *x509.Certificate) bool {
	if c.revoked.HasUser(cert.Subject.CommonName) {
		return true
	}
	return c.revoked.HasSerial(install.SerialHex(cert.SerialNumber))
}
