package quadlet

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	// ManagedByLabel is the exact line q8s writes into the main section of
	// every unit it generates. The server uses it to recognise its own files
	// (and only those) when cleaning up stale units after a redeploy.
	ManagedByLabel = "Label=app.kubernetes.io/managed-by=q8s"

	// TemplateHashLabel is the key of the label carrying a hash of the
	// rendered unit content. A repeated deploy of an unchanged spec can then
	// leave the file and its service alone.
	TemplateHashLabel = "q8s.io/template-hash"
)

// withTemplateHash appends the template-hash label to the main section of a
// rendered unit. The hash is computed over the content before the label is
// added, so rendering the same input twice always produces the same hash.
// Only .container and .pod content is passed here: a .timer has no Label key.
func withTemplateHash(content []byte) []byte {
	sum := sha256.Sum256(content)
	label := fmt.Sprintf("Label=%s=%x\n", TemplateHashLabel, sum[:6])
	s := string(content)
	if i := strings.Index(s, "\n\n["); i >= 0 {
		return []byte(s[:i+1] + label + s[i+1:])
	}
	return []byte(s + label)
}

// writeImageAndPull emits the Image= line plus a Pull= line derived from the
// container's imagePullPolicy, translating Kubernetes pull semantics to
// podman quadlet's Pull= key. Before this, q8s emitted no Pull= at all, so
// the unit inherited quadlet's implicit "missing" — indistinguishable from
// what we now emit for an unset policy, but the explicit line means a user
// who sets imagePullPolicy: Always actually gets Pull=always and their image
// refreshes on restart/rollout (the tic-864b bug: it was being dropped
// entirely). We honor whatever imagePullPolicy the user set and impose no
// default of our own beyond the neutral "missing". Callers have already
// validated the image string. See tic-864b.
func writeImageAndPull(b *strings.Builder, c corev1.Container) {
	b.WriteString(fmt.Sprintf("Image=%s\n", c.Image))
	b.WriteString(fmt.Sprintf("Pull=%s\n", pullPolicyToQuadlet(c.ImagePullPolicy)))
}

// pullPolicyToQuadlet maps a Kubernetes imagePullPolicy to a podman quadlet
// Pull= value. An explicit policy is honored verbatim; an unset policy maps
// to missing (pull only if the image isn't present locally) — the same
// neutral default podman quadlet uses, so q8s imposes nothing beyond what the
// user asked for. To get an image refresh on every restart, set
// imagePullPolicy: Always explicitly.
func pullPolicyToQuadlet(policy corev1.PullPolicy) string {
	switch policy {
	case corev1.PullNever:
		return "never"
	case corev1.PullAlways:
		return "always"
	case corev1.PullIfNotPresent:
		return "missing"
	default: // unset
		return "missing"
	}
}

// writeResourceLimits translates a Pod's resource limits into podman run
// flags via PodmanArgs=. All three (memory, swap, cpu) go through PodmanArgs
// rather than the native quadlet Memory= key: older podman quadlet
// generators (e.g. 5.4.2) reject the native Memory= key outright
// ("unsupported key Memory") and then emit NO unit at all, silently
// stranding the pod — whereas PodmanArgs is opaque passthrough every version
// accepts (it's already how --cpus is emitted). Same runtime effect as
// Memory= (both become `podman run --memory <bytes>`), but portable across
// podman versions. See tic-ecac.
//
// Limits are only emitted when the corresponding cgroup controller is
// delegated to the current process; on a rootless host where it isn't,
// setting the limit would make runc fail at container create, so it's
// silently skipped. resource.Quantity fields are validated by k8s's own
// JSON decoding before reaching here.
func writeResourceLimits(b *strings.Builder, limits corev1.ResourceList) {
	if mem, ok := limits[corev1.ResourceMemory]; ok && !mem.IsZero() && cgroupMemoryAvailable() {
		// --memory sets the limit; --memory-swap=-1 disables the swap
		// limit so runc doesn't try to write memory.swap.max, which
		// doesn't exist on cgroup v2 hosts without the swap controller.
		b.WriteString(fmt.Sprintf("PodmanArgs=--memory=%d --memory-swap=-1\n", mem.Value()))
	}
	if cpu, ok := limits[corev1.ResourceCPU]; ok && !cpu.IsZero() && cgroupCPUAvailable() {
		cores := float64(cpu.MilliValue()) / 1000.0
		b.WriteString(fmt.Sprintf("PodmanArgs=--cpus=%s\n", strconv.FormatFloat(cores, 'f', -1, 64)))
	}
}

// GPULimitResource is the extended resource name Kubernetes uses to request
// an NVIDIA GPU. q8s does not allocate individual devices: any request of at
// least one maps to the CDI device name nvidia.com/gpu=all, which podman
// resolves to every GPU CDI exposes on the host.
const GPULimitResource corev1.ResourceName = "nvidia.com/gpu"

