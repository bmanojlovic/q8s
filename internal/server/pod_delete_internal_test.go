package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"q8s/internal/quadlet"
	"q8s/internal/store"
	"q8s/internal/traefik"
)

func recordPodmanCalls(t *testing.T) *[]string {
	t.Helper()
	calls := &[]string{}
	old := podmanCommand
	podmanCommand = func(ctx context.Context, args ...string) ([]byte, error) {
		*calls = append(*calls, strings.Join(args, " "))
		return nil, nil
	}
	t.Cleanup(func() { podmanCommand = old })
	return calls
}

func hasCall(calls []string, want string) bool {
	for _, call := range calls {
		if call == want {
			return true
		}
	}
	return false
}

func callIndex(calls []string, want string) int {
	for i, call := range calls {
		if call == want {
			return i
		}
	}
	return -1
}

func writeOwnedUnit(t *testing.T, dir, name, ns, podName string) string {
	t.Helper()
	content := "[Container]\n" + quadlet.ManagedByLabel + "\n" +
		"Label=io.kubernetes.pod.namespace=" + ns + "\n" +
		"Label=io.kubernetes.pod.name=" + podName + "\n"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func multiContainerSpec() corev1.PodSpec {
	return corev1.PodSpec{Containers: []corev1.Container{
		{Name: "web", Image: "nginx"},
		{Name: "sidecar", Image: "busybox"},
	}}
}

func TestDeleteMultiContainerPodStopsPodBeforeQuadletRemoval(t *testing.T) {
	dir := t.TempDir()
	st := store.New()
	srv := &Server{config: Config{Store: st, QuadletDir: dir}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "duo", Namespace: "default"},
		Spec:       multiContainerSpec(),
	}
	if _, err := st.CreatePod(pod); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	podPath := writeOwnedUnit(t, dir, "default_duo.pod", "default", "duo")

	var events []string
	filePresentAtStop := false
	srv.stopUnitHook = func(name string) {
		events = append(events, "stop "+name)
		if name == "default_duo-pod.service" {
			if _, err := os.Stat(podPath); err == nil {
				filePresentAtStop = true
			}
		}
	}
	old := podmanCommand
	podmanCommand = func(ctx context.Context, args ...string) ([]byte, error) {
		events = append(events, "podman "+strings.Join(args, " "))
		if len(args) > 1 && args[0] == "pod" && args[1] == "rm" {
			if _, err := os.Stat(podPath); err != nil {
				filePresentAtStop = false
			}
		}
		return nil, nil
	}
	t.Cleanup(func() { podmanCommand = old })

	if err := srv.deletePodByName("default", "duo"); err != nil {
		t.Fatalf("deletePodByName: %v", err)
	}

	for _, want := range []string{
		"stop default_duo_web.service",
		"stop default_duo_sidecar.service",
		"stop default_duo-pod.service",
		"podman pod rm -f --ignore default_duo",
	} {
		if !hasCall(events, want) {
			t.Errorf("missing %q in events %v", want, events)
		}
	}
	if idxStop, idxRm := callIndex(events, "stop default_duo-pod.service"), callIndex(events, "podman pod rm -f --ignore default_duo"); idxStop < 0 || idxRm < 0 || idxStop > idxRm {
		t.Errorf("pod service must stop before podman pod rm, events: %v", events)
	}
	if !filePresentAtStop {
		t.Error("pod quadlet file was already gone when the pod was stopped or removed")
	}
	if _, err := os.Stat(podPath); !os.IsNotExist(err) {
		t.Errorf("pod quadlet file survived delete, stat err=%v", err)
	}
}

