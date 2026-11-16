package store_test

import (
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"q8s/internal/store"
)

func makeServiceAccount(ns, name string) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{"app": name}},
	}
}

func TestServiceAccountPersistenceRoundTrip(t *testing.T) {
	file := filepath.Join(settledTempDir(t), "store.json")
	st1, err := store.Load(file)
	if err != nil {
		t.Fatalf("Load (new file): %v", err)
	}
	t.Cleanup(st1.Close)
	created, err := st1.CreateServiceAccount(makeServiceAccount("default", "worker"))
	if err != nil {
		t.Fatalf("CreateServiceAccount: %v", err)
	}

	st2 := reloadUntil(t, file, func(st *store.Store) bool {
		_, err := st.GetServiceAccount("default", "worker")
		return err == nil
	})
	got, err := st2.GetServiceAccount("default", "worker")
	if err != nil {
		t.Fatalf("ServiceAccount not restored: %v", err)
	}
	if got.UID != created.UID || got.Labels["app"] != "worker" {
		t.Fatalf("ServiceAccount not round-tripped: %+v", got)
	}
	if _, err := st2.GetServiceAccount("other", "worker"); err == nil {
		t.Fatal("ServiceAccount is visible in another namespace")
	}
}

func TestDeleteNamespaceWithResourcesRemovesServiceAccounts(t *testing.T) {
	st := store.New()
	if _, err := st.CreateServiceAccount(makeServiceAccount("default", "worker")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateServiceAccount(makeServiceAccount("other", "worker2")); err != nil {
		t.Fatal(err)
	}

	if err := st.DeleteNamespaceWithResources("default"); err != nil {
		t.Fatalf("DeleteNamespaceWithResources: %v", err)
	}

	if _, err := st.GetServiceAccount("default", "worker"); err == nil {
		t.Fatal("default ServiceAccount survived namespace purge")
	}
	if _, err := st.GetServiceAccount("other", "worker2"); err != nil {
		t.Fatalf("other namespace ServiceAccount was purged: %v", err)
	}
}
