package quadlet

import (
	"fmt"
	"net"
	"path/filepath"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// NetworkAttachment is one namespace network a pod joins; IP may be empty.
type NetworkAttachment struct {
	Namespace string
	IP        string
}

// LastUsableIPv4 returns the highest host address of an IPv4 CIDR: the
// broadcast address minus one. Part B pins Traefik to this address on each
// namespace network so its IP is stable across restarts.
func LastUsableIPv4(cidr string) (string, error) {
	ip, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", fmt.Errorf("%q is not a valid CIDR: %w", cidr, err)
	}
	v4 := ip.To4()
	if v4 == nil {
		return "", fmt.Errorf("%q is not an IPv4 CIDR", cidr)
	}
	ones, bits := network.Mask.Size()
	if bits != 32 || ones > 30 {
		return "", fmt.Errorf("%q must be an IPv4 CIDR with a prefix of /30 or shorter", cidr)
	}
	base := network.IP.To4()
	last := make(net.IP, net.IPv4len)
	copy(last, base)
	for i := range last {
		last[i] |= ^network.Mask[i]
	}
	// broadcast - 1: skip the broadcast address itself.
	for i := len(last) - 1; i >= 0; i-- {
		if last[i] > 0 {
			last[i]--
			break
		}
		last[i] = 0xff
	}
	return last.String(), nil
}

// containerRender carries everything needed to render one .container unit.
// The same helper serves both shapes: a standalone single-container pod
// (standalone=true, no Pod= reference, network and published ports on the
// container) and a member of a Podman pod (podUnit set, no network or port
// options, which Podman rejects on a container that joins a pod).
type containerRender struct {
	pod            *corev1.Pod
	container      corev1.Container
	containerName  string // ContainerName= value
	podUnit        string // Pod= value; empty for a standalone container
	containerLabel string // io.kubernetes.container.name value; empty when unset
	podName        string // name argument of Container/PodUnits, used for DNS aliases
	configDir      string
	serviceAliases []string
	networks       []NetworkAttachment
	pvcMap         map[string]*corev1.PersistentVolumeClaim
	envFile        string
	standalone     bool
}

// PodUnits renders the quadlet files for a multi-container pod: one
// <ns>-<pod>.pod holding the pod-level settings and one
// <ns>-<pod>-<container>.container per spec container. The returned map is
// keyed by filename (the caller writes each file into the quadlet dir).
// Single-container pods must keep using Container, whose output is
// byte-identical to q8s before pods existed.
func PodUnits(name string, pod *corev1.Pod, configDir string, serviceAliases []string, pvcMap map[string]*corev1.PersistentVolumeClaim, envFile string, networks []NetworkAttachment) (map[string][]byte, error) {
	if err := validateRefName("pod name", name); err != nil {
		return nil, err
	}
	if err := validateRefName("pod namespace", pod.Namespace); err != nil {
		return nil, err
	}
	if len(pod.Spec.Containers) == 0 {
		return nil, fmt.Errorf("pod has no containers")
	}
	// FileDescriptorName is per unit, so one socket unit can only back one
	// container; a Podman pod has no single service to activate.
	if _, ok := pod.Annotations[AnnotationSocketActivation]; ok {
		return nil, fmt.Errorf("pod %s/%s: %s is not supported on multi-container pods", pod.Namespace, name, AnnotationSocketActivation)
	}

	podContent, err := renderPod(name, pod, serviceAliases, networks)
	if err != nil {
		return nil, err
	}
	podName := fmt.Sprintf("%s-%s", pod.Namespace, name)
	files := map[string][]byte{
		podName + ".pod": withTemplateHash(podContent),
	}
	for _, c := range pod.Spec.Containers {
		if err := validateRefName("container name", c.Name); err != nil {
			return nil, err
		}
		filename := fmt.Sprintf("%s-%s.container", podName, c.Name)
		if _, exists := files[filename]; exists {
			return nil, fmt.Errorf("pod %s/%s has duplicate container name %q", pod.Namespace, name, c.Name)
		}
		content, err := renderContainer(containerRender{
			pod:            pod,
			container:      c,
			containerName:  fmt.Sprintf("%s-%s", podName, c.Name),
			podUnit:        podName + ".pod",
			containerLabel: c.Name,
			podName:        name,
			configDir:      configDir,
			serviceAliases: serviceAliases,
			networks:       networks,
			pvcMap:         pvcMap,
			envFile:        envFile,
			standalone:     false,
		})
		if err != nil {
			return nil, err
		}
		files[filename] = withTemplateHash(content)
	}
	return files, nil
}

