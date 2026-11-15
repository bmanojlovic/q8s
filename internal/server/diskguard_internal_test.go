package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"q8s/internal/store"
)

func setStatfs(t *testing.T, fn func(path string, st *unix.Statfs_t) error) {
	t.Helper()
	old := statfsFn
	statfsFn = fn
	t.Cleanup(func() { statfsFn = old })
}

func statfsResult(free, size uint64) func(string, *unix.Statfs_t) error {
	return func(_ string, st *unix.Statfs_t) error {
		// Bsize 1 keeps the test values byte exact.
		st.Bsize = 1
		st.Blocks = size
		st.Bavail = free
		return nil
	}
}

func TestDiskPressureThresholds(t *testing.T) {
	oldBytes, oldPercent := minFreeBytes, minFreePercent
	t.Cleanup(func() { minFreeBytes, minFreePercent = oldBytes, oldPercent })
	minFreeBytes, minFreePercent = 100<<20, 5

	tests := []struct {
		name string
		free uint64
		size uint64
		low  bool
	}{
		{name: "big fs below 100MiB", free: 99 << 20, size: 10 << 30, low: true},
		{name: "big fs at 100MiB", free: 100 << 20, size: 10 << 30, low: false},
		{name: "big fs above 100MiB", free: 200 << 20, size: 10 << 30, low: false},
		{name: "small fs below 2.5MiB", free: 2 << 20, size: 50 << 20, low: true},
		{name: "small fs at 2.5MiB", free: 5 << 20 / 2, size: 50 << 20, low: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setStatfs(t, statfsResult(tc.free, tc.size))
			low, detail := diskPressure([]string{"/data"})
			if low != tc.low {
				t.Fatalf("diskPressure = (%v, %q), want low=%v", low, detail, tc.low)
			}
			if !tc.low && detail != "" {
				t.Errorf("detail = %q, want empty when not low", detail)
			}
		})
	}
}

func TestDiskPressureNamesFirstLowPath(t *testing.T) {
	setStatfs(t, statfsResult(52<<20, 10<<30))
	low, detail := diskPressure([]string{"/first", "/second"})
	if !low {
		t.Fatal("diskPressure = false, want true")
	}
	want := "/first: 52.0 MiB free, need 100.0 MiB"
	if detail != want {
		t.Errorf("detail = %q, want %q", detail, want)
	}
}

func TestDiskPressureWalksUpToExistingAncestor(t *testing.T) {
	var calls []string
	setStatfs(t, func(path string, st *unix.Statfs_t) error {
		calls = append(calls, path)
		if path != "/nonexistent-parent" {
			return unix.ENOENT
		}
		st.Bsize = 1
		st.Blocks = 10 << 30
		st.Bavail = 1 << 20
		return nil
	})
	low, _ := diskPressure([]string{"/nonexistent-parent/child"})
	if !low {
		t.Fatal("diskPressure = false, want true for the ancestor")
	}
	if len(calls) != 2 || calls[0] != "/nonexistent-parent/child" || calls[1] != "/nonexistent-parent" {
		t.Errorf("statfs calls = %v, want the child then its parent", calls)
	}
}

func TestDiskPressureStatfsErrorIsNotLow(t *testing.T) {
	setStatfs(t, func(string, *unix.Statfs_t) error { return unix.EACCES })
	low, detail := diskPressure([]string{"/data"})
	if low || detail != "" {
		t.Errorf("diskPressure = (%v, %q), want (false, empty)", low, detail)
	}
}

func TestDiskPressureCache(t *testing.T) {
	oldTTL := diskPressureTTL
	t.Cleanup(func() { diskPressureTTL = oldTTL })

	calls := 0
	setStatfs(t, func(_ string, st *unix.Statfs_t) error {
		calls++
		st.Bsize = 1
		st.Blocks = 10 << 30
		st.Bavail = 10 << 30
		return nil
	})
	s := &Server{config: Config{QuadletDir: "/q8s-quadlet"}}

	s.diskPressureLow()
	s.diskPressureLow()
	if calls != 1 {
		t.Fatalf("statfs calls = %d, want 1 while the result is cached", calls)
	}

	diskPressureTTL = 0
	s.diskPressureLow()
	if calls != 2 {
		t.Fatalf("statfs calls = %d, want 2 after the cache expired", calls)
	}
}

func newGuardServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st := store.New()
	s := &Server{
		config: Config{
			Store:      st,
			ConfigDir:  t.TempDir(),
			QuadletDir: t.TempDir(),
		},
		mux:     http.NewServeMux(),
		network: &networkState{subnets: map[string]string{}},
	}
	s.setupRoutes()
	s.handler = s.mux
	return s, st
}

const guardConfigMapBody = `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"example"},"data":{"k":"v"}}`

func TestServeHTTPRefusesWritesOnLowDisk(t *testing.T) {
	setStatfs(t, statfsResult(1<<20, 10<<30))
	s, st := newGuardServer(t)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			path := "/api/v1/namespaces/default/configmaps"
			if method != http.MethodPost {
				path += "/example"
			}
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, httptest.NewRequest(method, path, bytes.NewReader([]byte(guardConfigMapBody))))

			if rec.Code != http.StatusInsufficientStorage {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, http.StatusInsufficientStorage, rec.Body.String())
			}
			var status metav1.Status
			if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
				t.Fatalf("decode status: %v", err)
			}
			if status.Reason != metav1.StatusReason("InsufficientStorage") {
				t.Errorf("reason = %q, want InsufficientStorage", status.Reason)
			}
			if !strings.Contains(status.Message, "q8s refuses changes: low disk space (") {
				t.Errorf("message = %q, want the low disk space refusal", status.Message)
			}
		})
	}
	if got := st.ConfigMaps("default"); len(got) != 0 {
		t.Fatalf("store has %d configmaps after refused writes, want 0", len(got))
	}
}