func TestDeleteMultiContainerDeploymentRemovesBothPods(t *testing.T) {
	dir := t.TempDir()
	st := store.New()
	srv := &Server{config: Config{Store: st, QuadletDir: dir}}
	replicas := int32(2)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "duo", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{Spec: multiContainerSpec()},
		},
	}
	if _, err := st.CreateDeployment(dep); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	writeOwnedUnit(t, dir, "default_duo-0.pod", "default", "duo-0")
	writeOwnedUnit(t, dir, "default_duo-1.pod", "default", "duo-1")
	calls := recordPodmanCalls(t)

	if err := srv.deleteDeploymentByName("default", "duo"); err != nil {
		t.Fatalf("deleteDeploymentByName: %v", err)
	}

	for _, want := range []string{
		"pod rm -f --ignore default_duo-0",
		"pod rm -f --ignore default_duo-1",
	} {
		if !hasCall(*calls, want) {
			t.Errorf("missing %q in podman calls %v", want, *calls)
		}
	}
	if _, err := st.GetDeployment("default", "duo"); err == nil {
		t.Fatal("deleted deployment is still in the store")
	}
	for _, name := range []string{"duo-0", "duo-1"} {
		if _, err := st.GetPod("default", name); err == nil {
			t.Errorf("deleted deployment instance %q is still in the store", name)
		}
		if _, err := os.Stat(filepath.Join(dir, "default_"+name+".pod")); !os.IsNotExist(err) {
			t.Errorf("deleted instance %q quadlet survived, stat err=%v", name, err)
		}
	}
}

func TestScaleDownMultiContainerDeploymentRemovesOnlyDroppedInstance(t *testing.T) {
	dir := t.TempDir()
	st := store.New()
	srv := &Server{config: Config{Store: st, QuadletDir: dir}}
	replicas := int32(2)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "duo", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{Spec: multiContainerSpec()},
		},
	}
	if _, err := st.CreateDeployment(dep); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	writeOwnedUnit(t, dir, "default_duo-0.pod", "default", "duo-0")
	dropped := writeOwnedUnit(t, dir, "default_duo-1.pod", "default", "duo-1")
	calls := recordPodmanCalls(t)

	srv.scaleDeployment(dep, 2, 1)

	if !hasCall(*calls, "pod rm -f --ignore default_duo-1") {
		t.Errorf("missing pod rm for dropped instance in %v", *calls)
	}
	if hasCall(*calls, "pod rm -f --ignore default_duo-0") {
		t.Errorf("instance -0 must survive scale-down, calls %v", *calls)
	}
	if _, err := os.Stat(dropped); !os.IsNotExist(err) {
		t.Errorf("dropped instance quadlet survived, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "default_duo-0.pod")); err != nil {
		t.Errorf("instance -0 quadlet was removed: %v", err)
	}
}

func TestDeleteSingleContainerPodMakesNoPodmanPodCall(t *testing.T) {
	dir := t.TempDir()
	st := store.New()
	srv := &Server{config: Config{Store: st, QuadletDir: dir}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "solo", Namespace: "default"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "app", Image: "nginx"},
		}},
	}
	if _, err := st.CreatePod(pod); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	writeOwnedUnit(t, dir, "default_solo.container", "default", "solo")
	calls := recordPodmanCalls(t)

	if err := srv.deletePodByName("default", "solo"); err != nil {
		t.Fatalf("deletePodByName: %v", err)
	}

	if !hasCall(*calls, "rm -f default_solo") {
		t.Errorf("missing container removal in %v", *calls)
	}
	for _, call := range *calls {
		if strings.HasPrefix(call, "pod ") {
			t.Errorf("single-container delete must not touch podman pods, calls %v", *calls)
			break
		}
	}
	if _, err := st.GetPod("default", "solo"); err == nil {
		t.Fatal("deleted pod is still in the store")
	}
	if _, err := os.Stat(filepath.Join(dir, "default_solo.container")); !os.IsNotExist(err) {
		t.Fatalf("deleted pod quadlet survived, stat err=%v", err)
	}
}

