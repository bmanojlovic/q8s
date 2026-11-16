package store_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"q8s/internal/store"
)

func TestLoadReadOnlyParsesWithoutWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	st, err := store.Load(path)
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	if _, err := st.CreatePod(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
	}); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	infoBefore, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat store: %v", err)
	}

	read, err := store.LoadReadOnly(before)
	if err != nil {
		t.Fatalf("LoadReadOnly: %v", err)
	}
	if _, err := read.GetPod("default", "app"); err != nil {
		t.Fatalf("GetPod from read-only store: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read store after: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Error("LoadReadOnly rewrote store.json")
	}
	infoAfter, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat store after: %v", err)
	}
	if !infoBefore.ModTime().Equal(infoAfter.ModTime()) {
		t.Error("LoadReadOnly changed store.json's modification time")
	}
}

func TestLoadReadOnlyRejectsUnparseableData(t *testing.T) {
	if _, err := store.LoadReadOnly([]byte("not json")); err == nil {
		t.Fatal("LoadReadOnly accepted invalid JSON")
	}
}
