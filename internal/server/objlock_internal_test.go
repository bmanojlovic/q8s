package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"q8s/internal/store"
)

// --- keyed lock unit tests ---

func TestObjectLockSameKeySerializes(t *testing.T) {
	srv := &Server{}
	first := srv.lockObject("Deployment", "default", "web")
	acquired := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)
		unlock := srv.lockObject("Deployment", "default", "web")
		close(acquired)
		unlock()
	}()

	select {
	case <-acquired:
		first()
		t.Fatal("second lock for the same key was acquired while the first was held")
	case <-time.After(50 * time.Millisecond):
	}

	first()
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("second lock for the same key was not acquired after the first was released")
	}
	<-done
}

func TestObjectLockDifferentKeysDoNotBlock(t *testing.T) {
	srv := &Server{}
	first := srv.lockObject("Deployment", "default", "web")
	defer first()

	acquired := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		unlock := srv.lockObject("Deployment", "default", "api")
		close(acquired)
		unlock()
	}()

	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("a lock for a different key waited for an unrelated object")
	}
	<-done
}

func TestObjectLockSharedAllowsReadersAndRemovesEntries(t *testing.T) {
	srv := &Server{}
	key := objectLockKey("Namespace", "", "team")

	first := srv.lockObjectShared("Namespace", "", "team")
	if got := srv.objLocks.refs(key); got != 1 {
		t.Fatalf("refs while holding one shared lock = %d, want 1", got)
	}

	acquired := make(chan struct{})
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		unlock := srv.lockObjectShared("Namespace", "", "team")
		close(acquired)
		unlock()
	}()
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("a second shared lock for the same key blocked")
	}
	<-readerDone

	exclusiveAcquired := make(chan struct{})
	exclusiveDone := make(chan struct{})
	go func() {
		defer close(exclusiveDone)
		unlock := srv.lockObject("Namespace", "", "team")
		close(exclusiveAcquired)
		unlock()
	}()
	select {
	case <-exclusiveAcquired:
		first()
		t.Fatal("an exclusive lock was acquired while a shared lock was held")
	case <-time.After(50 * time.Millisecond):
	}

	first()
	select {
	case <-exclusiveAcquired:
	case <-time.After(2 * time.Second):
		t.Fatal("the exclusive lock was not acquired after the shared lock was released")
	}
	<-exclusiveDone

	srv.objLocks.mu.Lock()
	entries := len(srv.objLocks.entries)
	srv.objLocks.mu.Unlock()
	if entries != 0 {
		t.Fatalf("lock map still has %d entries after every unlock, want 0", entries)
	}
}

// --- helpers ---

// newObjectLockTestServer builds a Server whose systemd and podman side
// effects are replaced by the given hooks or by no-ops, so concurrent handler
// tests only observe the store and quadlet files.
func newObjectLockTestServer(t *testing.T, st *store.Store, quadletDir string) *Server {
	t.Helper()
	old := podmanCommand
	podmanCommand = func(context.Context, ...string) ([]byte, error) { return nil, nil }
	t.Cleanup(func() { podmanCommand = old })

	srv := &Server{config: Config{Store: st, QuadletDir: quadletDir}}
	srv.stopUnitHook = func(string) {}
	srv.startUnitHook = func(string) {}
	srv.mux = http.NewServeMux()
	srv.setupRoutes()
	return srv
}

func newObjectLockHTTP(t *testing.T, srv *Server) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(srv.applyMiddleware(srv.mux))
	t.Cleanup(ts.Close)
	return ts
}

func sendObjectLockRequest(method, url, contentType string, body []byte) (int, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return 0, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

func objectLockDeploymentBody(name string) []byte {
	body, _ := json.Marshal(map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]interface{}{"name": name, "namespace": "default"},
		"spec": map[string]interface{}{
			"replicas": 1,
			"selector": map[string]interface{}{"matchLabels": map[string]interface{}{"app": name}},
			"template": map[string]interface{}{
				"metadata": map[string]interface{}{"labels": map[string]interface{}{"app": name}},
				"spec": map[string]interface{}{
					"containers": []interface{}{map[string]interface{}{"name": "web", "image": "nginx"}},
				},
			},
		},
	})
	return body
}