// renderPod emits the [Pod] section. Only keys Podman accepts on a pod live
// here: network, published ports, /dev/shm and labels are pod-level because
// all containers in the pod share them.
func renderPod(name string, pod *corev1.Pod, serviceAliases []string, networks []NetworkAttachment) ([]byte, error) {
	var b strings.Builder

	b.WriteString("[Pod]\n")
	podName := fmt.Sprintf("%s-%s", pod.Namespace, name)
	b.WriteString(fmt.Sprintf("PodName=%s\n", podName))

	// The pod owns the user namespace, not its member containers: podman
	// rejects UserNS= on a container that joins a pod, and members inherit
	// the pod's namespace. Their Volume= lines still get idmap.
	if podUsesUserNS(pod.Spec) {
		b.WriteString("UserNS=auto\n")
	}

	// Network: host networking when requested, otherwise the namespace
	// networks. NetworkAlias only makes sense on the namespace bridge, and
	// podman applies the aliases on every attached network.
	if pod.Spec.HostNetwork {
		b.WriteString("Network=host\n")
	} else {
		if err := writeNetworks(&b, pod.Namespace, networks); err != nil {
			return nil, err
		}
		aliases, err := networkAliases(pod.Namespace, name, serviceAliases)
		if err != nil {
			return nil, err
		}
		for _, alias := range aliases {
			b.WriteString(fmt.Sprintf("NetworkAlias=%s\n", alias))
		}
	}

	// Every container's hostPort is a pod-level publish.
	for _, c := range pod.Spec.Containers {
		writePublishPorts(&b, c.Ports)
	}

	// /dev/shm is shared by every container in a pod, so the emptyDir
	// memory size limit belongs on the pod.
	if size, ok := podShmSize(pod); ok {
		b.WriteString(fmt.Sprintf("ShmSize=%d\n", size))
	}

	if err := writePodLabels(&b, pod, ""); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(pod.Spec.Containers))
	for _, c := range pod.Spec.Containers {
		names = append(names, c.Name)
	}
	b.WriteString(fmt.Sprintf("Label=q8s.io/containers=%s\n", strings.Join(names, ",")))

	return []byte(b.String()), nil
}