// writeGPUDevice emits AddDevice= when the container requests a GPU. A zero
// or negative request emits nothing (Kubernetes only admits whole GPUs, but
// a malformed spec should not accidentally expose the host devices).
func writeGPUDevice(b *strings.Builder, limits corev1.ResourceList) {
	gpu, ok := limits[GPULimitResource]
	if !ok || gpu.CmpInt64(1) < 0 {
		return
	}
	b.WriteString("AddDevice=nvidia.com/gpu=all\n")
}

// writeUlimits translates the q8s.io/ulimits annotation into one Ulimit=
// line per comma separated item. Podman has no single key for setting
// memlock and stacking in a GPU LLM server usually needs the same limit on
// every replica, so the pod (or pod template) carries the list and each
// generated unit repeats it. Every item is validated first: an item with a
// newline would otherwise open a second quadlet line.
func writeUlimits(b *strings.Builder, annotations map[string]string) error {
	raw, ok := annotations[AnnotationUlimits]
	if !ok || raw == "" {
		return nil
	}
	for _, item := range strings.Split(raw, ",") {
		if err := validateUlimitItem(item); err != nil {
			return err
		}
		b.WriteString(fmt.Sprintf("Ulimit=%s\n", item))
	}
	return nil
}

// writeEmptyDirShm translates the Kubernetes idiom for a larger /dev/shm:
// an emptyDir with medium: Memory mounted at /dev/shm. Podman always mounts
// its own /dev/shm, so the size has to go through ShmSize= instead of a
// Volume= line (a bind mount there would be rejected or shadow the tmpfs).
// Returns true when the volume was handled, including the no-sizeLimit case
// where the right translation is to emit nothing at all. Other emptyDir
// volumes return false and keep the existing named-volume line.
func writeEmptyDirShm(b *strings.Builder, vol corev1.Volume, mountPath string) bool {
	if !isEmptyDirShm(vol, mountPath) {
		return false
	}
	if vol.EmptyDir.SizeLimit != nil && vol.EmptyDir.SizeLimit.Sign() > 0 {
		b.WriteString(fmt.Sprintf("ShmSize=%d\n", vol.EmptyDir.SizeLimit.Value()))
	}
	return true
}

// isEmptyDirShm reports whether the volume is a memory-backed emptyDir
// mounted at /dev/shm, the Kubernetes idiom for a larger shared-memory
// mount. Callers decide whether the size goes on the container or the pod.
func isEmptyDirShm(vol corev1.Volume, mountPath string) bool {
	return vol.EmptyDir != nil && vol.EmptyDir.Medium == corev1.StorageMediumMemory && mountPath == "/dev/shm"
}

// writeHealthCheck emits the single Podman health check a unit can have.
// A livenessProbe owns it when present, exactly as before (only its exec
// handler is translated). Otherwise a readinessProbe supplies one, which is
// what an LLM server needs: it can report ready only after the model has
// loaded, and until then podman keeps the container marked unhealthy rather
// than restarting it (the readiness probe has no restart semantics in k8s
// either).
func writeHealthCheck(b *strings.Builder, c corev1.Container) error {
	if probe := c.LivenessProbe; probe != nil {
		if cmd := probe.Exec; cmd != nil && len(cmd.Command) > 0 {
			for _, part := range cmd.Command {
				if err := noControlChars("healthCheck command", part); err != nil {
					return err
				}
			}
			b.WriteString(fmt.Sprintf("HealthCmd=%s\n", strings.Join(cmd.Command, " ")))
		}
		if probe.InitialDelaySeconds > 0 {
			b.WriteString(fmt.Sprintf("HealthStartPeriod=%ds\n", probe.InitialDelaySeconds))
		}
		return nil
	}

	probe := c.ReadinessProbe
	if probe == nil {
		return nil
	}
	if cmd := probe.Exec; cmd != nil && len(cmd.Command) > 0 {
		for _, part := range cmd.Command {
			if err := noControlChars("healthCheck command", part); err != nil {
				return err
			}
		}
		b.WriteString(fmt.Sprintf("HealthCmd=%s\n", strings.Join(cmd.Command, " ")))
		if probe.InitialDelaySeconds > 0 {
			b.WriteString(fmt.Sprintf("HealthStartPeriod=%ds\n", probe.InitialDelaySeconds))
		}
		return nil
	}
	if get := probe.HTTPGet; get != nil {
		port, ok := resolveProbePort(c, get.Port)
		if !ok {
			// An unresolved named port must not turn into a health check
			// against port 0: emitting nothing leaves the container without
			// a probe, which is safer than a probe that can never pass and
			// matches how the liveness branch silently skips non-exec probes.
			return nil
		}
		path := get.Path
		if path == "" {
			path = "/"
		}
		if err := noControlChars("readinessProbe path", path); err != nil {
			return err
		}
		b.WriteString(fmt.Sprintf("HealthCmd=curl -fsS http://127.0.0.1:%d%s\n", port, path))
		if probe.PeriodSeconds > 0 {
			b.WriteString(fmt.Sprintf("HealthInterval=%ds\n", probe.PeriodSeconds))
		}
		if probe.InitialDelaySeconds > 0 {
			b.WriteString(fmt.Sprintf("HealthStartPeriod=%ds\n", probe.InitialDelaySeconds))
		}
	}
	return nil
}

