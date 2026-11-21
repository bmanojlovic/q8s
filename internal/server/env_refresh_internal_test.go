package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"q8s/internal/names"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"q8s/internal/quadlet"
	"q8s/internal/store"
)

func newEnvRefreshTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	base := t.TempDir()
	srv := &Server{config: Config{
		Store:      store.New(),
		QuadletDir: t.TempDir(),
		ConfigDir:  filepath.Join(base, "configmaps"),
	}}
	return srv, base
}

func createEnvTestSecret(t *testing.T, st *store.Store, ns, name string, data map[string]string) {
	t.Helper()
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	secret.Data = make(map[string][]byte, len(data))
	for k, v := range data {
		secret.Data[k] = []byte(v)
	}
	if _, err := st.CreateSecret(secret); err != nil {
		t.Fatalf("CreateSecret %s/%s: %v", ns, name, err)
	}
}

func createEnvTestConfigMap(t *testing.T, st *store.Store, ns, name string, data map[string]string) {
	t.Helper()
	if _, err := st.CreateConfigMap(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data:       data,
	}); err != nil {
		t.Fatalf("CreateConfigMap %s/%s: %v", ns, name, err)
	}
}

func envCronJob(ns, name string, env []corev1.EnvVar, envFrom []corev1.EnvFromSource, suspend bool) *batchv1.CronJob {
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 * * * *",
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							RestartPolicy: corev1.RestartPolicyNever,
							Containers: []corev1.Container{{
								Name:    "task",
								Image:   "alpine:3",
								Env:     env,
								EnvFrom: envFrom,
							}},
						},
					},
				},
			},
		},
	}
	if suspend {
		suspended := true
		cj.Spec.Suspend = &suspended
	}
	return cj
}

func createAndRenderCronJob(t *testing.T, srv *Server, cj *batchv1.CronJob) {
	t.Helper()
	if _, err := srv.config.Store.CreateCronJob(cj); err != nil {
		t.Fatalf("CreateCronJob %s/%s: %v", cj.Namespace, cj.Name, err)
	}
	srv.generateCronJobQuadlets(cj)
}

func sendMergePatch(t *testing.T, srv *Server, kind, ns, name string, body map[string]interface{}) {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/"+kind+"/"+name, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/merge-patch+json")
	switch kind {
	case "secrets":
		srv.handleSecrets(rec, req, ns, name)
	case "configmaps":
		srv.handleConfigMaps(rec, req, ns, name)
	default:
		t.Fatalf("unknown kind %q", kind)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH %s %s/%s = %d, body %s", kind, ns, name, rec.Code, rec.Body.String())
	}
}

func setOldMtime(t *testing.T, path string) time.Time {
	t.Helper()
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
	return old
}

func assertFileMtime(t *testing.T, path string, want time.Time) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if !info.ModTime().Equal(want) {
		t.Fatalf("%s mtime = %v, want %v", path, info.ModTime(), want)
	}
}

func readEnvTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func assertEnvFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read env file %s: %v", path, err)
	}
	if string(got) != want {
		t.Fatalf("env file %s = %q, want %q", path, got, want)
	}
}

func TestSecretUpdateRefreshesReferencingCronJobs(t *testing.T) {
	tests := []struct {
		name    string
		env     []corev1.EnvVar
		envFrom []corev1.EnvFromSource
	}{
		{
			name: "secretKeyRef",
			env: []corev1.EnvVar{{
				Name: "TOKEN",
				ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "creds"},
					Key:                  "TOKEN",
				}},
			}},
		},
		{
			name: "envFrom secretRef",
			envFrom: []corev1.EnvFromSource{{
				SecretRef: &corev1.SecretEnvSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: "creds"},
				},
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := newEnvRefreshTestServer(t)
			createEnvTestSecret(t, srv.config.Store, "default", "creds", map[string]string{"TOKEN": "old"})
			createAndRenderCronJob(t, srv, envCronJob("default", "refreshed", tt.env, tt.envFrom, false))

			envPath := srv.envFilePath("default", quadlet.CronJobEnvBaseName("refreshed"))
			assertEnvFile(t, envPath, "TOKEN=old\n")

			var starts, stops []string
			srv.startUnitHook = func(name string) { starts = append(starts, name) }
			srv.stopUnitHook = func(name string) { stops = append(stops, name) }

			sendMergePatch(t, srv, "secrets", "default", "creds", map[string]interface{}{
				"stringData": map[string]string{"TOKEN": "new"},
			})

			assertEnvFile(t, envPath, "TOKEN=new\n")
			if len(starts) != 0 || len(stops) != 0 {
				t.Fatalf("Secret update started or stopped units: starts=%v stops=%v", starts, stops)
			}
			if _, err := os.Stat(envPath); err != nil {
				t.Fatalf("env file missing after refresh: %v", err)
			}
		})
	}
}

