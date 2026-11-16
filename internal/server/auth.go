package server

import (
	"context"
	"crypto/x509"
	"fmt"
	"net/http"
	"strings"

	"q8s/internal/store"
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
	store      *store.Store
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
	return newAuthMiddleware(caCert, revokedFile, nil)
}

// NewAuthMiddlewareWithRevocationAndStore is NewAuthMiddlewareWithRevocation
// plus the store that service account certificates are checked against on
// every request.
func NewAuthMiddlewareWithRevocationAndStore(caCert []byte, revokedFile string, st *store.Store) (*AuthMiddleware, error) {
	return newAuthMiddleware(caCert, revokedFile, st)
}

func newAuthMiddleware(caCert []byte, revokedFile string, st *store.Store) (*AuthMiddleware, error) {
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
		store:      st,
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

		if strings.HasPrefix(cert.Subject.CommonName, serviceAccountUserPrefix) {
			if err := validateServiceAccountCertificate(a.store, cert); err != nil {
				http.Error(w, fmt.Sprintf("invalid service account certificate: %v", err), http.StatusUnauthorized)
				return
			}
		}

		next.ServeHTTP(w, r.WithContext(withUserInfo(r.Context(), userInfoFromCertificate(cert))))
	})
}

// serviceAccountUserPrefix is the Kubernetes user name prefix for a ServiceAccount.
const serviceAccountUserPrefix = "system:serviceaccount:"

// validateServiceAccountCertificate checks a service account certificate
// against live state. The certificate must carry exactly one workload URI
// SAN; that workload must still exist with the same UID, its pod template
// must still name the certificate's ServiceAccount, and the ServiceAccount
// must exist. Nothing is cached: the checks are a few map lookups.
func validateServiceAccountCertificate(st *store.Store, cert *x509.Certificate) error {
	namespace, name, ok := serviceAccountFromUserName(cert.Subject.CommonName)
	if !ok {
		return fmt.Errorf("malformed service account user name %q", cert.Subject.CommonName)
	}

	var binding *workloadRef
	for _, uri := range cert.URIs {
		ref, ok := parseWorkloadURI(uri)
		if !ok {
			continue
		}
		if binding != nil {
			return fmt.Errorf("certificate has more than one workload URI")
		}
		binding = &ref
	}
	if binding == nil {
		return fmt.Errorf("certificate has no workload URI")
	}
	if binding.Namespace != namespace {
		return fmt.Errorf("workload namespace %q does not match service account namespace %q", binding.Namespace, namespace)
	}

	target, err := lookupWorkloadTarget(st, binding.Kind, binding.Namespace, binding.Name)
	if err != nil {
		return err
	}
	if target.Ref.UID != binding.UID {
		return fmt.Errorf("%s %s/%s no longer has the certificate's UID", binding.Kind, binding.Namespace, binding.Name)
	}
	if target.ServiceAccount != name {
		return fmt.Errorf("%s %s/%s does not use service account %q", binding.Kind, binding.Namespace, binding.Name, name)
	}
	if _, err := st.GetServiceAccount(namespace, name); err != nil {
		return fmt.Errorf("serviceaccount %s/%s not found", namespace, name)
	}
	return nil
}

// serviceAccountFromUserName splits system:serviceaccount:<namespace>:<name>.
func serviceAccountFromUserName(userName string) (namespace, name string, ok bool) {
	rest, ok := strings.CutPrefix(userName, serviceAccountUserPrefix)
	if !ok {
		return "", "", false
	}
	namespace, name, ok = strings.Cut(rest, ":")
	if !ok || namespace == "" || name == "" || strings.Contains(name, ":") {
		return "", "", false
	}
	return namespace, name, true
}
