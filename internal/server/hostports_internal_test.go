package server

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"q8s/internal/quadlet"
	"q8s/internal/store"
)

func hostPortTestServer(t *testing.T) (*Server, *httptest.Server, *store.Store) {
	t.Helper()
	st := store.New()
	srv, ts, _ := newNetTestServer(t, st, t.TempDir(), t.TempDir())
	return srv, ts, st
}

func hostPortPodBody(ns, name string, annotations map[string]string, ports []interface{}) map[string]interface{} {
	meta := map[string]interface{}{"name": name, "namespace": ns}
	if len(annotations) > 0 {
		meta["annotations"] = annotations
	}
	container := map[string]interface{}{"name": name, "image": "nginx"}
	if len(ports) > 0 {
		container["ports"] = ports
	}
	return map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   meta,
		"spec": map[string]interface{}{
			"containers": []interface{}{container},
		},
	}
}

func hostPortDeploymentBody(ns, name, image string, replicas int, annotations map[string]string) map[string]interface{} {
	tmplMeta := map[string]interface{}{"labels": map[string]interface{}{"app": name}}
	if len(annotations) > 0 {
		tmplMeta["annotations"] = annotations
	}
	return map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]interface{}{"name": name, "namespace": ns},
		"spec": map[string]interface{}{
			"replicas": replicas,
			"selector": map[string]interface{}{"matchLabels": map[string]interface{}{"app": name}},
			"template": map[string]interface{}{
				"metadata": tmplMeta,
				"spec": map[string]interface{}{
					"containers": []interface{}{map[string]interface{}{"name": name, "image": image}},
				},
			},
		},
	}
}

func hostPortNodePortServiceBody(ns, name string, port, nodePort int) map[string]interface{} {
	svcPort := map[string]interface{}{"port": port, "targetPort": port, "protocol": "TCP"}
	if nodePort != 0 {
		svcPort["nodePort"] = nodePort
	}
	return map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Service",
		"metadata":   map[string]interface{}{"name": name, "namespace": ns},
		"spec": map[string]interface{}{
			"type":     "NodePort",
			"selector": map[string]interface{}{"app": name},
			"ports":    []interface{}{svcPort},
		},
	}
}

// hostPortRequireStatus reads and closes the response, failing with the body
// when the status is not the expected one.
func hostPortRequireStatus(t *testing.T, resp *http.Response, want int) string {
	t.Helper()
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("status = %d, want %d: %s", resp.StatusCode, want, body)
	}
	return string(body)
}

func TestHostPortClaimsOverlap(t *testing.T) {
	cases := []struct {
		name string
		a    hostPortClaim
		b    hostPortClaim
		want bool
	}{
		{
			name: "all addresses overlaps a specific IPv4 address",
			a:    hostPortClaim{Port: 443, Addr: "", Proto: "TCP"},
			b:    hostPortClaim{Port: 443, Addr: "192.0.2.10", Proto: "TCP"},
			want: true,
		},
		{
			name: "two different specific IPv4 addresses do not overlap",
			a:    hostPortClaim{Port: 443, Addr: "192.0.2.10", Proto: "TCP"},
			b:    hostPortClaim{Port: 443, Addr: "192.0.2.11", Proto: "TCP"},
			want: false,
		},
		{
			name: "TCP and UDP do not overlap",
			a:    hostPortClaim{Port: 443, Addr: "", Proto: "TCP"},
			b:    hostPortClaim{Port: 443, Addr: "", Proto: "UDP"},
			want: false,
		},
		{
			name: "equal IPv6 literals overlap",
			a:    hostPortClaim{Port: 8443, Addr: "2001:db8::1", Proto: "TCP"},
			b:    hostPortClaim{Port: 8443, Addr: "2001:db8::1", Proto: "TCP"},
			want: true,
		},
		{
			name: "all addresses overlaps an IPv6 literal",
			a:    hostPortClaim{Port: 8443, Addr: "", Proto: "TCP"},
			b:    hostPortClaim{Port: 8443, Addr: "2001:db8::1", Proto: "TCP"},
			want: true,
		},
		{
			name: "different ports do not overlap",
			a:    hostPortClaim{Port: 8443, Addr: "", Proto: "TCP"},
			b:    hostPortClaim{Port: 8444, Addr: "", Proto: "TCP"},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hostPortClaimsOverlap(tc.a, tc.b); got != tc.want {
				t.Errorf("hostPortClaimsOverlap(%+v, %+v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestHostPortClaimsNormalizeAddresses(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:  "web",
			Image: "nginx",
			Ports: []corev1.ContainerPort{
				{ContainerPort: 8080, HostPort: 8080, HostIP: "0.0.0.0"},
				{ContainerPort: 8081, HostPort: 8081, HostIP: "::"},
				{ContainerPort: 8082, HostPort: 8082, HostIP: "2001:db8::1"},
			},
		}}},
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
			quadlet.AnnotationSocketActivation: "web=[2001:db8::1]:8443",
		}},
	}
	claims := hostPortClaimsForWorkload("Pod default/web", &pod.Spec, pod.Annotations)
	byPort := make(map[int32]hostPortClaim, len(claims))
	for _, claim := range claims {
		byPort[claim.Port] = claim
	}
	if got := byPort[8443]; got.Addr != "2001:db8::1" || got.Proto != "TCP" || got.Kind != "socket" {
		t.Errorf("socket claim = %+v, want IPv6 address, TCP and kind socket", got)
	}
	if got := byPort[8080]; got.Addr != "" {
		t.Errorf("0.0.0.0 hostIP normalized to %q, want empty (all addresses)", got.Addr)
	}
	if got := byPort[8081]; got.Addr != "" {
		t.Errorf(":: hostIP normalized to %q, want empty (all addresses)", got.Addr)
	}
	if got := byPort[8082]; got.Addr != "2001:db8::1" {
		t.Errorf("hostIP = %q, want the canonical IPv6 literal", got.Addr)
	}
}

