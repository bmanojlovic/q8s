// Package names defines the host-side names q8s derives from Kubernetes
// object identities.
//
// The legacy format joins a namespace and an object name with "-". That is
// ambiguous because both parts may contain "-", so team/prod-db and
// team-prod/db both become team-prod-db. The new format joins them with "_",
// which cannot occur in a valid RFC 1123 namespace or object name.
//
// The package is deliberately pure: it performs no I/O, imports only the
// standard library, and stays below internal/cronruns and internal/quadlet so
// those packages can call it without an import cycle. Callers are responsible
// for validating namespace and object names before using a generated name.
package names

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Kind identifies one family of generated host names. A kind selects both the
// shape of the name and where a fixed segment belongs (for example the "__job"
// suffix of a Job or the "-pod.service" suffix Quadlet derives from a .pod
// file).
type Kind string

const (
	// Pod families. KindPod is the single-container container name and the
	// multi-container Podman pod name. KindPodContainer is one member of a
	// multi-container pod. The other kinds are the files and units Quadlet
	// derives from those bases.
	KindPod                 Kind = "pod"
	KindPodContainer        Kind = "pod-container"
	KindPodFile             Kind = "pod-file"
	KindPodService          Kind = "pod-service"
	KindPodContainerFile    Kind = "pod-container-file"
	KindPodContainerService Kind = "pod-container-service"
	KindPodUnit             Kind = "pod-unit"

	// Job and CronJob families. The base already carries the fixed "__job" or
	// "__cron" segment; the file and unit kinds append only their extension.
	KindJob        Kind = "job"
	KindJobFile    Kind = "job-file"
	KindJobService Kind = "job-service"

	KindCronJob        Kind = "cronjob"
	KindCronJobFile    Kind = "cronjob-file"
	KindCronJobService Kind = "cronjob-service"
	KindCronJobTimer   Kind = "cronjob-timer"

	// Secret-derived EnvironmentFile basenames. The file lives below a
	// per-namespace _env directory, so the Pod env name carries no namespace.
	KindEnvPod     Kind = "env-pod"
	KindEnvJob     Kind = "env-job"
	KindEnvCronJob Kind = "env-cronjob"

	// PVC names. The volume and the .volume file share the same base.
	KindPVCVolume Kind = "pvc-volume"
	KindPVCFile   Kind = "pvc-file"

	// Deployment replica names. The API Pod name stays <deployment>-<index>;
	// only the host prefix gains the namespace separator.
	KindDeploymentPod         Kind = "deployment-pod"
	KindDeploymentHost        Kind = "deployment-host"
	KindDeploymentVolumeOwner Kind = "deployment-volume-owner"

	// Socket activation units.
	KindSocketFile    Kind = "socket-file"
	KindSocketService Kind = "socket-service"

	// Traefik file provider documents and object keys.
	KindIngressFile         Kind = "ingress-file"
	KindMiddlewareFile      Kind = "middleware-file"
	KindIngressRouteTCPFile Kind = "ingressroutetcp-file"
	KindIngressRoute        Kind = "ingress-route"
	KindIngressRouteTCP     Kind = "ingressroutetcp-route"
	KindMiddlewareKey       Kind = "middleware-key"

	// Families the design deliberately leaves unchanged.
	KindNetwork        Kind = "network"
	KindEmptyDirVolume Kind = "emptydir-volume"
)

const (
	// MaxBaseLen is the byte budget for a generated name before its file or
	// unit extension is appended. It leaves room for ".container", ".service",
	// ".socket", ".timer", ".volume", ".network", ".pod", "-pod.service" and
	// the q8s- network prefix inside the 255-byte systemd and NAME_MAX limit.
	MaxBaseLen = 240

	// maxFirstComponent is the part of the first component (the namespace, or
	// the workload name for an env file) that survives truncation. Keeping it
	// whole in practice lets operators still recognize the namespace.
	maxFirstComponent = 63

	// hashHexLen is the number of SHA-256 hexadecimal characters appended on
	// overflow. 64 bits is far beyond the collision probability this naming
	// scheme needs and keeps the name short.
	hashHexLen = 16
)

// CompactSpec describes the fixed pieces around the components Compact joins.
// Family is the hash domain: artifacts that must share one truncated base,
// such as a Pod container, its .container file and its .service unit, use the
// same Family. Prefix and Suffix include their own separator. Ext is appended
// after the 240-byte base and is therefore outside the cap.
type CompactSpec struct {
	Family Kind
	Prefix string
	Suffix string
	Ext    string
}

