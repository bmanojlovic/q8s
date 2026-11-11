package server_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"q8s/internal/server"
	"q8s/internal/store"
)

func middlewareObject(ns, name string, spec map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "traefik.io/v1alpha1",
		"kind":       "Middleware",
		"metadata":   map[string]interface{}{"name": name, "namespace": ns},
		"spec":       spec,
	}
}

func ingressRouteTCPObject(ns, name string, spec map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "traefik.io/v1alpha1",
		"kind":       "IngressRouteTCP",
		"metadata":   map[string]interface{}{"name": name, "namespace": ns},
		"spec":       spec,
	}
}

func tcpSpec(entryPoint, svcName string, port interface{}) map[string]interface{} {
	return map[string]interface{}{
		"entryPoints": []interface{}{entryPoint},
		"routes": []interface{}{map[string]interface{}{
			"match": "HostSNI(`*`)",
			"services": []interface{}{map[string]interface{}{
				"name": svcName,
				"port": port,
			}},
		}},
	}
}

func getWithAccept(t *testing.T, url, accept string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", accept)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}

func readTraefikFile(t *testing.T, dir, filename string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filename))
	if err != nil {
		t.Fatalf("read traefik file %s: %v", filename, err)
	}
	return string(b)
}

func TestMiddlewareCRUD(t *testing.T) {
	ts, dir := ingressTestServer(t)
	collection := ts.URL + "/apis/traefik.io/v1alpha1/namespaces/default/middlewares"

	resp := post(t, collection, middlewareObject("default", "allow", map[string]interface{}{
		"ipAllowList": map[string]interface{}{"sourceRange": []string{"203.0.113.7/32"}},
	}))
	assertStatus(t, resp, http.StatusCreated)
	created := decodeBody(t, resp)
	assertKind(t, created, "Middleware")
	metadata, _ := created["metadata"].(map[string]interface{})
	if metadata["uid"] == nil || metadata["resourceVersion"] == nil || metadata["creationTimestamp"] == nil {
		t.Fatalf("server-owned metadata missing: %v", metadata)
	}
	readTraefikFile(t, dir, "middleware-default-allow.yaml")

	resp = get(t, collection)
	assertStatus(t, resp, http.StatusOK)
	list := decodeBody(t, resp)
	assertKind(t, list, "MiddlewareList")
	if items, _ := list["items"].([]interface{}); len(items) != 1 {
		t.Fatalf("expected one item, got %v", list["items"])
	}

	resp = get(t, collection+"/allow")
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp = getWithAccept(t, collection, "application/json;as=Table;v=v1;g=meta.k8s.io, application/json")
	assertStatus(t, resp, http.StatusOK)
	table := decodeBody(t, resp)
	assertKind(t, table, "Table")
	columns, _ := table["columnDefinitions"].([]interface{})
	if len(columns) != 2 {
		t.Fatalf("expected NAME and AGE columns, got %v", columns)
	}
	if columns[0].(map[string]interface{})["name"] != "Name" || columns[1].(map[string]interface{})["name"] != "Age" {
		t.Fatalf("unexpected table columns: %v", columns)
	}

	resp = do(t, http.MethodPut, collection+"/allow", middlewareObject("default", "allow", map[string]interface{}{
		"redirectRegex": map[string]interface{}{"regex": "^https://www\\.example\\.com/(.*)", "replacement": "https://example.com/${1}"},
	}))
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()
	if cfg := readTraefikFile(t, dir, "middleware-default-allow.yaml"); !strings.Contains(cfg, "redirectRegex:") {
		t.Fatalf("PUT did not rewrite the file:\n%s", cfg)
	}

	resp = do(t, http.MethodPatch, collection+"/allow", map[string]interface{}{
		"spec": map[string]interface{}{
			"redirectRegex": map[string]interface{}{"permanent": true},
		},
	})
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()
	resp = get(t, collection+"/allow")
	assertStatus(t, resp, http.StatusOK)
	got := decodeBody(t, resp)
	spec, _ := got["spec"].(map[string]interface{})
	redirect, _ := spec["redirectRegex"].(map[string]interface{})
	if redirect["permanent"] != true || redirect["replacement"] != "https://example.com/${1}" {
		t.Fatalf("merge patch did not merge redirectRegex: %v", redirect)
	}

	// Cluster-wide list sees objects from every namespace.
	resp = post(t, ts.URL+"/apis/traefik.io/v1alpha1/namespaces/other/middlewares", middlewareObject("other", "allow2", map[string]interface{}{
		"headers": map[string]interface{}{"frameDeny": true},
	}))
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()
	resp = get(t, ts.URL+"/apis/traefik.io/v1alpha1/middlewares")
	assertStatus(t, resp, http.StatusOK)
	all := decodeBody(t, resp)
	assertKind(t, all, "MiddlewareList")
	if items, _ := all["items"].([]interface{}); len(items) != 2 {
		t.Fatalf("cluster-wide list = %v", all["items"])
	}

	resp = do(t, http.MethodDelete, collection+"/allow", nil)
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()
	if _, err := os.Stat(filepath.Join(dir, "middleware-default-allow.yaml")); !os.IsNotExist(err) {
		t.Fatalf("middleware file was not removed: %v", err)
	}
	resp = get(t, collection+"/allow")
	assertStatus(t, resp, http.StatusNotFound)
	resp.Body.Close()

	// deletecollection
	resp = do(t, http.MethodDelete, ts.URL+"/apis/traefik.io/v1alpha1/namespaces/other/middlewares", nil)
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()
}

