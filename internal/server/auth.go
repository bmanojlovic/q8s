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
// If caCert is nil or empty, auth is disabled (development mode).
func NewAuthMiddleware(caCert []byte) *AuthMiddleware {
	if len(caCert) == 0 {
		return &AuthMiddleware{enabled: false}
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caCert) {
		return &AuthMiddleware{enabled: false}
	}

	return &AuthMiddleware{
		caCertPool: pool,
		enabled:    true,
	}
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