func objectLockPodBody(name string) []byte {
	return objectLockPodBodyInNamespace(name, "default")
}

func objectLockPodBodyInNamespace(name, namespace string) []byte {
	body, _ := json.Marshal(map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   map[string]interface{}{"name": name, "namespace": namespace},
		"spec": map[string]interface{}{
			"containers": []interface{}{map[string]interface{}{"name": "web", "image": "nginx"}},
		},
	})
	return body
}

func objectLockFilesWithPrefix(t *testing.T, dir, prefix string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir %s: %v", dir, err)
	}
	var matches []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) {
			matches = append(matches, entry.Name())
		}
	}
	return matches
}

func assertObjectLockDeploymentConsistent(t *testing.T, st *store.Store, dir, name string) {
	t.Helper()
	dep, err := st.GetDeployment("default", name)
	if err == nil {
		for i := int32(0); i < deploymentReplicas(dep); i++ {
			path := filepath.Join(dir, fmt.Sprintf("default-%s-%d.container", name, i))
			if _, statErr := os.Stat(path); statErr != nil {
				t.Fatalf("Deployment %s exists but its replica unit %s is missing: %v", name, filepath.Base(path), statErr)
			}
		}
		return
	}
	if files := objectLockFilesWithPrefix(t, dir, "default-"+name+"-"); len(files) != 0 {
		t.Fatalf("Deployment %s is gone but unit files remain: %v", name, files)
	}
	for _, pod := range st.Pods("default") {
		if strings.HasPrefix(pod.Name, name+"-") {
			t.Fatalf("Deployment %s is gone but replica Pod %s remains", name, pod.Name)
		}
	}
}

func assertObjectLockPodConsistent(t *testing.T, st *store.Store, dir, name string) {
	t.Helper()
	_, err := st.GetPod("default", name)
	path := filepath.Join(dir, fmt.Sprintf("default-%s.container", name))
	_, statErr := os.Stat(path)
	if err == nil {
		if statErr != nil {
			t.Fatalf("Pod %s exists but its unit %s is missing: %v", name, filepath.Base(path), statErr)
		}
		return
	}
	if statErr == nil {
		t.Fatalf("Pod %s is gone but unit %s remains", name, filepath.Base(path))
	}
}

// --- delete versus apply ---

