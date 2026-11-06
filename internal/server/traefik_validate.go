package server

import (
	"fmt"
	"net"
	"regexp"

	"k8s.io/apimachinery/pkg/util/intstr"

	"q8s/internal/traefik"
)

// httpTokenRE matches an HTTP header field name: RFC 7230 tchar.
var httpTokenRE = regexp.MustCompile("^[!#$%&'*+\\-.^_`|~0-9A-Za-z]+$")

// tcpCatchAllMatch is the only IngressRouteTCP rule q8s implements. A
// catch-all HostSNI router owns its entrypoint, which is why entrypoint
// conflicts between IngressRouteTCP objects are rejected.
const tcpCatchAllMatch = "HostSNI(`*`)"

// validateMiddleware checks the supported Middleware subset. The API calls it
// on create, update, patch and apply, so it is the single choke point.
func validateMiddleware(mw *traefik.Middleware) error {
	set := 0
	if mw.Spec.IPAllowList != nil {
		set++
	}
	if mw.Spec.RedirectRegex != nil {
		set++
	}
	if mw.Spec.RedirectScheme != nil {
		set++
	}
	if mw.Spec.Headers != nil {
		set++
	}
	if set != 1 {
		return fmt.Errorf("spec must set exactly one of ipAllowList, redirectRegex, redirectScheme or headers, got %d", set)
	}

	switch {
	case mw.Spec.IPAllowList != nil:
		return validateIPAllowList(mw.Spec.IPAllowList)
	case mw.Spec.RedirectRegex != nil:
		return validateRedirectRegex(mw.Spec.RedirectRegex)
	case mw.Spec.RedirectScheme != nil:
		return validateRedirectScheme(mw.Spec.RedirectScheme)
	default:
		return validateHeaders(mw.Spec.Headers)
	}
}