// resolveProbePort returns the numeric port an httpGet probe should hit. A
// named port is looked up in the container's own port list; the second
// result is false when the name matches nothing so the caller can skip the
// health check instead of guessing.
func resolveProbePort(c corev1.Container, port intstr.IntOrString) (int32, bool) {
	if port.Type == intstr.Int {
		return port.IntVal, true
	}
	for _, p := range c.Ports {
		if p.Name == port.StrVal {
			return p.ContainerPort, true
		}
	}
	return 0, false
}

// cgroupMemoryAvailable reports whether the cgroup v2 memory controller is
// delegated to the current process. On rootless systems this is often just
// "pids" unless the sysadmin enables memory delegation.
func cgroupMemoryAvailable() bool {
	cgroupOnce.Do(detectCgroupControllers)
	return cgroupHasMemory
}

// cgroupCPUAvailable reports whether the cgroup v2 cpu controller is delegated.
func cgroupCPUAvailable() bool {
	cgroupOnce.Do(detectCgroupControllers)
	return cgroupHasCPU
}

// cgroupControllersDetected is the combined set of detected cgroup controllers,
// exposed for testing. Tests can override cgroupHasMemory/cgroupHasCPU via
// SetCgroupOverride.
var (
	cgroupOnce      sync.Once
	cgroupHasMemory bool
	cgroupHasCPU    bool
)

// SetCgroupOverride forces the memory/cpu controller flags for testing.
// It returns a function that restores the original values.
func SetCgroupOverride(memory, cpu bool) func() {
	cgroupOnce.Do(func() {}) // ensure detectCgroupControllers won't run
	origMem, origCPU := cgroupHasMemory, cgroupHasCPU
	cgroupHasMemory = memory
	cgroupHasCPU = cpu
	return func() {
		cgroupHasMemory = origMem
		cgroupHasCPU = origCPU
	}
}

func detectCgroupControllers() {
	// Default to available — safe for root and cgroup v1 systems where the
	// detection below doesn't apply.
	cgroupHasMemory = true
	cgroupHasCPU = true

	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return
	}
	// cgroup v2 unified: line starts with "0::/"
	var cgroupPath string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "0::/") {
			cgroupPath = strings.TrimPrefix(line, "0::")
			break
		}
	}
	if cgroupPath == "" {
		return // cgroup v1 or unparseable — assume available
	}

	// Walk up from the process cgroup to find the nearest controllers file
	// that reflects what our user session can delegate. The process's own
	// cgroup.controllers shows what it inherited; the parent's
	// cgroup.subtree_control shows what new children can use.
	controllers, err := os.ReadFile("/sys/fs/cgroup" + cgroupPath + "/cgroup.controllers")
	if err != nil {
		return
	}

	fields := strings.Fields(string(controllers))
	cgroupHasMemory = false
	cgroupHasCPU = false
	for _, f := range fields {
		switch f {
		case "memory":
			cgroupHasMemory = true
		case "cpu":
			cgroupHasCPU = true
		}
	}
}

// Container generates a .container quadlet file content from a Pod.
// configDir is the base directory where ConfigMap files are written (may be
// empty). serviceAliases are the names of any Services whose selector
// matches this pod's labels — the caller resolves that match (it requires
// looking across Store objects, which this package has no access to), and
// each one becomes a Podman network alias so aardvark-dns resolves the
// Service name straight to this container.
// pvcMap maps PVC claim names to their full objects so that volume mounts
// can inspect storageClassName and annotations. A nil map is safe and falls
// back to the default (named volume with :Z).
//
// This is the single-container shape q8s has always produced. A pod with
// more than one container is rendered by PodUnits instead, which keeps the
// same per-container rendering but moves the pod-level settings into a .pod.
func Container(name string, pod *corev1.Pod, configDir string, serviceAliases []string, pvcMap map[string]*corev1.PersistentVolumeClaim, envFile string) ([]byte, error) {
	// Name/namespace are validated here even though the API boundary
	// already checks them: the Podman-label import path (reconcilePodmanPods)
	// constructs pods from container labels, which nothing else vets, and a
	// newline or "../" in them would inject unit-file lines or traverse the
	// output filename. validateRefName is the same RFC-1123 shape every
	// other reference in this file goes through.
	if err := validateRefName("pod name", name); err != nil {
		return nil, err
	}
	if err := validateRefName("pod namespace", pod.Namespace); err != nil {
		return nil, err
	}
	if len(pod.Spec.Containers) == 0 {
		return nil, fmt.Errorf("pod has no containers")
	}
	content, err := renderContainer(containerRender{
		pod:            pod,
		container:      pod.Spec.Containers[0],
		containerName:  fmt.Sprintf("%s-%s", pod.Namespace, name),
		configDir:      configDir,
		serviceAliases: serviceAliases,
		pvcMap:         pvcMap,
		envFile:        envFile,
		standalone:     true,
	})
	if err != nil {
		return nil, err
	}
	return withTemplateHash(content), nil
}