func TestServeHTTPAllowsReadsAndDeletesOnLowDisk(t *testing.T) {
	setStatfs(t, statfsResult(1<<20, 10<<30))
	s, _ := newGuardServer(t)

	for _, tc := range []struct {
		name   string
		method string
		path   string
	}{
		{name: "GET list", method: http.MethodGet, path: "/api/v1/namespaces/default/configmaps"},
		{name: "HEAD", method: http.MethodHead, path: "/api/v1/namespaces/default/configmaps"},
		{name: "DELETE", method: http.MethodDelete, path: "/api/v1/namespaces/default/configmaps/example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
			if rec.Code == http.StatusInsufficientStorage {
				t.Fatalf("%s %s got 507, want it to pass the disk guard", tc.method, tc.path)
			}
		})
	}
}

func TestServeHTTPAllowsPodStreamingSubresourcesOnLowDisk(t *testing.T) {
	setStatfs(t, statfsResult(1<<20, 10<<30))
	s, _ := newGuardServer(t)

	for _, sub := range []string{"exec", "attach", "portforward"} {
		for _, method := range []string{http.MethodPost, http.MethodGet} {
			t.Run(method+" "+sub, func(t *testing.T) {
				path := "/api/v1/namespaces/default/pods/example/" + sub
				rec := httptest.NewRecorder()
				s.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
				if rec.Code == http.StatusInsufficientStorage {
					t.Fatalf("%s %s got 507, want it to pass the disk guard", method, path)
				}
			})
		}
	}
}

func TestServeHTTPWritesWhenDiskIsNotLow(t *testing.T) {
	setStatfs(t, statfsResult(10<<30, 10<<30))
	s, st := newGuardServer(t)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/namespaces/default/configmaps",
		bytes.NewReader([]byte(guardConfigMapBody))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, http.StatusCreated, rec.Body.String())
	}
	if _, err := st.GetConfigMap("default", "example"); err != nil {
		t.Fatalf("configmap was not stored: %v", err)
	}
}

type nodeCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type nodeStatus struct {
	Status struct {
		Conditions []nodeCondition `json:"conditions"`
	} `json:"status"`
}

func diskPressureFromNode(t *testing.T, node nodeStatus) nodeCondition {
	t.Helper()
	for _, c := range node.Status.Conditions {
		if c.Type == "DiskPressure" {
			return c
		}
	}
	t.Fatalf("DiskPressure condition not found in %+v", node.Status.Conditions)
	return nodeCondition{}
}

func TestNodeDiskPressureCondition(t *testing.T) {
	tests := []struct {
		name       string
		free       uint64
		wantStatus string
		wantReason string
		wantMsg    string
	}{
		{
			name:       "low",
			free:       1 << 20,
			wantStatus: "True",
			wantReason: "KubeletHasDiskPressure",
			wantMsg:    "free, need",
		},
		{
			name:       "not low",
			free:       10 << 30,
			wantStatus: "False",
			wantReason: "KubeletHasNoDiskPressure",
			wantMsg:    "q8s has no disk pressure",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setStatfs(t, statfsResult(tc.free, 10<<30))
			s := &Server{config: Config{Store: store.New(), QuadletDir: t.TempDir()}}

			rec := httptest.NewRecorder()
			s.handleNodes(rec, httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil))
			var list struct {
				Items []nodeStatus `json:"items"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
				t.Fatalf("decode node list: %v", err)
			}
			if len(list.Items) != 1 {
				t.Fatalf("node list has %d items, want 1", len(list.Items))
			}
			assertDiskPressureCondition(t, diskPressureFromNode(t, list.Items[0]), tc.wantStatus, tc.wantReason, tc.wantMsg)

			hostname, err := os.Hostname()
			if err != nil {
				t.Fatalf("hostname: %v", err)
			}
			rec = httptest.NewRecorder()
			s.handleNodeGet(rec, httptest.NewRequest(http.MethodGet, "/api/v1/nodes/"+hostname, nil))
			var node nodeStatus
			if err := json.Unmarshal(rec.Body.Bytes(), &node); err != nil {
				t.Fatalf("decode node: %v", err)
			}
			assertDiskPressureCondition(t, diskPressureFromNode(t, node), tc.wantStatus, tc.wantReason, tc.wantMsg)
		})
	}
}

func assertDiskPressureCondition(t *testing.T, c nodeCondition, wantStatus, wantReason, wantMsg string) {
	t.Helper()
	if c.Status != wantStatus || c.Reason != wantReason {
		t.Errorf("condition = %+v, want status %q reason %q", c, wantStatus, wantReason)
	}
	if !strings.Contains(c.Message, wantMsg) {
		t.Errorf("message = %q, want it to contain %q", c.Message, wantMsg)
	}
}

func TestNodeSpecDiskPressureTaint(t *testing.T) {
	for _, tc := range []struct {
		name      string
		free      uint64
		wantTaint bool
	}{
		{"low", 1 << 20, true},
		{"not low", 10 << 30, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setStatfs(t, statfsResult(tc.free, 20<<30))
			s, _ := newGuardServer(t)
			spec := s.nodeSpec()
			taints, _ := spec["taints"].([]map[string]interface{})
			if got := len(taints) == 1 && taints[0]["key"] == "node.kubernetes.io/disk-pressure" && taints[0]["effect"] == "NoSchedule"; got != tc.wantTaint {
				t.Fatalf("taints = %v, want disk-pressure taint: %v", spec["taints"], tc.wantTaint)
			}
		})
	}
}
