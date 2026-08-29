package server

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	openapi_v2 "github.com/google/gnostic-models/openapiv2"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"

	"q8s/internal/quadlet"
)

// openapiV2Proto holds the lazily-computed protobuf encoding of openapiV2Schema.
var (
	openapiV2ProtoOnce  sync.Once
	openapiV2ProtoBytes []byte
	openapiV2ProtoErr   error
)

func getOpenapiV2Proto() ([]byte, error) {
	openapiV2ProtoOnce.Do(func() {
		doc, err := openapi_v2.ParseDocument([]byte(openapiV2Schema))
		if err != nil {
			openapiV2ProtoErr = err
			return
		}
		openapiV2ProtoBytes, openapiV2ProtoErr = proto.Marshal(doc)
	})
	return openapiV2ProtoBytes, openapiV2ProtoErr
}

func (s *Server) handleAPIRoot(w http.ResponseWriter, r *http.Request) {
	port := s.config.Port
	if port == 0 {
		port = 6443
	}
	data, _ := json.Marshal(map[string]interface{}{
		"kind":       "APIVersions",
		"apiVersion": "v1",
		"versions":   []string{"v1"},
		"serverAddressByClientCIDRs": []map[string]string{
			{"serverAddress": fmt.Sprintf("127.0.0.1:%d", port), "clientCIDR": "0.0.0.0/0"},
		},
	})
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func (s *Server) handleAPIV1(w http.ResponseWriter, r *http.Request) {
	data, _ := json.Marshal(metav1.APIResourceList{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "APIResourceList",
		},
		GroupVersion: "v1",
		APIResources: []metav1.APIResource{
			{Name: "nodes", SingularName: "node", Namespaced: false, Kind: "Node", Verbs: []string{"get", "list"}, ShortNames: []string{"no"}},
			{Name: "pods", SingularName: "pod", Namespaced: true, Kind: "Pod", Verbs: verbList(), ShortNames: []string{"po"}, Categories: []string{"all"}},
			{Name: "services", SingularName: "service", Namespaced: true, Kind: "Service", Verbs: verbList(), ShortNames: []string{"svc"}, Categories: []string{"all"}},
			{Name: "namespaces", SingularName: "namespace", Namespaced: false, Kind: "Namespace", Verbs: verbList(), ShortNames: []string{"ns"}},
			{Name: "persistentvolumeclaims", SingularName: "persistentvolumeclaim", Namespaced: true, Kind: "PersistentVolumeClaim", Verbs: verbList(), ShortNames: []string{"pvc"}},
			{Name: "configmaps", SingularName: "configmap", Namespaced: true, Kind: "ConfigMap", Verbs: verbList(), ShortNames: []string{"cm"}},
			{Name: "secrets", SingularName: "secret", Namespaced: true, Kind: "Secret", Verbs: verbList()},
			{Name: "events", SingularName: "event", Namespaced: true, Kind: "Event", Verbs: []string{"get", "list", "watch"}, ShortNames: []string{"ev"}},
		},
	})
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func (s *Server) handleAPIsRoot(w http.ResponseWriter, r *http.Request) {
	data, _ := json.Marshal(metav1.APIGroupList{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "APIGroupList",
		},
		Groups: []metav1.APIGroup{
			{
				Name: "apps",
				Versions: []metav1.GroupVersionForDiscovery{
					{GroupVersion: "apps/v1", Version: "v1"},
				},
				PreferredVersion: metav1.GroupVersionForDiscovery{
					GroupVersion: "apps/v1",
					Version:      "v1",
				},
			},
			{
				Name: "batch",
				Versions: []metav1.GroupVersionForDiscovery{
					{GroupVersion: "batch/v1", Version: "v1"},
				},
				PreferredVersion: metav1.GroupVersionForDiscovery{
					GroupVersion: "batch/v1",
					Version:      "v1",
				},
			},
			{
				Name: "networking.k8s.io",
				Versions: []metav1.GroupVersionForDiscovery{
					{GroupVersion: "networking.k8s.io/v1", Version: "v1"},
				},
				PreferredVersion: metav1.GroupVersionForDiscovery{
					GroupVersion: "networking.k8s.io/v1",
					Version:      "v1",
				},
			},
			{
				Name: "metrics.k8s.io",
				Versions: []metav1.GroupVersionForDiscovery{
					{GroupVersion: "metrics.k8s.io/v1beta1", Version: "v1beta1"},
				},
				PreferredVersion: metav1.GroupVersionForDiscovery{
					GroupVersion: "metrics.k8s.io/v1beta1",
					Version:      "v1beta1",
				},
			},
			{
				Name: "coordination.k8s.io",
				Versions: []metav1.GroupVersionForDiscovery{
					{GroupVersion: "coordination.k8s.io/v1", Version: "v1"},
				},
				PreferredVersion: metav1.GroupVersionForDiscovery{
					GroupVersion: "coordination.k8s.io/v1",
					Version:      "v1",
				},
			},
			{
				Name: "storage.k8s.io",
				Versions: []metav1.GroupVersionForDiscovery{
					{GroupVersion: "storage.k8s.io/v1", Version: "v1"},
				},
				PreferredVersion: metav1.GroupVersionForDiscovery{
					GroupVersion: "storage.k8s.io/v1",
					Version:      "v1",
				},
			},
		},
	})
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func (s *Server) handleAppsRoot(w http.ResponseWriter, r *http.Request) {
	data, _ := json.Marshal(metav1.APIGroup{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "APIGroup",
		},
		Name: "apps",
		Versions: []metav1.GroupVersionForDiscovery{
			{GroupVersion: "apps/v1", Version: "v1"},
		},
		PreferredVersion: metav1.GroupVersionForDiscovery{
			GroupVersion: "apps/v1",
			Version:      "v1",
		},
	})
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func (s *Server) handleAppsV1(w http.ResponseWriter, r *http.Request) {
	data, _ := json.Marshal(metav1.APIResourceList{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "APIResourceList",
		},
		GroupVersion: "apps/v1",
		APIResources: []metav1.APIResource{
			{Name: "deployments", SingularName: "deployment", Namespaced: true, Kind: "Deployment", Verbs: verbList(), ShortNames: []string{"deploy"}, Categories: []string{"all"}},
			{Name: "deployments/scale", SingularName: "", Namespaced: true, Kind: "Scale", Verbs: []string{"get", "patch", "update"}},
			{Name: "daemonsets", SingularName: "daemonset", Namespaced: true, Kind: "DaemonSet", Verbs: []string{"get", "list", "watch"}, ShortNames: []string{"ds"}, Categories: []string{"all"}},
			{Name: "statefulsets", SingularName: "statefulset", Namespaced: true, Kind: "StatefulSet", Verbs: []string{"get", "list", "watch"}, ShortNames: []string{"sts"}, Categories: []string{"all"}},
			{Name: "replicasets", SingularName: "replicaset", Namespaced: true, Kind: "ReplicaSet", Verbs: []string{"get", "list", "watch"}, ShortNames: []string{"rs"}, Categories: []string{"all"}},
		},
	})
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func (s *Server) handleBatchRoot(w http.ResponseWriter, r *http.Request) {
	data, _ := json.Marshal(metav1.APIGroup{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "APIGroup",
		},
		Name: "batch",
		Versions: []metav1.GroupVersionForDiscovery{
			{GroupVersion: "batch/v1", Version: "v1"},
		},
		PreferredVersion: metav1.GroupVersionForDiscovery{
			GroupVersion: "batch/v1",
			Version:      "v1",
		},
	})
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func (s *Server) handleBatchV1(w http.ResponseWriter, r *http.Request) {
	data, _ := json.Marshal(metav1.APIResourceList{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "APIResourceList",
		},
		GroupVersion: "batch/v1",
		APIResources: []metav1.APIResource{
			{Name: "jobs", SingularName: "job", Namespaced: true, Kind: "Job", Verbs: verbList(), Categories: []string{"all"}},
			{Name: "cronjobs", SingularName: "cronjob", Namespaced: true, Kind: "CronJob", Verbs: verbList(), ShortNames: []string{"cj"}, Categories: []string{"all"}},
		},
	})
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func (s *Server) handleNetworkingRoot(w http.ResponseWriter, r *http.Request) {
	data, _ := json.Marshal(metav1.APIGroup{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "APIGroup",
		},
		Name: "networking.k8s.io",
		Versions: []metav1.GroupVersionForDiscovery{
			{GroupVersion: "networking.k8s.io/v1", Version: "v1"},
		},
		PreferredVersion: metav1.GroupVersionForDiscovery{
			GroupVersion: "networking.k8s.io/v1",
			Version:      "v1",
		},
	})
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func (s *Server) handleNetworkingV1(w http.ResponseWriter, r *http.Request) {
	data, _ := json.Marshal(metav1.APIResourceList{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "APIResourceList",
		},
		GroupVersion: "networking.k8s.io/v1",
		APIResources: []metav1.APIResource{
			{Name: "ingresses", SingularName: "ingress", Namespaced: true, Kind: "Ingress", Verbs: verbList(), ShortNames: []string{"ing"}},
		},
	})
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	info := getVersionInfo()
	data, _ := json.Marshal(info)
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// getVersionInfo builds version.Info from the k8s.io/apimachinery module
// version embedded in the binary by the Go toolchain. The module uses scheme
// v0.{minor}.{patch} which maps to Kubernetes v1.{minor}.{patch}.
func getVersionInfo() version.Info {
	major, minor, gitVersion := "1", "36", "v1.36.0+q8s"
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range bi.Deps {
			if dep.Path == "k8s.io/apimachinery" {
				// dep.Version is e.g. "v0.36.2"
				v := strings.TrimPrefix(dep.Version, "v0.")
				parts := strings.SplitN(v, ".", 2)
				if len(parts) == 2 {
					major = "1"
					minor = parts[0]
					gitVersion = "v1." + v + "+q8s"
				}
				break
			}
		}
	}
	return version.Info{
		Major:        major,
		Minor:        minor,
		GitVersion:   gitVersion,
		GitCommit:    "q8s",
		GitTreeState: "clean",
		BuildDate:    time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		GoVersion:    runtime.Version(),
		Compiler:     runtime.Compiler,
		Platform:     runtime.GOOS + "/" + runtime.GOARCH,
	}
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

// handleOpenAPIv2 serves the Swagger 2.0 schema for client-side validation.
// client-go sends Accept: protobuf,json — we serve native protobuf when asked
// so kubectl doesn't need --validate=false.
func (s *Server) handleOpenAPIv2(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.Header.Get("Accept"), "protobuf") {
		pb, err := getOpenapiV2Proto()
		if err != nil {
			http.Error(w, "openapi proto: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/com.github.proto-openapi.spec.v2.v1.0+protobuf")
		w.Write(pb)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(openapiV2Schema))
}

// handleOpenAPIv3 returns 404 so kubectl falls back to the v2 endpoint.
// Serving our v3 with 406 caused per-group schema fetches to fail without
// retry; 404 is the clean "not supported" signal kubectl handles gracefully.
func (s *Server) handleOpenAPIv3(w http.ResponseWriter, r *http.Request) {
	http.NotFound(w, r)
}

// handleNodes returns a NodeList with a single synthetic node representing the
// local machine. Hostname, OS, architecture, and kernel version are read from
// the system at request time; kubelet version mirrors the API server version.
func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	hostname, _ := os.Hostname()
	var uts unix.Utsname
	_ = unix.Uname(&uts)

	kernelVersion := unix.ByteSliceToString(uts.Release[:])
	osImage := getOSImage()
	machineArch := unix.ByteSliceToString(uts.Machine[:])
	architecture := runtime.GOARCH

	vInfo := getVersionInfo()
	kubeletVersion := vInfo.GitVersion
	containerRuntime := "podman://" + getPodmanVersion()
	now := time.Now().UTC()
	bootTime := getBootTime()
	internalIP := getInternalIP()

	if isTableRequest(r) {
		t := newTable("1", nodeColumns)
		t.Rows = append(t.Rows, tableRow{
			Cells: []interface{}{
				hostname,          // Name
				"Ready",           // Status
				"<none>",          // Roles
				age(bootTime),     // Age
				kubeletVersion,    // Version
				internalIP,        // Internal-IP
				"<none>",          // External-IP
				osImage,           // OS-Image
				kernelVersion,     // Kernel-Version
				containerRuntime,  // Container-Runtime
			},
			Object: partialMeta{
				Kind:       "Node",
				APIVersion: "v1",
				Metadata: metav1.ObjectMeta{
					Name:              hostname,
					CreationTimestamp: metav1.NewTime(bootTime),
					Labels: map[string]string{
						"kubernetes.io/hostname":           hostname,
						"kubernetes.io/os":                 runtime.GOOS,
						"kubernetes.io/arch":               architecture,
						"node.kubernetes.io/instance-type": "q8s",
					},
				},
			},
		})
		encodeTable(w, t)
		return
	}

	node := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Node",
		"metadata": map[string]interface{}{
			"name":              hostname,
			"uid":               "q8s-node-" + hostname,
			"creationTimestamp": bootTime.Format(time.RFC3339),
			"labels": map[string]string{
				"kubernetes.io/hostname":           hostname,
				"kubernetes.io/os":                 runtime.GOOS,
				"kubernetes.io/arch":               architecture,
				"node.kubernetes.io/instance-type": "q8s",
			},
		},
		"spec": map[string]interface{}{},
		"status": map[string]interface{}{
			"conditions": []map[string]interface{}{
				{
					"type":               "Ready",
					"status":             "True",
					"lastHeartbeatTime":  now.Format(time.RFC3339),
					"lastTransitionTime": bootTime.Format(time.RFC3339),
					"reason":             "KubeletReady",
					"message":            "q8s node is always ready",
				},
				{
					"type":               "MemoryPressure",
					"status":             "False",
					"lastHeartbeatTime":  now.Format(time.RFC3339),
					"lastTransitionTime": bootTime.Format(time.RFC3339),
					"reason":             "KubeletHasSufficientMemory",
					"message":            "q8s has sufficient memory available",
				},
				{
					"type":               "DiskPressure",
					"status":             "False",
					"lastHeartbeatTime":  now.Format(time.RFC3339),
					"lastTransitionTime": bootTime.Format(time.RFC3339),
					"reason":             "KubeletHasNoDiskPressure",
					"message":            "q8s has no disk pressure",
				},
				{
					"type":               "PIDPressure",
					"status":             "False",
					"lastHeartbeatTime":  now.Format(time.RFC3339),
					"lastTransitionTime": bootTime.Format(time.RFC3339),
					"reason":             "KubeletHasSufficientPID",
					"message":            "q8s has sufficient PID available",
				},
			},
			"addresses": []map[string]string{
				{"type": "Hostname", "address": hostname},
				{"type": "InternalIP", "address": internalIP},
			},
			"nodeInfo": map[string]string{
				"machineID":               getMachineID(),
				"systemUUID":              getMachineID(),
				"bootID":                  getBootID(),
				"kernelVersion":           kernelVersion,
				"osImage":                 osImage,
				"containerRuntimeVersion": containerRuntime,
				"kubeletVersion":          kubeletVersion,
				"kubeProxyVersion":        kubeletVersion,
				"operatingSystem":         runtime.GOOS,
				"architecture":            machineArch,
			},
			"capacity": map[string]string{
				"cpu":               fmt.Sprintf("%d", runtime.NumCPU()),
				"memory":            getMemoryCapacity(),
				"ephemeral-storage": getEphemeralStorage(),
				"pods":              "110",
			},
			"allocatable": map[string]string{
				"cpu":               fmt.Sprintf("%d", runtime.NumCPU()),
				"memory":            getMemoryCapacity(),
				"ephemeral-storage": getEphemeralStorageAvailable(),
				"pods":              "110",
			},
		},
	}

	nodeList := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "NodeList",
		"metadata":   map[string]interface{}{"resourceVersion": "1"},
		"items":      []interface{}{node},
	}

	data, _ := json.Marshal(nodeList)
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// handleNodeGet handles GET /api/v1/nodes/{name} — returns the single synthetic
// node if the name matches the hostname, 404 otherwise.
func (s *Server) handleNodeGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/api/v1/nodes/")
	name = strings.TrimSuffix(name, "/")

	hostname, _ := os.Hostname()
	if name != hostname {
		http.Error(w, fmt.Sprintf(`{"kind":"Status","apiVersion":"v1","status":"Failure","message":"nodes %q not found","reason":"NotFound","code":404}`, name), http.StatusNotFound)
		return
	}

	// Reuse handleNodes logic but return a single Node instead of NodeList
	var uts unix.Utsname
	_ = unix.Uname(&uts)

	kernelVersion := unix.ByteSliceToString(uts.Release[:])
	osImage := getOSImage()
	machineArch := unix.ByteSliceToString(uts.Machine[:])

	vInfo := getVersionInfo()
	kubeletVersion := vInfo.GitVersion
	containerRuntime := "podman://" + getPodmanVersion()
	now := time.Now().UTC()
	bootTime := getBootTime()
	internalIP := getInternalIP()

	node := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Node",
		"metadata": map[string]interface{}{
			"name":              hostname,
			"uid":               "q8s-node-" + hostname,
			"creationTimestamp": bootTime.Format(time.RFC3339),
			"labels": map[string]string{
				"kubernetes.io/hostname":           hostname,
				"kubernetes.io/os":                 runtime.GOOS,
				"kubernetes.io/arch":               runtime.GOARCH,
				"node.kubernetes.io/instance-type": "q8s",
			},
		},
		"spec": map[string]interface{}{},
		"status": map[string]interface{}{
			"conditions": []map[string]interface{}{
				{"type": "Ready", "status": "True", "lastHeartbeatTime": now.Format(time.RFC3339), "lastTransitionTime": bootTime.Format(time.RFC3339), "reason": "KubeletReady", "message": "q8s node is always ready"},
				{"type": "MemoryPressure", "status": "False", "lastHeartbeatTime": now.Format(time.RFC3339), "lastTransitionTime": bootTime.Format(time.RFC3339), "reason": "KubeletHasSufficientMemory", "message": "q8s has sufficient memory available"},
				{"type": "DiskPressure", "status": "False", "lastHeartbeatTime": now.Format(time.RFC3339), "lastTransitionTime": bootTime.Format(time.RFC3339), "reason": "KubeletHasNoDiskPressure", "message": "q8s has no disk pressure"},
				{"type": "PIDPressure", "status": "False", "lastHeartbeatTime": now.Format(time.RFC3339), "lastTransitionTime": bootTime.Format(time.RFC3339), "reason": "KubeletHasSufficientPID", "message": "q8s has sufficient PID available"},
			},
			"addresses": []map[string]string{
				{"type": "Hostname", "address": hostname},
				{"type": "InternalIP", "address": internalIP},
			},
			"nodeInfo": map[string]string{
				"machineID":               getMachineID(),
				"systemUUID":              getMachineID(),
				"bootID":                  getBootID(),
				"kernelVersion":           kernelVersion,
				"osImage":                 osImage,
				"containerRuntimeVersion": containerRuntime,
				"kubeletVersion":          kubeletVersion,
				"kubeProxyVersion":        kubeletVersion,
				"operatingSystem":         runtime.GOOS,
				"architecture":            machineArch,
			},
			"capacity": map[string]string{
				"cpu":               fmt.Sprintf("%d", runtime.NumCPU()),
				"memory":            getMemoryCapacity(),
				"ephemeral-storage": getEphemeralStorage(),
				"pods":              "110",
			},
			"allocatable": map[string]string{
				"cpu":               fmt.Sprintf("%d", runtime.NumCPU()),
				"memory":            getMemoryCapacity(),
				"ephemeral-storage": getEphemeralStorageAvailable(),
				"pods":              "110",
			},
		},
	}

	data, _ := json.Marshal(node)
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// getMemoryCapacity reads total system memory from /proc/meminfo and returns
// it in Ki (kibibytes) as a Kubernetes resource quantity string.
func getMemoryCapacity() string {
	var sysinfo unix.Sysinfo_t
	if err := unix.Sysinfo(&sysinfo); err != nil {
		return "0Ki"
	}
	totalKi := sysinfo.Totalram * uint64(sysinfo.Unit) / 1024
	return fmt.Sprintf("%dKi", totalKi)
}