// Compact joins parts with "_" and caps the result at MaxBaseLen bytes. When
// the plain join fits it is returned unchanged. Otherwise it keeps the first
// component up to 63 bytes, keeps the fixed prefix and suffix, truncates the
// remaining components, and appends "_" plus 16 hexadecimal characters of
// SHA-256 over the untruncated tuple. The hash makes two different tuples
// collide only if SHA-256 collides.
//
// The extension in spec.Ext is appended after truncation. Every generated
// name therefore stays inside the 255-byte filename and systemd unit limit
// even with the longest extension q8s writes.
func Compact(spec CompactSpec, parts ...string) string {
	if len(parts) == 0 {
		panic("names: Compact needs at least one part")
	}
	base := spec.Prefix + strings.Join(parts, "_") + spec.Suffix
	if len(base) <= MaxBaseLen {
		return base + spec.Ext
	}
	return compactBase(spec, parts) + spec.Ext
}

// compactBase implements the truncating half of Compact. It never returns a
// string longer than MaxBaseLen.
func compactBase(spec CompactSpec, parts []string) string {
	first := parts[0]
	if len(first) > maxFirstComponent {
		first = first[:maxFirstComponent]
	}
	// For an env file the first component is the workload name rather than a
	// namespace, so a truncated trailing separator belongs to the name.
	if len(parts) == 1 {
		first = strings.TrimRight(first, "-_")
	}
	middle := strings.Join(parts[1:], "_")

	// Hash the full untruncated tuple, including the fixed suffix. The
	// family domain keeps derived artifacts on the same hash while keeping
	// unrelated families from sharing a truncated base. Prefix and extension
	// are left out because several artifacts in one family differ only there.
	seed := string(spec.Family) + "\x00" + strings.Join(parts, "_") + "\x00" + spec.Suffix
	sum := sha256.Sum256([]byte(seed))
	hash := "__" + hex.EncodeToString(sum[:])[:hashHexLen]

	// A separator is needed between the first component and the retained
	// middle. The suffix already carries its own separator when present.
	overhead := len(spec.Prefix) + len(first) + len(spec.Suffix) + len(hash)
	if middle != "" {
		overhead++
	}
	room := MaxBaseLen - overhead
	if room < 0 {
		room = 0
	}
	if len(middle) > room {
		middle = middle[:room]
	}
	// A truncated component can end on a separator. Removing it keeps the
	// hash segment visually separate from the name.
	middle = strings.TrimRight(middle, "-_")

	var b strings.Builder
	b.Grow(MaxBaseLen)
	b.WriteString(spec.Prefix)
	b.WriteString(first)
	if middle != "" {
		b.WriteString("_")
		b.WriteString(middle)
	}
	b.WriteString(spec.Suffix)
	b.WriteString(hash)
	return b.String()
}

// nameLayout is the format-independent shape of one kind. The legacy and new
// variants differ only in their separators and in the separator that belongs
// to a fixed segment such as "__job" or "-pod".
type nameLayout struct {
	prefixNew    string
	prefixLegacy string

	separatorNew    string
	separatorLegacy string

	parts []string

	suffixNew    string
	suffixLegacy string

	extension string

	// hashFamily groups artifacts that must share one truncated base.
	hashFamily Kind

	// compact is false for families the design keeps byte-identical.
	compact bool
}

// render builds the name without the overflow fallback. LegacyHost uses it
// directly; Host uses it only for the unchanged families.
func (l nameLayout) render(newFormat bool) string {
	prefix, separator, suffix := l.prefixLegacy, l.separatorLegacy, l.suffixLegacy
	if newFormat {
		prefix, separator, suffix = l.prefixNew, l.separatorNew, l.suffixNew
	}
	return prefix + strings.Join(l.parts, separator) + suffix + l.extension
}

