package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"q8s/internal/store"
)

type hostAccessTestEnv struct {
	ts         *httptest.Server
	srv        *Server
	st         *store.Store
	quadletDir string
}

func hostAccessPod(name, namespace string) *corev1.Pod {
	return &corev1.Pod{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "main", Image: "nginx:1.25"}},
		},
	}
}

func privilegedPod(name, namespace string) *corev1.Pod {
	pod := hostAccessPod(name, namespace)
	pod.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: boolPtr(true)}
	return pod
}

func hostPIDPod(name, namespace string) *corev1.Pod {
	pod := hostAccessPod(name, namespace)
	pod.Spec.HostPID = true
	return pod
}

func hostNetworkPod(name, namespace string) *corev1.Pod {
	pod := hostAccessPod(name, namespace)
	pod.Spec.HostNetwork = true
	return pod
}

func hostUsersPod(name, namespace string) *corev1.Pod {
	pod := hostAccessPod(name, namespace)
	pod.Spec.HostUsers = boolPtr(true)
	return pod
}

func privilegedDeployment(name, namespace string) *appsv1.Deployment {
	return &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:            "main",
						Image:           "nginx:1.25",
						SecurityContext: &corev1.SecurityContext{Privileged: boolPtr(true)},
					}},
				},
			},
		},
	}
}

func privilegedJob(name, namespace string) *batchv1.Job {
	return &batchv1.Job{
		TypeMeta:   metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:            "main",
						Image:           "busybox:1.36",
						SecurityContext: &corev1.SecurityContext{Privileged: boolPtr(true)},
					}},
				},
			},
		},
	}
}

func privilegedCronJob(name, namespace string) *batchv1.CronJob {
	job := privilegedJob(name, namespace)
	return &batchv1.CronJob{
		TypeMeta:   metav1.TypeMeta{APIVersion: "batch/v1", Kind: "CronJob"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: batchv1.CronJobSpec{
			Schedule: "* * * * *",
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: job.Spec,
			},
		},
	}
}

func hostPathPVC(name, namespace string) *corev1.PersistentVolumeClaim {
	class := "hostpath"
	return &corev1.PersistentVolumeClaim{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Annotations: map[string]string{"q8s.io/host-path": "/srv/data"},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: &class,
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		},
	}
}

func int32Ptr(n int32) *int32 { return &n }

func newHostAccessTestEnv(t *testing.T) (*hostAccessTestEnv, *testCA) {
	t.Helper()
	ca := newTestCA(t)
	ts, srv, st := newEnforceTestServer(t, ca)
	quadletDir := t.TempDir()
	srv.config.QuadletDir = quadletDir
	srv.config.ConfigDir = filepath.Join(t.TempDir(), "configmaps")
	return &hostAccessTestEnv{
		ts:         ts,
		srv:        srv,
		st:         st,
		quadletDir: quadletDir,
	}, ca
}

func (e *hostAccessTestEnv) URL(path string) string {
	return e.ts.URL + path
}