func TestStalePodUnitRemovesPodmanPod(t *testing.T) {
	dir := t.TempDir()
	srv := &Server{config: Config{QuadletDir: dir}}
	stale := writeOwnedUnit(t, dir, "default_duo.pod", "default", "duo")
	keep := writeOwnedUnit(t, dir, "default_duo.container", "default", "duo")
	calls := recordPodmanCalls(t)

	removed := srv.removeStaleOwnedUnits(dir, "default", "duo", map[string]bool{"default_duo.container": true})
	if !removed {
		t.Fatal("expected stale .pod to be removed")
	}
	if !hasCall(*calls, "pod rm -f --ignore default_duo") {
		t.Errorf("missing pod rm for stale .pod in %v", *calls)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale .pod survived, stat err=%v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("kept container unit was removed: %v", err)
	}
}

func newDeleteTestServer(t *testing.T) (*store.Store, *Server, string) {
	t.Helper()
	dataDir := t.TempDir()
	st, err := store.Load(filepath.Join(dataDir, "store.json"))
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	t.Cleanup(st.Close)
	return st, &Server{config: Config{Store: st, QuadletDir: t.TempDir()}}, dataDir
}

func breakStoreDir(t *testing.T, dataDir string) {
	t.Helper()
	if err := os.RemoveAll(dataDir); err != nil {
		t.Fatalf("remove data dir: %v", err)
	}
	if err := os.WriteFile(dataDir, []byte("not a directory"), 0600); err != nil {
		t.Fatalf("replace data dir with file: %v", err)
	}
}

func repairStoreDir(t *testing.T, dataDir string) {
	t.Helper()
	if err := os.Remove(dataDir); err != nil {
		t.Fatalf("remove data dir placeholder: %v", err)
	}
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		t.Fatalf("repair data dir: %v", err)
	}
}

func decodeStatus(t *testing.T, rec *httptest.ResponseRecorder) metav1.Status {
	t.Helper()
	var status metav1.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v (body %s)", err, rec.Body.String())
	}
	return status
}

func wantFailedDeleteMessage(t *testing.T, status metav1.Status, kind, name string) {
	t.Helper()
	wantPrefix := kind + ` "` + name + `" was stopped but could not be removed from the store (`
	if !strings.HasPrefix(status.Message, wantPrefix) {
		t.Fatalf("message = %q, want prefix %q", status.Message, wantPrefix)
	}
	if !strings.HasSuffix(status.Message, "); retry the delete") {
		t.Fatalf("message = %q, want the retry-the-delete suffix", status.Message)
	}
}

// TestDeletePodPersistFailureKeepsObjectForRetry: after the unit is stopped
// and its files are removed, a failed final store write keeps the pod in the
// store, starts nothing again and asks the caller to retry. The retry removes
// the store entry.
func TestDeletePodPersistFailureKeepsObjectForRetry(t *testing.T) {
	st, srv, dataDir := newDeleteTestServer(t)
	if _, err := st.CreatePod(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "solo", Namespace: "default"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
	}); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	podPath := writeOwnedUnit(t, srv.config.QuadletDir, "default_solo.container", "default", "solo")

	var stops, starts []string
	srv.stopUnitHook = func(name string) { stops = append(stops, name) }
	srv.startUnitHook = func(name string) { starts = append(starts, name) }
	calls := recordPodmanCalls(t)

	breakStoreDir(t, dataDir)
	rec := httptest.NewRecorder()
	srv.handlePods(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/namespaces/default/pods/solo", nil), "default", "solo")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
	}
	status := decodeStatus(t, rec)
	if status.Reason != metav1.StatusReason("InternalError") {
		t.Fatalf("reason = %q, want InternalError", status.Reason)
	}
	wantFailedDeleteMessage(t, status, "pod", "solo")
	if !hasCall(stops, "default_solo.service") {
		t.Fatalf("unit was not stopped before the store delete: %v", stops)
	}
	if !hasCall(*calls, "rm -f default_solo") {
		t.Fatalf("container was not removed before the store delete: %v", *calls)
	}
	if len(starts) != 0 {
		t.Fatalf("failed delete started units: %v", starts)
	}
	if _, err := st.GetPod("default", "solo"); err != nil {
		t.Fatalf("pod was not kept in the store after the failed delete: %v", err)
	}
	if _, err := os.Stat(podPath); !os.IsNotExist(err) {
		t.Fatalf("pod quadlet was restored after the failed delete, stat err=%v", err)
	}

	repairStoreDir(t, dataDir)
	rec = httptest.NewRecorder()
	srv.handlePods(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/namespaces/default/pods/solo", nil), "default", "solo")
	if rec.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if _, err := st.GetPod("default", "solo"); err == nil {
		t.Fatal("pod is still in the store after the retried delete")
	}
	if len(starts) != 0 {
		t.Fatalf("retried delete started units: %v", starts)
	}
}