func TestDeleteAndApplySameDeploymentStayConsistent(t *testing.T) {
	st := store.New()
	dir := t.TempDir()
	srv := newObjectLockTestServer(t, st, dir)
	ts := newObjectLockHTTP(t, srv)

	for i := 0; i < 50; i++ {
		name := fmt.Sprintf("web%d", i)
		url := ts.URL + "/apis/apps/v1/namespaces/default/deployments/" + name
		body := objectLockDeploymentBody(name)

		status, err := sendObjectLockRequest(http.MethodPatch, url, "application/apply-patch+yaml", body)
		if err != nil {
			t.Fatalf("iteration %d: initial apply: %v", i, err)
		}
		if status != http.StatusOK && status != http.StatusCreated {
			t.Fatalf("iteration %d: initial apply status = %d", i, status)
		}
		if files := objectLockFilesWithPrefix(t, dir, "default-"+name+"-"); len(files) == 0 {
			t.Fatalf("iteration %d: initial apply created no unit files", i)
		}

		start := make(chan struct{})
		var wg sync.WaitGroup
		results := make(chan error, 2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			status, err := sendObjectLockRequest(http.MethodDelete, url, "", nil)
			if err != nil {
				results <- fmt.Errorf("iteration %d: delete: %w", i, err)
				return
			}
			if status != http.StatusOK && status != http.StatusNotFound {
				results <- fmt.Errorf("iteration %d: delete status = %d", i, status)
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			status, err := sendObjectLockRequest(http.MethodPatch, url, "application/apply-patch+yaml", body)
			if err != nil {
				results <- fmt.Errorf("iteration %d: apply: %w", i, err)
				return
			}
			if status != http.StatusOK && status != http.StatusCreated && status != http.StatusNotFound {
				results <- fmt.Errorf("iteration %d: apply status = %d", i, status)
			}
		}()
		close(start)
		wg.Wait()
		close(results)
		for err := range results {
			if err != nil {
				t.Fatal(err)
			}
		}

		assertObjectLockDeploymentConsistent(t, st, dir, name)
	}
}

func TestDeleteAndApplySamePodStayConsistent(t *testing.T) {
	st := store.New()
	dir := t.TempDir()
	srv := newObjectLockTestServer(t, st, dir)
	ts := newObjectLockHTTP(t, srv)

	for i := 0; i < 50; i++ {
		name := fmt.Sprintf("box%d", i)
		url := ts.URL + "/api/v1/namespaces/default/pods/" + name
		body := objectLockPodBody(name)

		status, err := sendObjectLockRequest(http.MethodPatch, url, "application/apply-patch+yaml", body)
		if err != nil {
			t.Fatalf("iteration %d: initial apply: %v", i, err)
		}
		if status != http.StatusOK && status != http.StatusCreated {
			t.Fatalf("iteration %d: initial apply status = %d", i, status)
		}
		assertObjectLockPodConsistent(t, st, dir, name)

		start := make(chan struct{})
		var wg sync.WaitGroup
		results := make(chan error, 2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			status, err := sendObjectLockRequest(http.MethodDelete, url, "", nil)
			if err != nil {
				results <- fmt.Errorf("iteration %d: delete: %w", i, err)
				return
			}
			if status != http.StatusOK && status != http.StatusNotFound {
				results <- fmt.Errorf("iteration %d: delete status = %d", i, status)
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			status, err := sendObjectLockRequest(http.MethodPatch, url, "application/apply-patch+yaml", body)
			if err != nil {
				results <- fmt.Errorf("iteration %d: apply: %w", i, err)
				return
			}
			if status != http.StatusOK && status != http.StatusCreated && status != http.StatusNotFound {
				results <- fmt.Errorf("iteration %d: apply status = %d", i, status)
			}
		}()
		close(start)
		wg.Wait()
		close(results)
		for err := range results {
			if err != nil {
				t.Fatal(err)
			}
		}

		assertObjectLockPodConsistent(t, st, dir, name)
	}
}

// TestDeleteDeploymentReplicaDoesNotDeadlock exercises the path that takes
// Deployment before Pod before recreating the replica. A re-lock of either
// object in that path would hang here.
func TestDeleteDeploymentReplicaDoesNotDeadlock(t *testing.T) {
	st := store.New()
	dir := t.TempDir()
	srv := newObjectLockTestServer(t, st, dir)

	replicas := int32(2)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "web"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: "nginx"}}},
			},
		},
	}
	created, err := st.CreateDeployment(dep)
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	srv.generateDeploymentQuadlets(created)
	if _, err := st.GetPod("default", "web-0"); err != nil {
		t.Fatalf("initial replica web-0: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/namespaces/default/pods/web-0", nil)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.handlePods(rec, req, "default", "web-0")
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("deleting a Deployment replica deadlocked")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body %s", rec.Code, rec.Body.String())
	}
	if _, err := st.GetPod("default", "web-0"); err != nil {
		t.Fatalf("Deployment replica web-0 was not recreated: %v", err)
	}
}

// gatedReader signals when the handler has started reading the body and then
// blocks until release is closed. The pod POST handler reads its body only
// after taking the shared Namespace lock, so started proves the create holds
// that lock while a namespace delete can start waiting for the exclusive one.
type gatedReader struct {
	started chan struct{}
	release <-chan struct{}
	data    []byte
	once    sync.Once
}

