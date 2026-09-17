package quadlet

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

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

	var b strings.Builder

	b.WriteString("[Container]\n")

	if len(pod.Spec.Containers) == 0 {
		return nil, fmt.Errorf("pod has no containers")
	}
	if err := validateImage(pod.Spec.Containers[0].Image); err != nil {
		return nil, err
	}
	b.WriteString(fmt.Sprintf("Image=%s\n", pod.Spec.Containers[0].Image))

	containerName := fmt.Sprintf("%s-%s", pod.Namespace, name)
	b.WriteString(fmt.Sprintf("ContainerName=%s\n", containerName))

	// Exec: combine command (entrypoint override) and args
	cmd := append(pod.Spec.Containers[0].Command, pod.Spec.Containers[0].Args...)
	for _, part := range cmd {
		if err := noControlChars("command", part); err != nil {
			return nil, err
		}
	}
	if len(cmd) > 0 {
		b.WriteString(fmt.Sprintf("Exec=%s\n", shellJoin(cmd)))
	}

	if pod.Spec.Containers[0].WorkingDir != "" {
		if err := validatePath("workingDir", pod.Spec.Containers[0].WorkingDir); err != nil {
			return nil, err
		}
		b.WriteString(fmt.Sprintf("WorkingDir=%s\n", pod.Spec.Containers[0].WorkingDir))
	}

	if pod.Spec.Containers[0].SecurityContext != nil && pod.Spec.Containers[0].SecurityContext.RunAsUser != nil {
		b.WriteString(fmt.Sprintf("User=%d\n", *pod.Spec.Containers[0].SecurityContext.RunAsUser))
	}

	writeResourceLimits(&b, pod.Spec.Containers[0].Resources.Limits)

	if envFile != "" {
		if err := validatePath("envFile", envFile); err != nil {
			return nil, err
		}
		b.WriteString(fmt.Sprintf("EnvironmentFile=%s\n", envFile))
	}

	for _, env := range pod.Spec.Containers[0].Env {
		if err := validateEnvName(env.Name); err != nil {
			return nil, err
		}
		if err := noControlChars("environment variable value", env.Value); err != nil {
			return nil, err
		}
		b.WriteString(fmt.Sprintf("Environment=%s=%s\n", env.Name, env.Value))
	}

	// Ports — only publish when hostPort is explicitly set.
	// containerPort alone is informational (matches k8s semantics):
	// the container listens on that port inside the namespace network,
	// reachable via NetworkAlias from other pods.
	for _, port := range pod.Spec.Containers[0].Ports {
		if port.HostPort == 0 {
			continue
		}
		proto := "tcp"
		if port.Protocol == corev1.ProtocolUDP {
			proto = "udp"
		}
		// An explicit HostIP binds the published port to one interface —
		// the q8s replica-port allocator uses 127.0.0.1 so replica backends
		// are reachable on-host (Traefik) but never exposed on the LAN.
		// Without HostIP the publish binds all interfaces, matching k8s
		// hostPort semantics for manually-declared ports.
		if port.HostIP != "" {
			b.WriteString(fmt.Sprintf("PublishPort=%s:%d:%d/%s\n", port.HostIP, port.HostPort, port.ContainerPort, proto))
		} else {
			b.WriteString(fmt.Sprintf("PublishPort=%d:%d/%s\n", port.HostPort, port.ContainerPort, proto))
		}
	}

	// Volumes — resolve ConfigMap references to their on-disk directory.
	for _, vol := range pod.Spec.Volumes {
		for _, c := range pod.Spec.Containers {
			for _, vm := range c.VolumeMounts {
				if vm.Name != vol.Name {
					continue
				}
				if err := validatePath("mountPath", vm.MountPath); err != nil {
					return nil, err
				}
				switch {
				case vol.PersistentVolumeClaim != nil:
					if err := validateRefName("persistentVolumeClaim.claimName", vol.PersistentVolumeClaim.ClaimName); err != nil {
						return nil, err
					}
					if pvc, ok := pvcMap[vol.PersistentVolumeClaim.ClaimName]; ok {
						line, err := pvcVolumeDirective(pod.Namespace, pvc, vm.MountPath)
						if err != nil {
							return nil, err
						}
						b.WriteString(line)
					} else {
						// PVC not found in map — fall back to named volume with :Z
						b.WriteString(fmt.Sprintf("Volume=%s-%s.volume:%s:Z\n", pod.Namespace, vol.PersistentVolumeClaim.ClaimName, vm.MountPath))
					}
				case vol.ConfigMap != nil:
					if err := validateRefName("configMap.name", vol.ConfigMap.Name); err != nil {
						return nil, err
					}
					if configDir != "" {
						cmPath := fmt.Sprintf("%s/%s/%s", configDir, pod.Namespace, vol.ConfigMap.Name)
						b.WriteString(fmt.Sprintf("Volume=%s:%s:ro,z\n", cmPath, vm.MountPath))
					}
				case vol.Secret != nil:
					if err := validateRefName("secret.secretName", vol.Secret.SecretName); err != nil {
						return nil, err
					}
					if configDir != "" {
						secretDir := filepath.Join(filepath.Dir(configDir), "secrets")
						secPath := filepath.Join(secretDir, pod.Namespace, vol.Secret.SecretName)
						b.WriteString(fmt.Sprintf("Volume=%s:%s:ro,z\n", secPath, vm.MountPath))
					}
				default:
					if err := validateRefName("volume.name", vol.Name); err != nil {
						return nil, err
					}
					b.WriteString(fmt.Sprintf("Volume=%s:%s\n", vol.Name, vm.MountPath))
				}
			}
		}
	}

	// Network: use host networking when requested, otherwise use the namespace network.
	// NetworkAlias only makes sense on the namespace bridge — host networking
	// has no separate network namespace for aardvark-dns to resolve within.
	if pod.Spec.HostNetwork {
		b.WriteString("Network=host\n")
	} else {
		b.WriteString(fmt.Sprintf("Network=q8s-%s.network\n", pod.Namespace))
		for _, alias := range serviceAliases {
			if err := validateRefName("service alias", alias); err != nil {
				return nil, err
			}
			b.WriteString(fmt.Sprintf("NetworkAlias=%s\n", alias))
		}
	}

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
	for k, v := range pod.Labels {
		if err := validateLabelPair(k, v); err != nil {
			return nil, err
		}
		b.WriteString(fmt.Sprintf("Label=%s=%s\n", k, v))
	}

	if pod.Spec.Containers[0].LivenessProbe != nil {
		if cmd := pod.Spec.Containers[0].LivenessProbe.Exec; cmd != nil && len(cmd.Command) > 0 {
			for _, part := range cmd.Command {
				if err := noControlChars("healthCheck command", part); err != nil {
					return nil, err
				}
			}
			b.WriteString(fmt.Sprintf("HealthCmd=%s\n", strings.Join(cmd.Command, " ")))
		}
		if pod.Spec.Containers[0].LivenessProbe.InitialDelaySeconds > 0 {
			b.WriteString(fmt.Sprintf("HealthStartPeriod=%ds\n", pod.Spec.Containers[0].LivenessProbe.InitialDelaySeconds))
		}
	}

	b.WriteString("\n[Unit]\n")
	b.WriteString(fmt.Sprintf("Description=Pod %s\n", pod.Name))

	// Every policy maps to an explicit Restart= line. Emitting nothing for
	// Never (as older code did) is not neutral: quadlet/.container units
	// default to on-failure, so a "run once, never restart" pod restarted
	// on failure anyway. Defaulting empty to Always matches real k8s pod
	// defaulting — but that normalization happens at the API boundary
	// (handler POST/PATCH); the generator treats empty defensively the same
	// way.
	switch pod.Spec.RestartPolicy {
	case corev1.RestartPolicyNever:
		b.WriteString("\n[Service]\n")
		b.WriteString("Restart=no\n")
	default: // Always, OnFailure, empty (API defaulting) — restart
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
	b.WriteString(fmt.Sprintf("Image=%s\n", spec.Containers[0].Image))
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
		if err := noControlChars("environment variable value", env.Value); err != nil {
			return nil, err
		}
		b.WriteString(fmt.Sprintf("Environment=%s=%s\n", env.Name, env.Value))
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
				switch {
				case vol.PersistentVolumeClaim != nil:
					if err := validateRefName("persistentVolumeClaim.claimName", vol.PersistentVolumeClaim.ClaimName); err != nil {
						return nil, err
					}
					if pvc, ok := pvcMap[vol.PersistentVolumeClaim.ClaimName]; ok {
						line, err := pvcVolumeDirective(ns, pvc, vm.MountPath)
						if err != nil {
							return nil, err
						}
						b.WriteString(line)
					} else {
						b.WriteString(fmt.Sprintf("Volume=%s-%s.volume:%s:Z\n", ns, vol.PersistentVolumeClaim.ClaimName, vm.MountPath))
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
					b.WriteString(fmt.Sprintf("Volume=%s:%s\n", vol.Name, vm.MountPath))
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

	b.WriteString("\n[Unit]\n")
	b.WriteString(fmt.Sprintf("Description=Job %s/%s\n", ns, name))

	// Jobs don't restart; Type=oneshot is handled by the container exiting cleanly.
	b.WriteString("\n[Service]\n")
	b.WriteString("Restart=no\n")

	// No [Install] — the job is started on demand, not by a target.

	return []byte(b.String()), nil
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
	b.WriteString(fmt.Sprintf("Image=%s\n", spec.Containers[0].Image))
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
		if err := noControlChars("environment variable value", env.Value); err != nil {
			return nil, err
		}
		b.WriteString(fmt.Sprintf("Environment=%s=%s\n", env.Name, env.Value))
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
				switch {
				case vol.PersistentVolumeClaim != nil:
					if err := validateRefName("persistentVolumeClaim.claimName", vol.PersistentVolumeClaim.ClaimName); err != nil {
						return nil, err
					}
					if pvc, ok := pvcMap[vol.PersistentVolumeClaim.ClaimName]; ok {
						line, err := pvcVolumeDirective(ns, pvc, vm.MountPath)
						if err != nil {
							return nil, err
						}
						b.WriteString(line)
					} else {
						b.WriteString(fmt.Sprintf("Volume=%s-%s.volume:%s:Z\n", ns, vol.PersistentVolumeClaim.ClaimName, vm.MountPath))
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
					b.WriteString(fmt.Sprintf("Volume=%s:%s\n", vol.Name, vm.MountPath))
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

	b.WriteString("\n[Unit]\n")
	b.WriteString(fmt.Sprintf("Description=CronJob %s/%s\n", ns, name))

	b.WriteString("\n[Service]\n")
	b.WriteString("Restart=no\n")

	// No [Install] — activated by the timer.

	return []byte(b.String()), nil
}

// CronTimer generates the .timer quadlet for a CronJob.
// The timer unit name must match the container unit name so systemd links them.
func CronTimer(name string, cj *batchv1.CronJob) ([]byte, error) {
	if err := validateCronSchedule(cj.Spec.Schedule); err != nil {
		return nil, err
	}

	var b strings.Builder

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

	return []byte(b.String()), nil
}

// pvcVolumeDirective returns the Volume= value for a PVC-backed volume mount.
// It inspects the PVC's storageClassName and annotations to decide between a
// named-volume reference (with :Z or :z) and a hostpath bind mount.
func pvcVolumeDirective(ns string, pvc *corev1.PersistentVolumeClaim, mountPath string) (string, error) {
	class := pvcStorageClass(pvc)
	switch class {
	case StorageClassStandard:
		return fmt.Sprintf("Volume=%s-%s.volume:%s:Z\n", ns, pvc.Name, mountPath), nil
	case StorageClassShared:
		return fmt.Sprintf("Volume=%s-%s.volume:%s:z\n", ns, pvc.Name, mountPath), nil
	case StorageClassHostPath:
		hostPath, ok := pvc.Annotations[AnnotationHostPath]
		if !ok || hostPath == "" {
			return "", fmt.Errorf("PVC %s/%s uses storageClass %q but is missing annotation %s",
				ns, pvc.Name, StorageClassHostPath, AnnotationHostPath)
		}
		if err := validatePath("host-path annotation", hostPath); err != nil {
			return "", err
		}
		return fmt.Sprintf("Volume=%s:%s:Z\n", hostPath, mountPath), nil
	default:
		// Unknown class — fall back to named volume without SELinux option
		// for forward compatibility.
		return fmt.Sprintf("Volume=%s-%s.volume:%s\n", ns, pvc.Name, mountPath), nil
	}
}

// Network generates a .network quadlet file content.
// The Podman network is named "q8s-{name}" to avoid conflicts with reserved names like "default".
func Network(name string) ([]byte, error) {
	var b strings.Builder

	b.WriteString("[Network]\n")
	b.WriteString(fmt.Sprintf("NetworkName=q8s-%s\n", name))

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