// JobContainer generates a .container quadlet for a Job's pod template.
// The unit runs once (no Restart) and exits with the container's exit code.
func JobContainer(name string, job *batchv1.Job, configDir string, pvcMap map[string]*corev1.PersistentVolumeClaim, envFile string) ([]byte, error) {
	spec := job.Spec.Template.Spec
	ns := job.Namespace

	if err := validateRefName("job name", name); err != nil {
		return nil, err
	}
	if err := validateRefName("job namespace", ns); err != nil {
		return nil, err
	}

	var b strings.Builder
	b.WriteString("[Container]\n")

	if len(spec.Containers) == 0 {
		return nil, fmt.Errorf("job has no containers")
	}
	if err := validateImage(spec.Containers[0].Image); err != nil {
		return nil, err
	}
	writeImageAndPull(&b, spec.Containers[0])
	b.WriteString(fmt.Sprintf("ContainerName=%s-%s-job\n", ns, name))

	if cmd := append(spec.Containers[0].Command, spec.Containers[0].Args...); len(cmd) > 0 {
		for _, part := range cmd {
			if err := noControlChars("command", part); err != nil {
				return nil, err
			}
		}
		b.WriteString(fmt.Sprintf("Exec=%s\n", shellJoin(cmd)))
	}

	if spec.Containers[0].WorkingDir != "" {
		if err := validatePath("workingDir", spec.Containers[0].WorkingDir); err != nil {
			return nil, err
		}
		b.WriteString(fmt.Sprintf("WorkingDir=%s\n", spec.Containers[0].WorkingDir))
	}

	writeResourceLimits(&b, spec.Containers[0].Resources.Limits)

	if envFile != "" {
		if err := validatePath("envFile", envFile); err != nil {
			return nil, err
		}
		b.WriteString(fmt.Sprintf("EnvironmentFile=%s\n", envFile))
	}

	for _, env := range spec.Containers[0].Env {
		if err := validateEnvName(env.Name); err != nil {
			return nil, err
		}
		line, err := envLine(env.Name, env.Value)
		if err != nil {
			return nil, err
		}
		b.WriteString(line)
	}

	for _, vol := range spec.Volumes {
		for _, c := range spec.Containers {
			for _, vm := range c.VolumeMounts {
				if vm.Name != vol.Name {
					continue
				}
				if err := validatePath("mountPath", vm.MountPath); err != nil {
					return nil, err
				}
				if writeEmptyDirShm(&b, vol, vm.MountPath) {
					continue
				}
				readOnly := volumeMountReadOnly(vm, vol)
				switch {
				case vol.PersistentVolumeClaim != nil:
					if err := validateRefName("persistentVolumeClaim.claimName", vol.PersistentVolumeClaim.ClaimName); err != nil {
						return nil, err
					}
					if pvc, ok := pvcMap[vol.PersistentVolumeClaim.ClaimName]; ok {
						line, err := pvcVolumeDirective(ns, pvc, vm.MountPath, readOnly)
						if err != nil {
							return nil, err
						}
						b.WriteString(line)
					} else {
						b.WriteString(fmt.Sprintf("Volume=%s-%s.volume:%s%s\n", ns, vol.PersistentVolumeClaim.ClaimName, vm.MountPath, mountOptions(readOnly, "Z")))
					}
				case vol.ConfigMap != nil:
					if err := validateRefName("configMap.name", vol.ConfigMap.Name); err != nil {
						return nil, err
					}
					if configDir != "" {
						cmPath := fmt.Sprintf("%s/%s/%s", configDir, ns, vol.ConfigMap.Name)
						b.WriteString(fmt.Sprintf("Volume=%s:%s:ro,z\n", cmPath, vm.MountPath))
					}
				case vol.Secret != nil:
					if err := validateRefName("secret.secretName", vol.Secret.SecretName); err != nil {
						return nil, err
					}
					if configDir != "" {
						secretDir := filepath.Join(filepath.Dir(configDir), "secrets")
						secPath := filepath.Join(secretDir, ns, vol.Secret.SecretName)
						b.WriteString(fmt.Sprintf("Volume=%s:%s:ro,z\n", secPath, vm.MountPath))
					}
				default:
					if err := validateRefName("volume.name", vol.Name); err != nil {
						return nil, err
					}
					b.WriteString(fmt.Sprintf("Volume=%s:%s%s\n", vol.Name, vm.MountPath, mountOptions(readOnly, "")))
				}
			}
		}
	}

	if spec.HostNetwork {
		b.WriteString("Network=host\n")
	} else {
		b.WriteString(fmt.Sprintf("Network=q8s-%s.network\n", ns))
	}

	b.WriteString(fmt.Sprintf("Label=io.kubernetes.job.name=%s\n", job.Name))
	b.WriteString(fmt.Sprintf("Label=io.kubernetes.pod.namespace=%s\n", ns))
	b.WriteString(ManagedByLabel + "\n")

	writeGPUDevice(&b, spec.Containers[0].Resources.Limits)
	if err := writeUlimits(&b, job.Spec.Template.Annotations); err != nil {
		return nil, err
	}
	if err := writeHealthCheck(&b, spec.Containers[0]); err != nil {
		return nil, err
	}

	b.WriteString("\n[Unit]\n")
	b.WriteString(fmt.Sprintf("Description=Job %s/%s\n", ns, name))
	// Order after q8s-api (see the Container generator) so a reboot doesn't
	// race the --env-file target q8s re-renders before READY. tic-1507.
	b.WriteString("Wants=q8s-api.service\n")
	b.WriteString("After=q8s-api.service\n")

	// Jobs don't restart; Type=oneshot is handled by the container exiting cleanly.
	b.WriteString("\n[Service]\n")
	b.WriteString("Restart=no\n")

	// No [Install] — the job is started on demand, not by a target.

	return withTemplateHash([]byte(b.String())), nil
}

