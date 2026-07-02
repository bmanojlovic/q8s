package server

import (
	"fmt"
	"regexp"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
)

// nameRE matches RFC 1123 DNS label format, the same rule real k8s enforces
// on object names/namespaces (lowercase alphanumeric or '-', must start and
// end with an alphanumeric character).
//
// This isn't just k8s-compatibility pedantry: Namespace/Name get spliced
// directly into filesystem paths (ConfigMap/Secret file trees under
// ConfigDir/SecretDir) and podman/systemd command arguments (container
// names, quadlet filenames) all over this package, without a shell in
// between. An unvalidated name containing "../" is a path traversal into
// arbitrary-file-write; one starting with "-" is argument injection into
// whatever podman/systemctl command receives it. Rejecting anything that
// isn't a safe identifier at the API boundary closes the whole class at
// once, instead of trying to sanitize every individual call site downstream.
var nameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func validateName(kind, value string) error {
	if len(value) == 0 || len(value) > 253 {
		return fmt.Errorf("%s must be between 1 and 253 characters", kind)
	}
	if !nameRE.MatchString(value) {
		return fmt.Errorf("%s %q is invalid: a lowercase RFC 1123 label must consist of lowercase alphanumeric characters or '-', and must start and end with an alphanumeric character", kind, value)
	}
	return nil
}

// dataKeyRE matches the key format real k8s allows for ConfigMap/Secret data
// entries. These keys become filenames under ConfigDir/SecretDir the same
// way Namespace/Name do, and — unlike Namespace/Name — they can be
// introduced or changed via a JSON merge patch on an already-valid object,
// so they need their own check wherever Data/BinaryData/StringData is
// written, not just at create time.
var dataKeyRE = regexp.MustCompile(`^[-._a-zA-Z0-9]+$`)

func validateDataKey(key string) error {
	if len(key) == 0 || len(key) > 253 || key == "." || key == ".." {
		return fmt.Errorf("key %q is invalid", key)
	}
	if !dataKeyRE.MatchString(key) {
		return fmt.Errorf("key %q is invalid: must consist of alphanumeric characters, '-', '_' or '.'", key)
	}
	return nil
}

func validateDataKeys(data map[string]string, binaryData map[string][]byte) error {
	for k := range data {
		if err := validateDataKey(k); err != nil {
			return err
		}
	}
	for k := range binaryData {
		if err := validateDataKey(k); err != nil {
			return err
		}
	}
	return nil
}

// hostRE matches an RFC 1123 DNS subdomain (dot-separated labels), optionally
// prefixed with a single wildcard label ("*.example.com") — the host format
// Ingress rules and TLS entries accept in real k8s. Unlike nameRE this is
// deliberately multi-label: object names are a single DNS label, but a Host
// is a full domain name.
var hostRE = regexp.MustCompile(`^(\*\.)?[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

func validateHost(value string) error {
	if len(value) == 0 || len(value) > 253 {
		return fmt.Errorf("host must be between 1 and 253 characters")
	}
	if !hostRE.MatchString(value) {
		return fmt.Errorf("host %q is invalid", value)
	}
	return nil
}

// ingressPathRE rejects control characters/newlines in an Ingress path.
// Those values get spliced into the generated Traefik dynamic-config file,
// so an unescaped newline is the same injection shape as the quadlet bug:
// it can break out of its field and add attacker-controlled config blocks.
var ingressPathRE = regexp.MustCompile(`^/[^\x00-\x1f]*$`)

func validateIngressPath(value string) error {
	if value == "" {
		return nil
	}
	if !ingressPathRE.MatchString(value) {
		return fmt.Errorf("path %q is invalid: must start with '/' and contain no control characters", value)
	}
	for _, seg := range strings.Split(value, "/") {
		if seg == ".." || seg == "." {
			return fmt.Errorf("path %q is invalid: '.' and '..' segments are not allowed", value)
		}
	}
	return nil
}

func validatePathType(pt string) error {
	switch pt {
	case "Exact", "Prefix", "ImplementationSpecific":
		return nil
	default:
		return fmt.Errorf("pathType %q is invalid", pt)
	}
}

func validatePort(port int32) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("port %d is invalid: must be between 1 and 65535", port)
	}
	return nil
}

// validateIngress checks every user-controlled string field that ends up in
// the generated Traefik dynamic config or references another object by name.
func validateIngress(ing *networkingv1.Ingress) error {
	for _, rule := range ing.Spec.Rules {
		if rule.Host != "" {
			if err := validateHost(rule.Host); err != nil {
				return err
			}
		}
		if rule.HTTP == nil {
			continue
		}
		for _, p := range rule.HTTP.Paths {
			if err := validateIngressPath(p.Path); err != nil {
				return err
			}
			if p.PathType != nil {
				if err := validatePathType(string(*p.PathType)); err != nil {
					return err
				}
			}
			if p.Backend.Service != nil {
				if err := validateName("backend service name", p.Backend.Service.Name); err != nil {
					return err
				}
				if p.Backend.Service.Port.Number != 0 {
					if err := validatePort(p.Backend.Service.Port.Number); err != nil {
						return err
					}
				}
			}
		}
	}
	for _, tls := range ing.Spec.TLS {
		for _, h := range tls.Hosts {
			if err := validateHost(h); err != nil {
				return err
			}
		}
		if tls.SecretName != "" {
			if err := validateName("tls secretName", tls.SecretName); err != nil {
				return err
			}
		}
	}
	return nil
}
