package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"net"
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestWorkloadKubeconfigFor(t *testing.T) {
	ca := []byte("test-ca-pem")
	data, err := workloadKubeconfigFor(6443, ca)
	if err != nil {
		t.Fatalf("workloadKubeconfigFor: %v", err)
	}
	var doc struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Clusters   []struct {
			Name    string `json:"name"`
			Cluster struct {
				CertificateAuthorityData string `json:"certificate-authority-data"`
				Server                   string `json:"server"`
				TLSServerName            string `json:"tls-server-name"`
			} `json:"cluster"`
		} `json:"clusters"`
		Contexts []struct {
			Name    string `json:"name"`
			Context struct {
				Cluster string `json:"cluster"`
				User    string `json:"user"`
			} `json:"context"`
		} `json:"contexts"`
		CurrentContext string `json:"current-context"`
		Users          []struct {
			Name string `json:"name"`
			User struct {
				Exec struct {
					APIVersion      string   `json:"apiVersion"`
					Command         string   `json:"command"`
					Args            []string `json:"args"`
					InteractiveMode string   `json:"interactiveMode"`
				} `json:"exec"`
			} `json:"user"`
		} `json:"users"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse generated kubeconfig: %v\n%s", err, data)
	}
	if doc.APIVersion != "v1" || doc.Kind != "Config" {
		t.Fatalf("kubeconfig envelope = %q %q", doc.APIVersion, doc.Kind)
	}
	if len(doc.Clusters) != 1 || len(doc.Contexts) != 1 || len(doc.Users) != 1 {
		t.Fatalf("kubeconfig sections = %d clusters, %d contexts, %d users", len(doc.Clusters), len(doc.Contexts), len(doc.Users))
	}
	cluster := doc.Clusters[0].Cluster
	if cluster.Server != "https://host.containers.internal:6443" {
		t.Errorf("server = %q", cluster.Server)
	}
	if cluster.TLSServerName != "localhost" {
		t.Errorf("tls-server-name = %q", cluster.TLSServerName)
	}
	if cluster.CertificateAuthorityData != base64.StdEncoding.EncodeToString(ca) {
		t.Errorf("certificate-authority-data = %q", cluster.CertificateAuthorityData)
	}
	if doc.CurrentContext != doc.Contexts[0].Name || doc.Contexts[0].Context.Cluster != doc.Clusters[0].Name {
		t.Errorf("context wiring = %+v current %q", doc.Contexts[0], doc.CurrentContext)
	}
	user := doc.Users[0]
	if user.Name != doc.Contexts[0].Context.User {
		t.Errorf("user name %q does not match context user %q", user.Name, doc.Contexts[0].Context.User)
	}
	exec := user.User.Exec
	if exec.APIVersion != "client.authentication.k8s.io/v1" {
		t.Errorf("exec.apiVersion = %q", exec.APIVersion)
	}
	if exec.Command != "/var/run/q8s/bin/q8s" {
		t.Errorf("exec.command = %q", exec.Command)
	}
	if len(exec.Args) != 1 || exec.Args[0] != "credential" {
		t.Errorf("exec.args = %v", exec.Args)
	}
	if exec.InteractiveMode != "Never" {
		t.Errorf("exec.interactiveMode = %q", exec.InteractiveMode)
	}
}

func TestWriteWorkloadFiles(t *testing.T) {
	dir := t.TempDir()
	ca := []byte("test-ca-pem")
	if err := writeWorkloadFiles(dir, filepath.Join(dir, "bin"), 6443, ca); err != nil {
		t.Fatalf("writeWorkloadFiles: %v", err)
	}
	binaryInfo, err := os.Stat(filepath.Join(dir, "bin", "q8s"))
	if err != nil {
		t.Fatalf("binary copy: %v", err)
	}
	if binaryInfo.Mode().Perm() != 0755 {
		t.Errorf("binary mode = %v, want 0755", binaryInfo.Mode().Perm())
	}
	kubeconfigInfo, err := os.Stat(filepath.Join(dir, "kubeconfig"))
	if err != nil {
		t.Fatalf("kubeconfig: %v", err)
	}
	if kubeconfigInfo.Mode().Perm() != 0644 {
		t.Errorf("kubeconfig mode = %v, want 0644", kubeconfigInfo.Mode().Perm())
	}
	data, err := os.ReadFile(filepath.Join(dir, "kubeconfig"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("tls-server-name: localhost")) || !bytes.Contains(data, []byte("command: /var/run/q8s/bin/q8s")) {
		t.Fatalf("generated kubeconfig is missing the workload stanza:\n%s", data)
	}
}

func TestWriteFileAtomicReplacesContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	if err := writeFileAtomic(path, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new" {
		t.Fatalf("content = %q", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("temporary files were left behind: %v", entries)
	}
}

func TestRunCredentialReadsSocket(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "sa.sock")
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	const response = `{"apiVersion":"client.authentication.k8s.io/v1","kind":"ExecCredential","status":{"clientCertificateData":"cert","clientKeyData":"key","expirationTimestamp":"2030-01-01T00:00:00Z"}}`
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if line, err := bufio.NewReader(conn).ReadString('\n'); err != nil || line != "GET /credential\n" {
			return
		}
		conn.Write([]byte(response + "\n"))
	}()

	t.Setenv("Q8S_WORKLOAD_SOCKET", socketPath)
	var stdout, stderr bytes.Buffer
	if err := runCredential(nil, &stdout, &stderr); err != nil {
		t.Fatalf("runCredential: %v (stderr %q)", err, stderr.String())
	}
	if stdout.String() != response+"\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestRunCredentialReportsRefusal(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "sa.sock")
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		bufio.NewReader(conn).ReadString('\n')
		conn.Write([]byte("error: pod does not request a service account\n"))
	}()

	t.Setenv("Q8S_WORKLOAD_SOCKET", socketPath)
	var stdout, stderr bytes.Buffer
	err = runCredential(nil, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected an error for a refused credential")
	}
	if stdout.Len() != 0 {
		t.Fatalf("refusal wrote to stdout: %q", stdout.String())
	}
	if !bytes.Contains([]byte(err.Error()), []byte("does not request a service account")) {
		t.Fatalf("error = %q", err)
	}
}
