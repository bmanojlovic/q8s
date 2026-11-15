package store

import (
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// tic-89a6: ConfigMaps, Secrets and PVCs had no creationTimestamp (AGE
// <unknown>). Create sets it, update keeps it, and Load backfills objects an
// older q8s stored without one.
func TestCreationTimestampSetKeptAndBackfilled(t *testing.T) {
	s := New()
	meta := func(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name, Namespace: "default"} }
	cm, err := s.CreateConfigMap(&corev1.ConfigMap{ObjectMeta: meta("c")})
	if err != nil || cm.CreationTimestamp.IsZero() {
		t.Fatalf("configmap: err=%v ts=%v", err, cm.CreationTimestamp)
	}
	sec, err := s.CreateSecret(&corev1.Secret{ObjectMeta: meta("s")})
	if err != nil || sec.CreationTimestamp.IsZero() {
		t.Fatalf("secret: err=%v ts=%v", err, sec.CreationTimestamp)
	}
	pvc, err := s.CreatePVC(&corev1.PersistentVolumeClaim{ObjectMeta: meta("p")})
	if err != nil || pvc.CreationTimestamp.IsZero() {
		t.Fatalf("pvc: err=%v ts=%v", err, pvc.CreationTimestamp)
	}
	svc, err := s.CreateService(&corev1.Service{ObjectMeta: meta("v")})
	if err != nil || svc.CreationTimestamp.IsZero() {
		t.Fatalf("service: err=%v ts=%v", err, svc.CreationTimestamp)
	}
	upd, err := s.UpdateConfigMap(&corev1.ConfigMap{ObjectMeta: meta("c"), Data: map[string]string{"k": "v"}})
	if err != nil || !upd.CreationTimestamp.Equal(&cm.CreationTimestamp) {
		t.Fatalf("update changed creationTimestamp: err=%v got %v want %v", err, upd.CreationTimestamp, cm.CreationTimestamp)
	}

	dir := t.TempDir()
	file := filepath.Join(dir, "store.json")
	legacy := `{"resourceVersion":3,"configMaps":[{"metadata":{"name":"old","namespace":"default"}}],"secrets":[{"metadata":{"name":"old","namespace":"default"}}],"pvcs":[{"metadata":{"name":"old","namespace":"default"}}]}`
	if err := os.WriteFile(file, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(file)
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := loaded.GetConfigMap("default", "old"); c == nil || c.CreationTimestamp.IsZero() {
		t.Errorf("legacy configmap not backfilled: %+v", c)
	}
	if c, _ := loaded.GetSecret("default", "old"); c == nil || c.CreationTimestamp.IsZero() {
		t.Errorf("legacy secret not backfilled: %+v", c)
	}
	if c, _ := loaded.GetPVC("default", "old"); c == nil || c.CreationTimestamp.IsZero() {
		t.Errorf("legacy pvc not backfilled: %+v", c)
	}
}