func TestSocketCollisionBetweenPods(t *testing.T) {
	cases := []struct {
		name        string
		annotations map[string]string
		ports       []interface{}
		wantStatus  int
		wantMessage string
	}{
		{
			name:        "socket all addresses collides",
			annotations: map[string]string{quadlet.AnnotationSocketActivation: "web=8080"},
			wantStatus:  http.StatusConflict,
			wantMessage: "socket port 8080/TCP collides with socket port of Pod a/traefik",
		},
		{
			name:        "socket loopback address collides with all addresses",
			annotations: map[string]string{quadlet.AnnotationSocketActivation: "web=127.0.0.1:8080"},
			wantStatus:  http.StatusConflict,
			wantMessage: "socket port 8080/TCP collides with socket port of Pod a/traefik",
		},
		{
			name: "hostPort collides with socket",
			ports: []interface{}{map[string]interface{}{
				"containerPort": 8080, "hostPort": 8080, "protocol": "TCP",
			}},
			wantStatus:  http.StatusConflict,
			wantMessage: "hostPort 8080/TCP collides with socket port of Pod a/traefik",
		},
		{
			name: "hostPort UDP does not collide with a TCP socket",
			ports: []interface{}{map[string]interface{}{
				"containerPort": 8080, "hostPort": 8080, "protocol": "UDP",
			}},
			wantStatus: http.StatusCreated,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ts, _ := hostPortTestServer(t)
			body := hostPortPodBody("a", "traefik", map[string]string{
				quadlet.AnnotationSocketActivation: "web=8080",
			}, nil)
			hostPortRequireStatus(t, netTestRequest(t, http.MethodPost, ts.URL+"/api/v1/namespaces/a/pods", body), http.StatusCreated)
			netTestCreateNamespace(t, ts, "b")

			body = hostPortPodBody("b", "other", tc.annotations, tc.ports)
			resp := netTestRequest(t, http.MethodPost, ts.URL+"/api/v1/namespaces/b/pods", body)
			raw := hostPortRequireStatus(t, resp, tc.wantStatus)
			if tc.wantMessage != "" && !strings.Contains(raw, tc.wantMessage) {
				t.Errorf("message %q does not contain %q", raw, tc.wantMessage)
			}
		})
	}
}

func TestSocketCollidesWithExistingHostPort(t *testing.T) {
	_, ts, _ := hostPortTestServer(t)
	hostPort := []interface{}{map[string]interface{}{
		"containerPort": 8080, "hostPort": 8080, "protocol": "TCP",
	}}
	owner := hostPortPodBody("default", "owner", nil, hostPort)
	hostPortRequireStatus(t, netTestRequest(t, http.MethodPost, ts.URL+"/api/v1/namespaces/default/pods", owner), http.StatusCreated)

	socket := hostPortPodBody("default", "traefik", map[string]string{
		quadlet.AnnotationSocketActivation: "web=8080",
	}, nil)
	resp := netTestRequest(t, http.MethodPost, ts.URL+"/api/v1/namespaces/default/pods", socket)
	raw := hostPortRequireStatus(t, resp, http.StatusConflict)
	if !strings.Contains(raw, "socket port 8080/TCP collides with hostPort of Pod default/owner") {
		t.Errorf("unexpected conflict message: %s", raw)
	}
}