// Host returns the new-format generated name for kind. extra carries the
// variable components of the kind: the container name for KindPodContainer,
// KindPodContainerFile and KindPodContainerService (an empty or missing value
// selects the single-container shape), the socket entry for KindSocketFile, the Deployment
// replica index for KindDeploymentHost and KindDeploymentPod, the volume name
// for KindEmptyDirVolume, and the router index for KindIngressRoute and
// KindIngressRouteTCP.
//
// Names longer than MaxBaseLen are truncated with Compact. The families the
// design leaves unchanged (the namespace network, emptyDir volumes, the
// Middleware object key and the Deployment API Pod name) are returned exactly
// as today.
func Host(kind Kind, namespace, name string, extra ...string) string {
	l, ok := layoutFor(kind, namespace, name, extra)
	if !ok {
		panic("names: Host: invalid arguments for kind " + string(kind))
	}
	if !l.compact {
		return l.render(false)
	}
	return Compact(CompactSpec{
		Family: l.hashFamily,
		Prefix: l.prefixNew,
		Suffix: l.suffixNew,
		Ext:    l.extension,
	}, l.parts...)
}

// LegacyHost returns the name q8s generates today for kind. It is byte-for-byte
// compatible with the existing helpers and is intentionally not truncated:
// PR 2 can diff every call site against it before switching that call site to
// Host.
func LegacyHost(kind Kind, namespace, name string, extra ...string) string {
	l, ok := layoutFor(kind, namespace, name, extra)
	if !ok {
		panic("names: LegacyHost: invalid arguments for kind " + string(kind))
	}
	return l.render(false)
}