// getBootTime returns the machine boot time derived from the system uptime.
func getBootTime() time.Time {
	var sysinfo unix.Sysinfo_t
	if err := unix.Sysinfo(&sysinfo); err != nil {
		return time.Now().UTC()
	}
	return time.Now().Add(-time.Duration(sysinfo.Uptime) * time.Second).UTC()
}

// getInternalIP returns the first non-loopback IPv4 address on the machine.
func getInternalIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "127.0.0.1"
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			if ip4 := ip.To4(); ip4 != nil {
				return ip4.String()
			}
		}
	}
	return "127.0.0.1"
}

// getEphemeralStorage returns the available disk space on / in Ki.
func getEphemeralStorage() string {
	var stat unix.Statfs_t
	if err := unix.Statfs("/", &stat); err != nil {
		return "0Ki"
	}
	totalKi := stat.Blocks * uint64(stat.Bsize) / 1024
	return fmt.Sprintf("%dKi", totalKi)
}

// getEphemeralStorageAvailable returns the free disk space on / in Ki.
func getEphemeralStorageAvailable() string {
	var stat unix.Statfs_t
	if err := unix.Statfs("/", &stat); err != nil {
		return "0Ki"
	}
	availKi := stat.Bavail * uint64(stat.Bsize) / 1024
	return fmt.Sprintf("%dKi", availKi)
}