func TestIngressRouteTCPCRUD(t *testing.T) {
	ts, dir := ingressTestServer(t)
	collection := ts.URL + "/apis/traefik.io/v1alpha1/namespaces/default/ingressroutetcps"

	resp := post(t, collection, ingressRouteTCPObject("default", "smtp", tcpSpec("smtp", "stalwart", 25)))
	assertStatus(t, resp, http.StatusCreated)
	created := decodeBody(t, resp)
	assertKind(t, created, "IngressRouteTCP")
	readTraefikFile(t, dir, "tcp-default-smtp.yaml")

	resp = get(t, collection)
	assertStatus(t, resp, http.StatusOK)
	list := decodeBody(t, resp)
	assertKind(t, list, "IngressRouteTCPList")
	if items, _ := list["items"].([]interface{}); len(items) != 1 {
		t.Fatalf("expected one item, got %v", list["items"])
	}

	resp = get(t, collection+"/smtp")
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp = getWithAccept(t, collection, "application/json;as=Table;v=v1;g=meta.k8s.io, application/json")
	assertStatus(t, resp, http.StatusOK)
	table := decodeBody(t, resp)
	assertKind(t, table, "Table")
	columns, _ := table["columnDefinitions"].([]interface{})
	if len(columns) != 2 {
		t.Fatalf("expected NAME and AGE columns, got %v", columns)
	}

	resp = do(t, http.MethodPut, collection+"/smtp", ingressRouteTCPObject("default", "smtp", tcpSpec("smtps", "stalwart", 465)))
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp = do(t, http.MethodPatch, collection+"/smtp", map[string]interface{}{
		"metadata": map[string]interface{}{"labels": map[string]interface{}{"patched": "yes"}},
	})
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()
	resp = get(t, collection+"/smtp")
	assertStatus(t, resp, http.StatusOK)
	got := decodeBody(t, resp)
	metadata, _ := got["metadata"].(map[string]interface{})
	labels, _ := metadata["labels"].(map[string]interface{})
	if labels["patched"] != "yes" {
		t.Fatalf("patch did not persist label: %v", metadata)
	}

	resp = post(t, ts.URL+"/apis/traefik.io/v1alpha1/namespaces/other/ingressroutetcps", ingressRouteTCPObject("other", "imaps", tcpSpec("imaps", "stalwart", 993)))
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()
	resp = get(t, ts.URL+"/apis/traefik.io/v1alpha1/ingressroutetcps")
	assertStatus(t, resp, http.StatusOK)
	all := decodeBody(t, resp)
	assertKind(t, all, "IngressRouteTCPList")
	if items, _ := all["items"].([]interface{}); len(items) != 2 {
		t.Fatalf("cluster-wide list = %v", all["items"])
	}

	resp = do(t, http.MethodDelete, collection+"/smtp", nil)
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()
	if _, err := os.Stat(filepath.Join(dir, "tcp-default-smtp.yaml")); !os.IsNotExist(err) {
		t.Fatalf("tcp file was not removed: %v", err)
	}
	resp = get(t, collection+"/smtp")
	assertStatus(t, resp, http.StatusNotFound)
	resp.Body.Close()

	resp = do(t, http.MethodDelete, ts.URL+"/apis/traefik.io/v1alpha1/namespaces/other/ingressroutetcps", nil)
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()
}