// layoutFor maps a kind and its inputs to the two separators and the fixed
// pieces. Returning false means the caller omitted a required extra component.
func layoutFor(kind Kind, namespace, name string, extra []string) (nameLayout, bool) {
	get := func(i int) (string, bool) {
		if i < len(extra) {
			return extra[i], true
		}
		return "", false
	}

	// dashDash is a family whose names still use "-" for both formats.
	dashDash := func(parts []string) nameLayout {
		return nameLayout{
			separatorNew:    "-",
			separatorLegacy: "-",
			parts:           parts,
			hashFamily:      kind,
		}
	}

	switch kind {
	case KindPod:
		return nameLayout{
			separatorNew:    "_",
			separatorLegacy: "-",
			parts:           []string{namespace, name},
			hashFamily:      KindPod,
			compact:         true,
		}, true

	case KindPodContainer:
		parts := []string{namespace, name}
		// An empty container selects the single-container shape, matching
		// the historical podContainerName helper.
		if container, ok := get(0); ok && container != "" {
			parts = append(parts, container)
		}
		return nameLayout{
			separatorNew:    "_",
			separatorLegacy: "-",
			parts:           parts,
			hashFamily:      KindPodContainer,
			compact:         true,
		}, true

	case KindPodFile:
		return nameLayout{
			separatorNew:    "_",
			separatorLegacy: "-",
			parts:           []string{namespace, name},
			extension:       ".pod",
			hashFamily:      KindPod,
			compact:         true,
		}, true

	case KindPodService:
		return nameLayout{
			separatorNew:    "_",
			separatorLegacy: "-",
			parts:           []string{namespace, name},
			extension:       ".service",
			hashFamily:      KindPod,
			compact:         true,
		}, true

	case KindPodUnit:
		// Quadlet derives <base>-pod.service from <base>.pod. The suffix is
		// structural and stays a hyphen in both formats, so it belongs to the
		// extension rather than to the truncatable base.
		return nameLayout{
			separatorNew:    "_",
			separatorLegacy: "-",
			parts:           []string{namespace, name},
			extension:       "-pod.service",
			hashFamily:      KindPod,
			compact:         true,
		}, true

	case KindPodContainerFile:
		parts := []string{namespace, name}
		if container, ok := get(0); ok && container != "" {
			parts = append(parts, container)
		}
		return nameLayout{
			separatorNew:    "_",
			separatorLegacy: "-",
			parts:           parts,
			extension:       ".container",
			hashFamily:      KindPodContainer,
			compact:         true,
		}, true

	case KindPodContainerService:
		parts := []string{namespace, name}
		if container, ok := get(0); ok && container != "" {
			parts = append(parts, container)
		}
		return nameLayout{
			separatorNew:    "_",
			separatorLegacy: "-",
			parts:           parts,
			extension:       ".service",
			hashFamily:      KindPodContainer,
			compact:         true,
		}, true

	case KindJob:
		return nameLayout{
			separatorNew:    "_",
			separatorLegacy: "-",
			parts:           []string{namespace, name},
			suffixNew:       "__job",
			suffixLegacy:    "-job",
			hashFamily:      KindJob,
			compact:         true,
		}, true

	case KindJobFile:
		return nameLayout{
			separatorNew:    "_",
			separatorLegacy: "-",
			parts:           []string{namespace, name},
			suffixNew:       "__job",
			suffixLegacy:    "-job",
			extension:       ".container",
			hashFamily:      KindJob,
			compact:         true,
		}, true

	case KindJobService:
		return nameLayout{
			separatorNew:    "_",
			separatorLegacy: "-",
			parts:           []string{namespace, name},
			suffixNew:       "__job",
			suffixLegacy:    "-job",
			extension:       ".service",
			hashFamily:      KindJob,
			compact:         true,
		}, true

	case KindCronJob:
		return nameLayout{
			separatorNew:    "_",
			separatorLegacy: "-",
			parts:           []string{namespace, name},
			suffixNew:       "__cron",
			suffixLegacy:    "-cron",
			hashFamily:      KindCronJob,
			compact:         true,
		}, true

	case KindCronJobFile:
		return nameLayout{
			separatorNew:    "_",
			separatorLegacy: "-",
			parts:           []string{namespace, name},
			suffixNew:       "__cron",
			suffixLegacy:    "-cron",
			extension:       ".container",
			hashFamily:      KindCronJob,
			compact:         true,
		}, true

	case KindCronJobService:
		return nameLayout{
			separatorNew:    "_",
			separatorLegacy: "-",
			parts:           []string{namespace, name},
			suffixNew:       "__cron",
			suffixLegacy:    "-cron",
			extension:       ".service",
			hashFamily:      KindCronJob,
			compact:         true,
		}, true

	case KindCronJobTimer:
		return nameLayout{
			separatorNew:    "_",
			separatorLegacy: "-",
			parts:           []string{namespace, name},
			suffixNew:       "__cron",
			suffixLegacy:    "-cron",
			extension:       ".timer",
			hashFamily:      KindCronJob,
			compact:         true,
		}, true

	case KindEnvPod:
		// The namespace is a directory component, so only the basename is
		// generated. It stays <pod>.env, but a long Pod name is still capped
		// because the result is a filename.
		return nameLayout{
			separatorNew:    "_",
			separatorLegacy: "-",
			parts:           []string{name},
			extension:       ".env",
			hashFamily:      KindEnvPod,
			compact:         true,
		}, true

	case KindEnvJob:
		return nameLayout{
			separatorNew:    "_",
			separatorLegacy: "-",
			parts:           []string{name},
			suffixNew:       "__job",
			suffixLegacy:    "-job",
			extension:       ".env",
			hashFamily:      KindEnvJob,
			compact:         true,
		}, true

	case KindEnvCronJob:
		return nameLayout{
			separatorNew:    "_",
			separatorLegacy: "-",
			parts:           []string{name},
			suffixNew:       "__cron",
			suffixLegacy:    "-cron",
			extension:       ".env",
			hashFamily:      KindEnvCronJob,
			compact:         true,
		}, true

	case KindPVCVolume:
		return nameLayout{
			separatorNew:    "_",
			separatorLegacy: "-",
			parts:           []string{namespace, name},
			hashFamily:      KindPVCVolume,
			compact:         true,
		}, true

	case KindPVCFile:
		return nameLayout{
			separatorNew:    "_",
			separatorLegacy: "-",
			parts:           []string{namespace, name},
			extension:       ".volume",
			hashFamily:      KindPVCVolume,
			compact:         true,
		}, true

	case KindDeploymentPod:
		index, ok := get(0)
		if !ok || index == "" {
			return nameLayout{}, false
		}
		// The API Pod name is user visible and stays <deployment>-<index>.
		l := dashDash([]string{name + "-" + index})
		l.hashFamily = KindDeploymentPod
		return l, true

	case KindDeploymentHost:
		index, ok := get(0)
		if !ok || index == "" {
			return nameLayout{}, false
		}
		return nameLayout{
			separatorNew:    "_",
			separatorLegacy: "-",
			parts:           []string{namespace, name + "-" + index},
			hashFamily:      KindDeploymentHost,
			compact:         true,
		}, true

	case KindDeploymentVolumeOwner:
		index, ok := get(0)
		if !ok || index == "" {
			return nameLayout{}, false
		}
		// The emptyDir owner already uses "_" and is unchanged.
		l := dashDash([]string{name + "_" + index})
		l.separatorNew = "_"
		l.separatorLegacy = "_"
		l.hashFamily = KindDeploymentVolumeOwner
		return l, true

	case KindSocketFile:
		entry, ok := get(0)
		if !ok || entry == "" {
			return nameLayout{}, false
		}
		return nameLayout{
			separatorNew:    "_",
			separatorLegacy: "-",
			parts:           []string{namespace, name, entry},
			extension:       ".socket",
			hashFamily:      KindSocketFile,
			compact:         true,
		}, true

	case KindSocketService:
		return nameLayout{
			separatorNew:    "_",
			separatorLegacy: "-",
			parts:           []string{namespace, name},
			extension:       ".service",
			hashFamily:      KindSocketService,
			compact:         true,
		}, true

	case KindIngressFile:
		return nameLayout{
			separatorNew:    "_",
			separatorLegacy: "-",
			parts:           []string{namespace, name},
			extension:       ".yaml",
			hashFamily:      KindIngressFile,
			compact:         true,
		}, true

	case KindMiddlewareFile:
		return nameLayout{
			prefixNew:       "middleware_",
			prefixLegacy:    "middleware-",
			separatorNew:    "_",
			separatorLegacy: "-",
			parts:           []string{namespace, name},
			extension:       ".yaml",
			hashFamily:      KindMiddlewareFile,
			compact:         true,
		}, true

	case KindIngressRouteTCPFile:
		return nameLayout{
			prefixNew:       "tcp_",
			prefixLegacy:    "tcp-",
			separatorNew:    "_",
			separatorLegacy: "-",
			parts:           []string{namespace, name},
			extension:       ".yaml",
			hashFamily:      KindIngressRouteTCPFile,
			compact:         true,
		}, true

	case KindIngressRoute, KindIngressRouteTCP:
		index, ok := get(0)
		if !ok || index == "" {
			return nameLayout{}, false
		}
		return nameLayout{
			separatorNew:    "_",
			separatorLegacy: "-",
			parts:           []string{namespace, name, index},
			hashFamily:      kind,
			compact:         true,
		}, true

	case KindMiddlewareKey:
		// Users write <ns>-<name>@kubernetescrd in their Ingresses. The key
		// stays unchanged for manifest compatibility.
		return dashDash([]string{namespace, name}), true

	case KindNetwork:
		l := dashDash([]string{namespace})
		l.prefixNew = "q8s-"
		l.prefixLegacy = "q8s-"
		l.hashFamily = KindNetwork
		return l, true

	case KindEmptyDirVolume:
		volume, ok := get(0)
		if !ok || volume == "" {
			return nameLayout{}, false
		}
		return nameLayout{
			prefixNew:       "q8s_",
			prefixLegacy:    "q8s_",
			separatorNew:    "_",
			separatorLegacy: "_",
			parts:           []string{namespace, name, volume},
			hashFamily:      KindEmptyDirVolume,
			compact:         false,
		}, true
	}
	return nameLayout{}, false
}