// CronContainer generates the .container quadlet for a CronJob's pod template.
// It is triggered by the paired .timer unit, not installed directly.
func CronContainer(name string, cj *batchv1.CronJob, configDir string, pvcMap map[string]*corev1.PersistentVolumeClaim, envFile string) ([]byte, error) {
	spec := cj.Spec.JobTemplate.Spec.Template.Spec
	ns := cj.Namespace

	if err := validateRefName("cronjob name", name); err != nil {
		return nil, err
	}
	if err := validateRefName("cronjob namespace", ns); err != nil {
		return nil, err
	}

	var b strings.Builder
	b.WriteString("[Container]\n")

	if len(spec.Containers) == 0 {
		return nil, fmt.Errorf("cronjob has no containers")
	}
	if err := validateImage(spec.Containers[0].Image); err != nil {
		return nil, err
	}
	writeImageAndPull(&b, spec.Containers[0])
	b.WriteString(fmt.Sprintf("ContainerName=%s-%s-cron\n", ns, name))

	if cmd := append(spec.Containers[0].Command, spec.Containers[0].Args...); len(cmd) > 0 {
		for _, part := range cmd {
			if err := noControlChars("command", part); err != nil {
				return nil, err
			}
		}
		b.WriteString(fmt.Sprintf("Exec=%s\n", shellJoin(cmd)))
	}

	if spec.Containers[0].WorkingDir != "" {
		if err := validatePath("workingDir", spec.Containers[0].WorkingDir); err != nil {
			return nil, err
		}
		b.WriteString(fmt.Sprintf("WorkingDir=%s\n", spec.Containers[0].WorkingDir))
	}

	writeResourceLimits(&b, spec.Containers[0].Resources.Limits)

	if envFile != "" {
		if err := validatePath("envFile", envFile); err != nil {
			return nil, err
		}
		b.WriteString(fmt.Sprintf("EnvironmentFile=%s\n", envFile))
	}

	for _, env := range spec.Containers[0].Env {
		if err := validateEnvName(env.Name); err != nil {
			return nil, err
		}
		line, err := envLine(env.Name, env.Value)
		if err != nil {
			return nil, err
		}
		b.WriteString(line)
	}

	for _, vol := range spec.Volumes {
		for _, c := range spec.Containers {
			for _, vm := range c.VolumeMounts {
				if vm.Name != vol.Name {
					continue
				}
				if err := validatePath("mountPath", vm.MountPath); err != nil {
					return nil, err
				}
				if writeEmptyDirShm(&b, vol, vm.MountPath) {
					continue
				}
				readOnly := volumeMountReadOnly(vm, vol)
				switch {
				case vol.PersistentVolumeClaim != nil:
					if err := validateRefName("persistentVolumeClaim.claimName", vol.PersistentVolumeClaim.ClaimName); err != nil {
						return nil, err
					}
					if pvc, ok := pvcMap[vol.PersistentVolumeClaim.ClaimName]; ok {
						line, err := pvcVolumeDirective(ns, pvc, vm.MountPath, readOnly)
						if err != nil {
							return nil, err
						}
						b.WriteString(line)
					} else {
						b.WriteString(fmt.Sprintf("Volume=%s-%s.volume:%s%s\n", ns, vol.PersistentVolumeClaim.ClaimName, vm.MountPath, mountOptions(readOnly, "Z")))
					}
				case vol.ConfigMap != nil:
					if err := validateRefName("configMap.name", vol.ConfigMap.Name); err != nil {
						return nil, err
					}
					if configDir != "" {
						cmPath := fmt.Sprintf("%s/%s/%s", configDir, ns, vol.ConfigMap.Name)
						b.WriteString(fmt.Sprintf("Volume=%s:%s:ro,z\n", cmPath, vm.MountPath))
					}
				case vol.Secret != nil:
					if err := validateRefName("secret.secretName", vol.Secret.SecretName); err != nil {
						return nil, err
					}
					if configDir != "" {
						secretDir := filepath.Join(filepath.Dir(configDir), "secrets")
						secPath := filepath.Join(secretDir, ns, vol.Secret.SecretName)
						b.WriteString(fmt.Sprintf("Volume=%s:%s:ro,z\n", secPath, vm.MountPath))
					}
				default:
					if err := validateRefName("volume.name", vol.Name); err != nil {
						return nil, err
					}
					b.WriteString(fmt.Sprintf("Volume=%s:%s%s\n", vol.Name, vm.MountPath, mountOptions(readOnly, "")))
				}
			}
		}
	}

	if spec.HostNetwork {
		b.WriteString("Network=host\n")
	} else {
		b.WriteString(fmt.Sprintf("Network=q8s-%s.network\n", ns))
	}

	b.WriteString(fmt.Sprintf("Label=io.kubernetes.cronjob.name=%s\n", cj.Name))
	b.WriteString(fmt.Sprintf("Label=io.kubernetes.pod.namespace=%s\n", ns))
	b.WriteString(ManagedByLabel + "\n")

	writeGPUDevice(&b, spec.Containers[0].Resources.Limits)
	if err := writeUlimits(&b, cj.Spec.JobTemplate.Spec.Template.Annotations); err != nil {
		return nil, err
	}
	if err := writeHealthCheck(&b, spec.Containers[0]); err != nil {
		return nil, err
	}

	b.WriteString("\n[Unit]\n")
	b.WriteString(fmt.Sprintf("Description=CronJob %s/%s\n", ns, name))
	// Order after q8s-api (see the Container generator) so a reboot doesn't
	// race the --env-file target q8s re-renders before READY. tic-1507.
	b.WriteString("Wants=q8s-api.service\n")
	b.WriteString("After=q8s-api.service\n")

	b.WriteString("\n[Service]\n")
	b.WriteString("Restart=no\n")

	// No [Install] — activated by the timer.

	return withTemplateHash([]byte(b.String())), nil
}