// getMachineID reads /etc/machine-id.
func getMachineID() string {
	data, err := os.ReadFile("/etc/machine-id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// getBootID reads /proc/sys/kernel/random/boot_id.
func getBootID() string {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// getPodmanVersion shells out to `podman --version` and extracts the version
// string. Returns "unknown" if podman isn't available.
func getPodmanVersion() string {
	out, err := exec.Command("podman", "--version").Output()
	if err != nil {
		return "unknown"
	}
	// output is "podman version X.Y.Z\n"
	s := strings.TrimSpace(string(out))
	if _, v, ok := strings.Cut(s, "version "); ok {
		return v
	}
	return s
}

// getOSImage reads PRETTY_NAME from /etc/os-release. Falls back to
// "Linux" if the file is missing or unparseable.
func getOSImage() string {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "Linux"
	}
	for _, line := range strings.Split(string(data), "\n") {
		if key, val, ok := strings.Cut(line, "="); ok && key == "PRETTY_NAME" {
			return strings.Trim(val, "\"")
		}
	}
	return "Linux"
}

// openapiV2Schema is a Swagger 2.0 document with permissive definitions for all
// resource types supported by q8s. {"type":"object"} with no additionalProperties:false
// means any object structure is valid — kubectl validation passes for all fields.
const openapiV2Schema = `{
  "swagger": "2.0",
  "info": {"title": "q8s", "version": "v0"},
  "paths": {},
  "definitions": {
    "io.k8s.api.core.v1.Pod":                          {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.core.v1.PodList":                      {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.core.v1.Node":                         {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.core.v1.NodeList":                     {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.core.v1.Service":                      {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.core.v1.ServiceList":                  {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.core.v1.Namespace":                    {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.core.v1.NamespaceList":                {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.core.v1.PersistentVolumeClaim":        {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.core.v1.PersistentVolumeClaimList":    {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.core.v1.ConfigMap":                    {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.core.v1.ConfigMapList":                {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.core.v1.Secret":                       {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.core.v1.SecretList":                   {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.core.v1.Event":                        {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.core.v1.EventList":                    {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.apps.v1.Deployment":                   {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.apps.v1.DeploymentList":               {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.apps.v1.DaemonSet":                     {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.apps.v1.DaemonSetList":                 {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.apps.v1.StatefulSet":                   {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.apps.v1.StatefulSetList":               {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.apps.v1.ReplicaSet":                    {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.apps.v1.ReplicaSetList":                {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.batch.v1.Job":                         {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.batch.v1.JobList":                     {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.batch.v1.CronJob":                     {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.batch.v1.CronJobList":                 {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.networking.v1.Ingress":                {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.networking.v1.IngressList":            {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.storage.v1.StorageClass":              {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
    "io.k8s.api.storage.v1.StorageClassList":          {"type": "object", "x-kubernetes-preserve-unknown-fields": true}
  }
}`

func verbList() []string {
	return []string{"get", "list", "watch", "create", "update", "patch", "delete", "deletecollection"}
}

// --- storage.k8s.io ---

// handleStorageRoot serves the storage.k8s.io API group discovery.
func (s *Server) handleStorageRoot(w http.ResponseWriter, r *http.Request) {
	data, _ := json.Marshal(metav1.APIGroup{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "APIGroup"},
		Name:     "storage.k8s.io",
		Versions: []metav1.GroupVersionForDiscovery{
			{GroupVersion: "storage.k8s.io/v1", Version: "v1"},
		},
		PreferredVersion: metav1.GroupVersionForDiscovery{
			GroupVersion: "storage.k8s.io/v1",
			Version:      "v1",
		},
	})
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// handleStorageV1 serves the storage.k8s.io/v1 resource list.
func (s *Server) handleStorageV1(w http.ResponseWriter, r *http.Request) {
	data, _ := json.Marshal(metav1.APIResourceList{
		TypeMeta:     metav1.TypeMeta{APIVersion: "v1", Kind: "APIResourceList"},
		GroupVersion: "storage.k8s.io/v1",
		APIResources: []metav1.APIResource{
			{Name: "storageclasses", SingularName: "storageclass", Namespaced: false, Kind: "StorageClass", Verbs: []string{"get", "list"}, ShortNames: []string{"sc"}},
		},
	})
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// handleStorageClasses returns the fixed set of storage classes supported by q8s.
func (s *Server) handleStorageClasses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract name from path: /apis/storage.k8s.io/v1/storageclasses/{name}
	path := strings.TrimPrefix(r.URL.Path, "/apis/storage.k8s.io/v1/storageclasses")
	name := strings.Trim(path, "/")

	classes := []map[string]interface{}{
		{
			"apiVersion": "storage.k8s.io/v1",
			"kind":       "StorageClass",
			"metadata": map[string]interface{}{
				"name": quadlet.StorageClassStandard,
				"annotations": map[string]string{
					"storageclass.kubernetes.io/is-default-class": "true",
				},
			},
			"provisioner":       "q8s.io/podman-volume",
			"reclaimPolicy":     "Retain",
			"volumeBindingMode": "Immediate",
		},
		{
			"apiVersion": "storage.k8s.io/v1",
			"kind":       "StorageClass",
			"metadata":   map[string]interface{}{"name": quadlet.StorageClassShared},
			"provisioner":       "q8s.io/podman-volume",
			"reclaimPolicy":     "Retain",
			"volumeBindingMode": "Immediate",
		},
		{
			"apiVersion": "storage.k8s.io/v1",
			"kind":       "StorageClass",
			"metadata":   map[string]interface{}{"name": quadlet.StorageClassHostPath},
			"provisioner":       "q8s.io/host-path",
			"reclaimPolicy":     "Retain",
			"volumeBindingMode": "Immediate",
		},
	}

	// Single storageclass GET
	if name != "" {
		for _, sc := range classes {
			meta := sc["metadata"].(map[string]interface{})
			if meta["name"] == name {
				data, _ := json.Marshal(sc)
				w.Header().Set("Content-Type", "application/json")
				w.Write(data)
				return
			}
		}
		s.respondStatus(w, http.StatusNotFound, "NotFound", "storageclasses %q not found", name)
		return
	}

	// List
	data, _ := json.Marshal(map[string]interface{}{
		"apiVersion": "storage.k8s.io/v1",
		"kind":       "StorageClassList",
		"metadata":   map[string]interface{}{"resourceVersion": s.rv()},
		"items":      classes,
	})
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// DiscoveryResources returns the API resources for kubectl discovery.
func DiscoveryResources() []schema.GroupVersionResource {
	return []schema.GroupVersionResource{
		{Group: "", Version: "v1", Resource: "pods"},
		{Group: "", Version: "v1", Resource: "services"},
		{Group: "", Version: "v1", Resource: "namespaces"},
		{Group: "", Version: "v1", Resource: "persistentvolumeclaims"},
		{Group: "", Version: "v1", Resource: "configmaps"},
		{Group: "", Version: "v1", Resource: "secrets"},
		{Group: "", Version: "v1", Resource: "events"},
		{Group: "batch", Version: "v1", Resource: "jobs"},
		{Group: "batch", Version: "v1", Resource: "cronjobs"},
		{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"},
	}
}
