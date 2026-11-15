package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"

	"sigs.k8s.io/yaml"

	"q8s/pkg/install"
)

// Workload containers see the host workload directory at this path. It holds
// the copied q8s binary, the generated kubeconfig and the credential socket.
const (
	workloadMountDir    = "/var/run/q8s/workload"
	workloadBinaryPath  = "/var/run/q8s/bin/q8s"
	workloadKubeconfig  = workloadMountDir + "/kubeconfig"
	workloadServerHost  = "host.containers.internal"
	workloadClusterName = "q8s"
	workloadUserName    = "q8s-workload"
)

// workloadDir is the host directory of the per-host workload files:
// <W> = filepath.Join(filepath.Dir(ConfigDir), "workload"). Rootful that is
// /run/q8s/workload, rootless $XDG_RUNTIME_DIR/q8s/workload.
func workloadDir(d dirs) string {
	return filepath.Join(filepath.Dir(d.configDir), "workload")
}

// workloadBinDir is the on-disk directory holding the copy of the q8s binary
// that pods run as their exec credential plugin. It lives under the data dir
// rather than in the tmpfs workload dir so the binary does not cost RAM.
func workloadBinDir(d dirs) string {
	return filepath.Join(d.dataDir, "workload-bin")
}

// workloadKubeconfig renders the kubeconfig a pod uses. It contains no
// secrets: authentication goes through the exec credential plugin, which
// asks the local socket for a short-lived certificate.
func workloadKubeconfigFor(port int, caPEM []byte, namespace ...string) ([]byte, error) {
	ns := ""
	if len(namespace) > 0 {
		ns = namespace[0]
	}
	if port <= 0 {
		port = install.DefaultPort
	}
	cfg := workloadKubeconfigDoc{
		APIVersion: "v1",
		Kind:       "Config",
		Clusters: []workloadCluster{{
			Name: workloadClusterName,
			Cluster: workloadClusterData{
				CertificateAuthorityData: base64.StdEncoding.EncodeToString(caPEM),
				Server:                   fmt.Sprintf("https://%s:%d", workloadServerHost, port),
				TLSServerName:            "localhost",
			},
		}},
		Contexts: []workloadContext{{
			Name: workloadClusterName,
			Context: workloadContextData{
				Cluster:   workloadClusterName,
				User:      workloadUserName,
				Namespace: ns,
			},
		}},
		CurrentContext: workloadClusterName,
		Preferences:    map[string]interface{}{},
		Users: []workloadUser{{
			Name: workloadUserName,
			User: workloadUserData{Exec: workloadExec{
				APIVersion:      "client.authentication.k8s.io/v1",
				Command:         workloadBinaryPath,
				Args:            []string{"credential"},
				InteractiveMode: "Never",
			}},
		}},
	}
	return yaml.Marshal(cfg)
}

// writeWorkloadFiles writes the kubeconfig into dir and the binary copy into
// binDir (skipped when the copy is already identical, so a restart does not
// rewrite 26 MB for nothing).
// Both are written to a temporary file and renamed, so a running pod never
// sees a partially written file.
func writeWorkloadFiles(dir, binDir string, port int, caPEM []byte) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create workload directory %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0755); err != nil {
		return fmt.Errorf("chmod workload directory %s: %w", dir, err)
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate q8s binary: %w", err)
	}
	binary, err := os.ReadFile(exe)
	if err != nil {
		return fmt.Errorf("read q8s binary %s: %w", exe, err)
	}
	if err := os.MkdirAll(binDir, 0755); err != nil {
		return fmt.Errorf("create workload binary directory %s: %w", binDir, err)
	}
	binPath := filepath.Join(binDir, "q8s")
	if old, err := os.ReadFile(binPath); err != nil || !bytes.Equal(old, binary) {
		if err := writeFileAtomic(binPath, binary, 0755); err != nil {
			return fmt.Errorf("write workload q8s binary: %w", err)
		}
	}
	kubeconfig, err := workloadKubeconfigFor(port, caPEM)
	if err != nil {
		return fmt.Errorf("render workload kubeconfig: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(dir, "kubeconfig"), kubeconfig, 0644); err != nil {
		return fmt.Errorf("write workload kubeconfig: %w", err)
	}
	// Per-namespace kubeconfigs written by earlier runs are refreshed too, so
	// a changed port or CA reaches running pods without a restart.
	nsDirs, _ := os.ReadDir(filepath.Join(dir, "ns"))
	for _, e := range nsDirs {
		if !e.IsDir() {
			continue
		}
		data, err := workloadKubeconfigFor(port, caPEM, e.Name())
		if err != nil {
			return fmt.Errorf("render workload kubeconfig for %s: %w", e.Name(), err)
		}
		if err := writeFileAtomic(filepath.Join(dir, "ns", e.Name(), "kubeconfig"), data, 0644); err != nil {
			return fmt.Errorf("write workload kubeconfig for %s: %w", e.Name(), err)
		}
	}
	return nil
}

// writeFileAtomic writes data to path through a temporary file in the same
// directory and renames it into place.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// workloadKubeconfigDoc is the kubeconfig shape q8s generates for pods. It
// mirrors the regular kubeconfig plus the exec credential plugin stanza.
type workloadKubeconfigDoc struct {
	APIVersion     string                 `json:"apiVersion"`
	Kind           string                 `json:"kind"`
	Clusters       []workloadCluster      `json:"clusters"`
	Contexts       []workloadContext      `json:"contexts"`
	CurrentContext string                 `json:"current-context"`
	Preferences    map[string]interface{} `json:"preferences"`
	Users          []workloadUser         `json:"users"`
}

type workloadCluster struct {
	Name    string              `json:"name"`
	Cluster workloadClusterData `json:"cluster"`
}

type workloadClusterData struct {
	CertificateAuthorityData string `json:"certificate-authority-data"`
	Server                   string `json:"server"`
	TLSServerName            string `json:"tls-server-name"`
}

type workloadContext struct {
	Name    string              `json:"name"`
	Context workloadContextData `json:"context"`
}

type workloadContextData struct {
	Cluster   string `json:"cluster"`
	User      string `json:"user"`
	Namespace string `json:"namespace,omitempty"`
}

type workloadUser struct {
	Name string           `json:"name"`
	User workloadUserData `json:"user"`
}

type workloadUserData struct {
	Exec workloadExec `json:"exec"`
}

type workloadExec struct {
	APIVersion      string   `json:"apiVersion"`
	Command         string   `json:"command"`
	Args            []string `json:"args"`
	InteractiveMode string   `json:"interactiveMode"`
}
