package main

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	"q8s/internal/podman"
	"q8s/internal/quadlet"
)

// nextRecheck returns how long to wait before the next readiness recheck when
// at least one running pod has a container that is not ready yet and carries a
// readinessProbe or startupProbe. It reports false when no such pod exists, so
// the sync loop can stay idle instead of running an extra timer.
//
// containers is the live Podman info reconcileOnce already fetched, keyed by
// namespace/name. A container contributes only while Kubernetes could still
// be waiting for its probe: until its start time plus initialDelaySeconds plus
// failureThreshold probe periods. With both probes, a startupProbe's window is
// added to the readinessProbe's window because Kubernetes runs them one after
// the other. The result is the shortest period among the contributing
// containers, so a pod becoming ready is noticed on the probe's own cadence
// rather than at the next 30s resync tick.
func nextRecheck(pods []*corev1.Pod, containers map[string][]podman.ContainerInfo, now time.Time) (time.Duration, bool) {
	shortest := time.Duration(0)
	found := false
	for _, pod := range pods {
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}
		infos := containers[pod.Namespace+"/"+pod.Name]
		for _, c := range pod.Spec.Containers {
			if c.ReadinessProbe == nil && c.StartupProbe == nil {
				continue
			}
			if containerReady(pod, c.Name) {
				continue
			}
			probe := c.ReadinessProbe
			if probe == nil {
				probe = c.StartupProbe
			}
			period := probePeriod(probe)
			budget := probeBudget(probe)
			if c.ReadinessProbe != nil && c.StartupProbe != nil {
				budget += probeBudget(c.StartupProbe)
			}
			start, ok := containerStart(pod, infos, c.Name)
			if !ok || now.After(start.Add(budget)) {
				continue
			}
			if !found || period < shortest {
				shortest = period
				found = true
			}
		}
	}
	return shortest, found
}

// containerReady reports the Ready state of one container in a pod. A missing
// status entry is not ready: the container has not been observed yet.
func containerReady(pod *corev1.Pod, name string) bool {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == name {
			return cs.Ready
		}
	}
	return false
}

// containerStart returns the container's Podman start time. A multi-container
// pod member is matched by its io.kubernetes.container.name label, while a
// single-container pod falls back to its only container because older
// containers carry only the pod labels. ok is false when there is no usable
// start time (for example a Podman listing that failed this pass): the
// container then gets no extra recheck and the 30s resync covers it, so a
// missing listing can never keep the timer running without bound.
func containerStart(pod *corev1.Pod, infos []podman.ContainerInfo, name string) (time.Time, bool) {
	for _, ci := range infos {
		containerName := ci.ContainerName()
		if containerName == "" && len(pod.Spec.Containers) == 1 {
			containerName = pod.Spec.Containers[0].Name
		}
		if containerName != name {
			continue
		}
		if ci.StartedAt > 0 {
			return time.Unix(ci.StartedAt, 0), true
		}
		break
	}
	return time.Time{}, false
}

// probePeriodSeconds applies the Kubernetes default to an unset or invalid
// periodSeconds, matching the quadlet generator.
func probePeriodSeconds(p *corev1.Probe) int32 {
	if p != nil && p.PeriodSeconds > 0 {
		return p.PeriodSeconds
	}
	return quadlet.DefaultProbePeriodSeconds
}

// probePeriod returns the probe's check interval.
func probePeriod(p *corev1.Probe) time.Duration {
	return time.Duration(probePeriodSeconds(p)) * time.Second
}

// probeBudget returns how long Kubernetes may wait for a probe verdict:
// initialDelaySeconds plus failureThreshold probe periods.
func probeBudget(p *corev1.Probe) time.Duration {
	if p == nil {
		return 0
	}
	threshold := int32(quadlet.DefaultProbeFailureThreshold)
	if p.FailureThreshold > 0 {
		threshold = p.FailureThreshold
	}
	initial := p.InitialDelaySeconds
	if initial < 0 {
		initial = 0
	}
	return time.Duration(initial)*time.Second + time.Duration(threshold)*probePeriod(p)
}
