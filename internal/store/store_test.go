package store_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"q8s/internal/store"
)

// --- helpers ---

func newStore(t *testing.T) *store.Store {
	t.Helper()
	return store.New()
}

func makePod(ns, name, image string) *corev1.Pod {
	return &corev1.Pod{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: name, Image: image}}},
	}
}

func makeJob(ns, name string) *batchv1.Job {
	return &batchv1.Job{
		TypeMeta:   metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "c", Image: "img"}},
				},
			},
		},
	}
}

func makeCronJob(ns, name, schedule string) *batchv1.CronJob {
	return &batchv1.CronJob{
		TypeMeta:   metav1.TypeMeta{APIVersion: "batch/v1", Kind: "CronJob"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: batchv1.CronJobSpec{
			Schedule: schedule,
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "c", Image: "img"}},
						},
					},
				},
			},
		},
	}
}

// --- Namespace ---

func TestNewHasDefaultNamespace(t *testing.T) {
	st := newStore(t)
	ns, err := st.GetNamespace("default")
	if err != nil {
		t.Fatalf("expected default namespace: %v", err)
	}
	if ns.Name != "default" {
		t.Fatalf("expected default, got %s", ns.Name)
	}
}

func TestNamespaceCRUD(t *testing.T) {
	st := newStore(t)

	// Create
	created, err := st.CreateNamespace(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "production"},
	})
	if err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	if created.Name != "production" {
		t.Fatalf("expected production, got %s", created.Name)
	}
	if created.ResourceVersion == "" {
		t.Fatal("ResourceVersion should be set")
	}

	// Get
	got, err := st.GetNamespace("production")
	if err != nil {
		t.Fatalf("GetNamespace: %v", err)
	}
	if got.Name != "production" {
		t.Fatalf("expected production, got %s", got.Name)
	}

	// List
	nss := st.Namespaces()
	if len(nss) < 2 {
		t.Fatalf("expected at least 2 namespaces (default + production), got %d", len(nss))
	}

	// Delete
	if err := st.DeleteNamespace("production"); err != nil {
		t.Fatalf("DeleteNamespace: %v", err)
	}
	if _, err := st.GetNamespace("production"); err == nil {
		t.Fatal("expected error after delete")
	}
}

