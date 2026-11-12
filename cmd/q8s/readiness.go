package main

import (
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"q8s/internal/podman"
	"q8s/internal/quadlet"
)

// watchedHealthIDs returns the Podman container IDs whose health check can
// still change the readiness of a pod: the container is not ready, carries a
// probe that becomes podman's health check (liveness, readiness or startup),
// and is still inside the window in which Kubernetes could be waiting for a
// verdict. The sync loop uses the set to decide which systemd health check
// units to watch.
//
// Podman runs every health check as a transient systemd unit named
// <container id>-<hex suffix>.service. Those units are not in the quadlet
// directory and come and go with each check, so the container ID is the only
// stable thing to match on. Watching only containers that are still starting
// keeps the match cheap and bounded: a ready pod adds nothing, and a container
// whose probe window has passed falls back to the 30s resync tick.
//
// containers is the live Podman info reconcileOnce already fetched, keyed by
// namespace/name. A container contributes only until its start time plus
// initialDelaySeconds plus failureThreshold probe periods. With a
// startupProbe, its window is added to the main health check's window because
// Kubernetes runs them one after the other.
func watchedHealthIDs(pods []*corev1.Pod, containers map[string][]podman.ContainerInfo, now time.Time) map[string]struct{} {
	ids := make(map[string]struct{})
	for _, pod := range pods {
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}
		infos := containers[pod.Namespace+"/"+pod.Name]
		for _, c := range pod.Spec.Containers {
			// Ready follows podman's single health check, which the
			// livenessProbe owns when present, else the readinessProbe
			// (see quadlet.writeHealthCheck); a startupProbe runs first.
			probe := c.LivenessProbe
			if probe == nil {
				probe = c.ReadinessProbe
			}
			if probe == nil && c.StartupProbe == nil {
				continue
			}
			if containerReady(pod, c.Name) {
				continue
			}
			if probe == nil {
				probe = c.StartupProbe
			}
			budget := probeBudget(probe)
			if probe != c.StartupProbe && c.StartupProbe != nil {
				budget += probeBudget(c.StartupProbe)
			}
			start, ok := containerStart(pod, infos, c.Name)
			if !ok || now.After(start.Add(budget)) {
				continue
			}
			ci, ok := matchContainer(pod, infos, c.Name)
			if !ok || ci.ID == "" {
				continue
			}
			ids[ci.ID] = struct{}{}
		}
	}
	return ids
}

// healthCheckContainerID returns the container ID from the name of the
// transient systemd unit podman creates for a health check. Podman names the
// unit <64 lowercase hex container id>-<1..64 hex suffix>.service, with a
// matching .timer. It returns false for every other unit name: q8s units,
// timers, uppercase IDs, and IDs of the wrong length are not health checks.
func healthCheckContainerID(unit string) (string, bool) {
	base, ok := strings.CutSuffix(unit, ".service")
	if !ok {
		return "", false
	}
	id, suffix, ok := strings.Cut(base, "-")
	if !ok || len(id) != 64 || len(suffix) < 1 || len(suffix) > 64 {
		return "", false
	}
	if !isLowerHex(id) || !isLowerHex(suffix) {
		return "", false
	}
	return id, true
}

// isLowerHex reports whether s is made only of lowercase hex digits.
func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' || c >= 'a' && c <= 'f' {
			continue
		}
		return false
	}
	return true
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

// matchContainer returns the live Podman container for one spec container. A
// multi-container pod member is matched by its io.kubernetes.container.name
// label, while a single-container pod falls back to its only container
// because older containers carry only the pod labels.
func matchContainer(pod *corev1.Pod, infos []podman.ContainerInfo, name string) (podman.ContainerInfo, bool) {
	for _, ci := range infos {
		containerName := ci.ContainerName()
		if containerName == "" && len(pod.Spec.Containers) == 1 {
			containerName = pod.Spec.Containers[0].Name
		}
		if containerName == name {
			return ci, true
		}
	}
	return podman.ContainerInfo{}, false
}

// containerStart returns the container's Podman start time. ok is false when
// there is no usable start time (for example a Podman listing that failed
// this pass): the container then gets no health unit watch and the 30s resync
// covers it, so a missing listing can never keep a watch alive without bound.
func containerStart(pod *corev1.Pod, infos []podman.ContainerInfo, name string) (time.Time, bool) {
	ci, ok := matchContainer(pod, infos, name)
	if !ok || ci.StartedAt <= 0 {
		return time.Time{}, false
	}
	return time.Unix(ci.StartedAt, 0), true
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