// networkAliases expands a pod's matching Service names into the DNS names
// k8s clients expect on the cluster network. The pod's own <pod>.<ns> name
// is always present, even with no Service, so Traefik can dial a backend as
// <pod>.<ns>:<containerPort> without a Service existing.
func networkAliases(ns, podName string, services []string) ([]string, error) {
	// Dotted aliases cannot go through validateRefName (it rejects '.'), so
	// each component is vetted before it is joined.
	if err := validateRefName("pod namespace", ns); err != nil {
		return nil, err
	}
	if err := validateRefName("pod name", podName); err != nil {
		return nil, err
	}
	set := map[string]bool{
		fmt.Sprintf("%s.%s", podName, ns): true,
	}
	for _, svc := range services {
		if err := validateRefName("service alias", svc); err != nil {
			return nil, err
		}
		set[svc] = true
		set[fmt.Sprintf("%s.%s", svc, ns)] = true
		set[fmt.Sprintf("%s.%s.svc.cluster.local", svc, ns)] = true
	}
	aliases := make([]string, 0, len(set))
	for alias := range set {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	return aliases, nil
}

// writeNetworks emits the Network= lines for a non-hostNetwork pod. An empty
// attachment list is today's single own-namespace network; otherwise the pod
// joins each listed network, sorted by namespace, with an optional fixed IPv4.
func writeNetworks(b *strings.Builder, ns string, networks []NetworkAttachment) error {
	if len(networks) == 0 {
		b.WriteString(fmt.Sprintf("Network=q8s-%s.network\n", ns))
		return nil
	}
	attachments := append([]NetworkAttachment(nil), networks...)
	sort.SliceStable(attachments, func(i, j int) bool {
		return attachments[i].Namespace < attachments[j].Namespace
	})
	var lines strings.Builder
	own := false
	for _, n := range attachments {
		if err := validateRefName("network namespace", n.Namespace); err != nil {
			return err
		}
		if n.Namespace == ns {
			own = true
		}
		if n.IP == "" {
			lines.WriteString(fmt.Sprintf("Network=q8s-%s.network\n", n.Namespace))
			continue
		}
		ip := net.ParseIP(n.IP)
		if ip == nil || ip.To4() == nil {
			return fmt.Errorf("network %q: IP %q is not a valid IPv4 address", n.Namespace, n.IP)
		}
		lines.WriteString(fmt.Sprintf("Network=q8s-%s.network:ip=%s\n", n.Namespace, ip.To4().String()))
	}
	if !own {
		// A pod is always on its own namespace network, listed or not.
		b.WriteString(fmt.Sprintf("Network=q8s-%s.network\n", ns))
	}
	b.WriteString(lines.String())
	return nil
}

// renderContainer emits one .container unit. Its output for a
// single-container pod is byte-identical to the pre-pod Container function.
func renderContainer(r containerRender) ([]byte, error) {
	pod := r.pod
	c := r.container

	var b strings.Builder

	b.WriteString("[Container]\n")

	if err := validateImage(c.Image); err != nil {
		return nil, err
	}
	writeImageAndPull(&b, c)

	b.WriteString(fmt.Sprintf("ContainerName=%s\n", r.containerName))
	if r.podUnit != "" {
		b.WriteString(fmt.Sprintf("Pod=%s\n", r.podUnit))
	} else if podUsesUserNS(pod.Spec) {
		b.WriteString("UserNS=auto\n")
	}

	// Exec: combine command (entrypoint override) and args
	cmd := append(c.Command, c.Args...)
	for _, part := range cmd {
		if err := noControlChars("command", part); err != nil {
			return nil, err
		}
	}
	if len(cmd) > 0 {
		b.WriteString(fmt.Sprintf("Exec=%s\n", shellJoin(cmd)))
	}

	if c.WorkingDir != "" {
		if err := validatePath("workingDir", c.WorkingDir); err != nil {
			return nil, err
		}
		b.WriteString(fmt.Sprintf("WorkingDir=%s\n", c.WorkingDir))
	}

	if c.SecurityContext != nil && c.SecurityContext.RunAsUser != nil {
		b.WriteString(fmt.Sprintf("User=%d\n", *c.SecurityContext.RunAsUser))
	}

	writeResourceLimits(&b, c.Resources.Limits)
	writeGPUDevice(&b, c.Resources.Limits)
	if err := writeUlimits(&b, pod.Annotations); err != nil {
		return nil, err
	}

	if r.envFile != "" {
		if err := validatePath("envFile", r.envFile); err != nil {
			return nil, err
		}
		b.WriteString(fmt.Sprintf("EnvironmentFile=%s\n", r.envFile))
	}

	for _, env := range c.Env {
		if err := validateEnvName(env.Name); err != nil {
			return nil, err
		}
		line, err := envLine(env.Name, env.Value)
		if err != nil {
			return nil, err
		}
		b.WriteString(line)
	}

	// Published ports and network are pod-level in the pod shape; the
	// standalone shape keeps them on the container as before.
	if r.standalone {
		writePublishPorts(&b, c.Ports)
	}

	if err := writeContainerVolumes(&b, r); err != nil {
		return nil, err
	}

	if r.standalone {
		if pod.Spec.HostNetwork {
			b.WriteString("Network=host\n")
		} else {
			if err := writeNetworks(&b, pod.Namespace, r.networks); err != nil {
				return nil, err
			}
			aliases, err := networkAliases(pod.Namespace, r.podName, r.serviceAliases)
			if err != nil {
				return nil, err
			}
			for _, alias := range aliases {
				b.WriteString(fmt.Sprintf("NetworkAlias=%s\n", alias))
			}
		}
	}

	if err := writePodLabels(&b, pod, r.containerLabel); err != nil {
		return nil, err
	}

	if err := writeTemplateAnnotations(&b, pod); err != nil {
		return nil, err
	}

	if err := writeHealthCheck(&b, c); err != nil {
		return nil, err
	}

	b.WriteString("\n[Unit]\n")
	b.WriteString(fmt.Sprintf("Description=Pod %s\n", pod.Name))
	// Order after q8s-api so a host reboot doesn't race the container's
	// ExecStart --env-file against q8s-api re-rendering it (q8s renders the
	// per-pod _env files before sd_notify READY). Wants, not Requires: a
	// standalone `q8s serve` with no installed q8s-api.service must still
	// work - a missing unit here is a harmless no-op. See tic-1507.
	b.WriteString("Wants=q8s-api.service\n")
	b.WriteString("After=q8s-api.service\n")

	// Socket activation: the pod's .socket units hand it listening fds via
	// LISTEN_FDS. Podman only passes those through when the socket unit is
	// already active before the container service starts, hence Requires=
	// plus After= (and not just Wants=).
	if r.standalone {
		sockets, err := ParseSocketActivation(pod.Annotations)
		if err != nil {
			return nil, err
		}
		for _, s := range sockets {
			unit := fmt.Sprintf("%s-%s-%s.socket", pod.Namespace, r.podName, s.Name)
			b.WriteString(fmt.Sprintf("Requires=%s\n", unit))
			b.WriteString(fmt.Sprintf("After=%s\n", unit))
		}
	}

	// Every policy maps to an explicit Restart= line. Emitting nothing for
	// Never (as older code did) is not neutral: quadlet/.container units
	// default to on-failure, so a "run once, never restart" pod restarted
	// on failure anyway. Defaulting empty to Always matches real k8s pod
	// defaulting - but that normalization happens at the API boundary
	// (handler POST/PATCH); the generator treats empty defensively the same
	// way.
	switch pod.Spec.RestartPolicy {
	case corev1.RestartPolicyNever:
		b.WriteString("\n[Service]\n")
		b.WriteString("Restart=no\n")
	default: // Always, OnFailure, empty (API defaulting) - restart
		b.WriteString("StartLimitBurst=5\n")
		b.WriteString("StartLimitIntervalSec=60\n")
		b.WriteString("\n[Service]\n")
		if pod.Spec.RestartPolicy == corev1.RestartPolicyAlways || pod.Spec.RestartPolicy == "" {
			// Always means restart regardless of exit code -- on-failure
			// would silently strand the pod as "Succeeded" on a clean
			// exit 0 (confirmed live 2026-08-29: a container that exited
			// 0 due to missing config never restarted, even though the
			// Deployment's replicas stayed at 1).
			b.WriteString("Restart=always\n")
		} else {
			b.WriteString("Restart=on-failure\n")
		}
		b.WriteString("RestartSec=5\n")
	}

	b.WriteString("\n[Install]\n")
	b.WriteString("WantedBy=default.target\n")

	return []byte(b.String()), nil
}

// writePublishPorts emits PublishPort= for every explicitly declared
// hostPort. containerPort alone is informational (matches k8s semantics).
func writePublishPorts(b *strings.Builder, ports []corev1.ContainerPort) {
	for _, port := range ports {
		if port.HostPort == 0 {
			continue
		}
		proto := "tcp"
		if port.Protocol == corev1.ProtocolUDP {
			proto = "udp"
		}
		// An explicit HostIP binds the published port to one interface, so
		// a hand-published backend can stay on-host (for example loopback
		// only) and never be exposed on the LAN. Without HostIP the publish
		// binds all interfaces, matching k8s hostPort semantics.
		if port.HostIP != "" {
			b.WriteString(fmt.Sprintf("PublishPort=%s:%d:%d/%s\n", port.HostIP, port.HostPort, port.ContainerPort, proto))
		} else {
			b.WriteString(fmt.Sprintf("PublishPort=%d:%d/%s\n", port.HostPort, port.ContainerPort, proto))
		}
	}
}

// writeContainerVolumes emits the Volume= lines for the mounts of exactly
// this container. ConfigMap references resolve to their on-disk directory.
// A memory-backed emptyDir at /dev/shm is not a Volume= line: standalone it
// becomes ShmSize= on the container, and in a pod it is rendered once on the
// pod by renderPod.
func writeContainerVolumes(b *strings.Builder, r containerRender) error {
	pod := r.pod
	// A member of a userns pod needs idmap just like a standalone unit: the
	// namespace is inherited from the pod, the volume options are not.
	userNS := podUsesUserNS(pod.Spec)
	for _, vol := range pod.Spec.Volumes {
		for _, vm := range r.container.VolumeMounts {
			if vm.Name != vol.Name {
				continue
			}
			if err := validatePath("mountPath", vm.MountPath); err != nil {
				return err
			}
			if isEmptyDirShm(vol, vm.MountPath) {
				if r.standalone && vol.EmptyDir.SizeLimit != nil && vol.EmptyDir.SizeLimit.Sign() > 0 {
					b.WriteString(fmt.Sprintf("ShmSize=%d\n", vol.EmptyDir.SizeLimit.Value()))
				}
				continue
			}
			readOnly := volumeMountReadOnly(vm, vol)
			switch {
			case vol.PersistentVolumeClaim != nil:
				if err := validateRefName("persistentVolumeClaim.claimName", vol.PersistentVolumeClaim.ClaimName); err != nil {
					return err
				}
				if pvc, ok := r.pvcMap[vol.PersistentVolumeClaim.ClaimName]; ok {
					line, err := pvcVolumeDirective(pod.Namespace, pvc, vm.MountPath, readOnly, userNS)
					if err != nil {
						return err
					}
					b.WriteString(line)
				} else {
					// PVC not found in map - fall back to named volume with :Z
					b.WriteString(fmt.Sprintf("Volume=%s-%s.volume:%s%s\n", pod.Namespace, vol.PersistentVolumeClaim.ClaimName, vm.MountPath, volumeOptions(readOnly, "Z", userNS)))
				}
			case vol.ConfigMap != nil:
				if err := validateRefName("configMap.name", vol.ConfigMap.Name); err != nil {
					return err
				}
				if r.configDir != "" {
					cmPath := fmt.Sprintf("%s/%s/%s", r.configDir, pod.Namespace, vol.ConfigMap.Name)
					b.WriteString(fmt.Sprintf("Volume=%s:%s%s\n", cmPath, vm.MountPath, volumeOptions(true, "z", userNS)))
				}
			case vol.Secret != nil:
				if err := validateRefName("secret.secretName", vol.Secret.SecretName); err != nil {
					return err
				}
				if r.configDir != "" {
					secretDir := filepath.Join(filepath.Dir(r.configDir), "secrets")
					secPath := filepath.Join(secretDir, pod.Namespace, vol.Secret.SecretName)
					b.WriteString(fmt.Sprintf("Volume=%s:%s%s\n", secPath, vm.MountPath, volumeOptions(true, "z", userNS)))
				}
			default:
				if err := validateRefName("volume.name", vol.Name); err != nil {
					return err
				}
				b.WriteString(fmt.Sprintf("Volume=%s:%s%s\n", vol.Name, vm.MountPath, volumeOptions(readOnly, "", userNS)))
			}
		}
	}
	return nil
}

// podShmSize returns the size limit of a memory-backed emptyDir mounted at
// /dev/shm by any container in the pod. Found reports the idiom even when no
// sizeLimit was set (in which case size is 0 and no ShmSize= line is
// emitted).
func podShmSize(pod *corev1.Pod) (size int64, found bool) {
	for _, vol := range pod.Spec.Volumes {
		if vol.EmptyDir == nil || vol.EmptyDir.Medium != corev1.StorageMediumMemory {
			continue
		}
		for _, c := range pod.Spec.Containers {
			for _, vm := range c.VolumeMounts {
				if vm.Name != vol.Name || vm.MountPath != "/dev/shm" {
					continue
				}
				if vol.EmptyDir.SizeLimit != nil && vol.EmptyDir.SizeLimit.Sign() > 0 {
					return vol.EmptyDir.SizeLimit.Value(), true
				}
				return 0, false
			}
		}
	}
	return 0, false
}

// writePodLabels emits the labels q8s has always put on a pod's container.
// containerName is empty for a standalone container and for the .pod itself;
// a pod member adds io.kubernetes.container.name so status sync and
// `podman ps --filter label=...` can identify it.
func writePodLabels(b *strings.Builder, pod *corev1.Pod, containerName string) error {
	b.WriteString(fmt.Sprintf("Label=io.kubernetes.pod.name=%s\n", pod.Name))
	b.WriteString(fmt.Sprintf("Label=io.kubernetes.pod.namespace=%s\n", pod.Namespace))
	for _, or := range pod.OwnerReferences {
		if or.Kind == "Deployment" {
			b.WriteString(fmt.Sprintf("Label=io.kubernetes.pod.deployment=%s\n", or.Name))
		}
	}
	// Propagate the Pod's own labels onto the container so Service selector
	// matching can be done with a plain `podman ps --filter label=k=v` built
	// straight from Service.Spec.Selector, with no separate bookkeeping.
	// Sorted so an unchanged pod renders byte-identically every time (the
	// template hash depends on it).
	keys := make([]string, 0, len(pod.Labels))
	for k := range pod.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := validateLabelPair(k, pod.Labels[k]); err != nil {
			return err
		}
		b.WriteString(fmt.Sprintf("Label=%s=%s\n", k, pod.Labels[k]))
	}
	if containerName != "" {
		b.WriteString(fmt.Sprintf("Label=io.kubernetes.container.name=%s\n", containerName))
	}
	b.WriteString(ManagedByLabel + "\n")
	return nil
}