// CronTimer generates the .timer quadlet for a CronJob.
// The timer unit name must match the container unit name so systemd links them.
func CronTimer(name string, cj *batchv1.CronJob) ([]byte, error) {
	if err := validateCronSchedule(cj.Spec.Schedule); err != nil {
		return nil, err
	}

	var b strings.Builder

	b.WriteString("# app.kubernetes.io/managed-by=q8s\n")
	b.WriteString("[Unit]\n")
	b.WriteString(fmt.Sprintf("Description=Timer for CronJob %s/%s\n", cj.Namespace, name))

	b.WriteString("\n[Timer]\n")
	for _, cal := range cronToCalendars(cj.Spec.Schedule) {
		b.WriteString(fmt.Sprintf("OnCalendar=%s\n", cal))
	}
	b.WriteString("Persistent=true\n")

	b.WriteString("\n[Install]\n")
	b.WriteString("WantedBy=timers.target\n")

	return []byte(b.String()), nil
}

// cronToCalendars converts a 5-field cron expression into one or two systemd
// calendar expressions for OnCalendar=.
//
// The mapping is not a field-by-field rename; three systemd/cron differences
// matter:
//
//   - Bases: minute/hour are 0-based in both, but systemd months and days
//     start at 1. A blanket */N → 0/N rewrite (as an earlier version did)
//     produces *-0/2-* for "every other month", which systemd rejects. Time
//     fields keep the step shorthand (0/5); date fields are expanded into
//     explicit comma lists, which every systemd version parses.
//   - Day-of-week numbering: cron uses 0=Sunday…6=Saturday (7 also Sunday);
//     systemd uses 1=Monday…7=Sunday. Values are remapped, and when every
//     day is selected the DOW prefix is omitted.
//   - Combination semantics: cron ORs day-of-month with day-of-week when
//     both are restricted ("0 0 1 * 1" = the 1st of the month *or* every
//     Monday), while a single OnCalendar ANDs them. Two expressions cover
//     it: systemd timers fire on any of their OnCalendar= lines, and a
//     timer with both lines is exactly the union cron computes.
func cronToCalendars(cron string) []string {
	fields := strings.Fields(cron)
	if len(fields) != 5 {
		return []string{cron}
	}
	minute, hour, dom, month, dow := fields[0], fields[1], fields[2], fields[3], fields[4]

	// Time fields: keep cron step syntax, rebased to 0 ("0/5" = start at 0,
	// every 5 — the documented systemd shorthand for time components).
	step0 := func(f string) string {
		if f == "*" || f == "*/1" {
			return "*"
		}
		if strings.HasPrefix(f, "*/") {
			return "0/" + f[2:]
		}
		return f
	}
	timeSpec := fmt.Sprintf("%s:%s:00", step0(hour), step0(minute))

	// Date fields: expand to explicit lists (unambiguous on every systemd;
	// no per-field step-base to get wrong).
	list := func(f string, min, max int) string {
		if f == "*" {
			return "*"
		}
		vals, err := expandCronField(f, min, max)
		if err != nil {
			return f // unreachable: validateCronSchedule rejects it first
		}
		parts := make([]string, len(vals))
		for i, v := range vals {
			parts[i] = strconv.Itoa(v)
		}
		return strings.Join(parts, ",")
	}
	dateSpec := fmt.Sprintf("*-%s-%s", list(month, 1, 12), list(dom, 1, 31))

	// Day-of-week: expand, remap cron Sunday (0) to systemd 7, drop the
	// prefix when all seven days are selected.
	dowSpec := ""
	if days, err := expandCronField(dow, 0, 7); err == nil {
		set := make(map[int]bool, len(days))
		for _, d := range days {
			if d == 0 {
				d = 7
			}
			set[d] = true
		}
		if len(set) < 7 {
			parts := make([]string, 0, len(set))
			for d := 1; d <= 7; d++ {
				if set[d] {
					parts = append(parts, strconv.Itoa(d))
				}
			}
			dowSpec = strings.Join(parts, ",")
		}
	}

	switch {
	case dowSpec == "":
		// No (or every) day-of-week: date alone.
		return []string{dateSpec + " " + timeSpec}
	case dom != "*" && dow != "*":
		// Cron ORs the two restricted day fields; two OnCalendar lines
		// produce the same union in systemd.
		return []string{
			dowSpec + " *-*-* " + timeSpec,
			dateSpec + " " + timeSpec,
		}
	default:
		return []string{dowSpec + " " + dateSpec + " " + timeSpec}
	}
}