// TestDeleteDeploymentPersistFailureKeepsDeploymentAndPods: the deployment and
// its instance pods are removed in one mutation, so a failed write leaves all
// of them in the store.
func TestDeleteDeploymentPersistFailureKeepsDeploymentAndPods(t *testing.T) {
	st, srv, dataDir := newDeleteTestServer(t)
	replicas := int32(2)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: "nginx"}}}},
		},
	}
	created, err := st.CreateDeployment(dep)
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	for i := 0; i < int(replicas); i++ {
		name := fmt.Sprintf("web-%d", i)
		if _, err := st.CreatePod(&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
				OwnerReferences: []metav1.OwnerReference{{
					Kind: "Deployment", Name: created.Name, UID: created.UID,
				}},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: "nginx"}}},
		}); err != nil {
			t.Fatalf("CreatePod %s: %v", name, err)
		}
		writeOwnedUnit(t, srv.config.QuadletDir, "default_"+name+".container", "default", name)
	}

	var starts []string
	srv.startUnitHook = func(name string) { starts = append(starts, name) }
	recordPodmanCalls(t)

	breakStoreDir(t, dataDir)
	rec := httptest.NewRecorder()
	srv.handleDeployments(rec, httptest.NewRequest(http.MethodDelete, "/apis/apps/v1/namespaces/default/deployments/web", nil), "default", "web")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
	}
	status := decodeStatus(t, rec)
	wantFailedDeleteMessage(t, status, "deployment", "web")
	if _, err := st.GetDeployment("default", "web"); err != nil {
		t.Fatalf("deployment was not kept in the store after the failed delete: %v", err)
	}
	for i := 0; i < int(replicas); i++ {
		name := fmt.Sprintf("web-%d", i)
		if _, err := st.GetPod("default", name); err != nil {
			t.Fatalf("instance pod %s was not kept in the store after the failed delete: %v", name, err)
		}
	}
	if len(starts) != 0 {
		t.Fatalf("failed delete started units: %v", starts)
	}
}

