package server

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"q8s/internal/store"
)

func TestContainerIDFromCgroup(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cases := []struct {
		name    string
		content string
		want    string
		ok      bool
	}{
		{
			name:    "rootless scope path",
			content: "0::/user.slice/user-1000.slice/user@1000.service/user.slice/libpod-" + id + ".scope\n",
			want:    id,
			ok:      true,
		},
		{
			name:    "quadlet split payload path",
			content: "0::/system.slice/demo-web.service/libpod-payload-" + id + "\n",
			want:    id,
			ok:      true,
		},
		{
			name: "cgroup v1 multi line",
			content: "12:pids:/user.slice/user-1000.slice/user@1000.service/libpod-" + id + ".scope\n" +
				"4:cpu,cpuacct:/user.slice/user-1000.slice/user@1000.service/libpod-" + id + ".scope\n" +
				"1:name=systemd:/user.slice/user-1000.slice/user@1000.service/libpod-" + id + ".scope\n",
			want: id,
			ok:   true,
		},
		{
			name:    "host process",
			content: "0::/user.slice/user-1000.slice/session-3.scope\n",
			ok:      false,
		},
		{
			name:    "short id",
			content: "0::/system.slice/libpod-0123456789abcdef.scope\n",
			ok:      false,
		},
		{
			name:    "non hex id",
			content: "0::/system.slice/libpod-zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz.scope\n",
			ok:      false,
		},
		{
			name:    "payload marker wins over the libpod prefix",
			content: "0::/system.slice/x.service/libpod-payload-" + id + "\n",
			want:    id,
			ok:      true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := containerIDFromCgroup(tc.content)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("containerIDFromCgroup(%q) = %q, %v; want %q, %v", tc.content, got, ok, tc.want, tc.ok)
			}
		})
	}
}

type fakePeerResolver struct {
	labels map[string]string
	err    error
}

func (f fakePeerResolver) LabelsForPID(int) (map[string]string, error) {
	return f.labels, f.err
}