func TestIngressRouteTCPEntryPointConflict(t *testing.T) {
	ts, _ := ingressTestServer(t)
	first := ts.URL + "/apis/traefik.io/v1alpha1/namespaces/default/ingressroutetcps"
	second := ts.URL + "/apis/traefik.io/v1alpha1/namespaces/other/ingressroutetcps"

	resp := post(t, first, ingressRouteTCPObject("default", "smtp", tcpSpec("smtp", "stalwart", 25)))
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	resp = post(t, second, ingressRouteTCPObject("other", "smtp-other", tcpSpec("smtp", "stalwart", 25)))
	assertStatus(t, resp, http.StatusConflict)
	reason := decodeBody(t, resp)
	if reason["reason"] != "Conflict" {
		t.Fatalf("conflict reason = %v", reason["reason"])
	}

	// Another entrypoint is fine.
	resp = post(t, second, ingressRouteTCPObject("other", "imaps", tcpSpec("imaps", "stalwart", 993)))
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	// Updating the first object onto the second namespace's entrypoint conflicts.
	resp = do(t, http.MethodPut, first+"/smtp", ingressRouteTCPObject("default", "smtp", tcpSpec("imaps", "stalwart", 25)))
	assertStatus(t, resp, http.StatusConflict)
	resp.Body.Close()

	// The failed update did not change the stored object.
	resp = get(t, first+"/smtp")
	assertStatus(t, resp, http.StatusOK)
	got := decodeBody(t, resp)
	spec, _ := got["spec"].(map[string]interface{})
	entryPoints, _ := spec["entryPoints"].([]interface{})
	if len(entryPoints) != 1 || entryPoints[0] != "smtp" {
		t.Fatalf("failed conflict check mutated the object: %v", spec)
	}

	// Deleting the owner frees the entrypoint.
	resp = do(t, http.MethodDelete, second+"/imaps", nil)
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()
	resp = do(t, http.MethodPut, first+"/smtp", ingressRouteTCPObject("default", "smtp", tcpSpec("imaps", "stalwart", 25)))
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()
}

func TestApplyMiddlewareCreateAndUpdate(t *testing.T) {
	ts, dir := ingressTestServer(t)
	url := ts.URL + "/apis/traefik.io/v1alpha1/namespaces/default/middlewares/applied"

	body := `apiVersion: traefik.io/v1alpha1
kind: Middleware
metadata:
  name: applied
  namespace: default
  labels:
    app: demo
spec:
  ipAllowList:
    sourceRange:
      - 10.0.0.0/8
`
	resp := applyPatch(t, url, body)
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	resp = get(t, url)
	assertStatus(t, resp, http.StatusOK)
	created := decodeBody(t, resp)
	spec, _ := created["spec"].(map[string]interface{})
	allow, _ := spec["ipAllowList"].(map[string]interface{})
	if ranges, _ := allow["sourceRange"].([]interface{}); len(ranges) != 1 || ranges[0] != "10.0.0.0/8" {
		t.Fatalf("apply create spec = %v", spec)
	}

	body = strings.Replace(body, "10.0.0.0/8", "192.168.0.0/16", 1)
	resp = applyPatch(t, url+"?fieldManager=Terraform&force=false", body)
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp = get(t, url)
	assertStatus(t, resp, http.StatusOK)
	updated := decodeBody(t, resp)
	spec, _ = updated["spec"].(map[string]interface{})
	allow, _ = spec["ipAllowList"].(map[string]interface{})
	if ranges, _ := allow["sourceRange"].([]interface{}); len(ranges) != 1 || ranges[0] != "192.168.0.0/16" {
		t.Fatalf("apply update spec = %v", spec)
	}
	if cfg := readTraefikFile(t, dir, "middleware-default-applied.yaml"); !strings.Contains(cfg, "192.168.0.0/16") {
		t.Fatalf("apply update did not rewrite the file:\n%s", cfg)
	}
}