func TestNamespaceDuplicate(t *testing.T) {
	st := newStore(t)
	st.CreateNamespace(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns1"}})
	_, err := st.CreateNamespace(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns1"}})
	if err == nil {
		t.Fatal("expected error on duplicate namespace")
	}
}

func TestNamespaceDeleteNotFound(t *testing.T) {
	st := newStore(t)
	if err := st.DeleteNamespace("nonexistent"); err == nil {
		t.Fatal("expected error deleting nonexistent namespace")
	}
}

// --- Pod ---

func TestPodCRUD(t *testing.T) {
	st := newStore(t)

	// Create
	pod := makePod("default", "nginx", "nginx:latest")
	created, err := st.CreatePod(pod)
	if err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	if created.Name != "nginx" || created.Namespace != "default" {
		t.Fatalf("unexpected created pod: %+v", created)
	}
	if created.UID == "" {
		t.Fatal("UID should be set")
	}
	if created.ResourceVersion == "" {
		t.Fatal("ResourceVersion should be set")
	}

	// Get
	got, err := st.GetPod("default", "nginx")
	if err != nil {
		t.Fatalf("GetPod: %v", err)
	}
	if got.Name != "nginx" {
		t.Fatalf("expected nginx, got %s", got.Name)
	}

	// Namespaced list
	pods := st.Pods("default")
	if len(pods) != 1 {
		t.Fatalf("expected 1 pod, got %d", len(pods))
	}

	// All pods
	all := st.AllPods()
	if len(all) != 1 {
		t.Fatalf("expected 1 pod in AllPods, got %d", len(all))
	}

	// Pods in another namespace
	other := st.Pods("staging")
	if len(other) != 0 {
		t.Fatalf("expected 0 pods in staging, got %d", len(other))
	}

	// Delete
	if err := st.DeletePod("default", "nginx"); err != nil {
		t.Fatalf("DeletePod: %v", err)
	}
	if _, err := st.GetPod("default", "nginx"); err == nil {
		t.Fatal("expected error after delete")
	}
	if len(st.AllPods()) != 0 {
		t.Fatal("expected empty AllPods after delete")
	}
}

func TestPodDuplicate(t *testing.T) {
	st := newStore(t)
	st.CreatePod(makePod("default", "nginx", "nginx"))
	_, err := st.CreatePod(makePod("default", "nginx", "nginx"))
	if err == nil {
		t.Fatal("expected error on duplicate pod")
	}
}

func TestPodNotFound(t *testing.T) {
	st := newStore(t)
	if _, err := st.GetPod("default", "missing"); err == nil {
		t.Fatal("expected error for missing pod")
	}
	if err := st.DeletePod("default", "missing"); err == nil {
		t.Fatal("expected error deleting missing pod")
	}
}

func TestUpdatePodPhase(t *testing.T) {
	st := newStore(t)
	st.CreatePod(makePod("default", "nginx", "nginx"))

	st.UpdatePodPhase("default", "nginx", corev1.PodRunning)
	got, _ := st.GetPod("default", "nginx")
	if got.Status.Phase != corev1.PodRunning {
		t.Fatalf("expected Running, got %s", got.Status.Phase)
	}

	st.UpdatePodPhase("default", "nginx", corev1.PodFailed)
	got, _ = st.GetPod("default", "nginx")
	if got.Status.Phase != corev1.PodFailed {
		t.Fatalf("expected Failed, got %s", got.Status.Phase)
	}
}

func TestUpdatePodPhaseContainerReady(t *testing.T) {
	st := newStore(t)
	pod := makePod("default", "app", "img")
	st.CreatePod(pod)

	st.UpdatePodPhase("default", "app", corev1.PodRunning)
	got, _ := st.GetPod("default", "app")
	if len(got.Status.ContainerStatuses) == 0 {
		t.Fatal("expected container statuses to be populated")
	}
	if !got.Status.ContainerStatuses[0].Ready {
		t.Fatal("expected container to be ready when Running")
	}

	st.UpdatePodPhase("default", "app", corev1.PodFailed)
	got, _ = st.GetPod("default", "app")
	if got.Status.ContainerStatuses[0].Ready {
		t.Fatal("expected container NOT to be ready when Failed")
	}
}

func TestUpdatePodPhaseMissing(t *testing.T) {
	st := newStore(t)
	// Should not panic on missing pod
	st.UpdatePodPhase("default", "ghost", corev1.PodRunning)
}

// --- Service ---

func TestServiceCRUD(t *testing.T) {
	st := newStore(t)

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "mysvc", Namespace: "default"},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}},
	}

	created, err := st.CreateService(svc)
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	if created.Name != "mysvc" {
		t.Fatalf("unexpected name: %s", created.Name)
	}

	got, err := st.GetService("default", "mysvc")
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	if got.Name != "mysvc" {
		t.Fatalf("expected mysvc, got %s", got.Name)
	}

	svcs := st.Services("default")
	if len(svcs) != 1 {
		t.Fatalf("expected 1 service, got %d", len(svcs))
	}

	if err := st.DeleteService("default", "mysvc"); err != nil {
		t.Fatalf("DeleteService: %v", err)
	}
	if _, err := st.GetService("default", "mysvc"); err == nil {
		t.Fatal("expected error after delete")
	}
}

func TestServiceDuplicate(t *testing.T) {
	st := newStore(t)
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"}}
	st.CreateService(svc)
	_, err := st.CreateService(svc)
	if err == nil {
		t.Fatal("expected duplicate error")
	}
}

// --- PVC ---

func TestPVCCRUD(t *testing.T) {
	st := newStore(t)

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "mydata", Namespace: "default"},
	}

	created, err := st.CreatePVC(pvc)
	if err != nil {
		t.Fatalf("CreatePVC: %v", err)
	}
	if created.Name != "mydata" {
		t.Fatalf("unexpected name: %s", created.Name)
	}

	got, err := st.GetPVC("default", "mydata")
	if err != nil {
		t.Fatalf("GetPVC: %v", err)
	}
	if got.Name != "mydata" {
		t.Fatalf("expected mydata, got %s", got.Name)
	}

	pvcs := st.PVCs("default")
	if len(pvcs) != 1 {
		t.Fatalf("expected 1 PVC, got %d", len(pvcs))
	}
	if len(st.AllPVCs()) != 1 {
		t.Fatalf("expected 1 in AllPVCs, got %d", len(st.AllPVCs()))
	}

	if err := st.DeletePVC("default", "mydata"); err != nil {
		t.Fatalf("DeletePVC: %v", err)
	}
	if _, err := st.GetPVC("default", "mydata"); err == nil {
		t.Fatal("expected error after delete")
	}
}

