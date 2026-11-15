package server

import (
	"context"
	"crypto/x509"
	"fmt"
	"net/http"
)

// userInfo is the authenticated identity of a request, following the
// Kubernetes convention: the certificate Subject CommonName is the user name
// and every Subject Organization entry is a group.
type userInfo struct {
	Name   string
	Groups []string
}

// userInfoKey is the unexported context key holding a userInfo.
type userInfoKey struct{}

// withUserInfo stores u in ctx.
func withUserInfo(ctx context.Context, u userInfo) context.Context {
	return context.WithValue(ctx, userInfoKey{}, u)
}

// userFromContext returns the authenticated user stored in ctx.
func userFromContext(ctx context.Context) (userInfo, bool) {
	u, ok := ctx.Value(userInfoKey{}).(userInfo)
	return u, ok
}

// isAdmin reports whether u bypasses authorization entirely. system:masters is
// the Kubernetes cluster-admin group; q8s-user is the group the certificate
// `q8s install` has always issued.
func isAdmin(u userInfo) bool {
	for _, group := range u.Groups {
		if group == "system:masters" || group == "q8s-user" {
			return true
		}
	}
	return false
}

// userInfoFromCertificate builds the identity of a verified client
// certificate. Every authenticated user is a member of system:authenticated.
func userInfoFromCertificate(cert *x509.Certificate) userInfo {
	groups := make([]string, 0, len(cert.Subject.Organization)+1)
	for _, org := range cert.Subject.Organization {
		if org != "" {
			groups = append(groups, org)
		}
	}
	groups = append(groups, "system:authenticated")
	return userInfo{Name: cert.Subject.CommonName, Groups: groups}
}

// AuthMiddleware validates client X.509 certificates against a trusted CA.
type AuthMiddleware struct {
	caCertPool *x509.CertPool
	enabled    bool
	revoked    *revokedCache
}

// NewAuthMiddleware creates an auth middleware with the given CA cert PEM data.
// If caCert is nil or empty, auth is disabled (development mode). A non-empty
// caCert that does not parse as a certificate is an error: silently disabling
// client-cert verification because the CA file was truncated or corrupt would
// fail open — the opposite of what an mTLS deployment expects.
func NewAuthMiddleware(caCert []byte) (*AuthMiddleware, error) {
	return NewAuthMiddlewareWithRevocation(caCert, "")
}

// NewAuthMiddlewareWithRevocation is NewAuthMiddleware plus certificate
// revocation from revokedFile. An empty revokedFile disables revocation.
func NewAuthMiddlewareWithRevocation(caCert []byte, revokedFile string) (*AuthMiddleware, error) {
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
		revoked:    newRevokedCache(revokedFile),
	}, nil
}

// Handler returns an HTTP middleware that validates client certificates and
// stores the authenticated identity in the request context.
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

		if a.revoked.isRevoked(cert) {
			http.Error(w, "client certificate revoked", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r.WithContext(withUserInfo(r.Context(), userInfoFromCertificate(cert))))
	})
}
