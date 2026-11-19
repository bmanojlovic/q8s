package hostcheck

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"q8s/internal/store"
)

func networkStoreData(t *testing.T, namespaces ...string) []byte {
	t.Helper()
	storeFile := filepath.Join(t.TempDir(), "store.json")
	st, err := store.Load(storeFile)
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	for _, name := range namespaces {
		if _, err := st.GetNamespace(name); err == nil {
			continue
		}
		if _, err := st.CreateNamespace(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
			t.Fatalf("CreateNamespace %s: %v", name, err)
		}
	}
	data, err := os.ReadFile(storeFile)
	if err != nil {
		// Load does not write an unchanged store. Create a namespace that no
		// test lists as a network so the read-only snapshot exists.
		if _, createErr := st.CreateNamespace(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "seed"}}); createErr != nil {
			t.Fatalf("CreateNamespace seed: %v", createErr)
		}
		data, err = os.ReadFile(storeFile)
	}
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	return data
}

func networkEnv(t *testing.T, storeData, listOut []byte) Env {
	t.Helper()
	var networks []podmanNetwork
	for _, line := range strings.Split(string(listOut), "\n") {
		if name := strings.TrimSpace(line); name != "" {
			networks = append(networks, podmanNetwork{Name: name})
		}
	}
	jsonOut, err := json.Marshal(networks)
	if err != nil {
		t.Fatalf("marshal networks: %v", err)
	}
	return Env{
		Dirs: Dirs{Data: "/etc/q8s"},
		ReadFile: func(path string) ([]byte, error) {
			if path == "/etc/q8s/store.json" {
				return storeData, nil
			}
			return nil, os.ErrNotExist
		},
		Command: func(name string, args ...string) ([]byte, error) {
			if name != "podman" {
				return nil, os.ErrNotExist
			}
			switch strings.Join(args, " ") {
			case "network ls --filter label=app.kubernetes.io/managed-by=q8s --format json":
				return jsonOut, nil
			case "network ls --filter label=app.kubernetes.io/managed-by=q8s --format {{.Name}}":
				return listOut, nil
			default:
				return nil, os.ErrNotExist
			}
		},
	}
}

func TestNetworksOKForExpectedNamespaceNetworks(t *testing.T) {
	storeData := networkStoreData(t, "default", "demo")
	out := []byte("q8s-default\nq8s-demo\n")
	c := checkNetworks(networkEnv(t, storeData, out))
	if c.Status != StatusOK {
		t.Fatalf("status = %q, want ok (detail %q)", c.Status, c.Detail)
	}
}

func TestNetworksReportsNetworkWithoutStoredNamespace(t *testing.T) {
	storeData := networkStoreData(t, "default", "demo")
	out := []byte("q8s-default\nq8s-demo\nq8s-gone\n")
	c := checkNetworks(networkEnv(t, storeData, out))
	if c.Status != StatusWarn {
		t.Fatalf("status = %q, want warn (detail %q)", c.Status, c.Detail)
	}
	if !strings.Contains(c.Detail, "q8s-gone (no stored namespace uses it)") {
		t.Errorf("detail %q does not report q8s-gone", c.Detail)
	}
	for _, expected := range []string{"q8s-default", "q8s-demo"} {
		if strings.Contains(c.Detail, expected) {
			t.Errorf("detail %q reports expected network %q", c.Detail, expected)
		}
	}
	for _, want := range []string{
		"podman network rm q8s-gone",
		"systemctl --user stop q8s-gone-network.service",
		"systemctl --user reset-failed q8s-gone-network.service",
	} {
		if !strings.Contains(c.Fix, want) {
			t.Errorf("fix %q does not contain %q", c.Fix, want)
		}
	}
}