func TestTraefikAPIRejectsInvalidObjects(t *testing.T) {
	ts, _ := ingressTestServer(t)
	mwCollection := ts.URL + "/apis/traefik.io/v1alpha1/namespaces/default/middlewares"
	irtCollection := ts.URL + "/apis/traefik.io/v1alpha1/namespaces/default/ingressroutetcps"

	validTCP := func(name string) map[string]interface{} {
		return tcpSpec("smtp", "stalwart", 25)
	}
	twoServices := func() map[string]interface{} {
		spec := tcpSpec("smtp", "stalwart", 25)
		route := spec["routes"].([]interface{})[0].(map[string]interface{})
		route["services"] = []interface{}{
			map[string]interface{}{"name": "stalwart", "port": 25},
			map[string]interface{}{"name": "stalwart2", "port": 26},
		}
		return spec
	}
	withTLS := func(tls map[string]interface{}) map[string]interface{} {
		spec := validTCP("tls")
		spec["tls"] = tls
		return spec
	}
	withRouteField := func(key string, value interface{}) map[string]interface{} {
		spec := validTCP("routefield")
		route := spec["routes"].([]interface{})[0].(map[string]interface{})
		route[key] = value
		return spec
	}
	withProxy := func(version int) map[string]interface{} {
		spec := validTCP("proxy")
		route := spec["routes"].([]interface{})[0].(map[string]interface{})
		svc := route["services"].([]interface{})[0].(map[string]interface{})
		svc["proxyProtocol"] = map[string]interface{}{"version": version}
		return spec
	}

	cases := []struct {
		name string
		kind string
		spec map[string]interface{}
	}{
		{"middleware-zero-types", "Middleware", map[string]interface{}{}},
		{"middleware-two-types", "Middleware", map[string]interface{}{
			"ipAllowList": map[string]interface{}{"sourceRange": []string{"10.0.0.0/8"}},
			"headers":     map[string]interface{}{"frameDeny": true},
		}},
		{"middleware-empty-source-range", "Middleware", map[string]interface{}{
			"ipAllowList": map[string]interface{}{"sourceRange": []string{}},
		}},
		{"middleware-no-source-range", "Middleware", map[string]interface{}{
			"ipAllowList": map[string]interface{}{},
		}},
		{"middleware-bad-source-range", "Middleware", map[string]interface{}{
			"ipAllowList": map[string]interface{}{"sourceRange": []string{"not-an-ip"}},
		}},
		{"middleware-bad-excluded-ip", "Middleware", map[string]interface{}{
			"ipAllowList": map[string]interface{}{
				"sourceRange": []string{"10.0.0.0/8"},
				"ipStrategy":  map[string]interface{}{"excludedIPs": []string{"999.1.1.1"}},
			},
		}},
		{"middleware-negative-depth", "Middleware", map[string]interface{}{
			"ipAllowList": map[string]interface{}{
				"sourceRange": []string{"10.0.0.0/8"},
				"ipStrategy":  map[string]interface{}{"depth": -1},
			},
		}},
		{"middleware-bad-regex", "Middleware", map[string]interface{}{
			"redirectRegex": map[string]interface{}{"regex": "("},
		}},
		{"middleware-empty-regex", "Middleware", map[string]interface{}{
			"redirectRegex": map[string]interface{}{"replacement": "https://example.com"},
		}},
		{"middleware-bad-scheme", "Middleware", map[string]interface{}{
			"redirectScheme": map[string]interface{}{"scheme": "ftp"},
		}},
		{"middleware-bad-port", "Middleware", map[string]interface{}{
			"redirectScheme": map[string]interface{}{"scheme": "https", "port": "44a"},
		}},
		{"middleware-bad-header-name", "Middleware", map[string]interface{}{
			"headers": map[string]interface{}{"customRequestHeaders": map[string]string{"Bad Header": "x"}},
		}},
		{"middleware-crlf-header-value", "Middleware", map[string]interface{}{
			"headers": map[string]interface{}{"customResponseHeaders": map[string]string{"X-Test": "a\r\nInjected: yes"}},
		}},
		{"middleware-nul-header-value", "Middleware", map[string]interface{}{
			"headers": map[string]interface{}{"customResponseHeaders": map[string]string{"X-Test": "a\x00b"}},
		}},
		{"middleware-unknown-type", "Middleware", map[string]interface{}{
			"ipAllowList": map[string]interface{}{"sourceRange": []string{"10.0.0.0/8"}},
			"basicAuth":   map[string]interface{}{"secret": "users"},
		}},
		{"tcp-no-entrypoints", "IngressRouteTCP", map[string]interface{}{
			"routes": validTCP("x")["routes"],
		}},
		{"tcp-bad-entrypoint", "IngressRouteTCP", map[string]interface{}{
			"entryPoints": []interface{}{"bad name"},
			"routes":      validTCP("x")["routes"],
		}},
		{"tcp-no-routes", "IngressRouteTCP", map[string]interface{}{
			"entryPoints": []interface{}{"smtp"},
		}},
		{"tcp-wrong-match", "IngressRouteTCP", map[string]interface{}{
			"entryPoints": []interface{}{"smtp"},
			"routes": []interface{}{map[string]interface{}{
				"match":    "Host(`smtp.example.com`)",
				"services": []interface{}{map[string]interface{}{"name": "stalwart", "port": 25}},
			}},
		}},
		{"tcp-no-service", "IngressRouteTCP", map[string]interface{}{
			"entryPoints": []interface{}{"smtp"},
			"routes": []interface{}{map[string]interface{}{
				"match":    "HostSNI(`*`)",
				"services": []interface{}{},
			}},
		}},
		{"tcp-two-services", "IngressRouteTCP", twoServices()},
		{"tcp-bad-service-name", "IngressRouteTCP", tcpSpec("smtp", "Bad_Name", 25)},
		{"tcp-port-zero", "IngressRouteTCP", tcpSpec("smtp", "stalwart", 0)},
		{"tcp-port-too-high", "IngressRouteTCP", tcpSpec("smtp", "stalwart", 70000)},
		{"tcp-bad-port-name", "IngressRouteTCP", tcpSpec("smtp", "stalwart", "Bad Port")},
		{"tcp-proxy-version-3", "IngressRouteTCP", withProxy(3)},
		{"tcp-proxy-version-0", "IngressRouteTCP", withProxy(0)},
		{"tcp-tls-extra-field", "IngressRouteTCP", withTLS(map[string]interface{}{"passthrough": true, "certResolver": "le"})},
		{"tcp-route-extra-field", "IngressRouteTCP", withRouteField("priority", 10)},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name := fmt.Sprintf("bad-%d", i)
			var resp *http.Response
			if tc.kind == "Middleware" {
				resp = post(t, mwCollection, middlewareObject("default", name, tc.spec))
			} else {
				resp = post(t, irtCollection, ingressRouteTCPObject("default", name, tc.spec))
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnprocessableEntity {
				body := decodeBody(t, resp)
				t.Fatalf("status = %d, want 422: %v", resp.StatusCode, body)
			}
			body := decodeBody(t, resp)
			if body["reason"] != "Invalid" {
				t.Fatalf("reason = %v", body["reason"])
			}
		})
	}

	// A patch that introduces an unknown field is rejected too, not stored.
	resp := post(t, mwCollection, middlewareObject("default", "patch-unknown", map[string]interface{}{
		"ipAllowList": map[string]interface{}{"sourceRange": []string{"10.0.0.0/8"}},
	}))
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()
	resp = do(t, http.MethodPatch, mwCollection+"/patch-unknown", map[string]interface{}{
		"spec": map[string]interface{}{"basicAuth": map[string]interface{}{"secret": "users"}},
	})
	assertStatus(t, resp, http.StatusUnprocessableEntity)
	resp.Body.Close()
}

