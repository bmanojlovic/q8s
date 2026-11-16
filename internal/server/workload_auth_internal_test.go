package server

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"q8s/internal/store"
)

func startWorkloadHTTPServer(t *testing.T, srv *Server, ca *testCA) *httptest.Server {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.pem) {
		t.Fatal("append CA")
	}
	ts := httptest.NewUnstartedServer(srv)
	ts.TLS = &tls.Config{
		ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs:  pool,
		MinVersion: tls.VersionTLS12,
	}
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts
}

func credentialHTTPClient(t *testing.T, data []byte) *http.Client {
	t.Helper()
	var cred execCredential
	if err := json.Unmarshal(data, &cred); err != nil {
		t.Fatalf("decode ExecCredential: %v", err)
	}
	cert, err := tls.X509KeyPair([]byte(cred.Status.ClientCertificateData), []byte(cred.Status.ClientKeyData))
	if err != nil {
		t.Fatalf("load client certificate: %v", err)
	}
	return tlsClient(cert)
}

func credentialCertificate(t *testing.T, data []byte) *x509.Certificate {
	t.Helper()
	var cred execCredential
	if err := json.Unmarshal(data, &cred); err != nil {
		t.Fatalf("decode ExecCredential: %v", err)
	}
	block, _ := pem.Decode([]byte(cred.Status.ClientCertificateData))
	if block == nil {
		t.Fatal("clientCertificateData is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse credential certificate: %v", err)
	}
	return cert
}

