package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// handleMetricsRoot serves the metrics.k8s.io API group discovery.
func (s *Server) handleMetricsRoot(w http.ResponseWriter, r *http.Request) {
	data, _ := json.Marshal(metav1.APIGroup{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "APIGroup",
		},
		Name: "metrics.k8s.io",
		Versions: []metav1.GroupVersionForDiscovery{
			{GroupVersion: "metrics.k8s.io/v1beta1", Version: "v1beta1"},
		},
		PreferredVersion: metav1.GroupVersionForDiscovery{
			GroupVersion: "metrics.k8s.io/v1beta1",
			Version:      "v1beta1",
		},
	})
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// handleMetricsV1beta1 serves the metrics.k8s.io/v1beta1 resource list.
func (s *Server) handleMetricsV1beta1(w http.ResponseWriter, r *http.Request) {
	data, _ := json.Marshal(metav1.APIResourceList{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "APIResourceList",
		},
		GroupVersion: "metrics.k8s.io/v1beta1",
		APIResources: []metav1.APIResource{
			{Name: "nodes", SingularName: "node", Namespaced: false, Kind: "NodeMetrics", Verbs: []string{"get", "list"}},
			{Name: "pods", SingularName: "pod", Namespaced: true, Kind: "PodMetrics", Verbs: []string{"get", "list"}},
		},
	})
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// handleNodeMetrics serves GET /apis/metrics.k8s.io/v1beta1/nodes with real
// CPU and memory usage from /proc/stat and /proc/meminfo.
func (s *Server) handleNodeMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	hostname, _ := os.Hostname()
	cpuUsage := getCPUUsageNanocores()
	memUsage := getMemoryUsageKi()
	now := time.Now().UTC()

	nodeMetrics := map[string]interface{}{
		"kind":       "NodeMetrics",
		"apiVersion": "metrics.k8s.io/v1beta1",
		"metadata": map[string]interface{}{
			"name":              hostname,
			"creationTimestamp": now.Format(time.RFC3339),
		},
		"timestamp": now.Format(time.RFC3339),
		"window":    "30s",
		"usage": map[string]string{
			"cpu":    fmt.Sprintf("%dn", cpuUsage),
			"memory": fmt.Sprintf("%dKi", memUsage),
		},
	}

	nodeMetricsList := map[string]interface{}{
		"kind":       "NodeMetricsList",
		"apiVersion": "metrics.k8s.io/v1beta1",
		"metadata":   map[string]interface{}{},
		"items":      []interface{}{nodeMetrics},
	}

	data, _ := json.Marshal(nodeMetricsList)
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// handlePodMetrics serves GET /apis/metrics.k8s.io/v1beta1/pods and
// /apis/metrics.k8s.io/v1beta1/namespaces/{ns}/pods with per-pod CPU/memory
// from podman stats.
func (s *Server) handlePodMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Check if this is a namespaced request
	var nsFilter string
	path := r.URL.Path
	if strings.Contains(path, "/namespaces/") {
		parts := strings.Split(strings.TrimPrefix(path, "/apis/metrics.k8s.io/v1beta1/namespaces/"), "/")
		if len(parts) >= 1 {
			nsFilter = parts[0]
		}
	}

	pods := s.config.Store.AllPods()
	now := time.Now().UTC()
	var items []interface{}

	for _, pod := range pods {
		if nsFilter != "" && pod.Namespace != nsFilter {
			continue
		}
		containerName := fmt.Sprintf("%s-%s", pod.Namespace, pod.Name)
		cpu, mem := getPodStats(containerName)

		var containers []map[string]interface{}
		for _, c := range pod.Spec.Containers {
			containers = append(containers, map[string]interface{}{
				"name": c.Name,
				"usage": map[string]string{
					"cpu":    cpu,
					"memory": mem,
				},
			})
		}

		items = append(items, map[string]interface{}{
			"kind":       "PodMetrics",
			"apiVersion": "metrics.k8s.io/v1beta1",
			"metadata": map[string]interface{}{
				"name":              pod.Name,
				"namespace":         pod.Namespace,
				"creationTimestamp": now.Format(time.RFC3339),
			},
			"timestamp":  now.Format(time.RFC3339),
			"window":     "30s",
			"containers": containers,
		})
	}

	if items == nil {
		items = []interface{}{}
	}

	data, _ := json.Marshal(map[string]interface{}{
		"kind":       "PodMetricsList",
		"apiVersion": "metrics.k8s.io/v1beta1",
		"metadata":   map[string]interface{}{},
		"items":      items,
	})
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// getPodStats runs `podman stats --no-stream` for a container and returns
// CPU (nanocores string) and memory (Ki string) usage.
func getPodStats(containerName string) (cpu, mem string) {
	out, err := exec.Command("podman", "stats", "--no-stream", "--format",
		"{{.CPUPerc}} {{.MemUsage}}", containerName).Output()
	if err != nil {
		return "0", "0Ki"
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) < 3 {
		return "0", "0Ki"
	}

	// CPU: "1.23%" -> nanocores
	cpuStr := strings.TrimSuffix(fields[0], "%")
	cpuPct, _ := strconv.ParseFloat(cpuStr, 64)
	nanocores := int64(cpuPct / 100.0 * float64(runtime.NumCPU()) * 1e9)
	cpu = fmt.Sprintf("%dn", nanocores)

	// Memory: "123.4MB" / "1.2GB" / "456kB" -> Ki
	memStr := fields[1] // e.g. "123.4MB"
	mem = parseMemToKi(memStr)

	return cpu, mem
}