func workloadTestServer(t *testing.T, st *store.Store, resolver ContainerLabelResolver) (*Server, *testCA) {
	t.Helper()
	ca := newTestCA(t)
	serverCert := ca.issue(t, "server", nil)
	certBytes, keyBytes := certPEM(t, serverCert)
	dataDir := t.TempDir()
	certDir := filepath.Join(dataDir, "certs")
	if err := os.MkdirAll(certDir, 0755); err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(filepath.Join(certDir, "ca.crt"), ca.pem, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(certDir, "ca.key"), keyPEM, 0600); err != nil {
		t.Fatal(err)
	}
	srv, err := New(Config{
		Store:         st,
		CACert:        ca.pem,
		CertPEM:       certBytes,
		KeyPEM:        keyBytes,
		DataDir:       dataDir,
		WorkloadPeers: resolver,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv, ca
}

func addWorkloadPod(t *testing.T, st *store.Store, ns, name, serviceAccount string, automount *bool) {
	t.Helper()
	_, err := st.CreatePod(&corev1.Pod{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PodSpec{
			ServiceAccountName:           serviceAccount,
			AutomountServiceAccountToken: automount,
			Containers:                   []corev1.Container{{Name: "app", Image: "example.com/app:latest"}},
		},
	})
	if err != nil {
		t.Fatalf("create pod %s/%s: %v", ns, name, err)
	}
}

func addWorkloadServiceAccount(t *testing.T, st *store.Store, ns, name string) {
	t.Helper()
	_, err := st.CreateServiceAccount(&corev1.ServiceAccount{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
	})
	if err != nil {
		t.Fatalf("create serviceaccount %s/%s: %v", ns, name, err)
	}
}

func decodeExecCredential(t *testing.T, data []byte) (execCredential, map[string]interface{}) {
	t.Helper()
	var cred execCredential
	if err := json.Unmarshal(data, &cred); err != nil {
		t.Fatalf("decode ExecCredential: %v\n%s", err, data)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("decode raw ExecCredential: %v", err)
	}
	return cred, raw
}

func TestIssueServiceAccountCredentialThroughStore(t *testing.T) {
	st := store.New()
	addWorkloadPod(t, st, "demo", "web", "worker", nil)
	addWorkloadServiceAccount(t, st, "demo", "worker")
	srv, ca := workloadTestServer(t, st, fakePeerResolver{labels: map[string]string{
		"io.kubernetes.pod.namespace": "demo",
		"io.kubernetes.pod.name":      "web",
	}})

	before := time.Now().Truncate(time.Second)
	data, err := srv.credentialForLabels(map[string]string{
		"io.kubernetes.pod.namespace": "demo",
		"io.kubernetes.pod.name":      "web",
	})
	if err != nil {
		t.Fatalf("credentialForLabels: %v", err)
	}
	cred, raw := decodeExecCredential(t, data)

	if cred.APIVersion != "client.authentication.k8s.io/v1" || cred.Kind != "ExecCredential" {
		t.Fatalf("ExecCredential envelope = %q %q", cred.APIVersion, cred.Kind)
	}
	if len(raw) != 3 || raw["apiVersion"] == nil || raw["kind"] == nil || raw["status"] == nil {
		t.Fatalf("unexpected top-level ExecCredential fields: %v", raw)
	}
	status, _ := raw["status"].(map[string]interface{})
	if len(status) != 3 {
		t.Fatalf("unexpected status fields: %v", status)
	}
	for _, key := range []string{"clientCertificateData", "clientKeyData", "expirationTimestamp"} {
		if _, ok := status[key]; !ok {
			t.Fatalf("status is missing %q: %v", key, status)
		}
	}

	block, _ := pem.Decode([]byte(cred.Status.ClientCertificateData))
	if block == nil {
		t.Fatal("clientCertificateData is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	if cert.Subject.CommonName != "system:serviceaccount:demo:worker" {
		t.Errorf("CN = %q", cert.Subject.CommonName)
	}
	wantGroups := []string{"system:serviceaccounts", "system:serviceaccounts:demo"}
	if len(cert.Subject.Organization) != len(wantGroups) {
		t.Fatalf("O = %v, want %v", cert.Subject.Organization, wantGroups)
	}
	for i := range wantGroups {
		if cert.Subject.Organization[i] != wantGroups[i] {
			t.Errorf("O[%d] = %q, want %q", i, cert.Subject.Organization[i], wantGroups[i])
		}
	}
	if len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Errorf("ExtKeyUsage = %v, want [ClientAuth]", cert.ExtKeyUsage)
	}
	if cert.NotBefore.After(before) || cert.NotBefore.Before(before.Add(-2*time.Minute)) {
		t.Errorf("NotBefore = %v, want about one minute before %v", cert.NotBefore, before)
	}
	wantExpiry := before.Add(time.Hour)
	if diff := cert.NotAfter.Sub(wantExpiry); diff < -2*time.Second || diff > 2*time.Second {
		t.Errorf("NotAfter = %v, want about %v", cert.NotAfter, wantExpiry)
	}
	if got := cred.Status.ExpirationTimestamp; got != cert.NotAfter.UTC().Format(time.RFC3339) {
		t.Errorf("expirationTimestamp = %q, certificate ends at %q", got, cert.NotAfter.UTC().Format(time.RFC3339))
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.pem) {
		t.Fatal("append CA")
	}
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Fatalf("certificate does not verify against the CA: %v", err)
	}

	keyBlock, _ := pem.Decode([]byte(cred.Status.ClientKeyData))
	if keyBlock == nil {
		t.Fatal("clientKeyData is not PEM")
	}
	if _, err := x509.ParseECPrivateKey(keyBlock.Bytes); err != nil {
		t.Fatalf("parse client key: %v", err)
	}
}

func TestIssueServiceAccountCredentialRefusals(t *testing.T) {
	cases := []struct {
		name      string
		automount *bool
		podSA     string
		makeSA    bool
		labels    map[string]string
	}{
		{
			name:   "pod without serviceAccountName",
			podSA:  "",
			labels: map[string]string{"io.kubernetes.pod.namespace": "demo", "io.kubernetes.pod.name": "web"},
		},
		{
			name:      "automount false",
			podSA:     "worker",
			makeSA:    true,
			automount: boolPtr(false),
			labels:    map[string]string{"io.kubernetes.pod.namespace": "demo", "io.kubernetes.pod.name": "web"},
		},
		{
			name:   "missing serviceaccount",
			podSA:  "worker",
			makeSA: false,
			labels: map[string]string{"io.kubernetes.pod.namespace": "demo", "io.kubernetes.pod.name": "web"},
		},
		{
			name:   "unknown container",
			podSA:  "worker",
			makeSA: true,
			labels: map[string]string{"io.kubernetes.pod.namespace": "demo", "io.kubernetes.pod.name": "does-not-exist"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := store.New()
			addWorkloadPod(t, st, "demo", "web", tc.podSA, tc.automount)
			if tc.makeSA {
				addWorkloadServiceAccount(t, st, "demo", "worker")
			}
			srv, _ := workloadTestServer(t, st, fakePeerResolver{labels: tc.labels})
			data, err := srv.credentialForLabels(tc.labels)
			if err == nil {
				t.Fatalf("expected an error, got credential %s", data)
			}
			if len(data) != 0 {
				t.Fatalf("error path returned a credential: %s", data)
			}
		})
	}
}

func TestWorkloadSocketRoundTrip(t *testing.T) {
	st := store.New()
	addWorkloadPod(t, st, "demo", "web", "worker", nil)
	addWorkloadServiceAccount(t, st, "demo", "worker")
	srv, ca := workloadTestServer(t, st, fakePeerResolver{labels: map[string]string{
		"io.kubernetes.pod.namespace": "demo",
		"io.kubernetes.pod.name":      "web",
	}})

	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ServeWorkloadSocket(ctx, dir) }()

	socketPath := filepath.Join(dir, workloadSocketName)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(socketPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("workload socket %s did not appear", socketPath)
		}
		time.Sleep(10 * time.Millisecond)
	}

	data, err := FetchWorkloadCredential(socketPath)
	if err != nil {
		t.Fatalf("FetchWorkloadCredential: %v", err)
	}
	cred, _ := decodeExecCredential(t, data)
	if cred.Kind != "ExecCredential" || cred.Status.ClientCertificateData == "" {
		t.Fatalf("round-trip credential = %+v", cred)
	}
	block, _ := pem.Decode([]byte(cred.Status.ClientCertificateData))
	if block == nil {
		t.Fatal("round-trip certificate is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if cert.Subject.CommonName != "system:serviceaccount:demo:worker" {
		t.Fatalf("CN = %q", cert.Subject.CommonName)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.pem) {
		t.Fatal("append CA")
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatalf("round-trip certificate does not verify: %v", err)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("ServeWorkloadSocket returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ServeWorkloadSocket did not stop after context cancellation")
	}
	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Fatalf("socket %s survived shutdown: %v", socketPath, err)
	}
}

func TestWorkloadSocketRefusesHostPeer(t *testing.T) {
	st := store.New()
	srv, _ := workloadTestServer(t, st, fakePeerResolver{labels: map[string]string{
		"io.kubernetes.pod.namespace": "",
		"io.kubernetes.pod.name":      "",
	}})
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.ServeWorkloadSocket(ctx, dir)
	socketPath := filepath.Join(dir, workloadSocketName)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(socketPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("workload socket %s did not appear", socketPath)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if data, err := FetchWorkloadCredential(socketPath); err == nil {
		t.Fatalf("host peer received a credential: %s", data)
	}
}