// Storage class constants recognised by q8s.
const (
	// StorageClassStandard creates a podman named volume with exclusive
	// SELinux relabelling (:Z). This is the default when no storageClassName
	// is set.
	StorageClassStandard = "standard"

	// StorageClassShared creates a podman named volume with shared
	// SELinux relabelling (:z), allowing multiple containers to access it.
	StorageClassShared = "standard-shared"

	// StorageClassHostPath bind-mounts a host directory into the container.
	// The host path is read from the annotation AnnotationHostPath on the PVC.
	StorageClassHostPath = "hostpath"

	// AnnotationHostPath is the PVC annotation key that supplies the host
	// directory for the "hostpath" storage class.
	AnnotationHostPath = "q8s.io/host-path"

	// AnnotationSELinuxLabel selects how a hostpath PVC is relabelled:
	// "private" (the default, :Z) or "shared" (:z). A hostpath directory
	// is potentially used by other containers or services on the host, so
	// making :Z the default would relabel it for one pod and lock every
	// other user out.
	AnnotationSELinuxLabel = "q8s.io/selinux-label"

	// AnnotationUlimits carries a comma separated list of podman Ulimit=
	// items on a pod or pod template, for example
	// "memlock=-1:-1,nofile=65536".
	AnnotationUlimits = "q8s.io/ulimits"
)

// pvcStorageClass returns the effective storage class for a PVC, defaulting
// to "standard" when the field is nil or empty.
func pvcStorageClass(pvc *corev1.PersistentVolumeClaim) string {
	if pvc.Spec.StorageClassName != nil && *pvc.Spec.StorageClassName != "" {
		return *pvc.Spec.StorageClassName
	}
	return StorageClassStandard
}

// PVCVolumeName is the Podman named volume backing a PVC. Namespace-scoped
// so two claims with the same name in different namespaces (or from
// unrelated non-q8s deployments on the same host) can't silently collide on
// one volume — and so `podman volume ls` self-documents which claim owns
// what instead of requiring a live cross-reference.
func PVCVolumeName(ns, name string) string {
	return fmt.Sprintf("%s-%s", ns, name)
}

