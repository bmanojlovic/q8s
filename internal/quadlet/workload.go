package quadlet

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

const (
	// WorkloadMountPath is where the q8s workload dir is mounted inside a pod
	// that opted in to ServiceAccount credentials. The server reserves this
	// path and rejects a user volume mount there.
	WorkloadMountPath = "/var/run/q8s/workload"

	// WorkloadBinMountPath holds the q8s binary the kubeconfig's exec
	// credential plugin runs. It is a separate directory on disk (not the
	// tmpfs socket directory) so the binary does not cost RAM; both are
	// directory mounts, so a q8s upgrade that replaces the files by rename
	// is visible to running pods without a restart.
	WorkloadBinMountPath = "/var/run/q8s/bin"

	// workloadKubeconfigEnv is the environment variable that points clients
	// inside the container at the kubeconfig written by q8s serve.
	workloadKubeconfigEnv = "KUBECONFIG"
)

// WorkloadMount carries the host workload directory and the pod's opt-in
// decision into quadlet generation. Dir is the host directory written by q8s
// serve; an empty Dir disables the feature. Enabled is true when the pod set
// serviceAccountName and did not set automountServiceAccountToken to false.
type WorkloadMount struct {
	Dir     string
	BinDir  string
	Enabled bool
	// Namespace selects the per-namespace kubeconfig
	// (<mount>/ns/<namespace>/kubeconfig); empty uses the host-wide one.
	Namespace string
}

// WorkloadMountFor returns the WorkloadMount for one pod spec and host
// directory. It is the single place that decides whether the pod opted in, so
// every render path agrees on the rule.
func WorkloadMountFor(spec *corev1.PodSpec, dir, binDir string) WorkloadMount {
	enabled := spec != nil && spec.ServiceAccountName != "" &&
		(spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken)
	return WorkloadMount{Dir: dir, BinDir: binDir, Enabled: enabled}
}

// active reports whether the workload mount should be rendered.
func (w WorkloadMount) active() bool {
	return w.Dir != "" && w.BinDir != "" && w.Enabled
}

// workloadMountArg normalises the optional generator argument. Callers that
// do not pass a WorkloadMount keep the historic rendering exactly.
func workloadMountArg(opts []WorkloadMount) WorkloadMount {
	if len(opts) == 0 {
		return WorkloadMount{}
	}
	return opts[0]
}

// writeWorkloadMount emits the read-only workload bind mount and the
// KUBECONFIG environment line for a pod that opted in. Kubernetes mounts the
// credential into every container, so callers invoke this once per rendered
// container. An explicit KUBECONFIG in the container env wins over ours, so
// the environment line is skipped then (the volume is still mounted).
func writeWorkloadMount(b *strings.Builder, workload WorkloadMount, c corev1.Container, userNS bool) error {
	if !workload.active() {
		return nil
	}
	if err := validatePath("workloadDir", workload.Dir); err != nil {
		return err
	}
	if err := validatePath("workloadBinDir", workload.BinDir); err != nil {
		return err
	}
	b.WriteString(fmt.Sprintf("Volume=%s:%s%s\n", workload.Dir, WorkloadMountPath, volumeOptions(true, "z", userNS)))
	b.WriteString(fmt.Sprintf("Volume=%s:%s%s\n", workload.BinDir, WorkloadBinMountPath, volumeOptions(true, "z", userNS)))
	if containerSetsEnv(c, workloadKubeconfigEnv) {
		return nil
	}
	kubeconfig := WorkloadMountPath + "/kubeconfig"
	if workload.Namespace != "" {
		if err := validatePath("namespace", "/"+workload.Namespace); err != nil {
			return err
		}
		kubeconfig = WorkloadMountPath + "/ns/" + workload.Namespace + "/kubeconfig"
	}
	line, err := envLine(workloadKubeconfigEnv, kubeconfig)
	if err != nil {
		return err
	}
	b.WriteString(line)
	return nil
}

// containerSetsEnv reports whether the container declares the named
// environment variable explicitly. EnvFrom sources are not inspected: their
// values are not known at render time.
func containerSetsEnv(c corev1.Container, name string) bool {
	for _, env := range c.Env {
		if env.Name == name {
			return true
		}
	}
	return false
}