// --- ConfigMap ---

func TestConfigMapCRUD(t *testing.T) {
	st := newStore(t)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "myconfig", Namespace: "default"},
		Data:       map[string]string{"key1": "value1"},
	}

	created, err := st.CreateConfigMap(cm)
	if err != nil {
		t.Fatalf("CreateConfigMap: %v", err)
	}
	if created.Data["key1"] != "value1" {
		t.Fatalf("expected key1=value1, got %s", created.Data["key1"])
	}

	got, err := st.GetConfigMap("default", "myconfig")
	if err != nil {
		t.Fatalf("GetConfigMap: %v", err)
	}
	if got.Data["key1"] != "value1" {
		t.Fatalf("expected key1=value1, got %s", got.Data["key1"])
	}

	// Update
	updated, err := st.UpdateConfigMap(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "myconfig", Namespace: "default"},
		Data:       map[string]string{"key1": "newvalue"},
	})
	if err != nil {
		t.Fatalf("UpdateConfigMap: %v", err)
	}
	if updated.Data["key1"] != "newvalue" {
		t.Fatalf("expected newvalue, got %s", updated.Data["key1"])
	}

	// UID preserved on update
	if updated.UID != created.UID {
		t.Fatalf("UID should be preserved on update")
	}

	cms := st.ConfigMaps("default")
	if len(cms) != 1 {
		t.Fatalf("expected 1 configmap, got %d", len(cms))
	}

	if err := st.DeleteConfigMap("default", "myconfig"); err != nil {
		t.Fatalf("DeleteConfigMap: %v", err)
	}
	if _, err := st.GetConfigMap("default", "myconfig"); err == nil {
		t.Fatal("expected error after delete")
	}
}

func TestConfigMapUpdateNotFound(t *testing.T) {
	st := newStore(t)
	_, err := st.UpdateConfigMap(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "missing", Namespace: "default"},
	})
	if err == nil {
		t.Fatal("expected error updating nonexistent configmap")
	}
}

func TestConfigMapFiles(t *testing.T) {
	st := newStore(t)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "cfg", Namespace: "default"},
		Data:       map[string]string{"a": "1"},
	}
	st.CreateConfigMap(cm)

	files := st.ConfigMapFiles()
	if len(files) != 1 {
		t.Fatalf("expected 1 configmap file entry, got %d", len(files))
	}
	if files[0].Name != "cfg" {
		t.Fatalf("expected cfg, got %s", files[0].Name)
	}
}

// --- Secret ---

func TestSecretCRUD(t *testing.T) {
	st := newStore(t)

	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "mysecret", Namespace: "default"},
		Data:       map[string][]byte{"password": []byte("s3cr3t")},
	}

	created, err := st.CreateSecret(sec)
	if err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}

	got, err := st.GetSecret("default", "mysecret")
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	if string(got.Data["password"]) != "s3cr3t" {
		t.Fatalf("unexpected secret data")
	}

	secs := st.Secrets("default")
	if len(secs) != 1 {
		t.Fatalf("expected 1 secret, got %d", len(secs))
	}
	if len(st.AllSecrets()) != 1 {
		t.Fatalf("expected 1 in AllSecrets, got %d", len(st.AllSecrets()))
	}

	_ = created
	if err := st.DeleteSecret("default", "mysecret"); err != nil {
		t.Fatalf("DeleteSecret: %v", err)
	}
}

// --- Job ---

func TestJobCRUD(t *testing.T) {
	st := newStore(t)

	job := makeJob("default", "myjob")
	created, err := st.CreateJob(job)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if created.Name != "myjob" {
		t.Fatalf("unexpected name: %s", created.Name)
	}

	got, err := st.GetJob("default", "myjob")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Name != "myjob" {
		t.Fatalf("expected myjob, got %s", got.Name)
	}

	jobs := st.Jobs("default")
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	if len(st.AllJobs()) != 1 {
		t.Fatalf("expected 1 in AllJobs, got %d", len(st.AllJobs()))
	}

	if err := st.DeleteJob("default", "myjob"); err != nil {
		t.Fatalf("DeleteJob: %v", err)
	}
	if _, err := st.GetJob("default", "myjob"); err == nil {
		t.Fatal("expected error after delete")
	}
}