// parseMemToKi parses podman memory strings like "123.4MB", "1.2GB", "456kB" to Ki.
func parseMemToKi(s string) string {
	s = strings.TrimSpace(s)
	var multiplier float64
	switch {
	case strings.HasSuffix(s, "GB"):
		multiplier = 1024 * 1024 // GB -> Ki
		s = strings.TrimSuffix(s, "GB")
	case strings.HasSuffix(s, "MB"):
		multiplier = 1024 // MB -> Ki
		s = strings.TrimSuffix(s, "MB")
	case strings.HasSuffix(s, "kB"):
		multiplier = 1 // kB ≈ Ki
		s = strings.TrimSuffix(s, "kB")
	case strings.HasSuffix(s, "B"):
		multiplier = 1.0 / 1024
		s = strings.TrimSuffix(s, "B")
	default:
		return "0Ki"
	}
	val, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return "0Ki"
	}
	return fmt.Sprintf("%dKi", int64(val*multiplier))
}

// getCPUUsageNanocores samples /proc/stat twice (100ms apart) and computes
// the CPU usage in nanocores (1 core = 1e9 nanocores).
func getCPUUsageNanocores() int64 {
	idle1, total1 := readCPUStat()
	time.Sleep(100 * time.Millisecond)
	idle2, total2 := readCPUStat()

	idleDelta := idle2 - idle1
	totalDelta := total2 - total1
	if totalDelta == 0 {
		return 0
	}

	// usage fraction × number of cores × 1e9 nanocores/core
	usageFraction := float64(totalDelta-idleDelta) / float64(totalDelta)
	nanocores := int64(usageFraction * float64(runtime.NumCPU()) * 1e9)
	return nanocores
}

// readCPUStat reads the aggregate "cpu" line from /proc/stat and returns
// (idle ticks, total ticks).
func readCPUStat() (idle, total uint64) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			return 0, 0
		}
		// fields: cpu user nice system idle iowait irq softirq steal guest guest_nice
		var sum uint64
		var idleVal uint64
		for i := 1; i < len(fields); i++ {
			val, _ := strconv.ParseUint(fields[i], 10, 64)
			sum += val
			if i == 4 { // idle is the 4th value (0-indexed field 4)
				idleVal = val
			}
			if i == 5 { // iowait counts as idle
				idleVal += val
			}
		}
		return idleVal, sum
	}
	return 0, 0
}