// Volume generates a .volume quadlet file content.
// For hostpath PVCs no .volume file is needed — the caller should skip
// writing when nil bytes are returned with no error.
func Volume(pvc *corev1.PersistentVolumeClaim) ([]byte, error) {
	if pvcStorageClass(pvc) == StorageClassHostPath {
		return nil, nil
	}

	var b strings.Builder

	b.WriteString("[Volume]\n")
	b.WriteString(fmt.Sprintf("VolumeName=%s\n", PVCVolumeName(pvc.Namespace, pvc.Name)))
	b.WriteString(ManagedByLabel + "\n")

	return []byte(b.String()), nil
}

// pvcSELinuxRelabel returns the SELinux relabel suffix for a hostpath PVC.
// A missing annotation keeps the historical private label (:Z); "shared"
// selects :z so a directory can be mounted by several containers without one
// pod taking ownership of the host labels. Anything else is rejected rather
// than interpolated, because this value becomes part of a quadlet line.
func pvcSELinuxRelabel(pvc *corev1.PersistentVolumeClaim) (string, error) {
	mode, ok := pvc.Annotations[AnnotationSELinuxLabel]
	if !ok {
		return "Z", nil
	}
	switch mode {
	case "private":
		return "Z", nil
	case "shared":
		return "z", nil
	default:
		return "", fmt.Errorf("PVC %s/%s has invalid %s annotation %q (must be private or shared)",
			pvc.Namespace, pvc.Name, AnnotationSELinuxLabel, mode)
	}
}

// volumeMountReadOnly reports whether a mount must be read-only. Kubernetes
// forces read-only when either the volume mount or the PVC volume source asks
// for it.
func volumeMountReadOnly(vm corev1.VolumeMount, vol corev1.Volume) bool {
	if vm.ReadOnly {
		return true
	}
	return vol.PersistentVolumeClaim != nil && vol.PersistentVolumeClaim.ReadOnly
}

// mountOptions returns the colon-prefixed mount option suffix for a Volume=
// line. readOnly adds the podman ro option; relabel is the SELinux suffix
// (Z or z) or empty when the directive carries no SELinux relabel. The empty
// result keeps directives that had no options byte-identical.
func mountOptions(readOnly bool, relabel string) string {
	opts := relabel
	if readOnly {
		if opts == "" {
			opts = "ro"
		} else {
			opts = "ro," + opts
		}
	}
	if opts == "" {
		return ""
	}
	return ":" + opts
}

// pvcVolumeDirective returns the Volume= value for a PVC-backed volume mount.
// It inspects the PVC's storageClassName and annotations to decide between a
// named-volume reference (with :Z or :z) and a hostpath bind mount, and adds
// the ro option when readOnly is set.
func pvcVolumeDirective(ns string, pvc *corev1.PersistentVolumeClaim, mountPath string, readOnly bool) (string, error) {
	class := pvcStorageClass(pvc)
	switch class {
	case StorageClassStandard:
		return fmt.Sprintf("Volume=%s-%s.volume:%s%s\n", ns, pvc.Name, mountPath, mountOptions(readOnly, "Z")), nil
	case StorageClassShared:
		return fmt.Sprintf("Volume=%s-%s.volume:%s%s\n", ns, pvc.Name, mountPath, mountOptions(readOnly, "z")), nil
	case StorageClassHostPath:
		hostPath, ok := pvc.Annotations[AnnotationHostPath]
		if !ok || hostPath == "" {
			return "", fmt.Errorf("PVC %s/%s uses storageClass %q but is missing annotation %s",
				ns, pvc.Name, StorageClassHostPath, AnnotationHostPath)
		}
		if err := validatePath("host-path annotation", hostPath); err != nil {
			return "", err
		}
		relabel, err := pvcSELinuxRelabel(pvc)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Volume=%s:%s%s\n", hostPath, mountPath, mountOptions(readOnly, relabel)), nil
	default:
		// Unknown class — fall back to named volume without SELinux option
		// for forward compatibility.
		return fmt.Sprintf("Volume=%s-%s.volume:%s%s\n", ns, pvc.Name, mountPath, mountOptions(readOnly, "")), nil
	}
}

// Network generates a .network quadlet file content.
// The Podman network is named "q8s-{name}" to avoid conflicts with reserved names like "default".
func Network(name string) ([]byte, error) {
	var b strings.Builder

	b.WriteString("[Network]\n")
	b.WriteString(fmt.Sprintf("NetworkName=q8s-%s\n", name))
	b.WriteString(ManagedByLabel + "\n")

	return []byte(b.String()), nil
}

// shellJoin joins command parts for use in Exec= directives.
// Parts containing spaces or special characters are quoted.
func shellJoin(parts []string) string {
	var out strings.Builder
	for i, p := range parts {
		if i > 0 {
			out.WriteByte(' ')
		}
		if strings.ContainsAny(p, " \t\n\"'\\$`!") {
			out.WriteByte('"')
			out.WriteString(strings.ReplaceAll(p, `"`, `\"`))
			out.WriteByte('"')
		} else {
			out.WriteString(p)
		}
	}
	return out.String()
}