func versionStatus(t *testing.T, client *http.Client, baseURL string) int {
	t.Helper()
	resp, err := client.Get(baseURL + "/version")
	if err != nil {
		t.Fatalf("GET /version: %v", err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func addWorkloadPodObject(t *testing.T, st *store.Store, ns, name, serviceAccount string) *corev1.Pod {
	t.Helper()
	pod, err := st.CreatePod(&corev1.Pod{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PodSpec{
			ServiceAccountName: serviceAccount,
			Containers:         []corev1.Container{{Name: "app", Image: "example.com/app:latest"}},
		},
	})
	if err != nil {
		t.Fatalf("create pod %s/%s: %v", ns, name, err)
	}
	return pod
}

func addWorkloadJob(t *testing.T, st *store.Store, ns, name, serviceAccount string) *batchv1.Job {
	t.Helper()
	job, err := st.CreateJob(&batchv1.Job{
		TypeMeta:   metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					ServiceAccountName: serviceAccount,
					Containers:         []corev1.Container{{Name: "app", Image: "example.com/app:latest"}},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("create job %s/%s: %v", ns, name, err)
	}
	return job
}

func addWorkloadCronJob(t *testing.T, st *store.Store, ns, name, serviceAccount string) *batchv1.CronJob {
	t.Helper()
	cj, err := st.CreateCronJob(&batchv1.CronJob{
		TypeMeta:   metav1.TypeMeta{APIVersion: "batch/v1", Kind: "CronJob"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 * * * *",
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							ServiceAccountName: serviceAccount,
							Containers:         []corev1.Container{{Name: "app", Image: "example.com/app:latest"}},
						},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("create cronjob %s/%s: %v", ns, name, err)
	}
	return cj
}

func workloadPodLabels(ns, name string) map[string]string {
	return map[string]string{
		"io.kubernetes.pod.namespace": ns,
		"io.kubernetes.pod.name":      name,
	}
}

func workloadJobLabels(ns, name string) map[string]string {
	return map[string]string{
		"io.kubernetes.pod.namespace": ns,
		"io.kubernetes.job.name":      name,
	}
}

func workloadCronJobLabels(ns, name string) map[string]string {
	return map[string]string{
		"io.kubernetes.pod.namespace": ns,
		"io.kubernetes.cronjob.name":  name,
	}
}

type workloadAuthFixture struct {
	st     *store.Store
	srv    *Server
	ca     *testCA
	ts     *httptest.Server
	data   []byte
	client *http.Client
}

func newWorkloadAuthFixture(t *testing.T) *workloadAuthFixture {
	t.Helper()
	st := store.New()
	addWorkloadPodObject(t, st, "demo", "web", "worker")
	addWorkloadServiceAccount(t, st, "demo", "worker")
	srv, ca := workloadTestServer(t, st, nil)
	ts := startWorkloadHTTPServer(t, srv, ca)
	data, err := srv.credentialForLabels(workloadPodLabels("demo", "web"))
	if err != nil {
		t.Fatalf("credentialForLabels: %v", err)
	}
	return &workloadAuthFixture{
		st:     st,
		srv:    srv,
		ca:     ca,
		ts:     ts,
		data:   data,
		client: credentialHTTPClient(t, data),
	}
}

func TestServiceAccountCertificateRevalidatedPerRequest(t *testing.T) {
	t.Run("live pod and serviceaccount", func(t *testing.T) {
		f := newWorkloadAuthFixture(t)
		if got := versionStatus(t, f.client, f.ts.URL); got != http.StatusOK {
			t.Fatalf("GET /version with a live pod = %d, want %d", got, http.StatusOK)
		}
	})

	t.Run("deleted pod", func(t *testing.T) {
		f := newWorkloadAuthFixture(t)
		if err := f.st.DeletePod("demo", "web"); err != nil {
			t.Fatalf("delete pod: %v", err)
		}
		if got := versionStatus(t, f.client, f.ts.URL); got != http.StatusUnauthorized {
			t.Fatalf("GET /version after deleting the pod = %d, want %d", got, http.StatusUnauthorized)
		}
	})

	t.Run("deleted serviceaccount", func(t *testing.T) {
		f := newWorkloadAuthFixture(t)
		if err := f.st.DeleteServiceAccount("demo", "worker"); err != nil {
			t.Fatalf("delete serviceaccount: %v", err)
		}
		if got := versionStatus(t, f.client, f.ts.URL); got != http.StatusUnauthorized {
			t.Fatalf("GET /version after deleting the serviceaccount = %d, want %d", got, http.StatusUnauthorized)
		}
	})

	t.Run("changed serviceAccountName", func(t *testing.T) {
		f := newWorkloadAuthFixture(t)
		addWorkloadServiceAccount(t, f.st, "demo", "other")
		pod, err := f.st.GetPod("demo", "web")
		if err != nil {
			t.Fatal(err)
		}
		pod.Spec.ServiceAccountName = "other"
		if _, err := f.st.UpdatePod(pod); err != nil {
			t.Fatalf("update pod: %v", err)
		}
		if got := versionStatus(t, f.client, f.ts.URL); got != http.StatusUnauthorized {
			t.Fatalf("GET /version after changing serviceAccountName = %d, want %d", got, http.StatusUnauthorized)
		}
	})

	t.Run("certificate without workload URI", func(t *testing.T) {
		f := newWorkloadAuthFixture(t)
		client := tlsClient(f.ca.issue(t, "system:serviceaccount:demo:worker", []string{"system:serviceaccounts", "system:serviceaccounts:demo"}))
		if got := versionStatus(t, client, f.ts.URL); got != http.StatusUnauthorized {
			t.Fatalf("GET /version with an unbound service account certificate = %d, want %d", got, http.StatusUnauthorized)
		}
	})
}

func TestServiceAccountCertificateRevokedWhenPodRecreatedWithNewUID(t *testing.T) {
	st := store.New()
	addWorkloadServiceAccount(t, st, "demo", "worker")
	first := addWorkloadPodObject(t, st, "demo", "web", "worker")
	srv, ca := workloadTestServer(t, st, nil)
	ts := startWorkloadHTTPServer(t, srv, ca)

	data, err := srv.credentialForLabels(workloadPodLabels("demo", "web"))
	if err != nil {
		t.Fatalf("credentialForLabels: %v", err)
	}
	cert := credentialCertificate(t, data)
	wantURI := "urn:q8s:workload:pod:demo/web/" + string(first.UID)
	if len(cert.URIs) != 1 || cert.URIs[0].String() != wantURI {
		t.Fatalf("certificate URIs = %v, want %q", cert.URIs, wantURI)
	}
	client := credentialHTTPClient(t, data)
	if got := versionStatus(t, client, ts.URL); got != http.StatusOK {
		t.Fatalf("GET /version before recreating the pod = %d, want %d", got, http.StatusOK)
	}

	if err := st.DeletePod("demo", "web"); err != nil {
		t.Fatalf("delete pod: %v", err)
	}
	recreated := addWorkloadPodObject(t, st, "demo", "web", "worker")
	if recreated.UID == "" {
		t.Fatal("recreated pod has no UID")
	}
	if recreated.UID == first.UID {
		t.Fatalf("recreated pod reused the old UID %q", recreated.UID)
	}
	if got := versionStatus(t, client, ts.URL); got != http.StatusUnauthorized {
		t.Fatalf("GET /version with the old certificate after recreating the pod = %d, want %d", got, http.StatusUnauthorized)
	}
}

func TestJobAndCronJobServiceAccountCredentials(t *testing.T) {
	t.Run("job", func(t *testing.T) {
		st := store.New()
		addWorkloadServiceAccount(t, st, "demo", "worker")
		job := addWorkloadJob(t, st, "demo", "nightly", "worker")
		srv, ca := workloadTestServer(t, st, nil)
		ts := startWorkloadHTTPServer(t, srv, ca)

		data, err := srv.credentialForLabels(workloadJobLabels("demo", "nightly"))
		if err != nil {
			t.Fatalf("credentialForLabels: %v", err)
		}
		cert := credentialCertificate(t, data)
		if cert.Subject.CommonName != "system:serviceaccount:demo:worker" {
			t.Fatalf("CN = %q", cert.Subject.CommonName)
		}
		wantURI := "urn:q8s:workload:job:demo/nightly/" + string(job.UID)
		if len(cert.URIs) != 1 || cert.URIs[0].String() != wantURI {
			t.Fatalf("certificate URIs = %v, want %q", cert.URIs, wantURI)
		}
		client := credentialHTTPClient(t, data)
		if got := versionStatus(t, client, ts.URL); got != http.StatusOK {
			t.Fatalf("GET /version with a live job = %d, want %d", got, http.StatusOK)
		}
		if err := st.DeleteJob("demo", "nightly"); err != nil {
			t.Fatalf("delete job: %v", err)
		}
		if got := versionStatus(t, client, ts.URL); got != http.StatusUnauthorized {
			t.Fatalf("GET /version after deleting the job = %d, want %d", got, http.StatusUnauthorized)
		}
	})

	t.Run("cronjob", func(t *testing.T) {
		st := store.New()
		addWorkloadServiceAccount(t, st, "demo", "worker")
		cj := addWorkloadCronJob(t, st, "demo", "nightly", "worker")
		srv, ca := workloadTestServer(t, st, nil)
		ts := startWorkloadHTTPServer(t, srv, ca)

		data, err := srv.credentialForLabels(workloadCronJobLabels("demo", "nightly"))
		if err != nil {
			t.Fatalf("credentialForLabels: %v", err)
		}
		cert := credentialCertificate(t, data)
		if cert.Subject.CommonName != "system:serviceaccount:demo:worker" {
			t.Fatalf("CN = %q", cert.Subject.CommonName)
		}
		wantURI := "urn:q8s:workload:cronjob:demo/nightly/" + string(cj.UID)
		if len(cert.URIs) != 1 || cert.URIs[0].String() != wantURI {
			t.Fatalf("certificate URIs = %v, want %q", cert.URIs, wantURI)
		}
		client := credentialHTTPClient(t, data)
		if got := versionStatus(t, client, ts.URL); got != http.StatusOK {
			t.Fatalf("GET /version with a live cronjob = %d, want %d", got, http.StatusOK)
		}
		if err := st.DeleteCronJob("demo", "nightly"); err != nil {
			t.Fatalf("delete cronjob: %v", err)
		}
		if got := versionStatus(t, client, ts.URL); got != http.StatusUnauthorized {
			t.Fatalf("GET /version after deleting the cronjob = %d, want %d", got, http.StatusUnauthorized)
		}
	})
}

func TestJobWithoutServiceAccountCredentialRefused(t *testing.T) {
	st := store.New()
	addWorkloadJob(t, st, "demo", "nightly", "")
	srv, _ := workloadTestServer(t, st, nil)
	data, err := srv.credentialForLabels(workloadJobLabels("demo", "nightly"))
	if err == nil {
		t.Fatalf("job without serviceAccountName received a credential: %s", data)
	}
}
