package server_test

import (
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"q8s/internal/server"
	"q8s/internal/store"
)

// TestCreatePersistFailureAnswersInternalError is the HTTP half of the
// synchronous-save change: a store write that cannot reach disk must answer
// 500 InternalError instead of a create-specific 409, and the handler must
// not generate a quadlet for an object that was rolled back.
func TestCreatePersistFailureAnswersInternalError(t *testing.T) {
	dataDir := t.TempDir()
	file := filepath.Join(dataDir, "store.json")
	st, err := store.Load(file)
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	t.Cleanup(st.Close)

	quadletDir := t.TempDir()
	certPEM, keyPEM := genTestCert(t)
	srv, err := server.New(server.Config{
		Store:      st,
		CertPEM:    certPEM,
		KeyPEM:     keyPEM,
		QuadletDir: quadletDir,
		ConfigDir:  filepath.Join(t.TempDir(), "configmaps"),
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	if os.Geteuid() == 0 {
		// Root ignores directory permissions, so replace the data directory
		// with a regular file instead: every write below then fails with
		// ENOTDIR just like an unwritable directory would for a normal user.
		if err := os.RemoveAll(dataDir); err != nil {
			t.Fatalf("remove data dir: %v", err)
		}
		if err := os.WriteFile(dataDir, []byte("not a directory"), 0600); err != nil {
			t.Fatalf("replace data dir with file: %v", err)
		}
	} else {
		if err := os.Chmod(dataDir, 0500); err != nil {
			t.Fatalf("make data dir read-only: %v", err)
		}
		t.Cleanup(func() { os.Chmod(dataDir, 0700) })
	}

	resp := post(t, ts.URL+"/api/v1/namespaces/default/pods", podBody("default", "blocked", "nginx:latest"))
	if resp.StatusCode != 500 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("status = %d, want 500: %s", resp.StatusCode, body)
	}
	body := decodeBody(t, resp)
	if body["reason"] != "InternalError" {
		t.Fatalf("reason = %v, want InternalError", body["reason"])
	}
	if code, ok := body["code"].(float64); !ok || int(code) != 500 {
		t.Fatalf("code = %v, want 500", body["code"])
	}

	entries, err := os.ReadDir(quadletDir)
	if err != nil {
		t.Fatalf("read quadlet dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("quadlet dir is not empty after failed persist: %v", entries)
	}
	if _, err := st.GetPod("default", "blocked"); err == nil {
		t.Fatal("pod stayed in the store after the failed persist")
	}
}

// TestDeletePersistFailureAnswersInternalError: a DELETE whose final store
// write fails must answer 500 InternalError with a retry instruction. Nothing
// is restored: the pod stays in the store, but its units stay stopped and its
// quadlet file stays removed.
func TestDeletePersistFailureAnswersInternalError(t *testing.T) {
	dataDir := t.TempDir()
	file := filepath.Join(dataDir, "store.json")
	st, err := store.Load(file)
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	t.Cleanup(st.Close)

	quadletDir := t.TempDir()
	certPEM, keyPEM := genTestCert(t)
	srv, err := server.New(server.Config{
		Store:      st,
		CertPEM:    certPEM,
		KeyPEM:     keyPEM,
		QuadletDir: quadletDir,
		ConfigDir:  filepath.Join(t.TempDir(), "configmaps"),
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	if _, err := st.CreatePod(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "blocked", Namespace: "default"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
	}); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	quadlet := filepath.Join(quadletDir, "default-blocked.container")
	if err := os.WriteFile(quadlet, []byte("[Container]\nImage=nginx\n"), 0644); err != nil {
		t.Fatalf("write quadlet: %v", err)
	}

	if os.Geteuid() == 0 {
		if err := os.RemoveAll(dataDir); err != nil {
			t.Fatalf("remove data dir: %v", err)
		}
		if err := os.WriteFile(dataDir, []byte("not a directory"), 0600); err != nil {
			t.Fatalf("replace data dir with file: %v", err)
		}
	} else {
		if err := os.Chmod(dataDir, 0500); err != nil {
			t.Fatalf("make data dir read-only: %v", err)
		}
		t.Cleanup(func() { os.Chmod(dataDir, 0700) })
	}

	resp := do(t, "DELETE", ts.URL+"/api/v1/namespaces/default/pods/blocked", nil)
	body := decodeBody(t, resp)
	if resp.StatusCode != 500 {
		t.Fatalf("status = %d, want 500: %v", resp.StatusCode, body)
	}
	if body["reason"] != "InternalError" {
		t.Fatalf("reason = %v, want InternalError", body["reason"])
	}
	if code, ok := body["code"].(float64); !ok || int(code) != 500 {
		t.Fatalf("code = %v, want 500", body["code"])
	}
	message, _ := body["message"].(string)
	if !strings.HasPrefix(message, `pod "blocked" was stopped but could not be removed from the store (`) ||
		!strings.HasSuffix(message, "); retry the delete") {
		t.Fatalf("message = %q, want the retry-the-delete message", message)
	}
	if _, err := os.Stat(quadlet); !os.IsNotExist(err) {
		t.Fatalf("quadlet file was restored by a failed delete, stat err=%v", err)
	}
	if _, err := st.GetPod("default", "blocked"); err != nil {
		t.Fatalf("pod was not rolled back after failed delete: %v", err)
	}
}