// getMemoryUsageKi reads /proc/meminfo and returns used memory in Ki.
// Used = MemTotal - MemAvailable
func getMemoryUsageKi() int64 {
	var sysinfo unix.Sysinfo_t
	if err := unix.Sysinfo(&sysinfo); err != nil {
		return 0
	}
	totalKi := int64(sysinfo.Totalram * uint64(sysinfo.Unit) / 1024)

	// Read MemAvailable from /proc/meminfo for a more accurate value
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		// Fallback: use sysinfo free ram
		freeKi := int64(sysinfo.Freeram * uint64(sysinfo.Unit) / 1024)
		return totalKi - freeKi
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "MemAvailable:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				avail, _ := strconv.ParseInt(fields[1], 10, 64)
				return totalKi - avail
			}
		}
	}

	freeKi := int64(sysinfo.Freeram * uint64(sysinfo.Unit) / 1024)
	return totalKi - freeKi
}


// --- coordination.k8s.io (node lease for kubectl describe node) ---

// handleCoordinationRoot serves the coordination.k8s.io API group discovery.
func (s *Server) handleCoordinationRoot(w http.ResponseWriter, r *http.Request) {
	data, _ := json.Marshal(metav1.APIGroup{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "APIGroup"},
		Name:     "coordination.k8s.io",
		Versions: []metav1.GroupVersionForDiscovery{
			{GroupVersion: "coordination.k8s.io/v1", Version: "v1"},
		},
		PreferredVersion: metav1.GroupVersionForDiscovery{
			GroupVersion: "coordination.k8s.io/v1",
			Version:      "v1",
		},
	})
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// handleCoordinationV1 serves the coordination.k8s.io/v1 resource list.
func (s *Server) handleCoordinationV1(w http.ResponseWriter, r *http.Request) {
	data, _ := json.Marshal(metav1.APIResourceList{
		TypeMeta:     metav1.TypeMeta{APIVersion: "v1", Kind: "APIResourceList"},
		GroupVersion: "coordination.k8s.io/v1",
		APIResources: []metav1.APIResource{
			{Name: "leases", SingularName: "lease", Namespaced: true, Kind: "Lease", Verbs: []string{"get", "list"}},
		},
	})
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// handleLease serves GET /apis/coordination.k8s.io/v1/namespaces/{ns}/leases/{name}
// Returns a synthetic lease for the node so kubectl describe node doesn't error.
func (s *Server) handleLease(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse: /apis/coordination.k8s.io/v1/namespaces/{ns}/leases[/{name}]
	path := strings.TrimPrefix(r.URL.Path, "/apis/coordination.k8s.io/v1/namespaces/")
	parts := strings.SplitN(path, "/", 3)
	// parts[0] = namespace, parts[1] = "leases", parts[2] = name (optional)

	hostname, _ := os.Hostname()
	now := time.Now().UTC()

	lease := map[string]interface{}{
		"apiVersion": "coordination.k8s.io/v1",
		"kind":       "Lease",
		"metadata": map[string]interface{}{
			"name":              hostname,
			"namespace":         "kube-node-lease",
			"creationTimestamp": now.Format(time.RFC3339),
		},
		"spec": map[string]interface{}{
			"holderIdentity":       hostname,
			"leaseDurationSeconds": 40,
			"renewTime":            now.Format("2006-01-02T15:04:05.000000Z"),
		},
	}

	// If no specific name requested, return a list
	if len(parts) < 3 || parts[2] == "" {
		data, _ := json.Marshal(map[string]interface{}{
			"apiVersion": "coordination.k8s.io/v1",
			"kind":       "LeaseList",
			"metadata":   map[string]interface{}{"resourceVersion": "1"},
			"items":      []interface{}{lease},
		})
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
		return
	}

	// Single lease GET
	name := parts[2]
	if name != hostname {
		http.Error(w, fmt.Sprintf(`{"kind":"Status","apiVersion":"v1","status":"Failure","message":"leases.coordination.k8s.io %q not found","reason":"NotFound","code":404}`, name), http.StatusNotFound)
		return
	}

	data, _ := json.Marshal(lease)
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}
