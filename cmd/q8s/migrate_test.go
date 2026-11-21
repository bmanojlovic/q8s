package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"q8s/internal/server"
	"q8s/internal/store"
)

func newMigrateNamesTestServer(t *testing.T) (*server.Server, dirs, *store.Store) {
	t.Helper()
	dataDir := t.TempDir()
	d := dirs{
		dataDir:    dataDir,
		quadletDir: t.TempDir(),
		systemdDir: t.TempDir(),
		configDir:  t.TempDir(),
	}
	d.secretDir = filepath.Join(filepath.Dir(d.configDir), "secrets")
	st := store.New()
	srv := server.NewNamesMigrationServer(server.Config{
		Store:      st,
		QuadletDir: d.quadletDir,
		SystemdDir: d.systemdDir,
		ConfigDir:  d.configDir,
		TraefikDir: filepath.Join(dataDir, "traefik"),
		DataDir:    dataDir,
	})
	return srv, d, st
}

func writeMigrateMarker(t *testing.T, dataDir string) string {
	t.Helper()
	path := filepath.Join(dataDir, "names-v2")
	if err := os.WriteFile(path, []byte("names-v2\n"), 0644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	return path
}

func inactiveServe() (bool, string, error) { return false, "", nil }

func TestMigrateNamesToLegacyRefusesWithNewFormatPVC(t *testing.T) {
	srv, d, st := newMigrateNamesTestServer(t)
	if _, err := st.CreatePVC(&corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "default"},
	}); err != nil {
		t.Fatalf("CreatePVC: %v", err)
	}
	marker := writeMigrateMarker(t, d.dataDir)

	var out bytes.Buffer
	err := runMigrateNames(migrateNamesOptions{toLegacy: true}, srv, inactiveServe, &out)
	if err == nil {
		t.Fatal("to-legacy accepted a new-format PVC")
	}
	if !strings.Contains(out.String(), "default/data") {
		t.Errorf("output does not list the new-format PVC: %s", out.String())
	}
	if !strings.Contains(out.String(), "new-format volume") {
		t.Errorf("output does not explain the refusal: %s", out.String())
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Errorf("refusal removed the marker: %v", statErr)
	}
}

func TestMigrateNamesToLegacyRemovesMarkerAndPrintsNextStep(t *testing.T) {
	srv, d, _ := newMigrateNamesTestServer(t)
	marker := writeMigrateMarker(t, d.dataDir)

	var out bytes.Buffer
	if err := runMigrateNames(migrateNamesOptions{toLegacy: true}, srv, inactiveServe, &out); err != nil {
		t.Fatalf("runMigrateNames: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("marker survived rollback, stat err=%v", err)
	}
	for _, want := range []string{"previous q8s binary", "q8s start"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output does not contain %q: %s", want, out.String())
		}
	}
}

func TestMigrateNamesToLegacyDryRunKeepsMarker(t *testing.T) {
	srv, d, _ := newMigrateNamesTestServer(t)
	marker := writeMigrateMarker(t, d.dataDir)

	var out bytes.Buffer
	if err := runMigrateNames(migrateNamesOptions{toLegacy: true, dryRun: true}, srv, inactiveServe, &out); err != nil {
		t.Fatalf("runMigrateNames: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("dry run removed the marker: %v", err)
	}
	if !strings.Contains(out.String(), "Nothing was changed") {
		t.Errorf("dry run output does not say nothing changed: %s", out.String())
	}
}

func TestMigrateNamesRefusesWhileServeIsActive(t *testing.T) {
	srv, d, _ := newMigrateNamesTestServer(t)
	marker := writeMigrateMarker(t, d.dataDir)
	active := func() (bool, string, error) { return true, "q8s-api.service", nil }

	var out bytes.Buffer
	err := runMigrateNames(migrateNamesOptions{toLegacy: true}, srv, active, &out)
	if err == nil || !strings.Contains(err.Error(), "q8s-api.service") {
		t.Fatalf("active serve was not refused: %v", err)
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatalf("refusal removed the marker: %v", statErr)
	}
}

func TestMigrateNamesDryRunChangesNothing(t *testing.T) {
	srv, d, _ := newMigrateNamesTestServer(t)
	var out bytes.Buffer
	if err := runMigrateNames(migrateNamesOptions{dryRun: true}, srv, inactiveServe, &out); err != nil {
		t.Fatalf("runMigrateNames: %v", err)
	}
	if !strings.Contains(out.String(), "Nothing was changed") {
		t.Errorf("dry run output does not say nothing changed: %s", out.String())
	}
	if _, err := os.Stat(filepath.Join(d.dataDir, "names-v2")); !os.IsNotExist(err) {
		t.Errorf("dry run wrote the marker")
	}
}