func TestMiddlewareRenderingExact(t *testing.T) {
	ts, dir := ingressTestServer(t)
	cases := []struct {
		name string
		spec map[string]interface{}
		want string
	}{
		{
			name: "ip",
			spec: map[string]interface{}{"ipAllowList": map[string]interface{}{
				"sourceRange":      []string{"203.0.113.7/32", "10.0.0.0/8"},
				"ipStrategy":       map[string]interface{}{"depth": 1, "excludedIPs": []string{"1.2.3.4"}},
				"rejectStatusCode": 403,
			}},
			want: `http:
  middlewares:
    default-ip:
      ipAllowList:
        ipStrategy:
          depth: 1
          excludedIPs:
          - 1.2.3.4
        rejectStatusCode: 403
        sourceRange:
        - 203.0.113.7/32
        - 10.0.0.0/8
`,
		},
		{
			name: "regex",
			spec: map[string]interface{}{"redirectRegex": map[string]interface{}{
				"regex": "^http://example\\.com/(.*)", "replacement": "https://example.com/${1}", "permanent": true,
			}},
			want: `http:
  middlewares:
    default-regex:
      redirectRegex:
        permanent: true
        regex: ^http://example\.com/(.*)
        replacement: https://example.com/${1}
`,
		},
		{
			name: "scheme",
			spec: map[string]interface{}{"redirectScheme": map[string]interface{}{
				"scheme": "https", "port": "443", "permanent": true,
			}},
			want: `http:
  middlewares:
    default-scheme:
      redirectScheme:
        permanent: true
        port: "443"
        scheme: https
`,
		},
		{
			name: "headers",
			spec: map[string]interface{}{"headers": map[string]interface{}{
				"customRequestHeaders":    map[string]string{"X-Real-IP": "1"},
				"customResponseHeaders":   map[string]string{"X-Frame-Options": "SAMEORIGIN"},
				"stsSeconds":              31536000,
				"stsIncludeSubdomains":    true,
				"stsPreload":              true,
				"forceSTSHeader":          true,
				"frameDeny":               true,
				"contentTypeNosniff":      true,
				"browserXssFilter":        true,
				"referrerPolicy":          "no-referrer",
				"contentSecurityPolicy":   "default-src 'self'",
				"permissionsPolicy":       "geolocation=()",
				"customFrameOptionsValue": "SAMEORIGIN",
			}},
			want: `http:
  middlewares:
    default-headers:
      headers:
        browserXssFilter: true
        contentSecurityPolicy: default-src 'self'
        contentTypeNosniff: true
        customFrameOptionsValue: SAMEORIGIN
        customRequestHeaders:
          X-Real-IP: "1"
        customResponseHeaders:
          X-Frame-Options: SAMEORIGIN
        forceSTSHeader: true
        frameDeny: true
        permissionsPolicy: geolocation=()
        referrerPolicy: no-referrer
        stsIncludeSubdomains: true
        stsPreload: true
        stsSeconds: 31536000
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := post(t, ts.URL+"/apis/traefik.io/v1alpha1/namespaces/default/middlewares",
				middlewareObject("default", tc.name, tc.spec))
			assertStatus(t, resp, http.StatusCreated)
			resp.Body.Close()
			if got := readTraefikFile(t, dir, "middleware-default-"+tc.name+".yaml"); got != tc.want {
				t.Fatalf("rendered file:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
}

func TestIngressRouteTCPRenderingAndPodChurn(t *testing.T) {
	ts, dir := ingressTestServer(t)

	resp := post(t, ts.URL+"/api/v1/namespaces/default/services", map[string]interface{}{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]interface{}{"name": "stalwart", "namespace": "default"},
		"spec": map[string]interface{}{
			"selector": map[string]string{"app": "mail"},
			"ports":    []interface{}{map[string]interface{}{"name": "smtp", "port": 25, "targetPort": 2525}},
		},
	})
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	postPodWithPort(t, ts.URL, "default", "mail-0", map[string]string{"app": "mail"}, 2525, 0)

	spec := tcpSpec("smtp", "stalwart", 25)
	route := spec["routes"].([]interface{})[0].(map[string]interface{})
	route["services"].([]interface{})[0].(map[string]interface{})["proxyProtocol"] = map[string]interface{}{"version": 2}
	spec["tls"] = map[string]interface{}{"passthrough": true}

	resp = post(t, ts.URL+"/apis/traefik.io/v1alpha1/namespaces/default/ingressroutetcps",
		ingressRouteTCPObject("default", "smtp", spec))
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	want := `tcp:
  routers:
    default-smtp-0:
      entryPoints:
      - smtp
      rule: HostSNI(` + "`*`" + `)
      service: default-smtp-0
      tls:
        passthrough: true
  serversTransports:
    default-smtp-0:
      proxyProtocol:
        version: 2
  services:
    default-smtp-0:
      loadBalancer:
        servers:
        - address: mail-0.default:2525
        serversTransport: default-smtp-0
`
	if got := readTraefikFile(t, dir, "tcp-default-smtp.yaml"); got != want {
		t.Fatalf("rendered TCP file:\n%s\nwant:\n%s", got, want)
	}

	// A new matching pod is added to the servers; deleting it removes it again.
	postPodWithPort(t, ts.URL, "default", "mail-1", map[string]string{"app": "mail"}, 2525, 0)
	cfg := readTraefikFile(t, dir, "tcp-default-smtp.yaml")
	if !strings.Contains(cfg, "address: mail-0.default:2525") || !strings.Contains(cfg, "address: mail-1.default:2525") {
		t.Fatalf("pod create did not regenerate the TCP file:\n%s", cfg)
	}
	resp = do(t, http.MethodDelete, ts.URL+"/api/v1/namespaces/default/pods/mail-1", nil)
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()
	cfg = readTraefikFile(t, dir, "tcp-default-smtp.yaml")
	if strings.Contains(cfg, "mail-1.default:2525") {
		t.Fatalf("pod delete did not regenerate the TCP file:\n%s", cfg)
	}

	// Service churn is a backend change too: deleting the Service empties the
	// server list, recreating it brings the pod back.
	resp = do(t, http.MethodDelete, ts.URL+"/api/v1/namespaces/default/services/stalwart", nil)
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()
	cfg = readTraefikFile(t, dir, "tcp-default-smtp.yaml")
	if strings.Contains(cfg, "mail-0.default:2525") {
		t.Fatalf("Service delete did not regenerate the TCP file:\n%s", cfg)
	}

	resp = post(t, ts.URL+"/api/v1/namespaces/default/services", map[string]interface{}{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]interface{}{"name": "stalwart", "namespace": "default"},
		"spec": map[string]interface{}{
			"selector": map[string]string{"app": "mail"},
			"ports":    []interface{}{map[string]interface{}{"name": "smtp", "port": 25, "targetPort": 2525}},
		},
	})
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()
	cfg = readTraefikFile(t, dir, "tcp-default-smtp.yaml")
	if !strings.Contains(cfg, "address: mail-0.default:2525") {
		t.Fatalf("Service create did not regenerate the TCP file:\n%s", cfg)
	}
}

func TestIngressMiddlewareProviderRewrittenForFileProvider(t *testing.T) {
	ts, traefikDir := ingressTestServer(t)
	body := ingressBody("default", "mw", "example.com", "myservice", 80)
	body["metadata"].(map[string]interface{})["annotations"] = map[string]interface{}{
		"traefik.ingress.kubernetes.io/router.middlewares": "default-allow@kubernetescrd, external@file, plain",
	}
	resp := post(t, ts.URL+"/apis/networking.k8s.io/v1/namespaces/default/ingresses", body)
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	cfg := readTraefikConfig(t, traefikDir, "default", "mw")
	want := `      middlewares: ["default-allow@file", "external@file", "plain"]`
	if !strings.Contains(cfg, want) {
		t.Fatalf("middleware reference was not rewritten:\n%s\nwant line: %s", cfg, want)
	}
}

func TestReconcileIngressConfigsWritesTraefikSubsetFiles(t *testing.T) {
	traefikDir := t.TempDir()
	certPEM, keyPEM := genTestCert(t)
	st := store.New()
	srv, err := server.New(server.Config{
		Store:      st,
		CertPEM:    certPEM,
		KeyPEM:     keyPEM,
		TraefikDir: traefikDir,
		QuadletDir: t.TempDir(),
		ConfigDir:  filepath.Join(t.TempDir(), "configmaps"),
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	resp := post(t, ts.URL+"/apis/traefik.io/v1alpha1/namespaces/default/middlewares",
		middlewareObject("default", "allow", map[string]interface{}{
			"ipAllowList": map[string]interface{}{"sourceRange": []string{"10.0.0.0/8"}},
		}))
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()
	resp = post(t, ts.URL+"/apis/traefik.io/v1alpha1/namespaces/default/ingressroutetcps",
		ingressRouteTCPObject("default", "smtp", tcpSpec("smtp", "stalwart", 25)))
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	middlewareFile := filepath.Join(traefikDir, "middleware-default-allow.yaml")
	tcpFile := filepath.Join(traefikDir, "tcp-default-smtp.yaml")
	if err := os.Remove(middlewareFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(tcpFile); err != nil {
		t.Fatal(err)
	}

	srv.ReconcileIngressConfigs()
	if _, err := os.Stat(middlewareFile); err != nil {
		t.Fatalf("startup reconcile did not rewrite the Middleware file: %v", err)
	}
	if _, err := os.Stat(tcpFile); err != nil {
		t.Fatalf("startup reconcile did not rewrite the IngressRouteTCP file: %v", err)
	}
}
