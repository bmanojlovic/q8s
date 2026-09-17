package server

import (
	"crypto/x509"
	"fmt"
	"net/http"
)

// AuthMiddleware validates client X.509 certificates against a trusted CA.
type AuthMiddleware struct {
	caCertPool *x509.CertPool
	enabled    bool
}

// NewAuthMiddleware creates an auth middleware with the given CA cert PEM data.
// If caCert is nil or empty, auth is disabled (development mode). A non-empty
// caCert that does not parse as a certificate is an error: silently disabling
// client-cert verification because the CA file was truncated or corrupt would
// fail open — the opposite of what an mTLS deployment expects.
func NewAuthMiddleware(caCert []byte) (*AuthMiddleware, error) {
	if len(caCert) == 0 {
		return &AuthMiddleware{enabled: false}, nil
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("CA certificate is not parseable PEM")
	}

	return &AuthMiddleware{
		caCertPool: pool,
		enabled:    true,
	}, nil
}

// Handler returns an HTTP middleware that validates client certificates.
func (a *AuthMiddleware) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.enabled {
			next.ServeHTTP(w, r)
			return
		}

		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			http.Error(w, "client certificate required", http.StatusUnauthorized)
			return
		}

		cert := r.TLS.PeerCertificates[0]
		_, err := cert.Verify(x509.VerifyOptions{
			Roots:     a.caCertPool,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		})
		if err != nil {
			http.Error(w, fmt.Sprintf("invalid client certificate: %v", err), http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}