func TestUpdateJobStatus(t *testing.T) {
	st := newStore(t)
	st.CreateJob(makeJob("default", "j"))

	st.UpdateJobStatus("default", "j", 1, 0, 0)
	got, _ := st.GetJob("default", "j")
	if got.Status.Active != 1 {
		t.Fatalf("expected Active=1, got %d", got.Status.Active)
	}

	st.UpdateJobStatus("default", "j", 0, 1, 0)
	got, _ = st.GetJob("default", "j")
	if got.Status.Succeeded != 1 {
		t.Fatalf("expected Succeeded=1, got %d", got.Status.Succeeded)
	}

	st.UpdateJobStatus("default", "j", 0, 0, 1)
	got, _ = st.GetJob("default", "j")
	if got.Status.Failed != 1 {
		t.Fatalf("expected Failed=1, got %d", got.Status.Failed)
	}
}

func TestUpdateJobStatusMissing(t *testing.T) {
	st := newStore(t)
	// Should not panic on missing job
	st.UpdateJobStatus("default", "ghost", 1, 0, 0)
}

// --- CronJob ---

func TestCronJobCRUD(t *testing.T) {
	st := newStore(t)

	cj := makeCronJob("default", "mycron", "0 * * * *")
	created, err := st.CreateCronJob(cj)
	if err != nil {
		t.Fatalf("CreateCronJob: %v", err)
	}
	if created.Name != "mycron" {
		t.Fatalf("unexpected name: %s", created.Name)
	}

	got, err := st.GetCronJob("default", "mycron")
	if err != nil {
		t.Fatalf("GetCronJob: %v", err)
	}
	if got.Spec.Schedule != "0 * * * *" {
		t.Fatalf("unexpected schedule: %s", got.Spec.Schedule)
	}

	cjs := st.CronJobs("default")
	if len(cjs) != 1 {
		t.Fatalf("expected 1 cronjob, got %d", len(cjs))
	}
	if len(st.AllCronJobs()) != 1 {
		t.Fatalf("expected 1 in AllCronJobs, got %d", len(st.AllCronJobs()))
	}

	if err := st.DeleteCronJob("default", "mycron"); err != nil {
		t.Fatalf("DeleteCronJob: %v", err)
	}
	if _, err := st.GetCronJob("default", "mycron"); err == nil {
		t.Fatal("expected error after delete")
	}
}

// --- ResourceVersion ---

func TestResourceVersionIncrements(t *testing.T) {
	st := newStore(t)
	rv0 := st.ResourceVersion()

	st.CreatePod(makePod("default", "p1", "img"))
	rv1 := st.ResourceVersion()
	if rv1 <= rv0 {
		t.Fatalf("ResourceVersion should increment after create: %d -> %d", rv0, rv1)
	}

	st.CreatePod(makePod("default", "p2", "img"))
	rv2 := st.ResourceVersion()
	if rv2 <= rv1 {
		t.Fatalf("ResourceVersion should increment after second create: %d -> %d", rv1, rv2)
	}
}

// --- Persistence ---

func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "store.json")

	// Write
	st1, err := store.Load(file)
	if err != nil {
		t.Fatalf("Load (new file): %v", err)
	}
	if _, err := st1.CreatePod(makePod("default", "nginx", "nginx:latest")); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	if _, err := st1.CreateNamespace(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "staging"}}); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}

	// Give async save goroutine time to flush
	time.Sleep(100 * time.Millisecond)

	// Reload
	st2, err := store.Load(file)
	if err != nil {
		t.Fatalf("Load (existing file): %v", err)
	}

	pod, err := st2.GetPod("default", "nginx")
	if err != nil {
		t.Fatalf("pod not found after reload: %v", err)
	}
	if pod.Spec.Containers[0].Image != "nginx:latest" {
		t.Fatalf("unexpected image: %s", pod.Spec.Containers[0].Image)
	}

	if _, err := st2.GetNamespace("staging"); err != nil {
		t.Fatalf("namespace not found after reload: %v", err)
	}

	// Default namespace always present
	if _, err := st2.GetNamespace("default"); err != nil {
		t.Fatalf("default namespace missing after reload: %v", err)
	}
}

func TestLoadNonExistentFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "nosuchfile.json")
	st, err := store.Load(file)
	if err != nil {
		t.Fatalf("Load of missing file should return empty store: %v", err)
	}
	// Should have default namespace
	if _, err := st.GetNamespace("default"); err != nil {
		t.Fatalf("default namespace missing in new store: %v", err)
	}
}

func TestLoadCorruptedFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "store.json")
	if err := os.WriteFile(file, []byte("not valid json"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := store.Load(file)
	if err == nil {
		t.Fatal("expected error loading corrupted store file")
	}
}