func TestApplySecretRefreshesCronJobEnv(t *testing.T) {
	srv, _ := newEnvRefreshTestServer(t)
	createEnvTestSecret(t, srv.config.Store, "default", "creds", map[string]string{"TOKEN": "old"})
	createAndRenderCronJob(t, srv, envCronJob("default", "applied", []corev1.EnvVar{{
		Name: "TOKEN",
		ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "creds"},
			Key:                  "TOKEN",
		}},
	}}, nil, false))

	envPath := srv.envFilePath("default", quadlet.CronJobEnvBaseName("applied"))
	assertEnvFile(t, envPath, "TOKEN=old\n")

	h := srv.applyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.handleSecrets(w, r, "default", "creds")
	}))
	ts := httptest.NewServer(h)
	defer ts.Close()

	body := []byte("apiVersion: v1\nkind: Secret\nmetadata:\n  name: creds\n  namespace: default\ndata:\n  TOKEN: bmV3\n")
	req, err := http.NewRequest(http.MethodPatch, ts.URL+"/api/v1/namespaces/default/secrets/creds", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/apply-patch+yaml")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("apply Secret: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		t.Fatalf("apply Secret = %d, body %s", resp.StatusCode, buf.String())
	}

	assertEnvFile(t, envPath, "TOKEN=new\n")
}

func TestConfigMapUpdateRefreshesReferencingCronJobs(t *testing.T) {
	tests := []struct {
		name    string
		env     []corev1.EnvVar
		envFrom []corev1.EnvFromSource
	}{
		{
			name: "configMapKeyRef",
			env: []corev1.EnvVar{{
				Name: "TOKEN",
				ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "settings"},
					Key:                  "TOKEN",
				}},
			}},
		},
		{
			name: "envFrom configMapRef",
			envFrom: []corev1.EnvFromSource{{
				ConfigMapRef: &corev1.ConfigMapEnvSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: "settings"},
				},
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := newEnvRefreshTestServer(t)
			createEnvTestConfigMap(t, srv.config.Store, "default", "settings", map[string]string{"TOKEN": "old"})
			createAndRenderCronJob(t, srv, envCronJob("default", "refreshed", tt.env, tt.envFrom, false))

			containerPath := filepath.Join(srv.config.QuadletDir, names.Host(names.KindCronJobFile, "default", "refreshed"))
			if got := readEnvTestFile(t, containerPath); !strings.Contains(got, "Environment=TOKEN=old") {
				t.Fatalf("created container unit missing old ConfigMap value:\n%s", got)
			}

			var starts, stops []string
			srv.startUnitHook = func(name string) { starts = append(starts, name) }
			srv.stopUnitHook = func(name string) { stops = append(stops, name) }

			sendMergePatch(t, srv, "configmaps", "default", "settings", map[string]interface{}{
				"data": map[string]string{"TOKEN": "new"},
			})

			got := readEnvTestFile(t, containerPath)
			if !strings.Contains(got, "Environment=TOKEN=new") {
				t.Fatalf("container unit was not refreshed with the ConfigMap value:\n%s", got)
			}
			if strings.Contains(got, "Environment=TOKEN=old") {
				t.Fatalf("container unit still has the old ConfigMap value:\n%s", got)
			}
			if len(starts) != 0 || len(stops) != 0 {
				t.Fatalf("ConfigMap update started or stopped units: starts=%v stops=%v", starts, stops)
			}
		})
	}
}

func TestSecretUpdateLeavesOtherCronJobsAlone(t *testing.T) {
	srv, _ := newEnvRefreshTestServer(t)
	createEnvTestSecret(t, srv.config.Store, "default", "shared", map[string]string{"TOKEN": "old"})
	createEnvTestSecret(t, srv.config.Store, "default", "unrelated", map[string]string{"TOKEN": "old"})
	createEnvTestSecret(t, srv.config.Store, "demo", "shared", map[string]string{"TOKEN": "old"})

	secretRef := func(name string) []corev1.EnvVar {
		return []corev1.EnvVar{{
			Name: "TOKEN",
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: name},
				Key:                  "TOKEN",
			}},
		}}
	}
	createAndRenderCronJob(t, srv, envCronJob("default", "used", secretRef("shared"), nil, false))
	createAndRenderCronJob(t, srv, envCronJob("default", "otherref", secretRef("unrelated"), nil, false))
	createAndRenderCronJob(t, srv, envCronJob("demo", "otherns", secretRef("shared"), nil, false))

	used := srv.envFilePath("default", quadlet.CronJobEnvBaseName("used"))
	otherRef := srv.envFilePath("default", quadlet.CronJobEnvBaseName("otherref"))
	otherNS := srv.envFilePath("demo", quadlet.CronJobEnvBaseName("otherns"))
	otherRefOld := setOldMtime(t, otherRef)
	otherNSOld := setOldMtime(t, otherNS)

	sendMergePatch(t, srv, "secrets", "default", "shared", map[string]interface{}{
		"stringData": map[string]string{"TOKEN": "new"},
	})

	assertEnvFile(t, used, "TOKEN=new\n")
	assertEnvFile(t, otherRef, "TOKEN=old\n")
	assertEnvFile(t, otherNS, "TOKEN=old\n")
	assertFileMtime(t, otherRef, otherRefOld)
	assertFileMtime(t, otherNS, otherNSOld)
}

