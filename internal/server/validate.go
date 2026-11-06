package server

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"q8s/internal/quadlet"
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
// Backtick (0x60) and double quote (0x22) are rejected for the same reason:
// the path is interpolated into a Traefik rule expression (PathPrefix(`…`))
// and into a YAML double-quoted scalar — either character terminates its
// enclosing context and injects rule/config syntax.
var ingressPathRE = regexp.MustCompile(`^/[^\x00-\x1f\x22\x60]*$`)

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

// Traefik's own router annotations, read by its KubernetesIngress provider.
// q8s honors the same keys with the same meaning so an Ingress manifest
// written for real k8s + Traefik works here unchanged; other
// traefik.ingress.kubernetes.io/* annotations are deliberately ignored.
const (
	annRouterEntryPoints     = "traefik.ingress.kubernetes.io/router.entrypoints"
	annRouterMiddlewares     = "traefik.ingress.kubernetes.io/router.middlewares"
	annRouterTLS             = "traefik.ingress.kubernetes.io/router.tls"
	annRouterTLSCertResolver = "traefik.ingress.kubernetes.io/router.tls.certresolver"
	annRouterPriority        = "traefik.ingress.kubernetes.io/router.priority"
)

// traefikNameRE matches an entrypoint, certresolver or middleware name;
// traefikProviderRE matches the provider suffix of a middleware reference.
// Both values are spliced into the generated YAML, so anything that could
// terminate a quoted scalar is rejected here.
var (
	traefikNameRE     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	traefikProviderRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
)

func validateTraefikName(annotation, value string) error {
	if len(value) == 0 || len(value) > 253 {
		return fmt.Errorf("annotation %s: value must be between 1 and 253 characters", annotation)
	}
	if !traefikNameRE.MatchString(value) {
		return fmt.Errorf("annotation %s: value %q is invalid: must start with an alphanumeric character and contain only alphanumeric characters, '.', '_' or '-'", annotation, value)
	}
	return nil
}

func validateTraefikMiddlewareRef(annotation, value string) error {
	if len(value) == 0 || len(value) > 253 {
		return fmt.Errorf("annotation %s: middleware reference must be between 1 and 253 characters", annotation)
	}
	name, provider := value, ""
	hasProvider := false
	if i := strings.IndexByte(value, '@'); i >= 0 {
		name, provider = value[:i], value[i+1:]
		hasProvider = true
	}
	if !traefikNameRE.MatchString(name) || (hasProvider && !traefikProviderRE.MatchString(provider)) {
		return fmt.Errorf("annotation %s: middleware reference %q is invalid: expected <name> or <name>@<provider>", annotation, value)
	}
	return nil
}

// splitAnnotationList splits a comma-separated annotation value the way
// Traefik does: surrounding spaces are trimmed and empty items are dropped.
func splitAnnotationList(value string) []string {
	var out []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// validateTraefikAnnotations vets the router annotations before the object is
// stored. The values land in a YAML file Traefik loads, so a quote, newline,
// colon or '#' there is a config-injection shape, the same as an Ingress path.
func validateTraefikAnnotations(annotations map[string]string) error {
	if v, ok := annotations[annRouterEntryPoints]; ok {
		for _, ep := range splitAnnotationList(v) {
			if err := validateTraefikName(annRouterEntryPoints, ep); err != nil {
				return err
			}
		}
	}
	if v, ok := annotations[annRouterMiddlewares]; ok {
		for _, mw := range splitAnnotationList(v) {
			if err := validateTraefikMiddlewareRef(annRouterMiddlewares, mw); err != nil {
				return err
			}
		}
	}
	if v, ok := annotations[annRouterTLS]; ok {
		if _, err := strconv.ParseBool(v); err != nil {
			return fmt.Errorf("annotation %s: value %q is invalid: must be a boolean", annRouterTLS, v)
		}
	}
	if v, ok := annotations[annRouterTLSCertResolver]; ok {
		if err := validateTraefikName(annRouterTLSCertResolver, v); err != nil {
			return err
		}
	}
	if v, ok := annotations[annRouterPriority]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return fmt.Errorf("annotation %s: value %q is invalid: must be a non-negative integer", annRouterPriority, v)
		}
	}
	return nil
}