func TestNetworksFixUsesSystemManagerWhenRootful(t *testing.T) {
	storeData := networkStoreData(t, "default")
	env := networkEnv(t, storeData, []byte("q8s-gone\n"))
	env.Rootful = true
	c := checkNetworks(env)
	if c.Status != StatusWarn {
		t.Fatalf("status = %q, want warn (detail %q)", c.Status, c.Detail)
	}
	if !strings.Contains(c.Fix, "systemctl stop q8s-gone-network.service") {
		t.Errorf("fix %q does not contain the system manager stop", c.Fix)
	}
	if strings.Contains(c.Fix, "systemctl --user") {
		t.Errorf("rootful fix %q uses --user", c.Fix)
	}
}

func TestNetworksUsesTheManagedByLabelFilter(t *testing.T) {
	storeData := networkStoreData(t, "default")
	var got []string
	env := networkEnv(t, storeData, []byte("q8s-default\n"))
	baseCommand := env.Command
	env.Command = func(name string, args ...string) ([]byte, error) {
		got = append([]string{name}, args...)
		return baseCommand(name, args...)
	}
	if c := checkNetworks(env); c.Status != StatusOK {
		t.Fatalf("status = %q, want ok (detail %q)", c.Status, c.Detail)
	}
	want := []string{"podman", "network", "ls", "--filter", "label=app.kubernetes.io/managed-by=q8s", "--format", "json"}
	if !equalNetworkArgs(got, want) {
		t.Errorf("podman call = %v, want %v", got, want)
	}
}

func equalNetworkArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestNetworksFallsBackToNameFormat(t *testing.T) {
	storeData := networkStoreData(t, "default")
	env := networkEnv(t, storeData, []byte("q8s-default\n"))
	env.Command = func(name string, args ...string) ([]byte, error) {
		if strings.HasSuffix(strings.Join(args, " "), "--format {{.Name}}") {
			return []byte("q8s-default\n"), nil
		}
		return nil, os.ErrNotExist
	}
	c := checkNetworks(env)
	if c.Status != StatusOK {
		t.Fatalf("status = %q, want ok (detail %q)", c.Status, c.Detail)
	}
}

func TestNetworksIgnoresJSONNetworkWithoutTheManagedByLabel(t *testing.T) {
	storeData := networkStoreData(t, "default")
	env := networkEnv(t, storeData, nil)
	env.Command = func(name string, args ...string) ([]byte, error) {
		return []byte(`[{"name":"q8s-default"},{"name":"postgres-net","labels":{"owner":"db"}}]`), nil
	}
	c := checkNetworks(env)
	if c.Status != StatusOK {
		t.Fatalf("status = %q, want ok (detail %q)", c.Status, c.Detail)
	}
	if strings.Contains(c.Detail, "postgres-net") {
		t.Errorf("detail %q reports a network without the managed-by label", c.Detail)
	}
}

func TestNetworksSkippedWhenPodmanIsUnavailable(t *testing.T) {
	storeData := networkStoreData(t, "default")
	env := Env{
		Dirs: Dirs{Data: "/etc/q8s"},
		ReadFile: func(path string) ([]byte, error) {
			if path == "/etc/q8s/store.json" {
				return storeData, nil
			}
			return nil, os.ErrNotExist
		},
		Command: func(name string, args ...string) ([]byte, error) {
			return nil, os.ErrNotExist
		},
	}
	c := checkNetworks(env)
	if c.Status != StatusInfo {
		t.Fatalf("status = %q, want info", c.Status)
	}
	if !strings.Contains(c.Detail, "skipped:") {
		t.Errorf("detail %q should start with a skipped reason", c.Detail)
	}
}

func TestNetworksSkippedWithoutStore(t *testing.T) {
	env := Env{
		Dirs:     Dirs{Data: "/etc/q8s"},
		ReadFile: func(string) ([]byte, error) { return nil, os.ErrNotExist },
		Command: func(name string, args ...string) ([]byte, error) {
			t.Fatalf("podman must not run when the store is unreadable: %s %v", name, args)
			return nil, nil
		},
	}
	c := checkNetworks(env)
	if c.Status != StatusInfo {
		t.Fatalf("status = %q, want info", c.Status)
	}
	if !strings.Contains(c.Detail, "skipped:") {
		t.Errorf("detail %q should start with a skipped reason", c.Detail)
	}
}
