// Package traefik holds the subset of Traefik's CRD types q8s implements.
//
// The field names and JSON tags mirror Traefik v3.6's traefik.io/v1alpha1
// CRDs; only the fields q8s can render into its file provider are present.
// Anything else is rejected by the API's strict decoder instead of being
// silently dropped.
package traefik

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// API group and version served for both kinds.
const (
	Group      = "traefik.io"
	Version    = "v1alpha1"
	APIVersion = Group + "/" + Version
)

// --- Middleware ---

// Middleware is the CRD implementation of a Traefik HTTP middleware.
type Middleware struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec MiddlewareSpec `json:"spec,omitempty"`
}

// MiddlewareSpec holds exactly one middleware configuration. The API rejects
// an object that sets zero or more than one of the supported types.
type MiddlewareSpec struct {
	IPAllowList      *IPAllowList      `json:"ipAllowList,omitempty"`
	RedirectRegex    *RedirectRegex    `json:"redirectRegex,omitempty"`
	RedirectScheme   *RedirectScheme   `json:"redirectScheme,omitempty"`
	Headers          *Headers          `json:"headers,omitempty"`
	ReplacePath      *ReplacePath      `json:"replacePath,omitempty"`
	ReplacePathRegex *ReplacePathRegex `json:"replacePathRegex,omitempty"`
	StripPrefix      *StripPrefix      `json:"stripPrefix,omitempty"`
	StripPrefixRegex *StripPrefixRegex `json:"stripPrefixRegex,omitempty"`
	AddPrefix        *AddPrefix        `json:"addPrefix,omitempty"`
}

// IPAllowList limits allowed requests based on the client IP.
type IPAllowList struct {
	SourceRange      []string    `json:"sourceRange,omitempty"`
	IPStrategy       *IPStrategy `json:"ipStrategy,omitempty"`
	RejectStatusCode int         `json:"rejectStatusCode,omitempty"`
}

// IPStrategy tells Traefik how to determine the client IP.
type IPStrategy struct {
	Depth       int      `json:"depth,omitempty"`
	ExcludedIPs []string `json:"excludedIPs,omitempty"`
}

// RedirectRegex redirects a request using regex matching and replacement.
type RedirectRegex struct {
	Regex       string `json:"regex,omitempty"`
	Replacement string `json:"replacement,omitempty"`
	Permanent   bool   `json:"permanent,omitempty"`
}

// RedirectScheme redirects requests from one scheme/port to another.
type RedirectScheme struct {
	Scheme    string `json:"scheme,omitempty"`
	Port      string `json:"port,omitempty"`
	Permanent bool   `json:"permanent,omitempty"`
}

// ReplacePath replaces the request path with a fixed path.
type ReplacePath struct {
	Path string `json:"path,omitempty"`
}

// ReplacePathRegex replaces the request path using regex matching and
// replacement.
type ReplacePathRegex struct {
	Regex       string `json:"regex,omitempty"`
	Replacement string `json:"replacement,omitempty"`
}

// StripPrefix removes matching path prefixes from the request.
type StripPrefix struct {
	Prefixes []string `json:"prefixes,omitempty"`
}

// StripPrefixRegex removes path prefixes matching any of the regexes.
type StripPrefixRegex struct {
	Regex []string `json:"regex,omitempty"`
}

// AddPrefix prepends a path prefix to the request.
type AddPrefix struct {
	Prefix string `json:"prefix,omitempty"`
}

// Headers manages request and response headers.
type Headers struct {
	CustomRequestHeaders    map[string]string `json:"customRequestHeaders,omitempty"`
	CustomResponseHeaders   map[string]string `json:"customResponseHeaders,omitempty"`
	STSSeconds              int64             `json:"stsSeconds,omitempty"`
	STSIncludeSubdomains    bool              `json:"stsIncludeSubdomains,omitempty"`
	STSPreload              bool              `json:"stsPreload,omitempty"`
	ForceSTSHeader          bool              `json:"forceSTSHeader,omitempty"`
	FrameDeny               bool              `json:"frameDeny,omitempty"`
	ContentTypeNosniff      bool              `json:"contentTypeNosniff,omitempty"`
	BrowserXssFilter        bool              `json:"browserXssFilter,omitempty"`
	ReferrerPolicy          string            `json:"referrerPolicy,omitempty"`
	ContentSecurityPolicy   string            `json:"contentSecurityPolicy,omitempty"`
	PermissionsPolicy       string            `json:"permissionsPolicy,omitempty"`
	CustomFrameOptionsValue string            `json:"customFrameOptionsValue,omitempty"`
}

// MiddlewareList is the list kind of Middleware.
type MiddlewareList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Middleware `json:"items"`
}

// --- IngressRouteTCP ---

// IngressRouteTCP is the CRD implementation of a Traefik TCP router.
type IngressRouteTCP struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec IngressRouteTCPSpec `json:"spec,omitempty"`
}

// IngressRouteTCPSpec is the desired state of an IngressRouteTCP.
type IngressRouteTCPSpec struct {
	EntryPoints []string   `json:"entryPoints,omitempty"`
	Routes      []RouteTCP `json:"routes,omitempty"`
	TLS         *TLS       `json:"tls,omitempty"`
}

// RouteTCP holds the TCP route configuration.
type RouteTCP struct {
	Match    string       `json:"match,omitempty"`
	Services []ServiceTCP `json:"services,omitempty"`
}

// ServiceTCP defines an upstream TCP service to proxy traffic to.
type ServiceTCP struct {
	Name          string             `json:"name,omitempty"`
	Port          intstr.IntOrString `json:"port,omitempty"`
	ProxyProtocol *ProxyProtocol     `json:"proxyProtocol,omitempty"`
}

// ProxyProtocol configures the PROXY protocol version sent to the backend.
type ProxyProtocol struct {
	Version int `json:"version,omitempty"`
}

// TLS defines the TLS configuration on a TCP route.
type TLS struct {
	Passthrough bool `json:"passthrough,omitempty"`
}

// IngressRouteTCPList is the list kind of IngressRouteTCP.
type IngressRouteTCPList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []IngressRouteTCP `json:"items"`
}
