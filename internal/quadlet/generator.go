package quadlet

import (
	"fmt"
	"path/filepath"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

// Container generates a .container quadlet file content from a Pod.
// configDir is the base directory where ConfigMap files are written (may be empty).
func Container(name string, pod *corev1.Pod, configDir string) ([]byte, error) {
	var b strings.Builder

	b.WriteString("[Container]\n")

	if len(pod.Spec.Containers) == 0 {
		return nil, fmt.Errorf("pod has no containers")
	}
	b.WriteString(fmt.Sprintf("Image=%s\n", pod.Spec.Containers[0].Image))

	containerName := fmt.Sprintf("%s-%s", pod.Namespace, name)
	b.WriteString(fmt.Sprintf("ContainerName=%s\n", containerName))

	// Exec: combine command (entrypoint override) and args
	cmd := append(pod.Spec.Containers[0].Command, pod.Spec.Containers[0].Args...)
	if len(cmd) > 0 {
		b.WriteString(fmt.Sprintf("Exec=%s\n", shellJoin(cmd)))
	}

	if pod.Spec.Containers[0].WorkingDir != "" {
		b.WriteString(fmt.Sprintf("WorkingDir=%s\n", pod.Spec.Containers[0].WorkingDir))
	}

	if pod.Spec.Containers[0].SecurityContext != nil && pod.Spec.Containers[0].SecurityContext.RunAsUser != nil {
		b.WriteString(fmt.Sprintf("User=%d\n", *pod.Spec.Containers[0].SecurityContext.RunAsUser))
	}

	for _, env := range pod.Spec.Containers[0].Env {
		b.WriteString(fmt.Sprintf("Environment=%s=%s\n", env.Name, env.Value))
	}

	for _, port := range pod.Spec.Containers[0].Ports {
		hostPort := port.ContainerPort
		if port.HostPort != 0 {
			hostPort = port.HostPort
		}
		proto := "tcp"
		if port.Protocol == corev1.ProtocolUDP {
			proto = "udp"
		}
		b.WriteString(fmt.Sprintf("PublishPort=%d:%d/%s\n", hostPort, port.ContainerPort, proto))
	}

	// Volumes — resolve ConfigMap references to their on-disk directory.
	for _, vol := range pod.Spec.Volumes {
		for _, c := range pod.Spec.Containers {
			for _, vm := range c.VolumeMounts {
				if vm.Name != vol.Name {
					continue
				}
				switch {
				case vol.PersistentVolumeClaim != nil:
					b.WriteString(fmt.Sprintf("Volume=%s-%s.volume:%s\n", pod.Namespace, vol.PersistentVolumeClaim.ClaimName, vm.MountPath))
				case vol.ConfigMap != nil:
					if configDir != "" {
						cmPath := fmt.Sprintf("%s/%s/%s", configDir, pod.Namespace, vol.ConfigMap.Name)
						b.WriteString(fmt.Sprintf("Volume=%s:%s:ro,z\n", cmPath, vm.MountPath))
					}
				case vol.Secret != nil:
					if configDir != "" {
						secretDir := filepath.Join(filepath.Dir(configDir), "secrets")
						secPath := filepath.Join(secretDir, pod.Namespace, vol.Secret.SecretName)
						b.WriteString(fmt.Sprintf("Volume=%s:%s:ro,z\n", secPath, vm.MountPath))
					}
				default:
					b.WriteString(fmt.Sprintf("Volume=%s:%s\n", vol.Name, vm.MountPath))
				}
			}
		}
	}

	// Network: use host networking when requested, otherwise use the namespace network.
	if pod.Spec.HostNetwork {
		b.WriteString("Network=host\n")
	} else {
		b.WriteString(fmt.Sprintf("Network=q8s-%s.network\n", pod.Namespace))
	}

	b.WriteString(fmt.Sprintf("Label=io.kubernetes.pod.name=%s\n", pod.Name))
	b.WriteString(fmt.Sprintf("Label=io.kubernetes.pod.namespace=%s\n", pod.Namespace))

	if pod.Spec.Containers[0].LivenessProbe != nil {
		if cmd := pod.Spec.Containers[0].LivenessProbe.Exec; cmd != nil && len(cmd.Command) > 0 {
			b.WriteString(fmt.Sprintf("HealthCmd=%s\n", strings.Join(cmd.Command, " ")))
		}
		if pod.Spec.Containers[0].LivenessProbe.InitialDelaySeconds > 0 {
			b.WriteString(fmt.Sprintf("HealthStartPeriod=%d\n", pod.Spec.Containers[0].LivenessProbe.InitialDelaySeconds))
		}
	}

	b.WriteString("\n[Unit]\n")
	b.WriteString(fmt.Sprintf("Description=Pod %s\n", pod.Name))

	if pod.Spec.RestartPolicy == corev1.RestartPolicyAlways ||
		pod.Spec.RestartPolicy == corev1.RestartPolicyOnFailure {
		b.WriteString("\n[Service]\n")
		b.WriteString("Restart=on-failure\n")
		b.WriteString("RestartSec=5\n")
	}

	b.WriteString("\n[Install]\n")
	b.WriteString("WantedBy=default.target\n")

	return []byte(b.String()), nil
}

// JobContainer generates a .container quadlet for a Job's pod template.
// The unit runs once (no Restart) and exits with the container's exit code.
func JobContainer(name string, job *batchv1.Job, configDir string) ([]byte, error) {
	spec := job.Spec.Template.Spec
	ns := job.Namespace

	var b strings.Builder
	b.WriteString("[Container]\n")

	if len(spec.Containers) == 0 {
		return nil, fmt.Errorf("job has no containers")
	}
	b.WriteString(fmt.Sprintf("Image=%s\n", spec.Containers[0].Image))
	b.WriteString(fmt.Sprintf("ContainerName=%s-%s-job\n", ns, name))

	if cmd := append(spec.Containers[0].Command, spec.Containers[0].Args...); len(cmd) > 0 {
		b.WriteString(fmt.Sprintf("Exec=%s\n", shellJoin(cmd)))
	}

	if spec.Containers[0].WorkingDir != "" {
		b.WriteString(fmt.Sprintf("WorkingDir=%s\n", spec.Containers[0].WorkingDir))
	}

	for _, env := range spec.Containers[0].Env {
		b.WriteString(fmt.Sprintf("Environment=%s=%s\n", env.Name, env.Value))
	}

	for _, vol := range spec.Volumes {
		for _, c := range spec.Containers {
			for _, vm := range c.VolumeMounts {
				if vm.Name != vol.Name {
					continue
				}
				switch {
				case vol.PersistentVolumeClaim != nil:
					b.WriteString(fmt.Sprintf("Volume=%s-%s.volume:%s\n", ns, vol.PersistentVolumeClaim.ClaimName, vm.MountPath))
				case vol.ConfigMap != nil:
					if configDir != "" {
						cmPath := fmt.Sprintf("%s/%s/%s", configDir, ns, vol.ConfigMap.Name)
						b.WriteString(fmt.Sprintf("Volume=%s:%s:ro,z\n", cmPath, vm.MountPath))
					}
				case vol.Secret != nil:
					if configDir != "" {
						secretDir := filepath.Join(filepath.Dir(configDir), "secrets")
						secPath := filepath.Join(secretDir, ns, vol.Secret.SecretName)
						b.WriteString(fmt.Sprintf("Volume=%s:%s:ro,z\n", secPath, vm.MountPath))
					}
				default:
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
func CronContainer(name string, cj *batchv1.CronJob, configDir string) ([]byte, error) {
	spec := cj.Spec.JobTemplate.Spec.Template.Spec
	ns := cj.Namespace

	var b strings.Builder
	b.WriteString("[Container]\n")

	if len(spec.Containers) == 0 {
		return nil, fmt.Errorf("cronjob has no containers")
	}
	b.WriteString(fmt.Sprintf("Image=%s\n", spec.Containers[0].Image))
	b.WriteString(fmt.Sprintf("ContainerName=%s-%s-cron\n", ns, name))

	if cmd := append(spec.Containers[0].Command, spec.Containers[0].Args...); len(cmd) > 0 {
		b.WriteString(fmt.Sprintf("Exec=%s\n", shellJoin(cmd)))
	}

	if spec.Containers[0].WorkingDir != "" {
		b.WriteString(fmt.Sprintf("WorkingDir=%s\n", spec.Containers[0].WorkingDir))
	}

	for _, env := range spec.Containers[0].Env {
		b.WriteString(fmt.Sprintf("Environment=%s=%s\n", env.Name, env.Value))
	}

	for _, vol := range spec.Volumes {
		for _, c := range spec.Containers {
			for _, vm := range c.VolumeMounts {
				if vm.Name != vol.Name {
					continue
				}
				switch {
				case vol.PersistentVolumeClaim != nil:
					b.WriteString(fmt.Sprintf("Volume=%s-%s.volume:%s\n", ns, vol.PersistentVolumeClaim.ClaimName, vm.MountPath))
				case vol.ConfigMap != nil:
					if configDir != "" {
						cmPath := fmt.Sprintf("%s/%s/%s", configDir, ns, vol.ConfigMap.Name)
						b.WriteString(fmt.Sprintf("Volume=%s:%s:ro,z\n", cmPath, vm.MountPath))
					}
				case vol.Secret != nil:
					if configDir != "" {
						secretDir := filepath.Join(filepath.Dir(configDir), "secrets")
						secPath := filepath.Join(secretDir, ns, vol.Secret.SecretName)
						b.WriteString(fmt.Sprintf("Volume=%s:%s:ro,z\n", secPath, vm.MountPath))
					}
				default:
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
	var b strings.Builder

	b.WriteString("[Unit]\n")
	b.WriteString(fmt.Sprintf("Description=Timer for CronJob %s/%s\n", cj.Namespace, name))

	b.WriteString("\n[Timer]\n")
	b.WriteString(fmt.Sprintf("OnCalendar=%s\n", cronToOnCalendar(cj.Spec.Schedule)))
	b.WriteString("Persistent=true\n")

	b.WriteString("\n[Install]\n")
	b.WriteString("WantedBy=timers.target\n")

	return []byte(b.String()), nil
}

// cronToOnCalendar converts a 5-field cron expression to systemd OnCalendar format.
// minute hour dom month dow → *-{month}-{dom} {hour}:{minute}:00
// Step expressions like */5 (every 5 minutes) become 0/5 (systemd notation).
// */1 is simplified to * (every value).
func cronToOnCalendar(cron string) string {
	fields := strings.Fields(cron)
	if len(fields) != 5 {
		return cron
	}
	minute, hour, dom, month := fields[0], fields[1], fields[2], fields[3]

	toSys := func(f string) string {
		if f == "*" || f == "*/1" {
			return "*"
		}
		// Convert */N → 0/N (systemd step syntax)
		if strings.HasPrefix(f, "*/") {
			return "0/" + f[2:]
		}
		return f
	}

	return fmt.Sprintf("*-%s-%s %s:%s:00",
		toSys(month), toSys(dom), toSys(hour), toSys(minute))
}

// Volume generates a .volume quadlet file content.
func Volume(name string, pvcName string) ([]byte, error) {
	var b strings.Builder

	b.WriteString("[Volume]\n")
	b.WriteString(fmt.Sprintf("VolumeName=%s\n", pvcName))

	return []byte(b.String()), nil
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