// EffectivePVCVolumeName returns the Podman volume name a PVC must keep.
//
// persisted is the claim's spec.volumeName. That field is the only generated
// name q8s stores, so it is the authority: a claim with the old <ns>-<claim>
// value keeps that volume forever, and a claim with a new-format value keeps
// that value too. An unbound claim with no value is legacy only when an old
// <ns>-<claim>.volume file already exists on the host; otherwise it is new.
func EffectivePVCVolumeName(namespace, name, persistedVolumeName string, legacyFileExists bool) string {
	return effectivePVCName(namespace, name, persistedVolumeName, legacyFileExists)
}

// EffectivePVCFileBase returns the basename of the .volume file for a claim.
// The base matches the effective Podman volume name, so the file's
// VolumeName= directive and its filename can never disagree.
func EffectivePVCFileBase(namespace, name, persistedVolumeName string, legacyFileExists bool) string {
	return effectivePVCName(namespace, name, persistedVolumeName, legacyFileExists)
}

func effectivePVCName(namespace, name, persistedVolumeName string, legacyFileExists bool) string {
	if persistedVolumeName != "" {
		return persistedVolumeName
	}
	if legacyFileExists {
		return LegacyHost(KindPVCVolume, namespace, name)
	}
	return Host(KindPVCVolume, namespace, name)
}