// TestDeleteNamespacePersistFailureKeepsEveryObject: the namespace purge and
// the namespace delete are one mutation now, so a failed write leaves every
// object of the namespace in the store.
func TestDeleteNamespacePersistFailureKeepsEveryObject(t *testing.T) {
	st, srv, dataDir := newDeleteTestServer(t)
	if _, err := st.CreateNamespace(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team"}}); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}

	meta := func(name string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: name, Namespace: "team"}
	}
	if _, err := st.CreatePod(&corev1.Pod{
		ObjectMeta: meta("app"),
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
	}); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	if _, err := st.CreateService(&corev1.Service{ObjectMeta: meta("web")}); err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	if _, err := st.CreatePVC(&corev1.PersistentVolumeClaim{ObjectMeta: meta("data")}); err != nil {
		t.Fatalf("CreatePVC: %v", err)
	}
	if _, err := st.CreateConfigMap(&corev1.ConfigMap{ObjectMeta: meta("cfg")}); err != nil {
		t.Fatalf("CreateConfigMap: %v", err)
	}
	if _, err := st.CreateSecret(&corev1.Secret{ObjectMeta: meta("sec")}); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	if _, err := st.CreateServiceAccount(&corev1.ServiceAccount{ObjectMeta: meta("worker")}); err != nil {
		t.Fatalf("CreateServiceAccount: %v", err)
	}
	replicas := int32(1)
	if _, err := st.CreateDeployment(&appsv1.Deployment{
		ObjectMeta: meta("dep"),
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dep", Image: "nginx"}}}},
		},
	}); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	if _, err := st.CreateJob(&batchv1.Job{
		ObjectMeta: meta("job"),
		Spec:       batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "job", Image: "busybox"}}}}},
	}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if _, err := st.CreateCronJob(&batchv1.CronJob{
		ObjectMeta: meta("cron"),
		Spec: batchv1.CronJobSpec{
			Schedule:    "* * * * *",
			JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "cron", Image: "busybox"}}}}}},
		},
	}); err != nil {
		t.Fatalf("CreateCronJob: %v", err)
	}
	if _, err := st.CreateIngress(&networkingv1.Ingress{ObjectMeta: meta("ing")}); err != nil {
		t.Fatalf("CreateIngress: %v", err)
	}
	if _, err := st.CreateMiddleware(&traefik.Middleware{
		ObjectMeta: meta("mw"),
		Spec:       traefik.MiddlewareSpec{IPAllowList: &traefik.IPAllowList{SourceRange: []string{"127.0.0.1/32"}}},
	}); err != nil {
		t.Fatalf("CreateMiddleware: %v", err)
	}
	if _, err := st.CreateIngressRouteTCP(&traefik.IngressRouteTCP{
		ObjectMeta: meta("tcp"),
		Spec:       traefik.IngressRouteTCPSpec{EntryPoints: []string{"smtp"}},
	}); err != nil {
		t.Fatalf("CreateIngressRouteTCP: %v", err)
	}
	if _, err := st.CreateRole(&rbacv1.Role{ObjectMeta: meta("reader")}); err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	if _, err := st.CreateRoleBinding(&rbacv1.RoleBinding{
		ObjectMeta: meta("readers"),
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "reader"},
	}); err != nil {
		t.Fatalf("CreateRoleBinding: %v", err)
	}

	var starts []string
	srv.startUnitHook = func(name string) { starts = append(starts, name) }
	recordPodmanCalls(t)

	breakStoreDir(t, dataDir)
	rec := httptest.NewRecorder()
	srv.handleNamespaceDelete(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/namespaces/team", nil), "team")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
	}
	status := decodeStatus(t, rec)
	wantFailedDeleteMessage(t, status, "namespace", "team")
	if _, err := st.GetNamespace("team"); err != nil {
		t.Fatalf("namespace was not kept in the store after the failed delete: %v", err)
	}
	checks := []struct {
		kind string
		get  func() error
	}{
		{"pod", func() error { _, err := st.GetPod("team", "app"); return err }},
		{"service", func() error { _, err := st.GetService("team", "web"); return err }},
		{"pvc", func() error { _, err := st.GetPVC("team", "data"); return err }},
		{"configmap", func() error { _, err := st.GetConfigMap("team", "cfg"); return err }},
		{"secret", func() error { _, err := st.GetSecret("team", "sec"); return err }},
		{"serviceaccount", func() error { _, err := st.GetServiceAccount("team", "worker"); return err }},
		{"deployment", func() error { _, err := st.GetDeployment("team", "dep"); return err }},
		{"job", func() error { _, err := st.GetJob("team", "job"); return err }},
		{"cronjob", func() error { _, err := st.GetCronJob("team", "cron"); return err }},
		{"ingress", func() error { _, err := st.GetIngress("team", "ing"); return err }},
		{"middleware", func() error { _, err := st.GetMiddleware("team", "mw"); return err }},
		{"ingressroutetcp", func() error { _, err := st.GetIngressRouteTCP("team", "tcp"); return err }},
		{"role", func() error { _, err := st.GetRole("team", "reader"); return err }},
		{"rolebinding", func() error { _, err := st.GetRoleBinding("team", "readers"); return err }},
	}
	for _, check := range checks {
		if err := check.get(); err != nil {
			t.Errorf("%s was not kept in the store after the failed namespace delete: %v", check.kind, err)
		}
	}
	if len(starts) != 0 {
		t.Fatalf("failed delete started units: %v", starts)
	}
}
