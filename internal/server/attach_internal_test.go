package server

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"q8s/internal/store"
)

func TestExecPodmanArgs(t *testing.T) {
	args := execPodmanArgs("default-pod", []string{"nsenter", "--target", "1"}, true, "Q8S_EXEC_SESSION=abc")
	want := []string{"exec", "-i", "-e", "Q8S_EXEC_SESSION=abc", "-t", "default-pod", "nsenter", "--target", "1"}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("execPodmanArgs = %v, want %v", args, want)
	}
}

func TestExecPodmanArgsWithoutTTY(t *testing.T) {
	args := execPodmanArgs("default-pod", []string{"true"}, false, "Q8S_EXEC_SESSION=abc")
	want := []string{"exec", "-i", "-e", "Q8S_EXEC_SESSION=abc", "default-pod", "true"}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("execPodmanArgs = %v, want %v", args, want)
	}
}

func TestAttachPodmanArgs(t *testing.T) {
	args := attachPodmanArgs("default-pod", true)
	want := []string{"attach", "--sig-proxy=false", "default-pod"}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("attachPodmanArgs = %v, want %v", args, want)
	}
}

func TestAttachPodmanArgsWithoutStdin(t *testing.T) {
	args := attachPodmanArgs("default-pod", false)
	want := []string{"attach", "--sig-proxy=false", "--no-stdin", "default-pod"}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("attachPodmanArgs = %v, want %v", args, want)
	}
}

func TestNegotiateWSProtocol(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   string
	}{
		{name: "v5", header: "v5.channel.k8s.io", want: "v5.channel.k8s.io"},
		{name: "v4", header: "v4.channel.k8s.io", want: "v4.channel.k8s.io"},
		{name: "first supported wins", header: "v4.channel.k8s.io, v5.channel.k8s.io", want: "v4.channel.k8s.io"},
		{name: "unsupported", header: "v3.channel.k8s.io", want: ""},
		{name: "empty", header: "", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := negotiateWSProtocol(tc.header); got != tc.want {
				t.Errorf("negotiateWSProtocol(%q) = %q, want %q", tc.header, got, tc.want)
			}
		})
	}
}

func attachTestServer() *Server {
	return &Server{config: Config{Store: store.New()}}
}

// TestAttachSubresourceRoutes pins that pods/POD/attach reaches the attach
// handler instead of the generic unsupported-subresource 404.
func TestAttachSubresourceRoutes(t *testing.T) {
	s := attachTestServer()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/default/pods/example/attach", nil)
	rec := httptest.NewRecorder()

	s.handlePods(rec, req, "default", "example/attach")

	if rec.Code != http.StatusUpgradeRequired {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusUpgradeRequired, rec.Body.String())
	}
}

// TestAttachRequiresWebSocketUpgrade checks the same transport guard as exec.
func TestAttachRequiresWebSocketUpgrade(t *testing.T) {
	s := attachTestServer()

	for _, tc := range []struct {
		name   string
		method string
		want   int
	}{
		{name: "GET without upgrade", method: http.MethodGet, want: http.StatusUpgradeRequired},
		{name: "POST", method: http.MethodPost, want: http.StatusMethodNotAllowed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/api/v1/namespaces/default/pods/example/attach", nil)
			rec := httptest.NewRecorder()
			s.handlePodAttach(rec, req, "default", "example")
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d; body: %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}