func TestNonAdminCannotCreateHostAccessObjects(t *testing.T) {
	env, ca := newHostAccessTestEnv(t)
	alice := tlsClient(ca.issue(t, "alice", nil))
	admin := tlsClient(ca.issue(t, "root", []string{"system:masters"}))
	createRoleBindingForTest(t, env.srv, "team", "alice-edit", "ClusterRole", "edit",
		rbacv1.Subject{Kind: "User", Name: "alice"})

	cases := []struct {
		name       string
		path       string
		body       func(string) interface{}
		exists     func(*store.Store, string) bool
		wantReason string
	}{
		{
			name: "privileged pod",
			path: "/api/v1/namespaces/team/pods",
			body: func(name string) interface{} { return privilegedPod(name, "team") },
			exists: func(st *store.Store, name string) bool {
				_, err := st.GetPod("team", name)
				return err == nil
			},
			wantReason: "spec.containers[0].securityContext.privileged",
		},
		{
			name: "hostPID pod",
			path: "/api/v1/namespaces/team/pods",
			body: func(name string) interface{} { return hostPIDPod(name, "team") },
			exists: func(st *store.Store, name string) bool {
				_, err := st.GetPod("team", name)
				return err == nil
			},
			wantReason: "spec.hostPID",
		},
		{
			name: "hostNetwork pod",
			path: "/api/v1/namespaces/team/pods",
			body: func(name string) interface{} { return hostNetworkPod(name, "team") },
			exists: func(st *store.Store, name string) bool {
				_, err := st.GetPod("team", name)
				return err == nil
			},
			wantReason: "spec.hostNetwork",
		},
		{
			name: "hostUsers pod",
			path: "/api/v1/namespaces/team/pods",
			body: func(name string) interface{} { return hostUsersPod(name, "team") },
			exists: func(st *store.Store, name string) bool {
				_, err := st.GetPod("team", name)
				return err == nil
			},
			wantReason: "spec.hostUsers",
		},
		{
			name: "privileged deployment",
			path: "/apis/apps/v1/namespaces/team/deployments",
			body: func(name string) interface{} { return privilegedDeployment(name, "team") },
			exists: func(st *store.Store, name string) bool {
				_, err := st.GetDeployment("team", name)
				return err == nil
			},
			wantReason: "spec.containers[0].securityContext.privileged",
		},
		{
			name: "privileged job",
			path: "/apis/batch/v1/namespaces/team/jobs",
			body: func(name string) interface{} { return privilegedJob(name, "team") },
			exists: func(st *store.Store, name string) bool {
				_, err := st.GetJob("team", name)
				return err == nil
			},
			wantReason: "spec.containers[0].securityContext.privileged",
		},
		{
			name: "privileged cronjob",
			path: "/apis/batch/v1/namespaces/team/cronjobs",
			body: func(name string) interface{} { return privilegedCronJob(name, "team") },
			exists: func(st *store.Store, name string) bool {
				_, err := st.GetCronJob("team", name)
				return err == nil
			},
			wantReason: "spec.containers[0].securityContext.privileged",
		},
		{
			name: "hostpath pvc",
			path: "/api/v1/namespaces/team/persistentvolumeclaims",
			body: func(name string) interface{} { return hostPathPVC(name, "team") },
			exists: func(st *store.Store, name string) bool {
				_, err := st.GetPVC("team", name)
				return err == nil
			},
			wantReason: `spec.storageClassName "hostpath"`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name := "denied-" + strings.ReplaceAll(strings.ToLower(tc.name), " ", "-")
			resp := doAs(t, alice, http.MethodPost, env.URL(tc.path), tc.body(name))
			requireHostAccessForbidden(t, resp, name, tc.wantReason)
			if tc.exists(env.st, name) {
				t.Fatalf("%s was stored after a forbidden create", name)
			}
		})
	}
	entries, err := os.ReadDir(env.quadletDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("forbidden creates wrote quadlet files: %v", entries)
	}

	for _, tc := range cases {
		t.Run("admin "+tc.name, func(t *testing.T) {
			name := "admin-" + strings.ReplaceAll(strings.ToLower(tc.name), " ", "-")
			requireStatus(t, doAs(t, admin, http.MethodPost, env.URL(tc.path), tc.body(name)), http.StatusCreated)
			if !tc.exists(env.st, name) {
				t.Fatalf("%s was not stored for an administrator", name)
			}
		})
	}
}

func TestHostAccessDryRunIsChecked(t *testing.T) {
	env, ca := newHostAccessTestEnv(t)
	alice := tlsClient(ca.issue(t, "alice", nil))
	admin := tlsClient(ca.issue(t, "root", []string{"system:masters"}))
	createRoleBindingForTest(t, env.srv, "team", "alice-edit", "ClusterRole", "edit",
		rbacv1.Subject{Kind: "User", Name: "alice"})

	pod := privilegedPod("dry-run", "team")
	resp := doAs(t, alice, http.MethodPost, env.URL("/api/v1/namespaces/team/pods?dryRun=All"), pod)
	requireHostAccessForbidden(t, resp, "dry-run", "spec.containers[0].securityContext.privileged")
	if _, err := env.st.GetPod("team", "dry-run"); err == nil {
		t.Fatal("dry-run request stored the pod")
	}

	requireStatus(t, doAs(t, admin, http.MethodPost, env.URL("/api/v1/namespaces/team/pods?dryRun=All"), pod), http.StatusCreated)
	if _, err := env.st.GetPod("team", "dry-run"); err == nil {
		t.Fatal("admin dry-run request stored the pod")
	}
}

