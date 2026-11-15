package server_test

import (
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

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