func TestDeploymentSocketClaimDoesNotCollideWithItself(t *testing.T) {
	for _, replicas := range []int{0, 1} {
		t.Run(fmt.Sprintf("replicas %d", replicas), func(t *testing.T) {
			_, ts, _ := hostPortTestServer(t)
			annotation := map[string]string{quadlet.AnnotationSocketActivation: "web=8443"}
			body := hostPortDeploymentBody("default", "edge", "nginx:1", replicas, annotation)
			url := ts.URL + "/apis/apps/v1/namespaces/default/deployments"
			hostPortRequireStatus(t, netTestRequest(t, http.MethodPost, url, body), http.StatusCreated)

			other := hostPortPodBody("default", "other", annotation, nil)
			resp := netTestRequest(t, http.MethodPost, ts.URL+"/api/v1/namespaces/default/pods", other)
			raw := hostPortRequireStatus(t, resp, http.StatusConflict)
			if !strings.Contains(raw, "socket port 8443/TCP collides with socket port of Deployment default/edge") {
				t.Errorf("unexpected conflict message: %s", raw)
			}

			// An image change must not collide with the Deployment's own
			// claim or with its replica pod.
			body = hostPortDeploymentBody("default", "edge", "nginx:2", replicas, annotation)
			hostPortRequireStatus(t, netTestRequest(t, http.MethodPut, url+"/edge", body), http.StatusOK)

			// Scaling across the 0/1 boundary must not self-collide either.
			scale := map[string]interface{}{"spec": map[string]interface{}{"replicas": 1 - replicas}}
			hostPortRequireStatus(t, netTestRequest(t, http.MethodPatch, url+"/edge", scale), http.StatusOK)
		})
	}
}

func TestNodePortServiceAndSocketCollision(t *testing.T) {
	t.Run("service against existing socket", func(t *testing.T) {
		_, ts, st := hostPortTestServer(t)
		pod := hostPortPodBody("default", "traefik", map[string]string{
			quadlet.AnnotationSocketActivation: "web=30080",
		}, nil)
		hostPortRequireStatus(t, netTestRequest(t, http.MethodPost, ts.URL+"/api/v1/namespaces/default/pods", pod), http.StatusCreated)

		svc := hostPortNodePortServiceBody("default", "np", 80, 30080)
		resp := netTestRequest(t, http.MethodPost, ts.URL+"/api/v1/namespaces/default/services", svc)
		raw := hostPortRequireStatus(t, resp, http.StatusConflict)
		if !strings.Contains(raw, "nodePort 30080/TCP collides with socket port of Pod default/traefik") {
			t.Errorf("unexpected conflict message: %s", raw)
		}
		if _, err := st.GetService("default", "np"); err == nil {
			t.Error("the conflicting Service was left in the store")
		}
	})

	t.Run("socket against existing service", func(t *testing.T) {
		_, ts, _ := hostPortTestServer(t)
		svc := hostPortNodePortServiceBody("default", "np", 80, 30080)
		hostPortRequireStatus(t, netTestRequest(t, http.MethodPost, ts.URL+"/api/v1/namespaces/default/services", svc), http.StatusCreated)

		pod := hostPortPodBody("default", "traefik", map[string]string{
			quadlet.AnnotationSocketActivation: "web=30080",
		}, nil)
		resp := netTestRequest(t, http.MethodPost, ts.URL+"/api/v1/namespaces/default/pods", pod)
		raw := hostPortRequireStatus(t, resp, http.StatusConflict)
		if !strings.Contains(raw, "socket port 30080/TCP collides with nodePort of Service default/np") {
			t.Errorf("unexpected conflict message: %s", raw)
		}
	})

	t.Run("auto assigned nodePort against existing socket", func(t *testing.T) {
		_, ts, st := hostPortTestServer(t)
		pod := hostPortPodBody("default", "traefik", map[string]string{
			quadlet.AnnotationSocketActivation: "web=30000",
		}, nil)
		hostPortRequireStatus(t, netTestRequest(t, http.MethodPost, ts.URL+"/api/v1/namespaces/default/pods", pod), http.StatusCreated)

		svc := hostPortNodePortServiceBody("default", "auto", 80, 0)
		resp := netTestRequest(t, http.MethodPost, ts.URL+"/api/v1/namespaces/default/services", svc)
		raw := hostPortRequireStatus(t, resp, http.StatusConflict)
		if !strings.Contains(raw, "nodePort 30000/TCP collides with socket port of Pod default/traefik") {
			t.Errorf("unexpected conflict message: %s", raw)
		}
		if _, err := st.GetService("default", "auto"); err == nil {
			t.Error("the conflicting Service was left in the store")
		}
	})
}