// validateIngress checks every user-controlled string field that ends up in
// the generated Traefik dynamic config or references another object by name.
func validateIngress(ing *networkingv1.Ingress) error {
	if err := validateTraefikAnnotations(ing.Annotations); err != nil {
		return err
	}
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
		// secretName is validated for shape only and then deliberately
		// ignored: TLS termination is Traefik's job (its default cert unless
		// the admin configures one there). q8s does not read certificate
		// secrets — see "TLS termination" in docs/ingress.md before adding
		// anything here.
		if tls.SecretName != "" {
			if err := validateName("tls secretName", tls.SecretName); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateSingleContainer rejects pod specs q8s cannot faithfully run.
// Pods and Deployments may carry any number of containers (they become a
// Podman pod), but the Job/CronJob generators still build their one unit
// from spec.containers[0], so sidecars there would be accepted and silently
// never started. initContainers and ephemeralContainers are unsupported
// everywhere. Rejecting them at the API boundary makes kubectl apply fail
// loudly instead.
func validateSingleContainer(kind string, spec *corev1.PodSpec, allowMultipleContainers bool) error {
	n := 0
	if spec != nil {
		n = len(spec.Containers)
	}
	if n == 0 {
		return fmt.Errorf("%s has no containers", kind)
	}
	if n > 1 && !allowMultipleContainers {
		return fmt.Errorf("%s has %d containers: sidecars in Jobs/CronJobs are not supported yet", kind, n)
	}
	if len(spec.InitContainers) > 0 {
		return fmt.Errorf("%s has %d initContainers: initContainers are not supported", kind, len(spec.InitContainers))
	}
	if len(spec.EphemeralContainers) > 0 {
		return fmt.Errorf("%s has %d ephemeralContainers: ephemeralContainers are not supported", kind, len(spec.EphemeralContainers))
	}
	return nil
}

// annAttachAllNetworks asks q8s to attach the pod to every namespace
// network. Only pods carrying this opted-in annotation may join a network
// outside their own namespace.
const annAttachAllNetworks = "q8s.io/attach-all-networks"

// validatePodAnnotations rejects q8s pod annotations that cannot be honored
// by the requested shape. replicas is 0 for a bare Pod.
func validatePodAnnotations(kind string, spec *corev1.PodSpec, annotations map[string]string, replicas int32) error {
	containers := 0
	if spec != nil {
		containers = len(spec.Containers)
	}
	if _, ok := annotations[quadlet.AnnotationSocketActivation]; ok {
		pod := &corev1.Pod{}
		pod.Annotations = annotations
		if spec != nil {
			pod.Spec = *spec
		}
		if err := quadlet.ValidateSocketActivation(pod); err != nil {
			return fmt.Errorf("%s: %w", kind, err)
		}
		if containers > 1 {
			return fmt.Errorf("%s: annotation %s is not supported on multi-container pods", kind, quadlet.AnnotationSocketActivation)
		}
		if replicas > 1 {
			return fmt.Errorf("%s: annotation %s requires replicas <= 1, got %d (two instances cannot own the same listen socket)", kind, quadlet.AnnotationSocketActivation, replicas)
		}
	}
	if v, ok := annotations[annAttachAllNetworks]; ok {
		attachAll, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("%s: annotation %s: value %q is invalid: must be a boolean", kind, annAttachAllNetworks, v)
		}
		if attachAll && containers > 1 {
			return fmt.Errorf("%s: annotation %s is not supported on multi-container pods", kind, annAttachAllNetworks)
		}
		if attachAll && replicas > 1 {
			return fmt.Errorf("%s: annotation %s requires replicas <= 1, got %d (every instance would claim the same fixed address in each namespace network)", kind, annAttachAllNetworks, replicas)
		}
	}
	return nil
}

// validatePatchedIdentity checks that a PATCH did not alter the object's
// namespace or name. Real k8s rejects metadata.name/namespace changes, and
// q8s's Store.Update* keys on the patched object's own identity — so a
// patch that changed them would either fail the lookup (benign 404-shaped
// InternalError) or, when a same-named object exists at the target identity,
// silently re-key and overwrite that other object. Enforcing immutability
// here keeps the identity that the POST-time validateName call vetted.
func validatePatchedIdentity(kind, patchedNS, patchedName, urlNS, urlName string) error {
	if err := validateName("namespace", patchedNS); err != nil {
		return err
	}
	if err := validateName("name", patchedName); err != nil {
		return err
	}
	if patchedNS != urlNS || patchedName != urlName {
		return fmt.Errorf("%s %q: namespace and name are immutable", kind, urlName)
	}
	return nil
}