func TestSuspendedCronJobRefreshesEnvWithoutEnablingTimer(t *testing.T) {
	srv, _ := newEnvRefreshTestServer(t)
	createEnvTestSecret(t, srv.config.Store, "default", "creds", map[string]string{"TOKEN": "old"})
	cj := envCronJob("default", "susp", []corev1.EnvVar{{
		Name: "TOKEN",
		ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "creds"},
			Key:                  "TOKEN",
		}},
	}}, nil, true)
	createAndRenderCronJob(t, srv, cj)

	envPath := srv.envFilePath("default", quadlet.CronJobEnvBaseName("susp"))
	timerPath := filepath.Join(srv.config.QuadletDir, names.Host(names.KindCronJobTimer, "default", "susp"))
	timerOld := setOldMtime(t, timerPath)

	var starts, stops []string
	srv.startUnitHook = func(name string) { starts = append(starts, name) }
	srv.stopUnitHook = func(name string) { stops = append(stops, name) }

	sendMergePatch(t, srv, "secrets", "default", "creds", map[string]interface{}{
		"stringData": map[string]string{"TOKEN": "new"},
	})

	assertEnvFile(t, envPath, "TOKEN=new\n")
	assertFileMtime(t, timerPath, timerOld)
	if len(starts) != 0 || len(stops) != 0 {
		t.Fatalf("suspended CronJob refresh started or stopped units: starts=%v stops=%v", starts, stops)
	}
	stored, err := srv.config.Store.GetCronJob("default", "susp")
	if err != nil {
		t.Fatalf("GetCronJob: %v", err)
	}
	if stored.Spec.Suspend == nil || !*stored.Spec.Suspend {
		t.Fatal("Secret refresh changed the suspended CronJob's spec.suspend")
	}
}

func TestSecretUpdateDoesNotRewriteDeploymentEnvFile(t *testing.T) {
	srv, _ := newEnvRefreshTestServer(t)
	createEnvTestSecret(t, srv.config.Store, "default", "creds", map[string]string{"TOKEN": "old"})
	replicas := int32(1)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "web"}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:  "web",
					Image: "nginx:1",
					Env: []corev1.EnvVar{{
						Name: "TOKEN",
						ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: "creds"},
							Key:                  "TOKEN",
						}},
					}},
				}}},
			},
		},
	}
	if _, err := srv.config.Store.CreateDeployment(dep); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	srv.generateDeploymentQuadlets(dep)

	envPath := srv.envFilePath("default", "web-0")
	assertEnvFile(t, envPath, "TOKEN=old\n")
	old := setOldMtime(t, envPath)

	sendMergePatch(t, srv, "secrets", "default", "creds", map[string]interface{}{
		"stringData": map[string]string{"TOKEN": "new"},
	})

	assertEnvFile(t, envPath, "TOKEN=old\n")
	assertFileMtime(t, envPath, old)
}

func TestDeletingJobRemovesItsEnvFile(t *testing.T) {
	srv, _ := newEnvRefreshTestServer(t)
	createEnvTestSecret(t, srv.config.Store, "default", "creds", map[string]string{"TOKEN": "old"})
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "once", Namespace: "default"},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever,
				Containers: []corev1.Container{{
					Name:  "worker",
					Image: "busybox",
					Env: []corev1.EnvVar{{
						Name: "TOKEN",
						ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: "creds"},
							Key:                  "TOKEN",
						}},
					}},
				}},
			},
		}},
	}
	if _, err := srv.config.Store.CreateJob(job); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	srv.generateJobQuadlet(job)

	envPath := srv.envFilePath("default", quadlet.JobEnvBaseName("once"))
	assertEnvFile(t, envPath, "TOKEN=old\n")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/apis/batch/v1/namespaces/default/jobs/once", nil)
	srv.handleJobs(rec, req, "default", "once")
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE job = %d, body %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(envPath); !os.IsNotExist(err) {
		t.Fatalf("job env file still exists after delete, stat err=%v", err)
	}
}

func TestDeletingCronJobRemovesItsEnvFile(t *testing.T) {
	srv, _ := newEnvRefreshTestServer(t)
	createEnvTestSecret(t, srv.config.Store, "default", "creds", map[string]string{"TOKEN": "old"})
	createAndRenderCronJob(t, srv, envCronJob("default", "gone", []corev1.EnvVar{{
		Name: "TOKEN",
		ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "creds"},
			Key:                  "TOKEN",
		}},
	}}, nil, false))

	envPath := srv.envFilePath("default", quadlet.CronJobEnvBaseName("gone"))
	assertEnvFile(t, envPath, "TOKEN=old\n")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/apis/batch/v1/namespaces/default/cronjobs/gone", nil)
	srv.handleCronJobs(rec, req, "default", "gone")
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE cronjob = %d, body %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(envPath); !os.IsNotExist(err) {
		t.Fatalf("cronjob env file still exists after delete, stat err=%v", err)
	}
}
