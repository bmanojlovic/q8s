package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"q8s/internal/store"
)

// namespaceNetworkServer returns a server with one stored namespace and its
// rendered .network file, the state namespace delete starts from.
func namespaceNetworkServer(t *testing.T, ns string) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	st := store.New()
	if _, err := st.CreateNamespace(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	path := filepath.Join(dir, "q8s-"+ns+".network")
	if err := os.WriteFile(path, []byte("[Network]\nNetworkName=q8s-"+ns+"\n"), 0644); err != nil {
		t.Fatalf("write network quadlet: %v", err)
	}
	return &Server{config: Config{Store: st, QuadletDir: dir}}, path
}

func TestNamespaceDeleteStopsUnitBeforeFileRemovalAndRemovesNetwork(t *testing.T) {
	srv, networkFile := namespaceNetworkServer(t, "demo")

	var events []string
	filePresentAtStop := false
	srv.stopUnitHook = func(name string) {
		events = append(events, "stop "+name)
		if name == "q8s-demo-network.service" {
			if _, err := os.Stat(networkFile); err == nil {
				filePresentAtStop = true
			}
		}
	}

	old := podmanCommand
	podmanCommand = func(ctx context.Context, args ...string) ([]byte, error) {
		events = append(events, "podman "+strings.Join(args, " "))
		return nil, nil
	}
	t.Cleanup(func() { podmanCommand = old })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/namespaces/demo", nil)
	srv.handleNamespaceDelete(rec, req, "demo")
	if rec.Code != http.StatusOK {
		t.Fatalf("namespace delete status = %d, body %s", rec.Code, rec.Body.String())
	}

	if !filePresentAtStop {
		t.Error("network quadlet was already gone when the network unit was stopped")
	}
	if _, err := os.Stat(networkFile); !os.IsNotExist(err) {
		t.Errorf("network quadlet survived delete, stat err=%v", err)
	}
	idxStop := callIndex(events, "stop q8s-demo-network.service")
	idxRm := callIndex(events, "podman network rm q8s-demo")
	if idxStop < 0 || idxRm < 0 || idxStop > idxRm {
		t.Errorf("network unit must stop before podman network rm, events: %v", events)
	}
	for _, call := range events {
		if strings.HasPrefix(call, "podman network") && call != "podman network rm q8s-demo" {
			t.Errorf("unexpected network call: %s", call)
		}
	}
}

func TestNamespaceDeleteLogsFailedNetworkRemovalAndStillSucceeds(t *testing.T) {
	srv, _ := namespaceNetworkServer(t, "demo")

	old := podmanCommand
	podmanCommand = func(ctx context.Context, args ...string) ([]byte, error) {
		if len(args) == 3 && args[0] == "network" && args[1] == "rm" {
			return nil, errors.New("Error: network q8s-demo is in use by container abc")
		}
		return nil, nil
	}
	t.Cleanup(func() { podmanCommand = old })

	oldStdout := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = writer
	defer func() {
		os.Stdout = oldStdout
		writer.Close()
		reader.Close()
	}()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/namespaces/demo", nil)
	srv.handleNamespaceDelete(rec, req, "demo")
	if rec.Code != http.StatusOK {
		t.Fatalf("namespace delete status = %d, body %s", rec.Code, rec.Body.String())
	}

	writer.Close()
	os.Stdout = oldStdout
	out, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read captured log: %v", err)
	}
	logged := string(out)
	if !strings.Contains(logged, "remove network q8s-demo") || !strings.Contains(logged, "in use by container abc") {
		t.Errorf("failed network rm was not logged, output: %q", logged)
	}
}

func TestNamespaceDeleteDoesNotLogAlreadyMissingNetwork(t *testing.T) {
	srv, _ := namespaceNetworkServer(t, "demo")

	old := podmanCommand
	podmanCommand = func(ctx context.Context, args ...string) ([]byte, error) {
		if len(args) == 3 && args[0] == "network" && args[1] == "rm" {
			return nil, errors.New("Error: network q8s-demo: network not found")
		}
		return nil, nil
	}
	t.Cleanup(func() { podmanCommand = old })

	oldStdout := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = writer
	defer func() {
		os.Stdout = oldStdout
		writer.Close()
		reader.Close()
	}()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/namespaces/demo", nil)
	srv.handleNamespaceDelete(rec, req, "demo")
	if rec.Code != http.StatusOK {
		t.Fatalf("namespace delete status = %d, body %s", rec.Code, rec.Body.String())
	}

	writer.Close()
	os.Stdout = oldStdout
	out, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read captured log: %v", err)
	}
	if strings.Contains(string(out), "remove network q8s-demo") {
		t.Errorf("missing network was logged as a failure: %q", string(out))
	}
}

func TestDryRunNamespaceDeleteLeavesNetworkAlone(t *testing.T) {
	srv, networkFile := namespaceNetworkServer(t, "demo")

	var events []string
	srv.stopUnitHook = func(name string) { events = append(events, "stop "+name) }
	old := podmanCommand
	podmanCommand = func(ctx context.Context, args ...string) ([]byte, error) {
		events = append(events, "podman "+strings.Join(args, " "))
		return nil, nil
	}
	t.Cleanup(func() { podmanCommand = old })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/namespaces/demo?dryRun=All", nil)
	srv.handleNamespaceDelete(rec, req, "demo")
	if rec.Code != http.StatusOK {
		t.Fatalf("dry-run namespace delete status = %d, body %s", rec.Code, rec.Body.String())
	}

	if _, err := os.Stat(networkFile); err != nil {
		t.Errorf("dry-run delete removed the network quadlet: %v", err)
	}
	for _, event := range events {
		if strings.Contains(event, "network") {
			t.Errorf("dry-run delete touched the network: %s", event)
		}
	}
	if _, err := srv.config.Store.GetNamespace("demo"); err != nil {
		t.Fatalf("dry-run delete removed the namespace: %v", err)
	}
}