func TestHostAccessPatchAndScaleChecks(t *testing.T) {
	env, ca := newHostAccessTestEnv(t)
	alice := tlsClient(ca.issue(t, "alice", nil))
	admin := tlsClient(ca.issue(t, "root", []string{"system:masters"}))
	createRoleBindingForTest(t, env.srv, "team", "alice-edit", "ClusterRole", "edit",
		rbacv1.Subject{Kind: "User", Name: "alice"})

	requireStatus(t, doAs(t, admin, http.MethodPost,
		env.URL("/apis/apps/v1/namespaces/team/deployments"), privilegedDeployment("host-dep", "team")),
		http.StatusCreated)
	requireStatus(t, doAs(t, admin, http.MethodPost,
		env.URL("/api/v1/namespaces/team/pods"), hostAccessPod("plain", "team")),
		http.StatusCreated)

	t.Run("image-only patch of a privileged deployment", func(t *testing.T) {
		body := map[string]interface{}{
			"spec": map[string]interface{}{
				"template": map[string]interface{}{
					"spec": map[string]interface{}{
						"containers": []interface{}{map[string]interface{}{"name": "main", "image": "nginx:1.26"}},
					},
				},
			},
		}
		resp := doAs(t, alice, http.MethodPatch, env.URL("/apis/apps/v1/namespaces/team/deployments/host-dep"), body)
		requireHostAccessForbidden(t, resp, "host-dep", "spec.containers[0].securityContext.privileged")

		dep, err := env.st.GetDeployment("team", "host-dep")
		if err != nil {
			t.Fatal(err)
		}
		if got := dep.Spec.Template.Spec.Containers[0].Image; got != "nginx:1.25" {
			t.Fatalf("forbidden patch changed the image to %q", got)
		}
	})

	t.Run("patch setting hostPID", func(t *testing.T) {
		body := map[string]interface{}{"spec": map[string]interface{}{"hostPID": true}}
		resp := doAs(t, alice, http.MethodPatch, env.URL("/api/v1/namespaces/team/pods/plain"), body)
		requireHostAccessForbidden(t, resp, "plain", "spec.hostPID")

		pod, err := env.st.GetPod("team", "plain")
		if err != nil {
			t.Fatal(err)
		}
		if pod.Spec.HostPID {
			t.Fatal("forbidden patch set hostPID")
		}
	})

	t.Run("scale of a privileged deployment is allowed", func(t *testing.T) {
		createRoleForTest(t, env.srv, "team", "deployment-scale", rbacv1.PolicyRule{
			APIGroups: []string{"apps"}, Resources: []string{"deployments/scale"}, Verbs: []string{"patch", "update"},
		})
		createRoleBindingForTest(t, env.srv, "team", "alice-deployment-scale", "Role", "deployment-scale",
			rbacv1.Subject{Kind: "User", Name: "alice"})
		body := map[string]interface{}{"spec": map[string]interface{}{"replicas": 2}}
		resp := doAs(t, alice, http.MethodPatch, env.URL("/apis/apps/v1/namespaces/team/deployments/host-dep/scale"), body)
		requireStatus(t, resp, http.StatusOK)

		dep, err := env.st.GetDeployment("team", "host-dep")
		if err != nil {
			t.Fatal(err)
		}
		if got := deploymentReplicas(dep); got != 2 {
			t.Fatalf("scale changed replicas to %d, want 2", got)
		}
	})

	t.Run("admin may patch the privileged deployment", func(t *testing.T) {
		body := map[string]interface{}{
			"spec": map[string]interface{}{
				"template": map[string]interface{}{
					"spec": map[string]interface{}{
						"containers": []interface{}{map[string]interface{}{"name": "main", "image": "nginx:1.27"}},
					},
				},
			},
		}
		requireStatus(t, doAs(t, admin, http.MethodPatch, env.URL("/apis/apps/v1/namespaces/team/deployments/host-dep"), body), http.StatusOK)
	})
}

func requireHostAccessForbidden(t *testing.T, resp *http.Response, name, wantReason string) {
	t.Helper()
	if resp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("status = %d, want 403: %s", resp.StatusCode, body)
	}
	body := statusBody(t, resp)
	if body["reason"] != "Forbidden" {
		t.Fatalf("reason = %v, want Forbidden", body["reason"])
	}
	message, _ := body["message"].(string)
	if !strings.Contains(message, "uses host access") {
		t.Fatalf("message = %q, want host access refusal", message)
	}
	if !strings.Contains(message, wantReason) {
		t.Fatalf("message = %q, want reason %q", message, wantReason)
	}
	if !strings.Contains(message, `"`+name+`"`) {
		t.Fatalf("message = %q, want object name %q", message, name)
	}
	if !strings.HasSuffix(message, "which only an administrator may configure") {
		t.Fatalf("message = %q, want administrator-only suffix", message)
	}
}