func validateIPAllowList(list *traefik.IPAllowList) error {
	// An allowlist with no sources is almost certainly a mistake in the
	// manifest (an unset variable), and what Traefik does with it is not
	// what anyone protecting an admin UI wants to rely on.
	if len(list.SourceRange) == 0 {
		return fmt.Errorf("spec.ipAllowList.sourceRange must list at least one IP or CIDR")
	}
	for _, source := range list.SourceRange {
		if err := validateIPOrCIDR("sourceRange", source); err != nil {
			return err
		}
	}
	if list.IPStrategy != nil {
		if list.IPStrategy.Depth < 0 {
			return fmt.Errorf("spec.ipAllowList.ipStrategy.depth %d is invalid: must be >= 0", list.IPStrategy.Depth)
		}
		for _, ip := range list.IPStrategy.ExcludedIPs {
			if err := validateIPOrCIDR("ipStrategy.excludedIPs", ip); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateIPOrCIDR(field, value string) error {
	if value == "" {
		return fmt.Errorf("spec.%s contains an empty IP or CIDR", field)
	}
	if net.ParseIP(value) != nil {
		return nil
	}
	if _, _, err := net.ParseCIDR(value); err != nil {
		return fmt.Errorf("spec.%s value %q is invalid: must be an IP address or CIDR", field, value)
	}
	return nil
}

func validateRedirectRegex(redirect *traefik.RedirectRegex) error {
	if redirect.Regex == "" {
		return fmt.Errorf("spec.redirectRegex.regex is required")
	}
	if _, err := regexp.Compile(redirect.Regex); err != nil {
		return fmt.Errorf("spec.redirectRegex.regex %q is invalid: %s", redirect.Regex, err.Error())
	}
	return nil
}

func validateRedirectScheme(redirect *traefik.RedirectScheme) error {
	if redirect.Scheme != "http" && redirect.Scheme != "https" {
		return fmt.Errorf("spec.redirectScheme.scheme %q is invalid: must be http or https", redirect.Scheme)
	}
	if redirect.Port != "" {
		for _, r := range redirect.Port {
			if r < '0' || r > '9' {
				return fmt.Errorf("spec.redirectScheme.port %q is invalid: must contain digits only", redirect.Port)
			}
		}
	}
	return nil
}

func validateHeaders(headers *traefik.Headers) error {
	for field, values := range map[string]map[string]string{
		"customRequestHeaders":  headers.CustomRequestHeaders,
		"customResponseHeaders": headers.CustomResponseHeaders,
	} {
		for name, value := range values {
			if !httpTokenRE.MatchString(name) {
				return fmt.Errorf("spec.headers.%s name %q is invalid: must be an HTTP header token", field, name)
			}
			if containsCRLFOrNUL(value) {
				return fmt.Errorf("spec.headers.%s[%q] value contains a CR, LF or NUL character", field, name)
			}
		}
	}
	if headers.STSSeconds < 0 {
		return fmt.Errorf("spec.headers.stsSeconds %d is invalid: must be >= 0", headers.STSSeconds)
	}
	return nil
}

func containsCRLFOrNUL(value string) bool {
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '\r', '\n', 0:
			return true
		}
	}
	return false
}

// validateIngressRouteTCP checks the supported IngressRouteTCP subset.
func validateIngressRouteTCP(irt *traefik.IngressRouteTCP) error {
	if len(irt.Spec.EntryPoints) == 0 {
		return fmt.Errorf("spec.entryPoints is required and must contain at least one entrypoint")
	}
	for _, entryPoint := range irt.Spec.EntryPoints {
		if !traefikNameRE.MatchString(entryPoint) {
			return fmt.Errorf("spec.entryPoints value %q is invalid: must start with an alphanumeric character and contain only alphanumeric characters, '.', '_' or '-'", entryPoint)
		}
	}
	if len(irt.Spec.Routes) == 0 {
		return fmt.Errorf("spec.routes is required and must contain at least one route")
	}
	for i, route := range irt.Spec.Routes {
		if route.Match != tcpCatchAllMatch {
			return fmt.Errorf("spec.routes[%d].match %q is not supported: only the catch-all HostSNI(`*`) matcher is supported on an IngressRouteTCP", i, route.Match)
		}
		if len(route.Services) != 1 {
			return fmt.Errorf("spec.routes[%d].services must contain exactly one service, got %d", i, len(route.Services))
		}
		svc := route.Services[0]
		if err := validateName("service name", svc.Name); err != nil {
			return fmt.Errorf("spec.routes[%d].services[0].name: %w", i, err)
		}
		switch svc.Port.Type {
		case intstr.String:
			if err := validateName("service port name", svc.Port.StrVal); err != nil {
				return fmt.Errorf("spec.routes[%d].services[0].port: %w", i, err)
			}
		default:
			if err := validatePort(svc.Port.IntVal); err != nil {
				return fmt.Errorf("spec.routes[%d].services[0].port: %w", i, err)
			}
		}
		if svc.ProxyProtocol != nil && svc.ProxyProtocol.Version != 1 && svc.ProxyProtocol.Version != 2 {
			return fmt.Errorf("spec.routes[%d].services[0].proxyProtocol.version %d is invalid: must be 1 or 2", i, svc.ProxyProtocol.Version)
		}
	}
	return nil
}

// ingressRouteTCPConflict returns an error when another IngressRouteTCP (in any
// namespace) already owns one of obj's entrypoints. A catch-all HostSNI route
// occupies the whole entrypoint, so two of them would silently shadow each
// other in Traefik's TCP router table.
func (s *Server) ingressRouteTCPConflict(obj *traefik.IngressRouteTCP) error {
	for _, other := range s.config.Store.AllIngressRouteTCPs() {
		if other.Namespace == obj.Namespace && other.Name == obj.Name {
			continue
		}
		for _, entryPoint := range obj.Spec.EntryPoints {
			for _, used := range other.Spec.EntryPoints {
				if entryPoint == used {
					return fmt.Errorf("entryPoint %q is already used by IngressRouteTCP %s/%s", entryPoint, other.Namespace, other.Name)
				}
			}
		}
	}
	return nil
}