func (r *gatedReader) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	if r.release != nil {
		<-r.release
		r.release = nil
	}
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

// TestNamespaceDeleteRacingPodCreateLeavesNoUnit gates the create's body so it
// holds the shared Namespace lock before the delete starts. The delete then
// waits for the exclusive lock and cleans up the pod and its unit file before
// removing the namespace.
func TestNamespaceDeleteRacingPodCreateLeavesNoUnit(t *testing.T) {
	st := store.New()
	if _, err := st.CreateNamespace(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team"}}); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	dir := t.TempDir()
	srv := newObjectLockTestServer(t, st, dir)
	ts := newObjectLockHTTP(t, srv)

	key := objectLockKey("Namespace", "", "team")
	releaseBody := make(chan struct{})
	started := make(chan struct{})
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/namespaces/team/pods", &gatedReader{started: started, release: releaseBody, data: objectLockPodBodyInNamespace("late", "team")})
	if err != nil {
		t.Fatalf("new pod create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	createResult := make(chan objectLockResult, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			createResult <- objectLockResult{err: err}
			return
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		createResult <- objectLockResult{status: resp.StatusCode}
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("pod create did not start reading its body (and so did not hold the Namespace lock)")
	}

	deleteResult := make(chan objectLockResult, 1)
	go func() {
		status, err := sendObjectLockRequest(http.MethodDelete, ts.URL+"/api/v1/namespaces/team", "", nil)
		deleteResult <- objectLockResult{status: status, err: err}
	}()
	waitForObjectLockRefs(t, srv, key, 2)

	close(releaseBody)
	create := <-createResult
	if create.err != nil {
		t.Fatalf("pod create: %v", create.err)
	}
	if create.status != http.StatusCreated {
		t.Fatalf("pod create status = %d, want 201", create.status)
	}
	del := <-deleteResult
	if del.err != nil {
		t.Fatalf("namespace delete: %v", del.err)
	}
	if del.status != http.StatusOK {
		t.Fatalf("namespace delete status = %d, want 200", del.status)
	}

	if _, err := st.GetNamespace("team"); err == nil {
		t.Fatal("namespace still exists after delete")
	}
	if _, err := st.GetPod("team", "late"); err == nil {
		t.Fatal("pod remains in the store after its namespace was deleted")
	}
	if _, err := os.Stat(filepath.Join(dir, "team-late.container")); err == nil {
		t.Fatal("unit file remains for a pod whose namespace was deleted")
	}
}

// TestNamespacedCreateRefusedAfterNamespaceDelete covers the other side of the
// same race: a create that passed its pre-lock check must be refused when the
// namespace disappeared before it got the shared lock.
func TestNamespacedCreateRefusedAfterNamespaceDelete(t *testing.T) {
	st := store.New()
	if _, err := st.CreateNamespace(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team"}}); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	srv := &Server{config: Config{Store: st}}

	hold := srv.lockObject("Namespace", "", "team")
	type beginResult struct {
		ok     bool
		unlock func()
	}
	results := make(chan beginResult, 1)
	go func() {
		rec := httptest.NewRecorder()
		unlock, ok := srv.beginNamespacedWrite(rec, "team", true)
		results <- beginResult{ok: ok, unlock: unlock}
	}()
	key := objectLockKey("Namespace", "", "team")
	waitForObjectLockRefs(t, srv, key, 2)

	if err := st.DeleteNamespaceWithResources("team"); err != nil {
		t.Fatalf("DeleteNamespaceWithResources: %v", err)
	}
	hold()

	result := <-results
	if result.ok {
		result.unlock()
		t.Fatal("namespaced create was allowed after its namespace was deleted")
	}
}

type objectLockResult struct {
	status int
	err    error
}

func waitForObjectLockRefs(t *testing.T, srv *Server, key string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if srv.objLocks.refs(key) >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("lock %q did not reach %d refs (has %d)", key, want, srv.objLocks.refs(key))
}
