package quadlet

import (
	"sort"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	"q8s/internal/cronruns"
	"q8s/internal/names"
)

// GeneratedName is one Podman container name, systemd unit name,
// EnvironmentFile name or named volume that a workload object makes q8s own.
type GeneratedName struct {
	// Kind is a short description of the generated name: container, unit,
	// env file or volume.
	Kind string
	// Name is the name as it appears on the host.
	Name string
	// Key is the collision identity. It is usually the same as Name. An env
	// file is the exception: q8s writes it below a per-namespace directory,
	// so its Key carries the namespace even though the file name does not.
	Key string
}

const (
	GeneratedKindContainer = "container"
	GeneratedKindUnit      = "unit"
	GeneratedKindEnvFile   = "env file"
	GeneratedKindVolume    = "volume"
)

// DeploymentReplicaCount is the number of pod instances a Deployment asks
// for. A nil replicas field means one, as in Kubernetes; an explicit zero
// stays zero because it is a real desired state.
func DeploymentReplicaCount(dep *appsv1.Deployment) int32 {
	if dep == nil || dep.Spec.Replicas == nil {
		return 1
	}
	if *dep.Spec.Replicas < 0 {
		return 0
	}
	return *dep.Spec.Replicas
}

// DeploymentInstanceName is the name of Deployment instance i. q8s
// materializes every replica as a Pod with this name.
func DeploymentInstanceName(deployment string, index int32) string {
	// The API Pod name stays <deployment>-<index> in both formats.
	return names.Host(names.KindDeploymentPod, "", deployment, strconv.Itoa(int(index)))
}

// podBaseName is the base q8s joins from namespace and pod name. It is the
// container name of a single-container pod and the file base of a .pod unit.
func podBaseName(namespace, pod string) string {
	return names.Host(names.KindPod, namespace, pod)
}

// podContainerName is the ContainerName= value of a standalone pod or one
// member of a multi-container pod. An empty container selects the standalone
// shape.
func podContainerName(namespace, pod, container string) string {
	return names.Host(names.KindPodContainer, namespace, pod, container)
}

// EnvFileName is the EnvironmentFile name q8s writes for the workload whose
// generated pod name is base. The file lives in a per-namespace directory, so
// its identity in GeneratedNames carries the namespace separately.
func EnvFileName(base string) string {
	return names.Host(names.KindEnvPod, "", base)
}

// JobEnvBaseName is the generated pod-name base of a Job's EnvironmentFile.
// The names package includes the .env extension, which EnvFileName adds here.
func JobEnvBaseName(job string) string {
	return strings.TrimSuffix(names.Host(names.KindEnvJob, "", job), ".env")
}

// CronJobEnvBaseName is the generated pod-name base of a CronJob's
// EnvironmentFile.
func CronJobEnvBaseName(cronJob string) string {
	return strings.TrimSuffix(names.Host(names.KindEnvCronJob, "", cronJob), ".env")
}

// GeneratedNames returns every name q8s generates for a workload object:
// container names, systemd unit names, EnvironmentFile names and emptyDir
// volume names. obj is a *corev1.Pod, *appsv1.Deployment, *batchv1.Job or
// *batchv1.CronJob; any other value returns nil.
//
// The function is pure and is the single definition of the set. The
// generator and the store both derive their names from the helpers here, and
// TestGeneratedNamesMatchGenerator checks that the rendered quadlets stay a
// subset of this set.
func GeneratedNames(obj interface{}) []GeneratedName {
	seen := make(map[string]GeneratedName)
	add := func(kind, name, key string) {
		if name == "" {
			return
		}
		if key == "" {
			key = name
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = GeneratedName{Kind: kind, Name: name, Key: key}
	}
	addPod := func(namespace, podName string, spec *corev1.PodSpec, volumeOwner string) {
		if spec == nil {
			return
		}
		if len(spec.Containers) > 1 {
			// PodUnits writes a .pod unit and one .container per member.
			add(GeneratedKindUnit, names.Host(names.KindPodFile, namespace, podName), "")
			add(GeneratedKindUnit, names.Host(names.KindPodUnit, namespace, podName), "")
			for _, c := range spec.Containers {
				name := podContainerName(namespace, podName, c.Name)
				add(GeneratedKindContainer, name, "")
				add(GeneratedKindUnit, names.Host(names.KindPodContainerService, namespace, podName, c.Name), "")
			}
		} else {
			// Container writes the historic single-container shape.
			add(GeneratedKindContainer, podBaseName(namespace, podName), "")
			add(GeneratedKindUnit, names.Host(names.KindPodService, namespace, podName), "")
		}
		env := EnvFileName(podName)
		add(GeneratedKindEnvFile, env, namespace+"/"+env)
		for _, volume := range EmptyDirVolumeNamesForSpec(namespace, volumeOwner, spec) {
			add(GeneratedKindVolume, volume, "")
		}
	}

	switch o := obj.(type) {
	case *corev1.Pod:
		addPod(o.Namespace, o.Name, &o.Spec, emptyDirVolumeOwner(o))
	case *appsv1.Deployment:
		for i := int32(0); i < DeploymentReplicaCount(o); i++ {
			addPod(o.Namespace, DeploymentInstanceName(o.Name, i), &o.Spec.Template.Spec,
				DeploymentReplicaVolumeOwner(o.Name, i))
		}
	case *batchv1.Job:
		name := cronruns.JobContainerName(o.Namespace, o.Name)
		add(GeneratedKindContainer, name, "")
		add(GeneratedKindUnit, cronruns.JobUnitName(o.Namespace, o.Name), "")
		env := EnvFileName(JobEnvBaseName(o.Name))
		add(GeneratedKindEnvFile, env, o.Namespace+"/"+env)
		for _, volume := range EmptyDirVolumeNamesForSpec(o.Namespace, o.Name, &o.Spec.Template.Spec) {
			add(GeneratedKindVolume, volume, "")
		}
	case *batchv1.CronJob:
		name := cronruns.ContainerName(o.Namespace, o.Name)
		add(GeneratedKindContainer, name, "")
		add(GeneratedKindUnit, names.Host(names.KindCronJobService, o.Namespace, o.Name), "")
		add(GeneratedKindUnit, names.Host(names.KindCronJobTimer, o.Namespace, o.Name), "")
		env := EnvFileName(CronJobEnvBaseName(o.Name))
		add(GeneratedKindEnvFile, env, o.Namespace+"/"+env)
		for _, volume := range EmptyDirVolumeNamesForSpec(o.Namespace, o.Name, &o.Spec.JobTemplate.Spec.Template.Spec) {
			add(GeneratedKindVolume, volume, "")
		}
	default:
		return nil
	}

	names := make([]GeneratedName, 0, len(seen))
	for _, name := range seen {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return names[i].Key < names[j].Key })
	return names
}