func TestApplyHostAccessAndFallbackAuthorization(t *testing.T) {
	env, ca := newHostAccessTestEnv(t)
	alice := tlsClient(ca.issue(t, "alice", nil))
	bob := tlsClient(ca.issue(t, "bob", nil))
	createRoleBindingForTest(t, env.srv, "team", "alice-edit", "ClusterRole", "edit",
		rbacv1.Subject{Kind: "User", Name: "alice"})
	createRoleForTest(t, env.srv, "team", "pod-patch", rbacv1.PolicyRule{
		APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"patch"},
	})
	createRoleBindingForTest(t, env.srv, "team", "bob-patch", "Role", "pod-patch",
		rbacv1.Subject{Kind: "User", Name: "bob"})

	privilegedYAML := `apiVersion: v1
kind: Pod
metadata:
  name: applied-privileged
  namespace: team
spec:
  hostPID: true
  containers:
  - name: main
    image: nginx:1.25
`
	t.Run("apply of a privileged pod", func(t *testing.T) {
		resp := doRawAs(t, alice, http.MethodPatch, env.URL("/api/v1/namespaces/team/pods/applied-privileged"),
			"application/apply-patch+yaml", privilegedYAML)
		requireHostAccessForbidden(t, resp, "applied-privileged", "spec.hostPID")
		if _, err := env.st.GetPod("team", "applied-privileged"); err == nil {
			t.Fatal("forbidden apply created the pod")
		}
	})

	podYAML := func(name string) string {
		return `apiVersion: v1
kind: Pod
metadata:
  name: ` + name + `
  namespace: team
spec:
  containers:
  - name: main
    image: nginx:1.25
`
	}

	t.Run("apply with only patch is denied create", func(t *testing.T) {
		resp := doRawAs(t, bob, http.MethodPatch, env.URL("/api/v1/namespaces/team/pods/apply-new"),
			"application/apply-patch+yaml", podYAML("apply-new"))
		requireForbidden(t, resp, `pods is forbidden: User "bob" cannot create resource "pods" in API group "" in the namespace "team"`)
		if _, err := env.st.GetPod("team", "apply-new"); err == nil {
			t.Fatal("apply fallback created the pod without create permission")
		}
	})

	t.Run("apply with create and patch creates the pod", func(t *testing.T) {
		createRoleForTest(t, env.srv, "team", "pod-create-patch", rbacv1.PolicyRule{
			APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"create", "patch"},
		})
		createRoleBindingForTest(t, env.srv, "team", "bob-create-patch", "Role", "pod-create-patch",
			rbacv1.Subject{Kind: "User", Name: "bob"})
		resp := doRawAs(t, bob, http.MethodPatch, env.URL("/api/v1/namespaces/team/pods/apply-created"),
			"application/apply-patch+yaml", podYAML("apply-created"))
		requireStatus(t, resp, http.StatusCreated)
		if _, err := env.st.GetPod("team", "apply-created"); err != nil {
			t.Fatalf("apply did not create the pod: %v", err)
		}
	})
}

func TestPOSTToNamedObjectURLIsMethodNotAllowed(t *testing.T) {
	env, ca := newHostAccessTestEnv(t)
	alice := tlsClient(ca.issue(t, "alice", nil))
	createRoleBindingForTest(t, env.srv, "team", "alice-edit", "ClusterRole", "edit",
		rbacv1.Subject{Kind: "User", Name: "alice"})

	resp := doAs(t, alice, http.MethodPost, env.URL("/api/v1/namespaces/team/pods/debug"),
		hostAccessPod("debug", "team"))
	requireStatus(t, resp, http.StatusMethodNotAllowed)
	if _, err := env.st.GetPod("team", "debug"); err == nil {
		t.Fatal("POST to a named pod URL created the pod")
	}

	// The streaming subresources that take POST stay reachable.
	resp = doAs(t, alice, http.MethodPost, env.URL("/api/v1/namespaces/team/pods/web/exec"), nil)
	if resp.StatusCode == http.StatusMethodNotAllowed {
		resp.Body.Close()
		t.Fatal("POST to pods/exec was refused as a named object URL")
	}
	resp.Body.Close()
}

// doRawAs sends a request with an arbitrary body and Content-Type for a user.
func doRawAs(t *testing.T, client *http.Client, method, url, contentType, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}