// lastAppliedConfigAnnotation is the kubectl bookkeeping annotation. It
// carries the whole applied manifest as JSON, which would bloat every unit
// and never says anything about the pod template being rolled.
const lastAppliedConfigAnnotation = "kubectl.kubernetes.io/last-applied-configuration"

// writeTemplateAnnotations writes the Deployment pod-template annotations as
// Annotation= directives, so kubectl rollout restart
// (kubectl.kubernetes.io/restartedAt) and checksum/config bumps actually
// change the rendered unit and restart the pod. The values are written out
// verbatim (not hashed) because the quadlet files double as a recovery
// source. Only Deployment-owned pods get this: a plain pod's annotation does
// not restart it in Kubernetes either, and Jobs/CronJobs must keep their
// current output.
func writeTemplateAnnotations(b *strings.Builder, pod *corev1.Pod) error {
	deploymentOwned := false
	for _, or := range pod.OwnerReferences {
		if or.Kind == "Deployment" {
			deploymentOwned = true
			break
		}
	}
	if !deploymentOwned || len(pod.Annotations) == 0 {
		return nil
	}

	// Sorted so an unchanged pod renders byte-identically every time (the
	// template hash depends on it).
	keys := make([]string, 0, len(pod.Annotations))
	for k := range pod.Annotations {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k == lastAppliedConfigAnnotation {
			continue
		}
		if err := validateAnnotationKey(k); err != nil {
			return err
		}
		line, err := unitAssignmentLine("Annotation", k, pod.Annotations[k], true)
		if err != nil {
			return err
		}
		b.WriteString(line)
	}
	return nil
}