func TestServiceUpdateAgainstExistingSocket(t *testing.T) {
	_, ts, st := hostPortTestServer(t)
	pod := hostPortPodBody("default", "traefik", map[string]string{
		quadlet.AnnotationSocketActivation: "web=30081",
	}, nil)
	hostPortRequireStatus(t, netTestRequest(t, http.MethodPost, ts.URL+"/api/v1/namespaces/default/pods", pod), http.StatusCreated)

	svc := hostPortNodePortServiceBody("default", "np", 80, 30082)
	hostPortRequireStatus(t, netTestRequest(t, http.MethodPost, ts.URL+"/api/v1/namespaces/default/services", svc), http.StatusCreated)

	patch := map[string]interface{}{
		"spec": map[string]interface{}{"ports": []interface{}{map[string]interface{}{
			"port": 80, "targetPort": 80, "protocol": "TCP", "nodePort": 30081,
		}}},
	}
	resp := netTestRequest(t, http.MethodPatch, ts.URL+"/api/v1/namespaces/default/services/np", patch)
	raw := hostPortRequireStatus(t, resp, http.StatusConflict)
	if !strings.Contains(raw, "nodePort 30081/TCP collides with socket port of Pod default/traefik") {
		t.Errorf("unexpected conflict message: %s", raw)
	}
	stored, err := st.GetService("default", "np")
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	if got := stored.Spec.Ports[0].NodePort; got != 30082 {
		t.Errorf("refused Service update left nodePort %d, want the previous 30082", got)
	}
}

func TestDryRunSocketConflict(t *testing.T) {
	_, ts, st := hostPortTestServer(t)
	pod := hostPortPodBody("default", "traefik", map[string]string{
		quadlet.AnnotationSocketActivation: "web=8080",
	}, nil)
	hostPortRequireStatus(t, netTestRequest(t, http.MethodPost, ts.URL+"/api/v1/namespaces/default/pods", pod), http.StatusCreated)

	other := hostPortPodBody("default", "dryrun", map[string]string{
		quadlet.AnnotationSocketActivation: "web=8080",
	}, nil)
	resp := netTestRequest(t, http.MethodPost, ts.URL+"/api/v1/namespaces/default/pods?dryRun=All", other)
	hostPortRequireStatus(t, resp, http.StatusConflict)
	if _, err := st.GetPod("default", "dryrun"); err == nil {
		t.Error("the dry-run Pod was stored")
	}
}

func TestApplySocketConflict(t *testing.T) {
	_, ts, st := hostPortTestServer(t)
	pod := hostPortPodBody("default", "traefik", map[string]string{
		quadlet.AnnotationSocketActivation: "web=8080",
	}, nil)
	hostPortRequireStatus(t, netTestRequest(t, http.MethodPost, ts.URL+"/api/v1/namespaces/default/pods", pod), http.StatusCreated)

	applied := []byte(`apiVersion: v1
kind: Pod
metadata:
  name: applied
  namespace: default
  annotations:
    q8s.io/socket-activation: web=8080
spec:
  containers:
  - name: applied
    image: nginx
`)
	req, err := http.NewRequest(http.MethodPatch, ts.URL+"/api/v1/namespaces/default/pods/applied", bytes.NewReader(applied))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/apply-patch+yaml")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw := hostPortRequireStatus(t, resp, http.StatusConflict)
	if !strings.Contains(raw, "socket port 8080/TCP collides with socket port of Pod default/traefik") {
		t.Errorf("unexpected apply conflict message: %s", raw)
	}
	if _, err := st.GetPod("default", "applied"); err == nil {
		t.Error("the conflicting applied Pod was stored")
	}
}

func TestReplicaPodPatchDoesNotCollideWithItsDeployment(t *testing.T) {
	_, ts, st := hostPortTestServer(t)
	annotation := map[string]string{quadlet.AnnotationSocketActivation: "web=8443"}
	body := hostPortDeploymentBody("default", "edge", "nginx:1", 1, annotation)
	url := ts.URL + "/apis/apps/v1/namespaces/default/deployments"
	hostPortRequireStatus(t, netTestRequest(t, http.MethodPost, url, body), http.StatusCreated)

	var replica string
	for _, pod := range st.AllPods() {
		if podOwnedByDeployment(pod, "edge") {
			replica = pod.Name
		}
	}
	if replica == "" {
		t.Fatal("deployment edge has no replica pod")
	}
	patch := map[string]interface{}{"metadata": map[string]interface{}{"labels": map[string]interface{}{"tier": "edge"}}}
	resp := netTestRequest(t, http.MethodPatch, ts.URL+"/api/v1/namespaces/default/pods/"+replica, patch)
	hostPortRequireStatus(t, resp, http.StatusOK)
}
